package wordpress

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/adapter"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/config"
	"github.com/danieliser/capability-broker/pkg/policy"
)

// fakeRouter stands in for wp-mcp-router over an in-memory transport and records what it was asked.
type fakeRouter struct{ last map[string]any }

func newFakeRouter(t *testing.T) (*fakeRouter, *mcp.ClientSession) {
	t.Helper()
	fr := &fakeRouter{}
	server := mcp.NewServer(&mcp.Implementation{Name: "wp-mcp-router", Version: "0"}, nil)
	siteSchema := json.RawMessage(`{"type":"object","properties":{"ability_name":{"type":"string"},"arguments":{"type":"object"},"compact":{"type":"boolean"},"site":{"type":"string","enum":["a","b","c"]}},"required":["ability_name"]}`)
	server.AddTool(&mcp.Tool{Name: "wp_run", Description: "run", InputSchema: siteSchema}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args map[string]any
		_ = json.Unmarshal(req.Params.Arguments, &args)
		fr.last = args
		out, _ := json.Marshal(map[string]any{"site": args["site"], "result": map[string]any{"id": 7, "status": args["arguments"].(map[string]any)["status"], "title": "t", "content": "long body", "_links": "noise"}})
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(out)}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "wp_list_sites", Description: "sites", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"sites":[{"id":"a","url":"https://a.example"},{"id":"b","url":"https://b.example"}]}`}}}, nil
	})
	server.AddTool(&mcp.Tool{Name: "wp_run_across", Description: "fan out", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{}, nil
	})
	ct, st := mcp.NewInMemoryTransports()
	if _, err := server.Connect(context.Background(), st, nil); err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	cs, err := client.Connect(context.Background(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return fr, cs
}

func TestDraftForcedSiteNarrowedAndCompacted(t *testing.T) {
	cat, _ := catalog.Load()
	a := New(config.WordPress{Timeout: 10 * time.Second}, cat)
	fr, cs := newFakeRouter(t)
	tools, err := a.discover(context.Background(), cs)
	if err != nil {
		t.Fatal(err)
	}
	a.session, a.tools = cs, tools
	a.learnSites(context.Background(), cs)
	if a.SiteHosts()["a"] != "a.example" {
		t.Fatalf("site hosts not learned: %v", a.SiteHosts())
	}
	for _, tl := range tools {
		if tl.Name == "wp_run_across" {
			t.Fatal("wp_run_across exposed")
		}
	}
	g := &auth.Grant{ID: "g", Kind: auth.KindAgent, Capabilities: auth.Capabilities{"wp-mcp-router": {Sites: []string{"a"}, Operations: []string{"content.draft"}, Constraints: map[string]any{"post_types": []any{"post"}}}}}
	visible, _ := a.Tools(context.Background(), g)
	for _, tl := range visible {
		if tl.Name == "wp_run" {
			enum := tl.InputSchema["properties"].(map[string]any)["site"].(map[string]any)["enum"].([]any)
			if len(enum) != 1 || enum[0] != "a" {
				t.Fatalf("site enum not narrowed: %v", enum)
			}
		}
	}
	conn, _ := cat.Connector("wp-mcp-router")
	args := map[string]any{"ability_name": "core/create-post", "site": "a", "arguments": map[string]any{"title": "x", "status": "publish"}}
	op, _ := conn.Resolve("wp_run", args)
	res, err := a.Execute(context.Background(), adapter.Call{Grant: g, Op: op, Tool: "wp_run", Decision: policy.Decision{Allow: true, Args: args}})
	if err != nil {
		t.Fatal(err)
	}
	if fr.last["arguments"].(map[string]any)["status"] != "draft" {
		t.Fatalf("publish not forced to draft: %v", fr.last)
	}
	if fr.last["compact"] != true {
		t.Fatal("compact not requested from the router")
	}
	compact := res.Compact.(map[string]any)["result"].(map[string]any)
	if _, leaked := compact["content"]; leaked || compact["id"] != float64(7) {
		t.Fatalf("compact shape wrong: %v", compact)
	}
	if res.Account != "a" {
		t.Fatalf("account %q", res.Account)
	}
}

func TestSiteHostsNeverWaitsOnChildStartup(t *testing.T) {
	a := New(config.WordPress{Timeout: time.Second}, adapter.Must(catalog.Load()))
	a.mu.Lock() // simulates ensure() holding the lock while npx starts
	defer a.mu.Unlock()
	done := make(chan struct{})
	go func() { a.SiteHosts(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("SiteHosts blocked behind the child-startup lock")
	}
}
