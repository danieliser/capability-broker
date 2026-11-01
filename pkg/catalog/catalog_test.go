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
