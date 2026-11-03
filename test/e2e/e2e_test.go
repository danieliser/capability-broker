// Package e2e runs the broker front against a real stdio upstream (cmd/mockmcp) and checks that owner clients
// see the upstream exactly as if connected directly: tools, results, errors, instructions, prompts, resources,
// templates and runtime list changes; plus the per-connector shaping options.
package e2e

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/adapters/upstream"
	"github.com/danieliser/capability-broker/pkg/audit"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/connectors"
	"github.com/danieliser/capability-broker/pkg/durable"
	"github.com/danieliser/capability-broker/pkg/mcpfront"
	"github.com/danieliser/capability-broker/pkg/registry"
)

var mockBin, cleanupBin string

// adapters holds the last newEnv's adapters, for lifecycle assertions.
var adapters map[string]*upstream.Adapter

func TestMain(m *testing.M) {
	dir, _ := os.MkdirTemp("", "mockmcp")
	mockBin = filepath.Join(dir, "mockmcp")
	cleanupBin = filepath.Join(dir, "brokerd-memory-cleanup")
	if out, err := exec.Command("go", "build", "-o", dir, "../../cmd/mockmcp", "../../cmd/brokerd-memory-cleanup").CombinedOutput(); err != nil {
		panic(string(out))
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

type env struct {
	srv   *httptest.Server
	store *auth.Store
	front *mcpfront.Front
}

func newEnv(t testing.TB, specs ...*connectors.Spec) *env {
	t.Helper()
	db, err := durable.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat, _ := catalog.Load("")
	reg := registry.New()
	f := &mcpfront.Front{Store: auth.NewStore(db), Catalog: cat, Audit: audit.NewWriter(db), Registry: reg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Stateless: true}
	t.Cleanup(f.Audit.Close)
	cacheDir := t.TempDir()
	adapters = map[string]*upstream.Adapter{}
	for _, sp := range specs {
		if sp.StartupTimeout == 0 {
			sp.StartupTimeout = 30 * time.Second
		}
		if sp.CallTimeout == 0 {
			sp.CallTimeout = 30 * time.Second
		}
		ad := upstream.New(sp, cat, nil)
		ad.OnChange = func() { f.Refresh(context.Background()) }
		ad.CacheDir = cacheDir
		if sp.IdleStop > 0 {
			wctx, cancel := context.WithCancel(context.Background())
			t.Cleanup(cancel)
			go ad.WatchIdle(wctx)
		}
		adapters[sp.Name] = ad
		reg.Set(sp.Name, ad)
		if err := ad.Start(context.Background()); err != nil {
			t.Fatalf("start %s: %v", sp.Name, err)
		}
		t.Cleanup(func() { _ = ad.Close() })
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/{connector}", func(w http.ResponseWriter, r *http.Request) { f.ServeConnector(w, r, r.PathValue("connector")) })
	mux.HandleFunc("/mcp", f.ServeAggregate)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &env{srv: srv, store: f.Store, front: f}
}

func (e *env) owner(t testing.TB, client string) string {
	_, tok, err := e.store.Mint(context.Background(), auth.MintRequest{Kind: auth.KindClient, Name: client, Capabilities: auth.OwnerCapabilities(), IssuedBy: "t"})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

func (e *env) connect(t testing.TB, token, path string, opts *mcp.ClientOptions) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "0"}, opts)
	s, err := c.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: e.srv.URL + path, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: bearer{token}}}, nil)
	if err != nil {
		t.Fatalf("connect %s: %v", path, err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mock(name string) *connectors.Spec { return &connectors.Spec{Name: name, Command: mockBin} }

func toolNamed(t *testing.T, s *mcp.ClientSession, name string) *mcp.Tool {
	t.Helper()
	res, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range res.Tools {
		if tl.Name == name {
			return tl
		}
	}
	return nil
}

func call(t *testing.T, s *mcp.ClientSession, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func firstText(r *mcp.CallToolResult) string {
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func TestOwnerSeesUpstreamVerbatim(t *testing.T) {
	e := newEnv(t, mock("mock"))
	s := e.connect(t, e.owner(t, "claude-code"), "/mcp/mock", nil)
	ctx := context.Background()

	if got := s.InitializeResult().Instructions; !strings.HasPrefix(got, "mockmcp: test server") {
		t.Errorf("instructions: %q", got)
	}
	echo := toolNamed(t, s, "echo")
	if echo == nil || echo.Title != "Echo" || echo.Annotations == nil || !echo.Annotations.ReadOnlyHint || echo.OutputSchema == nil || echo.Meta["mock/kind"] != "echo" {
		t.Fatalf("echo definition not verbatim: %+v", echo)
	}
	if strings.Contains(mustJSON(echo.InputSchema), "verbose") {
		t.Error("owner schema carries the broker's verbose flag")
	}
	r := call(t, s, "echo", map[string]any{"text": "hi"})
	if sc, _ := r.StructuredContent.(map[string]any); sc["echo"] != "hi" || sc["length"] != float64(2) {
		t.Errorf("structuredContent: %#v", r.StructuredContent)
	}
	if got := firstText(call(t, s, "markdown", nil)); got != "# Title\n\n- one\n- two\n" {
		t.Errorf("markdown not verbatim: %q", got)
	}
	px := call(t, s, "pixel", nil)
	if len(px.Content) != 2 {
		t.Fatalf("pixel content blocks: %d", len(px.Content))
	}
	if _, ok := px.Content[1].(*mcp.ImageContent); !ok {
		t.Errorf("image block lost: %T", px.Content[1])
	}
	fail := call(t, s, "fail", nil)
	if !fail.IsError || firstText(fail) != "mock failure: as requested" {
		t.Errorf("upstream error not verbatim: %v %q", fail.IsError, firstText(fail))
	}

	prompts, err := s.ListPrompts(ctx, nil)
	if err != nil || len(prompts.Prompts) != 1 || prompts.Prompts[0].Name != "greet" || prompts.Prompts[0].Title != "Greet" {
		t.Fatalf("prompts: %v %+v", err, prompts)
	}
	gp, err := s.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet", Arguments: map[string]string{"name": "Sir"}})
	if err != nil || gp.Messages[0].Content.(*mcp.TextContent).Text != "Say hello to Sir." {
		t.Fatalf("get prompt: %v %+v", err, gp)
	}
	res, err := s.ListResources(ctx, nil)
	if err != nil || len(res.Resources) != 1 || res.Resources[0].URI != "mock://readme" {
		t.Fatalf("resources: %v %+v", err, res)
	}
	rr, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: "mock://readme"})
	if err != nil || rr.Contents[0].Text != "# Mock readme" {
		t.Fatalf("read resource: %v %+v", err, rr)
	}
	tpls, err := s.ListResourceTemplates(ctx, nil)
	if err != nil || len(tpls.ResourceTemplates) != 1 || tpls.ResourceTemplates[0].URITemplate != "mock://items/{id}" {
		t.Fatalf("templates: %v %+v", err, tpls)
	}
	ri, err := s.ReadResource(ctx, &mcp.ReadResourceParams{URI: "mock://items/7"})
	if err != nil || ri.Contents[0].Text != `{"id":"7"}` {
		t.Fatalf("read via template: %v %+v", err, ri)
	}
}

func TestRuntimeListChangesReachClients(t *testing.T) {
	e := newEnv(t, mock("mock"))
	s := e.connect(t, e.owner(t, "claude-code"), "/mcp/mock", nil)
	call(t, s, "grow", nil)
	deadline := time.Now().Add(10 * time.Second)
	for toolNamed(t, s, "added_1") == nil {
		if time.Now().After(deadline) {
			t.Fatal("tool added upstream at runtime never reached the client")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := firstText(call(t, s, "added_1", nil)); got != "added_1 ran" {
		t.Errorf("added tool call: %q", got)
	}
	p, _ := s.ListPrompts(context.Background(), nil)
	found := false
	for _, x := range p.Prompts {
		found = found || x.Name == "prompt_added_1"
	}
	if !found {
		t.Error("prompt added upstream at runtime never reached the client")
	}
}

func TestShapingOptions(t *testing.T) {
	sp := mock("mock")
	sp.Tools = &connectors.ToolFilter{Exclude: []string{"fail"}}
	sp.Strip = []string{"annotations", "meta", "prompts"}
	sp.Response = &connectors.ResponseFilter{DropKeys: []string{"debug", "_links"}, MaxChars: 200, Tools: []string{"noisy"}}
	e := newEnv(t, sp)
	s := e.connect(t, e.owner(t, "claude-code"), "/mcp/mock", nil)

	if toolNamed(t, s, "fail") != nil {
		t.Error("excluded tool still listed")
	}
	if r, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "fail"}); err == nil && !r.IsError {
		t.Error("excluded tool still callable")
	}
	echo := toolNamed(t, s, "echo")
	if echo.Annotations != nil || echo.Meta != nil {
		t.Errorf("strip did not remove annotations/meta: %+v", echo)
	}
	if p, _ := s.ListPrompts(context.Background(), nil); p != nil && len(p.Prompts) != 0 {
		t.Errorf("prompts stripped but listed: %+v", p.Prompts)
	}
	got := firstText(call(t, s, "noisy", nil))
	if strings.Contains(got, "debug") || strings.Contains(got, "_links") || !strings.Contains(got, `"keep":1`) || !strings.Contains(got, "more characters cut by the broker") {
		t.Errorf("response filter: %q", got)
	}
	if got := firstText(call(t, s, "markdown", nil)); got != "# Title\n\n- one\n- two\n" {
		t.Errorf("response filter leaked onto an unlisted tool: %q", got)
	}
}

func TestOnDemandDiscovery(t *testing.T) {
	sp := mock("mock")
	sp.Expose = "broker"
	e := newEnv(t, sp, mock("direct"))
	s := e.connect(t, e.owner(t, "claude-code"), "/mcp?prefix=none&exclude=direct", nil)
	if !strings.Contains(s.InitializeResult().Instructions, "on demand from: mock") {
		t.Errorf("instructions: %q", s.InitializeResult().Instructions)
	}
	if toolNamed(t, s, "echo") != nil {
		t.Error("on-demand tool preloaded")
	}
	if toolNamed(t, s, "search_tools") == nil || toolNamed(t, s, "call_tool") == nil {
		t.Fatal("discovery tools missing")
	}
	hits := firstText(call(t, s, "search_tools", map[string]any{"query": "echo text"}))
	if !strings.Contains(hits, `"tool":"echo"`) {
		t.Errorf("search: %s", hits)
	}
	r := call(t, s, "call_tool", map[string]any{"connector": "mock", "tool": "echo", "arguments": map[string]any{"text": "via broker"}})
	if sc, _ := r.StructuredContent.(map[string]any); sc["echo"] != "via broker" {
		t.Errorf("call_tool result: %#v %q", r.StructuredContent, firstText(r))
	}
}

func TestClientTargeting(t *testing.T) {
	sp := mock("mock")
	sp.Clients = []string{"claude-code"}
	e := newEnv(t, sp)
	if resp := e.rawPost(t, e.owner(t, "codex"), "/mcp/mock"); resp != http.StatusNotFound {
		t.Errorf("codex reached a claude-code-only connector: %d", resp)
	}
	s := e.connect(t, e.owner(t, "claude-code"), "/mcp/mock", nil)
	if toolNamed(t, s, "echo") == nil {
		t.Error("claude-code lost its connector")
	}
}

func (e *env) rawPost(t *testing.T, token, path string) int {
	req, _ := http.NewRequest("POST", e.srv.URL+path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := (&http.Client{Transport: bearer{token}}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestIdleStopKeepsListingAndRestartsOnCall(t *testing.T) {
	sp := mock("mock")
	sp.OnDemandStart = true
	sp.IdleStop = 2 * time.Second
	e := newEnv(t, sp)
	ad := adapters["mock"]
	s := e.connect(t, e.owner(t, "claude-code"), "/mcp/mock", nil)
	deadline := time.Now().Add(15 * time.Second)
	for ad.Connected() {
		if time.Now().After(deadline) {
			t.Fatal("upstream never stopped after idle_stop")
		}
		time.Sleep(200 * time.Millisecond)
	}
	if toolNamed(t, s, "echo") == nil {
		t.Fatal("tools vanished while the upstream was stopped")
	}
	r := call(t, s, "echo", map[string]any{"text": "wake"})
	if sc, _ := r.StructuredContent.(map[string]any); sc["echo"] != "wake" {
		t.Fatalf("call after idle stop: %#v", r.StructuredContent)
	}
	if !ad.Connected() {
		t.Error("call did not start the upstream again")
	}
	// A fresh adapter lists from the cache without starting anything.
	fresh := upstream.New(sp, e.front.Catalog, nil)
	fresh.CacheDir = ad.CacheDir
	if !fresh.LoadCache() || fresh.Connected() || !fresh.Ready() {
		t.Fatal("cache did not make a cold on-demand connector listable")
	}
}
