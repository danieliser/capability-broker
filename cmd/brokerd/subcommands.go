package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/danieliser/capability-broker/pkg/api"
	"github.com/danieliser/capability-broker/pkg/config"
)

func adminClient() (*api.AdminClient, config.Config, error) {
	cfg := config.FromEnv()
	c, err := api.NewAdminClient(cfg.AdminSocketPath(), cfg.AdminTokenPath())
	return c, cfg, err
}

func printJSON(v any) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func fail(format string, args ...any) int {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	return 1
}

func runSubcommand(name string, args []string) int {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	switch name {
	case "status":
		c, _, err := adminClient()
		if err != nil {
			return fail("%v", err)
		}
		var out map[string]any
		if err := c.Do(ctx, "GET", "/v1/status", nil, &out); err != nil {
			return fail("%v", err)
		}
		printJSON(out)
		return 0
	case "reload":
		c, _, err := adminClient()
		if err != nil {
			return fail("%v", err)
		}
		var out map[string]any
		if err := c.Do(ctx, "POST", "/v1/reload", nil, &out); err != nil {
			return fail("%v", err)
		}
		printJSON(out)
		return 0
	case "auth":
		return runAuth(args)
	case "client":
		return runClient(ctx, args)
	case "grant":
		return runGrant(ctx, args)
	case "queries":
		return runQueries(ctx, args)
	case "receipts":
		return runReceipts(ctx, args)
	}
	return fail("unknown subcommand %s", name)
}

// client mint --name claude-code | client install claude-code [--config ~/.claude.json] | client codex-snippet
func runClient(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return fail("usage: brokerd client mint --name <name> | install <claude-code|codex> | revoke --grant <id>")
	}
	switch args[0] {
	case "mint":
		fs := flag.NewFlagSet("client mint", flag.ContinueOnError)
		name := fs.String("name", "", "client name (claude-code, codex, cursor)")
		if err := fs.Parse(args[1:]); err != nil || *name == "" {
			return fail("usage: brokerd client mint --name <name>")
		}
		c, cfg, err := adminClient()
		if err != nil {
			return fail("%v", err)
		}
		var out struct {
			GrantID string            `json:"grant_id"`
			Token   string            `json:"token"`
			Servers map[string]string `json:"servers"`
		}
		if err := c.Do(ctx, "POST", "/v1/grants", map[string]any{"kind": "client", "name": *name, "owner": true, "issued_by": "brokerd client mint"}, &out); err != nil {
			return fail("%v", err)
		}
		dir := filepath.Join(cfg.DataDir, "clients")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fail("%v", err)
		}
		path := filepath.Join(dir, *name+".token")
		if err := os.WriteFile(path, []byte(out.Token+"\n"), 0o600); err != nil {
			return fail("%v", err)
		}
		fmt.Printf("minted owner client token for %s (grant %s), stored at %s\n", *name, out.GrantID, path)
		for conn, url := range out.Servers {
			fmt.Printf("  %-14s %s\n", conn, url)
		}
		return 0
	case "install":
		return runClientInstall(ctx, args[1:])
	case "adopt":
		return runClientAdopt(ctx, args[1:])
	case "restore":
		return runClientRestore(ctx, args[1:])
	}
	return fail("unknown client subcommand %s", args[0])
}

// grant mint --agent <id> --session <id> --project <key> --capabilities <file.json> [--ttl 1h]
func runGrant(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return fail("usage: brokerd grant mint|list|revoke ...")
	}
	c, _, err := adminClient()
	if err != nil {
		return fail("%v", err)
	}
	switch args[0] {
	case "mint":
		fs := flag.NewFlagSet("grant mint", flag.ContinueOnError)
		agent := fs.String("agent", "", "agent id")
		session := fs.String("session", "", "session id")
		project := fs.String("project", "", "project key")
		capsPath := fs.String("capabilities", "", "capabilities JSON file")
		ttl := fs.Duration("ttl", time.Hour, "lifetime")
		if err := fs.Parse(args[1:]); err != nil || *agent == "" || *session == "" || *capsPath == "" {
			return fail("usage: brokerd grant mint --agent <id> --session <id> --project <key> --capabilities <file.json> [--ttl 1h]")
		}
		raw, err := os.ReadFile(*capsPath)
		if err != nil {
			return fail("%v", err)
		}
		var out map[string]any
		if err := c.Do(ctx, "POST", "/v1/grants", map[string]any{"kind": "agent", "name": *agent, "agent_id": *agent, "session_id": *session,
			"project_key": *project, "capabilities": json.RawMessage(raw), "ttl_seconds": int(ttl.Seconds()), "issued_by": "brokerd grant mint"}, &out); err != nil {
			return fail("%v", err)
		}
		printJSON(out)
		return 0
	case "list":
		var out map[string]any
		if err := c.Do(ctx, "GET", "/v1/grants", nil, &out); err != nil {
			return fail("%v", err)
		}
		printJSON(out)
		return 0
	case "revoke":
		if len(args) < 2 {
			return fail("usage: brokerd grant revoke <grant-id>")
		}
		var out map[string]any
		if err := c.Do(ctx, "DELETE", "/v1/grants/"+args[1]+"?reason=cli", nil, &out); err != nil {
			return fail("%v", err)
		}
		printJSON(out)
		return 0
	}
	return fail("unknown grant subcommand %s", args[0])
}

// queries push --project companykit --file .persist/broker/queries.yaml
func runQueries(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return fail("usage: brokerd queries push --project <key> --file <queries.yaml> | list")
	}
	c, _, err := adminClient()
	if err != nil {
		return fail("%v", err)
	}
	switch args[0] {
	case "push":
		fs := flag.NewFlagSet("queries push", flag.ContinueOnError)
		project := fs.String("project", "", "project key")
		file := fs.String("file", "", "queries yaml")
		if err := fs.Parse(args[1:]); err != nil || *project == "" || *file == "" {
			return fail("usage: brokerd queries push --project <key> --file <queries.yaml>")
		}
		raw, err := os.ReadFile(*file)
		if err != nil {
			return fail("%v", err)
		}
		var out map[string]any
		if err := c.Do(ctx, "POST", "/v1/catalog/queries", map[string]any{"project": *project, "yaml": string(raw), "pushed_by": "brokerd queries push"}, &out); err != nil {
			return fail("%v", err)
		}
		printJSON(out)
		return 0
	case "list":
		var out map[string]any
		if err := c.Do(ctx, "GET", "/v1/catalog/queries", nil, &out); err != nil {
			return fail("%v", err)
		}
		printJSON(out)
		return 0
	}
	return fail("unknown queries subcommand %s", args[0])
}

// receipts tail [--after N] [--limit N]
func runReceipts(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("receipts", flag.ContinueOnError)
	after := fs.Int64("after", 0, "cursor")
	limit := fs.Int("limit", 50, "rows")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	c, _, err := adminClient()
	if err != nil {
		return fail("%v", err)
	}
	var out map[string]any
	if err := c.Do(ctx, "GET", fmt.Sprintf("/v1/receipts?after=%d&limit=%d", *after, *limit), nil, &out); err != nil {
		return fail("%v", err)
	}
	printJSON(out)
	return 0
}
