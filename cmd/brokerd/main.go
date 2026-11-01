// brokerd is the capability broker daemon: one always-on process that holds the
// upstream credentials, exposes each connector as an ordinary MCP server on
// loopback, and enforces per-grant policy with receipts. Subcommands (client,
// grant, queries, receipts, status) talk to the running daemon's admin socket.
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
	"github.com/danieliser/capability-broker/pkg/adapters/clickhouse"
	"github.com/danieliser/capability-broker/pkg/adapters/memory"
	"github.com/danieliser/capability-broker/pkg/adapters/wordpress"
	"github.com/danieliser/capability-broker/pkg/api"
	"github.com/danieliser/capability-broker/pkg/audit"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/buildinfo"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/config"
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
	flag.StringVar(&cfg.DataDir, "data-dir", cfg.DataDir, "Data root (database, admin socket, admin token, secrets)")
	noWP := flag.Bool("no-wordpress", false, "Do not start the wp-mcp-router child")
	stateless := flag.Bool("stateless", false, "Serve sessionless Streamable HTTP (MCP 2026-07-28). Off by default: Claude Code 2.1.316 listed no tools from a sessionless endpoint; Codex 0.166 and the Go SDK client work either way")
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
	if err := run(cfg, *noWP, *stateless); err != nil {
		log.Fatalf("brokerd: %v", err)
	}
}

func run(cfg config.Config, noWP bool, stateless bool) error {
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
	cat, err := catalog.Load()
	if err != nil {
		return err
	}
	if n, err := api.RestoreOverlays(ctx, db, cat); err != nil {
		return fmt.Errorf("restore catalog overlays: %w", err)
	} else if n > 0 {
		logger.Info("catalog overlays restored", "projects", n)
	}

	store := auth.NewStore(db)
	writer := audit.NewWriter(db)
	adapters := map[string]adapter.Adapter{
		"clickhouse": clickhouse.New(cfg.ClickHouse, cfg.Secrets.ClickHousePassword, cat),
		"memory":     memory.New(cfg.Memory, cfg.Secrets.AutoMemAPIKey, cat),
	}
	var sites mcpfront.SiteHostSource
	if !noWP {
		wp := wordpress.New(cfg.WordPress, cat)
		adapters["wp-mcp-router"] = wp
		sites = wp
		go func() {
			if err := wp.Start(ctx); err != nil {
				logger.Warn("wp-mcp-router not started; connector degraded until first successful call", "err", err.Error())
			} else {
				logger.Info("wp-mcp-router child connected")
			}
		}()
		defer wp.Close()
	}
	front := &mcpfront.Front{Store: store, Catalog: cat, Audit: writer, Adapters: adapters, Sites: sites, Logger: logger, SessionTimeout: 30 * time.Minute, Stateless: stateless}
	connectors := make([]string, 0, len(adapters))
	for name := range adapters {
		connectors = append(connectors, name)
	}

	frontSrv := &http.Server{Addr: cfg.FrontAddr(), Handler: api.FrontHandler(front, connectors), ReadHeaderTimeout: 10 * time.Second}
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
		logger.Info("front listening", "addr", cfg.FrontAddr(), "connectors", connectors, "stateless", stateless)
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
