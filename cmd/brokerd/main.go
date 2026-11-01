// brokerd is the capability broker daemon: one always-on process that holds the
// upstream credentials, hosts any stdio MCP server declared in connectors.yaml,
// exposes each as an ordinary MCP server on loopback, and enforces per-grant
// policy with receipts. Subcommands talk to the running daemon's admin socket.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/danieliser/capability-broker/pkg/api"
	"github.com/danieliser/capability-broker/pkg/audit"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/buildinfo"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/config"
	"github.com/danieliser/capability-broker/pkg/durable"
	"github.com/danieliser/capability-broker/pkg/mcpfront"
	"github.com/danieliser/capability-broker/pkg/registry"
	"github.com/danieliser/capability-broker/pkg/reload"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "client", "grant", "queries", "receipts", "status", "reload":
			os.Exit(runSubcommand(os.Args[1], os.Args[2:]))
		}
	}
	cfg := config.FromEnv()
	showVersion := flag.Bool("version", false, "Print version and exit")
	showBuild := flag.Bool("build-info", false, "Print build identity as JSON and exit")
	requireBuild := flag.String("require-build", "", "Require exact version@commit identity and exit")
	flag.StringVar(&cfg.Host, "host", cfg.Host, "Front bind address (loopback only)")
	flag.IntVar(&cfg.Port, "port", cfg.Port, "Front port")
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Data root (database, admin socket, admin token, secrets, connectors.yaml, catalog.d)")
	stateless := flag.Bool("stateless", true, "Serve sessionless Streamable HTTP (MCP 2026-07-28); --stateless=false keeps Mcp-Session-Id sessions, which sampling and elicitation relay need")
	flag.Parse()

	id := buildinfo.Current()
	if *showVersion {
		fmt.Println(id.Version)
		return
	}
	if *showBuild {
		_ = json.NewEncoder(os.Stdout).Encode(id)
		return
	}
	if *requireBuild != "" {
		if err := id.Verify(*requireBuild); err != nil {
			log.Fatalf("build qualification failed: %v", err)
		}
		fmt.Printf("verified brokerd %s@%s\n", id.Version, id.Commit)
		return
	}
	if ip := net.ParseIP(cfg.Host); ip == nil || !ip.IsLoopback() {
		log.Fatalf("front host must be a loopback address, got %q", cfg.Host)
	}
	if err := run(cfg, *stateless); err != nil {
		log.Fatalf("brokerd: %v", err)
	}
}

func run(cfg config.Config, stateless bool) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger.Info("brokerd starting", "version", buildinfo.Version, "commit", buildinfo.Commit, "data_dir", cfg.DataDir)
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return err
	}
	_ = os.Chmod(cfg.DataDir, 0o700)
	if err := cfg.LoadSecrets(); err != nil {
		return err
	}
	adminToken, err := auth.LoadOrCreateAdminToken(cfg.AdminTokenPath())
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := durable.Open(ctx, cfg.DatabasePath())
	if err != nil {
		return err
	}
	defer db.Close()
	cat, err := catalog.Load(cfg.CatalogDir())
	if err != nil {
		return err
	}
	if n, err := api.RestoreOverlays(ctx, db, cat); err != nil {
		return fmt.Errorf("restore catalog overlays: %w", err)
	} else if n > 0 {
		logger.Info("named-query overlays restored", "projects", n)
	}
	store := auth.NewStore(db)
	writer := audit.NewWriter(db)
	reg := registry.New()
	front := &mcpfront.Front{Store: store, Catalog: cat, Audit: writer, Registry: reg, Logger: logger, SessionTimeout: 30 * time.Minute, Stateless: stateless}
	reloader := &reload.Reloader{Config: cfg, Catalog: cat, Registry: reg, Front: front, Base: ctx,
		Log: func(msg string, kv ...any) { logger.Info(msg, kv...) }}
	if res, err := reloader.Apply(ctx); err != nil {
		return fmt.Errorf("initial load: %w", err)
	} else if len(res.Added) == 0 {
		logger.Warn("no connectors configured; copy connectors.example.yaml to the data dir", "path", cfg.ConnectorsPath())
	}
	defer closeAll(reg, 5*time.Second)
	// Hot reload: poll the data-root files, and apply on SIGHUP too.
	go reloader.Watch(ctx, 2*time.Second)
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-hup:
				if _, err := reloader.Apply(ctx); err != nil {
					logger.Warn("reload refused; keeping the last good state", "err", err.Error())
				}
			}
		}
	}()

	frontSrv := &http.Server{Addr: cfg.FrontAddr(), Handler: api.FrontHandler(front), ReadHeaderTimeout: 10 * time.Second}
	admin := &api.Admin{Token: adminToken, Store: store, Catalog: cat, Audit: writer, Front: front, Registry: reg, DB: db,
		FrontURL: "http://" + cfg.FrontAddr(), Started: time.Now(),
		Reload: func(ctx context.Context) (map[string]any, error) {
			res, err := reloader.Apply(ctx)
			if err != nil {
				return nil, err
			}
			b, _ := json.Marshal(res)
			var out map[string]any
			_ = json.Unmarshal(b, &out)
			return out, nil
		}}
	_ = os.Remove(cfg.AdminSocketPath())
	ln, err := net.Listen("unix", cfg.AdminSocketPath())
	if err != nil {
		return fmt.Errorf("admin socket: %w", err)
	}
	_ = os.Chmod(cfg.AdminSocketPath(), 0o600)
	adminSrv := &http.Server{Handler: admin.Handler(), ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("front listening", "addr", cfg.FrontAddr(), "connectors", reg.Names(), "stateless", stateless)
		if err := frontSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("front: %w", err)
		}
	}()
	go func() {
		logger.Info("admin listening", "socket", cfg.AdminSocketPath())
		if err := adminSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- fmt.Errorf("admin: %w", err)
		}
	}()
	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case err := <-errCh:
		return err
	}
	// Clients hold long-lived streams (SSE, subscriptions/listen); a graceful drain would wait on them, so give
	// in-flight calls 2s and then close.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := frontSrv.Shutdown(shutdownCtx); err != nil {
		_ = frontSrv.Close()
	}
	if err := adminSrv.Shutdown(shutdownCtx); err != nil {
		_ = adminSrv.Close()
	}
	_ = os.Remove(cfg.AdminSocketPath())
	return nil
}

// closeAll stops every child in parallel and gives up after limit.
func closeAll(reg *registry.Registry, limit time.Duration) {
	done := make(chan struct{})
	var wg sync.WaitGroup
	for _, ad := range reg.Snapshot() {
		if c, ok := ad.(interface{ Close() error }); ok {
			wg.Add(1)
			go func() { defer wg.Done(); _ = c.Close() }()
		}
	}
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(limit):
	}
}
