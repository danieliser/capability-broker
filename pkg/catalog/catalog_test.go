package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAndResolve(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	ch, ok := c.Connector("clickhouse")
	if !ok {
		t.Fatal("clickhouse missing")
	}
	op, ok := ch.Resolve("run_query", map[string]any{"query": "@scheduler.freshness"})
	if !ok || op.ID != "clickhouse.named_query" {
		t.Fatalf("named query resolved to %v", op)
	}
	op, ok = ch.Resolve("run_query", map[string]any{"query": "SELECT 1"})
	if !ok || op.ID != "clickhouse.sql.select" {
		t.Fatalf("raw sql resolved to %v", op)
	}
	if _, ok := ch.Resolve("drop_table", nil); ok {
		t.Fatal("unknown tool resolved")
	}
	wp, _ := c.Connector("wp-mcp-router")
	op, ok = wp.Resolve("wp_run", map[string]any{"ability_name": "core/create-post"})
	if !ok || op.ID != "wordpress.content.draft" {
		t.Fatalf("create-post resolved to %v", op)
	}
	op, ok = wp.Resolve("wp_run", map[string]any{"ability_name": "core/delete-post"})
	if ok {
		t.Fatalf("delete-post resolved to %v; must be denied by name", op.ID)
	}
	if !wp.ToolExposed("wp_run") || !wp.ToolExposed("wp_run_across") || wp.ToolExposed("never_listed") {
		t.Fatal("tool exposure wrong")
	}
	synth := c.Ensure("thing", []string{"do_x", "do_y"})
	if synth.Source != "synthesized" || len(synth.Operations) != 2 || synth.Operations[0].RequiresCapability != "operations:do_x" {
		t.Fatalf("synthesized catalog wrong: %+v", synth)
	}
	if again := c.Ensure("thing", nil); again != synth {
		t.Fatal("Ensure replaced an existing catalog")
	}
}

func TestOverlayDirReplacesEmbedded(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "clickhouse.yaml"), []byte("connector: clickhouse\nversion: 9\noperations:\n  - id: x.y\n    tool: run_query\n    risk: read\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	ch, _ := c.Connector("clickhouse")
	if ch.Source != "overlay" || ch.Version != 9 || len(ch.Operations) != 1 {
		t.Fatalf("overlay not applied: %+v", ch)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.yaml"), []byte("connector: other\noperations: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); err == nil {
		t.Fatal("mismatched overlay file name accepted")
	}
}

func TestBlockMCPDraftIsCreateOnly(t *testing.T) {
	c, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	wp, _ := c.Connector("wp-mcp-router")
	for _, name := range []string{"get-page-blocks", "get-post-info", "resolve-url"} {
		op, ok := wp.Resolve("wp_run", map[string]any{"ability_name": "gk-block-mcp/" + name})
		if !ok || op.ID != "wordpress.content.read" {
			t.Fatalf("%s must resolve as content.read: %+v", name, op)
		}
	}
	op, ok := wp.Resolve("wp_run", map[string]any{"ability_name": "gk-block-mcp/create-post"})
	if !ok || op.ID != "wordpress.content.draft" {
		t.Fatalf("Block MCP create-post must resolve as content.draft: %+v", op)
	}
	for _, name := range []string{"core/update-post", "gk-block-mcp/update-post", "gk-block-mcp/update-blocks", "gk-block-mcp/delete-post", "unknown/create-post"} {
		if op, ok := wp.Resolve("wp_run", map[string]any{"ability_name": name, "arguments": map[string]any{"post_id": 468162, "post_type": "docs", "status": "draft"}}); ok {
			t.Fatalf("%s must be refused before upstream execution: %+v", name, op)
		}
	}
}

func TestPushQueries(t *testing.T) {
	c, _ := Load("")
	v0 := c.PolicyVersion()
	d, err := c.PushQueries("companykit", []byte(`
queries:
  - id: scheduler.freshness
    sql: SELECT 1 WHERE now() > {since:DateTime}
    params: { since: { type: DateTime, required: true } }
`))
	if err != nil || d == "" {
		t.Fatal(err)
	}
	if _, ok := c.Query("scheduler.freshness"); !ok {
		t.Fatal("query not stored")
	}
	if c.PolicyVersion() == v0 {
		t.Fatal("policy version unchanged after push")
	}
	if _, err := c.PushQueries("other", []byte("queries:\n  - id: scheduler.freshness\n    sql: SELECT 2\n")); err == nil {
		t.Fatal("cross-project id clash accepted")
	}
	if _, err := c.PushQueries("companykit", []byte("queries:\n  - id: a.b\n    sql: SELECT 1\n    params: { x: { type: String } }\n")); err == nil {
		t.Fatal("param without placeholder accepted")
	}
}

func TestDeclarativeConstraintCatalogValidation(t *testing.T) {
	for _, rule := range []string{
		"{kind: typo, path: to, key: to}",
		"{kind: prefix, key: subject_prefix}",
		"{kind: maximum, path: amount}",
		"{kind: row_limit, path: arguments..limit, key: max_rows}",
		"{kind: row_limit, path: limit, key: max_rows, default: 1.5}",
		"{kind: max_per_day, key: max_per_day, path: x}",
	} {
		raw := "connector: x\noperations:\n  - id: x.send\n    tool: send\n    risk: reversible\n    argument_constraints: [" + rule + "]\n"
		if _, err := parseConnector([]byte(raw), "test"); err == nil {
			t.Fatalf("bad rule accepted: %s", rule)
		}
	}
	cat, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	var count int
	for _, name := range cat.Connectors() {
		conn, _ := cat.Connector(name)
		for _, op := range conn.Operations {
			count++
			found := false
			for _, rule := range op.ArgumentConstraints {
				found = found || rule.Kind == "max_per_day"
			}
			if !found {
				t.Errorf("operation missing daily constraint: %s", op.ID)
			}
		}
	}
	if count != 17 {
		t.Fatalf("operation map needs updating: got %d", count)
	}
}
