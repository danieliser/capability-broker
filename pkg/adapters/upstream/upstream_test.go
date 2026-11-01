package upstream

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/adapter"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/connectors"
	"github.com/danieliser/capability-broker/pkg/policy"
)

// fakeChild is an in-memory MCP server standing in for a stdio upstream; it records the last call.
type fakeChild struct {
	last   map[string]any
	lastTo string
	reply  func(tool string, args map[string]any) any
}

func (f *fakeChild) serve(t *testing.T, tools ...string) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "child", Version: "0"}, nil)
	for _, name := range tools {
		name := name
		server.AddTool(&mcp.Tool{Name: name, Description: "desc " + name,
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"},"params":{"type":"object"},"site":{"type":"string","enum":["a","b"]},"arguments":{"type":"object"},"tags":{"type":"array"}}}`)},
			func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args map[string]any
				_ = json.Unmarshal(req.Params.Arguments, &args)
				f.last, f.lastTo = args, name
				out, _ := json.Marshal(f.reply(name, args))
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(out)}}}, nil
			})
	}
	ct, st := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

func adapterWith(t *testing.T, name string, child *fakeChild, tools ...string) (*Adapter, *catalog.Catalog) {
	t.Helper()
	cat, _ := catalog.Load("")
	if _, err := cat.PushQueries("p", []byte("queries:\n  - id: s.fresh\n    sql: SELECT {since:DateTime}\n    params: { since: { type: DateTime, required: true } }\n")); err != nil {
		t.Fatal(err)
	}
	a := New(&connectors.Spec{Name: name, Command: "unused", CallTimeout: 5 * time.Second, StartupTimeout: 5 * time.Second}, cat, nil)
	cs := child.serve(t, tools...)
	disc, err := discover(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, d := range disc {
		names = append(names, d.Name)
	}
	a.session, a.tools, a.conn = cs, disc, cat.Ensure(name, names)
	return a, cat
}

func agent(caps auth.Capabilities) *auth.Grant {
	return &auth.Grant{ID: "g", Kind: auth.KindAgent, Capabilities: caps}
}

func run(t *testing.T, a *Adapter, cat *catalog.Catalog, g *auth.Grant, tool string, args map[string]any, verbose bool) (*adapter.Result, error) {
	t.Helper()
	conn, _ := cat.Connector(a.Connector())
	op, ok := conn.Resolve(tool, args)
	if !ok {
		t.Fatalf("%s did not resolve", tool)
	}
	d, err := policy.Evaluate(context.Background(), g, a.Connector(), op, args, policy.Env{Catalog: cat}, noCounters{})
	if err != nil || !d.Allow {
		t.Fatalf("policy: %v %+v", err, d)
	}
	return a.Execute(context.Background(), adapter.Call{Grant: g, Op: op, Tool: tool, Decision: d, Verbose: verbose, CallID: "c1"})
}

type noCounters struct{}

func (noCounters) Counter(context.Context, string, string) (int64, error)   { return 0, nil }
func (noCounters) Increment(context.Context, string, string) (int64, error) { return 1, nil }

func TestClickHouseNamedQueryRewrittenForTheChild(t *testing.T) {
	child := &fakeChild{reply: func(string, map[string]any) any {
		return map[string]any{"columns": []any{"x"}, "rows": []any{[]any{1}, []any{2}, []any{3}}}
	}}
	a, cat := adapterWith(t, "clickhouse", child, "run_query", "list_databases", "list_tables")
	g := agent(auth.Capabilities{"clickhouse": {Queries: []string{"s.fresh"}}})
	tools, _ := a.Tools(context.Background(), g)
	if len(tools) != 1 || tools[0].Name != "run_query" || !strings.Contains(tools[0].Description, "@s.fresh(since:DateTime!)") {
		t.Fatalf("tools for named-query grant: %+v", tools)
	}
	res, err := run(t, a, cat, g, "run_query", map[string]any{"query": "@s.fresh", "params": map[string]any{"since": "2026-10-01 00:00:00"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if child.last["query"] != "SELECT {since:DateTime}" || child.last["params"].(map[string]any)["since"] != "2026-10-01 00:00:00" {
		t.Fatalf("child saw %v", child.last)
	}
	if _, bad := child.last["verbose"]; bad {
		t.Fatal("verbose leaked to the child")
	}
	if res.Compact == nil || res.Outcome != adapter.OutcomeCompleted {
		t.Fatalf("result %+v", res)
	}
	if _, err := run(t, a, cat, g, "run_query", map[string]any{"query": "@s.fresh", "params": map[string]any{"since": "x", "extra": 1}}, false); err == nil {
		t.Fatal("undeclared param accepted")
	}
	owner := &auth.Grant{ID: "o", Kind: auth.KindClient, Capabilities: auth.OwnerCapabilities()}
	tools, _ = a.Tools(context.Background(), owner)
	if len(tools) != 3 {
		t.Fatalf("owner tools %d", len(tools))
	}
	if _, err := run(t, a, cat, owner, "run_query", map[string]any{"query": "SELECT 1; DROP TABLE x"}, false); err == nil {
		t.Fatal("multi-statement accepted")
	}
}

func TestMemoryNamespaceFilterAndDeniedTools(t *testing.T) {
	child := &fakeChild{reply: func(tool string, args map[string]any) any {
		return map[string]any{"results": []any{
			map[string]any{"id": "m1", "memory": map[string]any{"content": strings.Repeat("x", 500), "tags": []any{"agent:me"}}},
			map[string]any{"id": "m2", "memory": map[string]any{"content": "other", "tags": []any{"agent:you"}}},
		}, "count": 2}
	}}
	a, cat := adapterWith(t, "memory", child, "store_memory", "recall_memory", "associate_memories", "update_memory", "delete_memory", "check_database_health")
	g := agent(auth.Capabilities{"memory": {Namespaces: []string{"agent:me"}, Operations: []string{"recall"}}})
	tools, _ := a.Tools(context.Background(), g)
	names := []string{}
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "check_database_health,recall_memory" {
		t.Fatalf("recall-only grant sees %v", names)
	}
	owner := &auth.Grant{ID: "o", Kind: auth.KindClient, Capabilities: auth.OwnerCapabilities()}
	tools, _ = a.Tools(context.Background(), owner)
	ownerHas := map[string]bool{}
	for _, tl := range tools {
		ownerHas[tl.Name] = true
	}
	if !ownerHas["delete_memory"] || !ownerHas["update_memory"] {
		t.Fatal("owner should reach update_memory and delete_memory")
	}
	res, err := run(t, a, cat, g, "recall_memory", map[string]any{"query": "q"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if child.last["tag_match"] != "exact" || child.last["scope_fallback"] != false {
		t.Fatalf("scoped_recall guard not applied: %v", child.last)
	}
	body := res.Compact.(map[string]any)
	results := body["results"].([]any)
	if len(results) != 1 || body["count"] != 1 {
		t.Fatalf("foreign namespace not filtered: %v", body)
	}
	content := results[0].(map[string]any)["memory"].(map[string]any)["content"].(string)
	if !strings.Contains(content, "more chars") || len(content) > 260 {
		t.Fatalf("compact did not trim: %d", len(content))
	}
	verbose := res.Verbose.(map[string]any)["results"].([]any)[0].(map[string]any)["memory"].(map[string]any)["content"].(string)
	if len(verbose) != 500 {
		t.Fatal("verbose was trimmed")
	}
}

func TestWordPressDraftForcedAndSiteNarrowed(t *testing.T) {
	child := &fakeChild{reply: func(tool string, args map[string]any) any {
		if tool == "wp_list_sites" {
			return map[string]any{"sites": []any{map[string]any{"id": "a", "url": "https://a.example"}}}
		}
		return map[string]any{"site": args["site"], "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": `{"success":true,"data":{"id":7,"status":"draft","title":"t","content":"body","_links":{}}}`}}, "isError": false}}
	}}
	a, cat := adapterWith(t, "wp-mcp-router", child, "wp_run", "wp_list_sites", "wp_run_across")
	a.learnSites(context.Background(), a.session, "wp_list_sites")
	if a.SiteHosts()["a"] != "a.example" {
		t.Fatalf("hosts %v", a.SiteHosts())
	}
	g := agent(auth.Capabilities{"wp-mcp-router": {Sites: []string{"a"}, Operations: []string{"content.draft"}, Constraints: map[string]any{"post_types": []any{"post"}}}})
	tools, _ := a.Tools(context.Background(), g)
	if len(tools) != 1 || tools[0].Name != "wp_run" {
		t.Fatalf("tools %+v", tools)
	}
	enum := tools[0].InputSchema["properties"].(map[string]any)["site"].(map[string]any)["enum"].([]any)
	if len(enum) != 1 || enum[0] != "a" {
		t.Fatalf("enum %v", enum)
	}
	res, err := run(t, a, cat, g, "wp_run", map[string]any{"ability_name": "core/create-post", "site": "a", "arguments": map[string]any{"title": "x", "status": "publish"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	if child.last["arguments"].(map[string]any)["status"] != "draft" || child.last["compact"] != true {
		t.Fatalf("guards not applied: %v", child.last)
	}
	out := res.Compact.(map[string]any)["result"].(map[string]any)
	if out["id"] != float64(7) || out["content"] != nil || out["_links"] != nil {
		t.Fatalf("compact result %v", out)
	}
	if res.Account != "a" {
		t.Fatalf("account %q", res.Account)
	}
}

func TestSynthesizedCatalogForUnknownConnector(t *testing.T) {
	child := &fakeChild{reply: func(string, map[string]any) any { return map[string]any{"ok": true} }}
	a, cat := adapterWith(t, "thing", child, "do_x", "do_y")
	g := agent(auth.Capabilities{"thing": {Operations: []string{"do_x"}}})
	tools, _ := a.Tools(context.Background(), g)
	if len(tools) != 1 || tools[0].Name != "do_x" {
		t.Fatalf("tools %+v", tools)
	}
	if _, err := run(t, a, cat, g, "do_x", map[string]any{}, false); err != nil {
		t.Fatal(err)
	}
}

func TestSiteHostsNeverWaitsOnChildStartup(t *testing.T) {
	cat, _ := catalog.Load("")
	a := New(&connectors.Spec{Name: "wp-mcp-router", Command: "unused"}, cat, nil)
	a.mu.Lock()
	defer a.mu.Unlock()
	done := make(chan struct{})
	go func() { a.SiteHosts(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SiteHosts blocked behind the child-startup lock")
	}
}

func TestRelayAttributesProgressAndSoleCaller(t *testing.T) {
	r := newRelay()
	if r.sole() != nil {
		t.Fatal("empty relay has a sole call")
	}
	r.begin("c1", nil, "client-token")
	if got := r.byToken("c1"); got == nil || got.progressToken != "client-token" {
		t.Fatal("token lookup failed")
	}
	if r.sole() == nil {
		t.Fatal("single in-flight call not found")
	}
	r.begin("c2", nil, nil)
	if r.sole() != nil {
		t.Fatal("two in-flight calls must not resolve to one")
	}
	r.end("c1")
	r.end("c2")
	if r.byToken("c1") != nil {
		t.Fatal("ended call still registered")
	}
}
