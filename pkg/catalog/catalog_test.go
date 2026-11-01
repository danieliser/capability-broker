package catalog

import "testing"

func TestLoadAndResolve(t *testing.T) {
	c, err := Load()
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
	if !wp.ToolExposed("wp_run") || wp.ToolExposed("wp_run_across") {
		t.Fatal("tool exposure wrong")
	}
}

func TestPushQueries(t *testing.T) {
	c, _ := Load()
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
