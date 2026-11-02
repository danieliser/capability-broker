package policy

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/durable"
)

type fixture struct {
	store *auth.Store
	cat   *catalog.Catalog
	env   Env
}

func setup(t *testing.T) fixture {
	t.Helper()
	db, err := durable.Open(context.Background(), filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat, err := catalog.Load("")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cat.PushQueries("companykit", []byte("queries:\n  - id: scheduler.freshness\n    sql: SELECT {since:DateTime}\n    params: { since: { type: DateTime } }\n    limits: { max_rows: 50 }\n  - id: other.q\n    sql: SELECT 1\n")); err != nil {
		t.Fatal(err)
	}
	return fixture{store: auth.NewStore(db), cat: cat, env: Env{Catalog: cat, SiteHosts: map[string]string{"wppopupmaker": "wppopupmaker.com"}}}
}

func (f fixture) agent(t *testing.T, caps auth.Capabilities) *auth.Grant {
	g, _, err := f.store.Mint(context.Background(), auth.MintRequest{Kind: auth.KindAgent, Name: "a", AgentID: "a", SessionID: "s",
		Capabilities: caps, IssuedBy: "t", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func (f fixture) owner(t *testing.T) *auth.Grant {
	g, _, err := f.store.Mint(context.Background(), auth.MintRequest{Kind: auth.KindClient, Name: "cc", Capabilities: auth.OwnerCapabilities(), IssuedBy: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func (f fixture) eval(t *testing.T, g *auth.Grant, connector, tool string, args map[string]any) Decision {
	t.Helper()
	conn, _ := f.cat.Connector(connector)
	op, ok := conn.Resolve(tool, args)
	if !ok {
		t.Fatalf("%s/%s did not resolve", connector, tool)
	}
	d, err := Evaluate(context.Background(), g, connector, op, args, f.env, f.store)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestClickHouseNamedQueries(t *testing.T) {
	f := setup(t)
	g := f.agent(t, auth.Capabilities{"clickhouse": {Queries: []string{"scheduler.freshness"}, Limits: map[string]float64{"max_rows": 2000, "calls_per_run": 3}}})

	d := f.eval(t, g, "clickhouse", "run_query", map[string]any{"query": "@scheduler.freshness", "params": map[string]any{"since": "2026-10-01"}})
	if !d.Allow || d.Query == nil || d.Query.ID != "scheduler.freshness" {
		t.Fatalf("named query denied: %+v", d)
	}
	if d.Limits["max_rows"].(float64) != 50 {
		t.Fatalf("query limit not the minimum: %v", d.Limits["max_rows"])
	}
	d = f.eval(t, g, "clickhouse", "run_query", map[string]any{"query": "@other.q"})
	if d.Allow || d.Code != CodeDeniedArgument {
		t.Fatalf("query outside grant allowed: %+v", d)
	}
	d = f.eval(t, g, "clickhouse", "run_query", map[string]any{"query": "SELECT 1"})
	if d.Allow || d.Code != CodeDeniedOperation {
		t.Fatalf("raw sql allowed without sql.select: %+v", d)
	}
	d = f.eval(t, g, "clickhouse", "list_tables", map[string]any{"database": "companykit"})
	if d.Allow {
		t.Fatalf("schema listing allowed without sql.select: %+v", d)
	}
	// calls_per_run: 3 counted so far (two named-query evaluations consumed counters? no: denied ones never reach calls_per_run)
	f.eval(t, g, "clickhouse", "run_query", map[string]any{"query": "@scheduler.freshness"})
	f.eval(t, g, "clickhouse", "run_query", map[string]any{"query": "@scheduler.freshness"})
	d = f.eval(t, g, "clickhouse", "run_query", map[string]any{"query": "@scheduler.freshness"})
	if d.Allow || d.Code != CodeLimitExceeded {
		t.Fatalf("calls_per_run not enforced: %+v", d)
	}
}

func TestOwnerProfile(t *testing.T) {
	f := setup(t)
	o := f.owner(t)
	d := f.eval(t, o, "clickhouse", "run_query", map[string]any{"query": "SELECT 1"})
	if !d.Allow || d.Limits["max_rows"].(float64) != DefaultMaxRows*10 {
		t.Fatalf("owner sql denied or wrong limit: %+v", d)
	}
	d = f.eval(t, o, "clickhouse", "run_query", map[string]any{"query": "@other.q"})
	if !d.Allow {
		t.Fatalf("owner named query denied: %+v", d)
	}
	d = f.eval(t, o, "wp-mcp-router", "wp_run", map[string]any{"ability_name": "core/get-post", "site": "prps"})
	if !d.Allow {
		t.Fatalf("owner wp denied: %+v", d)
	}
}

func TestWordPressSitesAndDrafts(t *testing.T) {
	f := setup(t)
	g := f.agent(t, auth.Capabilities{"wp-mcp-router": {Sites: []string{"wppopupmaker"}, Operations: []string{"content.read", "content.draft"},
		Limits: map[string]float64{"max_drafts_per_run": 1}, Constraints: map[string]any{"post_types": []any{"post", "guides"}}}})

	d := f.eval(t, g, "wp-mcp-router", "wp_run", map[string]any{"ability_name": "core/get-post", "arguments": map[string]any{"id": 1}})
	if !d.Allow || d.Args["site"] != "wppopupmaker" {
		t.Fatalf("single-site default not applied: %+v", d)
	}
	d = f.eval(t, g, "wp-mcp-router", "wp_run", map[string]any{"ability_name": "core/get-post", "site": "codeatlantic"})
	if d.Allow || d.Code != CodeDeniedArgument {
		t.Fatalf("sibling site allowed: %+v", d)
	}
	d = f.eval(t, g, "wp-mcp-router", "wp_run", map[string]any{"ability_name": "core/create-post", "site": "wppopupmaker", "arguments": map[string]any{"title": "x", "post_type": "page"}})
	if d.Allow {
		t.Fatalf("post_type outside grant allowed: %+v", d)
	}
	d = f.eval(t, g, "wp-mcp-router", "wp_run", map[string]any{"ability_name": "core/create-post", "site": "wppopupmaker", "arguments": map[string]any{"title": "x"}})
	if !d.Allow || d.Args["arguments"].(map[string]any)["post_type"] != "post" {
		t.Fatalf("draft denied: %+v", d)
	}
	d = f.eval(t, g, "wp-mcp-router", "wp_run", map[string]any{"ability_name": "core/create-post", "site": "wppopupmaker", "arguments": map[string]any{"title": "y"}})
	if d.Allow || d.Code != CodeLimitExceeded {
		t.Fatalf("second draft allowed: %+v", d)
	}
	d = f.eval(t, g, "wp-mcp-router", "wp_get_content_by_url", map[string]any{"url": "https://code-atlantic.com/x"})
	if d.Allow {
		t.Fatalf("foreign host allowed: %+v", d)
	}
	d = f.eval(t, g, "wp-mcp-router", "wp_search_abilities", map[string]any{"query": "post"})
	if !d.Allow || len(d.Args["sites"].([]any)) != 1 {
		t.Fatalf("sites filter not forced: %+v", d)
	}
	conn, _ := f.cat.Connector("wp-mcp-router")
	if _, ok := conn.Resolve("wp_run", map[string]any{"ability_name": "core/update-user"}); ok {
		t.Fatal("update-user resolved")
	}
}

func TestMemoryNamespaces(t *testing.T) {
	f := setup(t)
	g := f.agent(t, auth.Capabilities{"memory": {Namespaces: []string{"agent:x", "project:p"}, Operations: []string{"recall", "store"},
		Limits: map[string]float64{"store_per_run": 1}, Constraints: map[string]any{"max_importance": 0.8}}})
	d := f.eval(t, g, "memory", "store_memory", map[string]any{"content": "hello", "tags": []any{"topic"}, "importance": 0.95})
	if !d.Allow {
		t.Fatalf("store denied: %+v", d)
	}
	tags := d.Args["tags"].([]any)
	if len(tags) != 2 || tags[1] != "agent:x" || d.Args["importance"] != 0.8 {
		t.Fatalf("namespace tag not forced or importance not clamped: %+v", d.Args)
	}
	d = f.eval(t, g, "memory", "store_memory", map[string]any{"content": "hi", "tags": []any{"agent:other"}})
	if d.Allow {
		t.Fatalf("foreign namespace tag allowed: %+v", d)
	}
	d = f.eval(t, g, "memory", "recall_memory", map[string]any{"query": "q"})
	if !d.Allow || len(d.Args["tags"].([]any)) != 2 {
		t.Fatalf("recall not scoped: %+v", d)
	}
	g2 := f.agent(t, auth.Capabilities{"memory": {Namespaces: []string{"agent:y"}, Operations: []string{"recall"}}})
	d = f.eval(t, g2, "memory", "store_memory", map[string]any{"content": "x"})
	if d.Allow || d.Code != CodeDeniedOperation {
		t.Fatalf("store allowed on recall-only grant: %+v", d)
	}
}

func TestBlockMCPRefresherGrant(t *testing.T) {
	f := setup(t)
	g := f.agent(t, auth.Capabilities{"wp-mcp-router": {Sites: []string{"wppopupmaker"}, Operations: []string{"content.read", "content.draft"},
		Limits: map[string]float64{"max_drafts_per_run": 1, "calls_per_run": 30}, Constraints: map[string]any{"post_types": []any{"docs"}}}})
	for _, name := range []string{"get-post-info", "get-page-blocks", "resolve-url"} {
		d := f.eval(t, g, "wp-mcp-router", "wp_run", map[string]any{"ability_name": "gk-block-mcp/" + name, "site": "wppopupmaker"})
		if !d.Allow {
			t.Fatalf("%s denied: %+v", name, d)
		}
	}
	for _, pt := range []string{"", "post", "page"} {
		d := f.eval(t, g, "wp-mcp-router", "wp_run", map[string]any{"ability_name": "gk-block-mcp/create-post", "site": "wppopupmaker", "arguments": map[string]any{"title": "x", "post_type": pt}})
		if d.Allow || d.Code != CodeDeniedArgument {
			t.Fatalf("non-doc draft %q admitted: %+v", pt, d)
		}
	}
	d := f.eval(t, g, "wp-mcp-router", "wp_run", map[string]any{"ability_name": "gk-block-mcp/create-post", "site": "other", "arguments": map[string]any{"title": "x", "post_type": "docs"}})
	if d.Allow {
		t.Fatal("other site admitted")
	}
	for i := 0; i < 2; i++ {
		d = f.eval(t, g, "wp-mcp-router", "wp_run", map[string]any{"ability_name": "gk-block-mcp/create-post", "site": "wppopupmaker", "arguments": map[string]any{"title": "x", "post_type": "docs"}})
		if i == 0 && !d.Allow || i == 1 && (d.Allow || d.Code != CodeLimitExceeded) {
			t.Fatalf("draft attempt %d: %+v", i+1, d)
		}
	}
}
