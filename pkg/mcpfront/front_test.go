package mcpfront

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/adapter"
	"github.com/danieliser/capability-broker/pkg/audit"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/durable"
	"github.com/danieliser/capability-broker/pkg/registry"
)

// fakeCH answers named queries without ClickHouse, recording what it saw.
type fakeCH struct {
	cat   *catalog.Catalog
	calls []adapter.Call
}

func (f *fakeCH) Connector() string { return "clickhouse" }
func (f *fakeCH) Tools(_ context.Context, g *auth.Grant) ([]catalog.Tool, error) {
	cap, _ := g.Capabilities.For("clickhouse")
	var out []catalog.Tool
	for _, name := range []string{"run_query", "list_databases", "list_tables"} {
		if !g.Capabilities.IsOwner() && cap.SQL != "select" && name != "run_query" {
			continue
		}
		out = append(out, catalog.Tool{Name: name, Description: name, InputSchema: map[string]any{"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}}}})
	}
	return out, nil
}
func (f *fakeCH) Execute(_ context.Context, c adapter.Call) (*adapter.Result, error) {
	f.calls = append(f.calls, c)
	qid := ""
	if c.Decision.Query != nil {
		qid = c.Decision.Query.ID
	}
	return &adapter.Result{Compact: map[string]any{"rows": 1, "query": qid}, Verbose: map[string]any{"rows": 1, "data": [][]any{{1}}},
		Outcome: adapter.OutcomeCompleted, UpstreamRequestID: "q-1", OutcomeClass: "ok"}, nil
}
func (f *fakeCH) Health(context.Context) error { return nil }

type harness struct {
	srv   *httptest.Server
	store *auth.Store
	audit *audit.Writer
	front *Front
	fake  *fakeCH
}

var statelessMode = true

func newHarness(t *testing.T) *harness {
	t.Helper()
	db, err := durable.Open(context.Background(), filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	cat, _ := catalog.Load("")
	if _, err := cat.PushQueries("companykit", []byte("queries:\n  - id: scheduler.freshness\n    sql: SELECT {since:DateTime}\n    params: { since: { type: DateTime } }\n")); err != nil {
		t.Fatal(err)
	}
	fake := &fakeCH{cat: cat}
	reg := registry.New()
	reg.Set("clickhouse", fake)
	reg.Set("memory", fake)
	f := &Front{Store: auth.NewStore(db), Catalog: cat, Audit: audit.NewWriter(db), Registry: reg,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Stateless: statelessMode}
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp/{connector}", func(w http.ResponseWriter, r *http.Request) { f.ServeConnector(w, r, r.PathValue("connector")) })
	mux.HandleFunc("/mcp", f.ServeAggregate)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{srv: srv, store: f.Store, audit: f.Audit, front: f, fake: fake}
}

func (h *harness) connect(t *testing.T, token, connector string) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tr := &mcp.StreamableClientTransport{Endpoint: h.srv.URL + "/mcp/" + connector, DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: &bearerRT{token: token, next: http.DefaultTransport}}}
	s, err := client.Connect(context.Background(), tr, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type bearerRT struct {
	token string
	next  http.RoundTripper
}

func (b *bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.next.RoundTrip(r)
}

func (h *harness) agent(t *testing.T) (*auth.Grant, string) {
	g, tok, err := h.store.Mint(context.Background(), auth.MintRequest{Kind: auth.KindAgent, Name: "a", AgentID: "a", SessionID: "s",
		Capabilities: auth.Capabilities{"clickhouse": {Queries: []string{"scheduler.freshness"}}}, IssuedBy: "t", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return g, tok
}

func text(res *mcp.CallToolResult) string {
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			return tc.Text
		}
	}
	return ""
}

func TestAgentSeesOnlyGrantedToolsAndCanRunNamedQuery(t *testing.T) {
	h := newHarness(t)
	_, tok := h.agent(t)
	s := h.connect(t, tok, "clickhouse")
	tools, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 1 || tools.Tools[0].Name != "run_query" {
		t.Fatalf("tools: %+v", tools.Tools)
	}
	schema, _ := json.Marshal(tools.Tools[0].InputSchema)
	if !strings.Contains(string(schema), `"verbose"`) {
		t.Fatal("verbose flag not injected")
	}
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_query", Arguments: map[string]any{"query": "@scheduler.freshness", "params": map[string]any{"since": "2026-10-01 00:00:00"}}})
	if err != nil || res.IsError {
		t.Fatalf("call: %v %s", err, text(res))
	}
	if !strings.Contains(text(res), `"query":"scheduler.freshness"`) {
		t.Fatalf("compact body: %s", text(res))
	}
	res, _ = s.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_query", Arguments: map[string]any{"query": "@scheduler.freshness", "verbose": true}})
	if !strings.Contains(text(res), `"data"`) {
		t.Fatalf("verbose body: %s", text(res))
	}
	res, _ = s.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_query", Arguments: map[string]any{"query": "SELECT 1"}})
	if !res.IsError || !strings.Contains(text(res), "BROKER_DENIED_OPERATION") {
		t.Fatalf("raw sql not denied: %s", text(res))
	}
	if len(h.fake.calls) != 2 {
		t.Fatalf("upstream reached %d times, want 2", len(h.fake.calls))
	}
	receipts, _ := h.audit.List(context.Background(), 0, 100)
	var intents, outcomes, denials int
	for _, r := range receipts {
		switch r.State {
		case audit.StateIntent:
			intents++
		case audit.StateCompleted:
			outcomes++
		case audit.StateDenied:
			denials++
		}
	}
	if intents != 2 || outcomes != 2 || denials != 1 {
		t.Fatalf("receipts intents=%d outcomes=%d denials=%d", intents, outcomes, denials)
	}
}

func TestUnknownConnectorAndForeignConnectorAreRefusedBeforeMCP(t *testing.T) {
	h := newHarness(t)
	_, tok := h.agent(t)
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/mcp/memory", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("foreign connector status %d", resp.StatusCode)
	}
	req, _ = http.NewRequest(http.MethodPost, h.srv.URL+"/mcp/clickhouse", strings.NewReader(`{}`))
	resp, _ = http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing bearer status %d", resp.StatusCode)
	}
}

func TestRevokeMidSession(t *testing.T) {
	h := newHarness(t)
	g, tok := h.agent(t)
	s := h.connect(t, tok, "clickhouse")
	res, _ := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_query", Arguments: map[string]any{"query": "@scheduler.freshness"}})
	if res.IsError {
		t.Fatalf("first call failed: %s", text(res))
	}
	if err := h.store.Revoke(context.Background(), g.ID, "test"); err != nil {
		t.Fatal(err)
	}
	h.front.Invalidate(g.ID)
	_, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_query", Arguments: map[string]any{"query": "@scheduler.freshness"}})
	if err == nil {
		t.Fatal("call after revocation succeeded")
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	_, err = client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: h.srv.URL + "/mcp/clickhouse", DisableStandaloneSSE: true, MaxRetries: -1,
		HTTPClient: &http.Client{Transport: &bearerRT{token: tok, next: http.DefaultTransport}}}, nil)
	if err == nil {
		t.Fatal("reconnect with revoked bearer succeeded")
	}
}

func TestTwoGrantsOneBackend(t *testing.T) {
	h := newHarness(t)
	ga, ta := h.agent(t)
	_, tb := h.agent(t)
	sa := h.connect(t, ta, "clickhouse")
	sb := h.connect(t, tb, "clickhouse")
	if r, _ := sa.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_query", Arguments: map[string]any{"query": "@scheduler.freshness"}}); r.IsError {
		t.Fatal(text(r))
	}
	_ = h.store.Revoke(context.Background(), ga.ID, "x")
	h.front.Invalidate(ga.ID)
	if r, err := sb.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_query", Arguments: map[string]any{"query": "@scheduler.freshness"}}); err != nil || r.IsError {
		t.Fatalf("second grant broken by first's revocation: %v %s", err, text(r))
	}
}

func TestLegacyStatefulModeStillWorks(t *testing.T) {
	statelessMode = false
	defer func() { statelessMode = true }()
	TestAgentSeesOnlyGrantedToolsAndCanRunNamedQuery(t)
	TestRevokeMidSession(t)
}

// renamedFake serves the same fake tools under another connector name.
type renamedFake struct {
	*fakeCH
	name string
}

func (r renamedFake) Connector() string { return r.name }

func TestHotAddedConnectorAppearsOnTheAggregateWithoutReconnecting(t *testing.T) {
	for _, stateless := range []bool{false, true} {
		statelessMode = stateless
		h := newHarness(t)
		statelessMode = true
		_, tok, err := h.store.Mint(context.Background(), auth.MintRequest{Kind: auth.KindClient, Name: "cc", Capabilities: auth.OwnerCapabilities(), IssuedBy: "t"})
		if err != nil {
			t.Fatal(err)
		}
		changed := make(chan struct{}, 4)
		client := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, &mcp.ClientOptions{
			ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) { changed <- struct{}{} },
		})
		s, err := client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: h.srv.URL + "/mcp?exclude=memory", MaxRetries: -1,
			HTTPClient: &http.Client{Transport: &bearerRT{token: tok, next: http.DefaultTransport}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { s.Close() })
		names := func() []string {
			res, err := s.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			var out []string
			for _, tl := range res.Tools {
				out = append(out, tl.Name)
			}
			sort.Strings(out)
			return out
		}
		if got := strings.Join(names(), ","); got != "clickhouse__list_databases,clickhouse__list_tables,clickhouse__run_query" {
			t.Fatalf("stateless=%v initial aggregate tools: %s", stateless, got)
		}
		h.front.Catalog.Ensure("extra", []string{"run_query", "list_databases", "list_tables"}) // synthesized: owner reaches every tool
		h.front.Registry.Set("extra", renamedFake{h.fake, "extra"})
		h.front.Refresh(context.Background())
		if !stateless {
			select {
			case <-changed:
			case <-time.After(3 * time.Second):
				t.Fatal("no tools/list_changed after a connector was added")
			}
		}
		if got := strings.Join(names(), ","); !strings.Contains(got, "extra__run_query") {
			t.Fatalf("stateless=%v hot-added connector missing: %s", stateless, got)
		}
		res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "extra__run_query", Arguments: map[string]any{"query": "@scheduler.freshness"}})
		if err != nil || res.IsError {
			t.Fatalf("call through hot-added connector: %v %s", err, text(res))
		}
		h.front.Registry.Remove("extra")
		h.front.Refresh(context.Background())
		if got := strings.Join(names(), ","); strings.Contains(got, "extra__") {
			t.Fatalf("removed connector still listed: %s", got)
		}
		s.Close()
	}
}

func TestEmptyAggregateStillAdvertisesTools(t *testing.T) {
	h := newHarness(t)
	_, tok, _ := h.store.Mint(context.Background(), auth.MintRequest{Kind: auth.KindClient, Name: "cc", Capabilities: auth.OwnerCapabilities(), IssuedBy: "t"})
	s, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: h.srv.URL + "/mcp?exclude=clickhouse,memory", MaxRetries: -1, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: &bearerRT{token: tok, next: http.DefaultTransport}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	caps := s.InitializeResult().Capabilities
	if caps.Tools == nil || !caps.Tools.ListChanged {
		t.Fatalf("empty aggregate must advertise tools with listChanged, got %+v", caps.Tools)
	}
}

func TestPlainAggregateKeepsUpstreamNamesUnlessTheyClash(t *testing.T) {
	h := newHarness(t)
	_, tok, _ := h.store.Mint(context.Background(), auth.MintRequest{Kind: auth.KindClient, Name: "cc", Capabilities: auth.OwnerCapabilities(), IssuedBy: "t"})
	list := func(url string) string {
		s, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{
			Endpoint: h.srv.URL + url, MaxRetries: -1, DisableStandaloneSSE: true,
			HTTPClient: &http.Client{Transport: &bearerRT{token: tok, next: http.DefaultTransport}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		res, err := s.ListTools(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tl := range res.Tools {
			names = append(names, tl.Name)
		}
		sort.Strings(names)
		return strings.Join(names, ",")
	}
	if got := list("/mcp?exclude=memory&prefix=none"); got != "list_databases,list_tables,run_query" {
		t.Fatalf("single connector, plain: %s", got)
	}
	// clickhouse and memory fakes expose the same three names: every one clashes, so all stay prefixed.
	if got := list("/mcp?prefix=none"); !strings.Contains(got, "clickhouse__run_query") || !strings.Contains(got, "memory__run_query") || strings.Contains(got, ",run_query") {
		t.Fatalf("clashing names must stay prefixed: %s", got)
	}
	s, _ := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{
		Endpoint: h.srv.URL + "/mcp?exclude=memory&prefix=none", MaxRetries: -1, DisableStandaloneSSE: true,
		HTTPClient: &http.Client{Transport: &bearerRT{token: tok, next: http.DefaultTransport}}}, nil)
	defer s.Close()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: "run_query", Arguments: map[string]any{"query": "@scheduler.freshness"}})
	if err != nil || res.IsError {
		t.Fatalf("plain call: %v %s", err, text(res))
	}
}
