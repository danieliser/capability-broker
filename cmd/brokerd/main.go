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
	"syscall"
	"time"

	"github.com/danieliser/capability-broker/pkg/adapter"
	"github.com/danieliser/capability-broker/pkg/adapters/upstream"
	"github.com/danieliser/capability-broker/pkg/api"
	"github.com/danieliser/capability-broker/pkg/audit"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/buildinfo"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/config"
	"github.com/danieliser/capability-broker/pkg/connectors"
	"github.com/danieliser/capability-broker/pkg/durable"
	"github.com/danieliser/capability-broker/pkg/mcpfront"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "client", "grant", "queries", "receipts", "status":
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
	specs, err := connectors.Load(cfg.ConnectorsPath(), cfg.Secrets)
	if err != nil {
		return err
	}
	if len(specs) == 0 {
		logger.Warn("no connectors configured; copy connectors.example.yaml to the data dir", "path", cfg.ConnectorsPath())
	}

	store := auth.NewStore(db)
	writer := audit.NewWriter(db)
	adapters := map[string]adapter.Adapter{}
	var ups []*upstream.Adapter
	for _, spec := range specs {
		ad := upstream.New(spec, cat, func(msg string, kv ...any) { logger.Info(msg, kv...) })
		adapters[spec.Name] = ad
		ups = append(ups, ad)
	}
	for _, ad := range ups {
		ad := ad
		go func() {
			if err := ad.Start(ctx); err != nil {
				logger.Warn("connector not started; degraded until first successful call", "connector", ad.Connector(), "err", err.Error())
			}
		}()
		defer ad.Close()
	}
	front := &mcpfront.Front{Store: store, Catalog: cat, Audit: writer, Adapters: adapters, Logger: logger, SessionTimeout: 30 * time.Minute, Stateless: stateless}
	names := make([]string, 0, len(adapters))
	for name := range adapters {
		names = append(names, name)
	}

	frontSrv := &http.Server{Addr: cfg.FrontAddr(), Handler: api.FrontHandler(front, names), ReadHeaderTimeout: 10 * time.Second}
	admin := &api.Admin{Token: adminToken, Store: store, Catalog: cat, Audit: writer, Front: front, Adapters: adapters, DB: db,
		FrontURL: "http://" + cfg.FrontAddr(), Started: time.Now()}
	_ = os.Remove(cfg.AdminSocketPath())
	ln, err := net.Listen("unix", cfg.AdminSocketPath())
	if err != nil {
		return fmt.Errorf("admin socket: %w", err)
	}
	_ = os.Chmod(cfg.AdminSocketPath(), 0o600)
	adminSrv := &http.Server{Handler: admin.Handler(), ReadHeaderTimeout: 10 * time.Second}

	errCh := make(chan error, 2)
	go func() {
		logger.Info("front listening", "addr", cfg.FrontAddr(), "connectors", names, "stateless", stateless)
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
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = frontSrv.Shutdown(shutdownCtx)
	_ = adminSrv.Shutdown(shutdownCtx)
	_ = os.Remove(cfg.AdminSocketPath())
	return nil
}
