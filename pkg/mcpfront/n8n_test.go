package mcpfront

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/adapters/upstream"
	"github.com/danieliser/capability-broker/pkg/audit"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/connectors"
)

const fixtureRevision = "0123456789abcdef0123456789abcdef01234567"

// Real Streamable HTTP upstream -> real broker HTTP -> SQLite receipts, fixtures only.
type n8nFixture struct {
	mu            sync.Mutex
	calls         []map[string]any
	omitExecution bool
	rpcError      bool
	isError       bool
}

func (f *n8nFixture) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.calls) }

func n8nHarness(t *testing.T) (*harness, *n8nFixture, *connectors.Spec, string) {
	t.Helper()
	h := newHarness(t)
	dir := t.TempDir()
	var overlay strings.Builder
	overlay.WriteString("connector: n8n-flows\nversion: 1\nadapter_version: '1.0'\noperations:\n")
	for _, effect := range []string{"read", "write", "send", "spend"} {
		overlay.WriteString("  - id: flows.run\n    tool: fixture_" + effect + "\n    risk: reversible\n    requires_capability: operations:flows.run\n    argument_constraints:\n      - {kind: max_per_day, key: max_per_day, default: 0}\n    flow:\n      id: fixture." + effect + "\n      effect_class: " + effect + "\n      git_revision: " + fixtureRevision + "\n      workflow_id: wf-" + effect + "\n      workflow_version: v1\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "n8n-flows.yaml"), []byte(overlay.String()), 0600); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	h.front.Catalog = cat
	f := &n8nFixture{}
	srv := mcp.NewServer(&mcp.Implementation{Name: "n8n-fixture", Version: "1"}, &mcp.ServerOptions{Instructions: "Original n8n fixture instructions."})
	for _, name := range []string{"create_workflow", "update_workflow", "delete_workflow", "execute_workflow", "renamed_management", "fixture_read", "fixture_write", "fixture_send", "fixture_spend"} {
		tool := &mcp.Tool{Name: name, Title: "Original " + name, Description: "Fixture " + name, Meta: mcp.Meta{"fixture": "original"},
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}, InputSchema: json.RawMessage(`{"type":"object","additionalProperties":true}`)}
		srv.AddTool(tool, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			args := map[string]any{}
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			f.mu.Lock()
			f.calls = append(f.calls, args)
			omit := f.omitExecution
			rpcError := f.rpcError
			isError := f.isError
			f.mu.Unlock()
			if rpcError {
				return nil, fmt.Errorf("fixture dispatch response lost")
			}
			payload := map[string]any{"echo": args}
			if strings.HasPrefix(req.Params.Name, "fixture_") {
				effect := strings.TrimPrefix(req.Params.Name, "fixture_")
				payload = map[string]any{"persist_run_id": args["persist_run_id"], "logical_flow_id": args["logical_flow_id"], "git_revision": args["git_revision"],
					"n8n_workflow_id": "wf-" + effect, "n8n_workflow_version": "v1", "n8n_execution_id": "exec-fixture-1", "data": args["input"]}
				if omit {
					delete(payload, "n8n_execution_id")
				}
			}
			raw, _ := json.Marshal(payload)
			return &mcp.CallToolResult{Meta: mcp.Meta{"original": "result"}, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}, &mcp.TextContent{Text: "original second block"}}, StructuredContent: payload, IsError: req.Params.Name == "update_workflow" || isError}, nil
		})
	}
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{Stateless: true})
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer fixture-only-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(httpSrv.Close)
	var ownerSpec *connectors.Spec
	for _, name := range []string{"n8n", "n8n-flows"} {
		spec := &connectors.Spec{Name: name, URL: httpSrv.URL, Headers: map[string]string{"Authorization": "Bearer fixture-only-token"},
			OwnerOnly: name == "n8n", StartupTimeout: 5 * time.Second, CallTimeout: 5 * time.Second}
		ad := upstream.New(spec, cat, nil)
		if err := ad.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		h.front.Registry.Set(name, ad)
		t.Cleanup(func() { _ = ad.Close() })
		if name == "n8n" {
			ownerSpec = spec
		}
	}
	return h, f, ownerSpec, httpSrv.URL
}

func flowToken(t *testing.T, h *harness, caps auth.Capabilities, runID string) string {
	t.Helper()
	_, tok, err := h.store.Mint(context.Background(), auth.MintRequest{Kind: auth.KindAgent, Name: "fixture-agent", AgentID: "fixture-agent", ProjectKey: "fixture-project", SessionID: "fixture-session", RunID: runID, Capabilities: caps, TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
func flowCaps(ops ...string) auth.Capabilities {
	return auth.Capabilities{"n8n-flows": {Operations: ops, Constraints: map[string]any{"flow_ids": []any{"fixture.read", "fixture.write", "fixture.send", "fixture.spend"}}, Limits: map[string]float64{"max_per_day": 20}}}
}
func callFixture(t *testing.T, s *mcp.ClientSession, tool string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	res, err := s.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return res
}
func sameJSON(t *testing.T, a, b any) {
	t.Helper()
	ja, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	jb, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ja) != string(jb) {
		t.Fatalf("passthrough changed:\n%s\n%s", ja, jb)
	}
}

func TestN8nOwnerVerbatimHTTP(t *testing.T) {
	h, _, _, url := n8nHarness(t)
	_, tok, err := h.store.Mint(context.Background(), auth.MintRequest{Kind: auth.KindClient, Name: "codex", Capabilities: auth.OwnerCapabilities()})
	if err != nil {
		t.Fatal(err)
	}
	owner := h.connect(t, tok, "n8n")
	direct, err := mcp.NewClient(&mcp.Implementation{Name: "direct", Version: "1"}, nil).Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: &bearerRT{token: "fixture-only-token", next: http.DefaultTransport}}, DisableStandaloneSSE: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer direct.Close()
	want, err := direct.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := owner.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	sameJSON(t, want.Tools, got.Tools)
	if owner.InitializeResult().Instructions != direct.InitializeResult().Instructions {
		t.Fatal("instructions changed")
	}
	for _, tool := range []string{"create_workflow", "update_workflow", "delete_workflow"} {
		args := map[string]any{"verbose": true, "arbitrary": []any{"fixture", 7.0}, "nested": map[string]any{"enabled": false}}
		sameJSON(t, callFixture(t, direct, tool, args), callFixture(t, owner, tool, args))
	}
}

func TestN8nAgentIsolationEffectsAndReceipts(t *testing.T) {
	h, f, _, _ := n8nHarness(t)
	caps := flowCaps("flows.run")
	tok := flowToken(t, h, caps, "persist-fixture-run")
	s := h.connect(t, tok, "n8n-flows")
	listed, err := s.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "fixture_read" {
		t.Fatalf("agent tools: %+v", listed.Tools)
	}
	args := map[string]any{"persist_run_id": "forged", "logical_flow_id": "fixture.spend", "git_revision": "forged", "input": map[string]any{"fixture": true}}
	res := callFixture(t, s, "fixture_read", args)
	if res.IsError {
		t.Fatal(text(res))
	}
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	if last["persist_run_id"] != "persist-fixture-run" || last["logical_flow_id"] != "fixture.read" || last["git_revision"] != fixtureRevision {
		t.Fatalf("correlation: %v", last)
	}
	before := f.count()
	// Call the internal dispatch too: a guessed tool must fail even when bypassing tools/list.
	g, err := h.store.Authenticate(context.Background(), tok, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), grantKey, g)
	for _, tool := range []string{"create_workflow", "update_workflow", "delete_workflow", "fixture_write", "fixture_send", "fixture_spend"} {
		raw, _ := json.Marshal(map[string]any{"effect_class": "read", "idempotency_key": "fixture-key"})
		got, err := h.front.call(ctx, "n8n-flows", tool, &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: raw}})
		if err != nil || !got.IsError {
			t.Fatalf("%s allowed: %v %v", tool, got, err)
		}
	}
	if f.count() != before {
		t.Fatal("denied calls reached upstream")
	}
	receipts, err := h.audit.List(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	var intent, outcome, denied int
	for _, r := range receipts {
		if r.Connector != "n8n-flows" {
			continue
		}
		switch r.State {
		case audit.StateIntent:
			intent++
			if r.Operation != "flows.run" || r.RunID != "persist-fixture-run" || !strings.Contains(r.ArgsSummary, "fixture.read") {
				t.Fatalf("intent: %+v", r)
			}
		case audit.StateCompleted:
			outcome++
			if r.UpstreamRequestID != "exec-fixture-1" {
				t.Fatalf("outcome: %+v", r)
			}
		case audit.StateDenied:
			denied++
		}
	}
	if intent != 1 || outcome != 1 || denied != 6 {
		t.Fatalf("receipts %d/%d/%d", intent, outcome, denied)
	}
}

func TestN8nOwnerOnlyEnforcedForExistingGrant(t *testing.T) {
	h, f, spec, _ := n8nHarness(t)
	// Store.Mint intentionally bypasses admin validation to model a pre-existing grant.
	tok := flowToken(t, h, auth.Capabilities{"n8n": {Operations: []string{"create_workflow", "flows.run"}}}, "run")
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/mcp/n8n", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("owner-only HTTP status %d", res.StatusCode)
	}
	g, _ := h.store.Authenticate(context.Background(), tok, time.Now())
	ad, _ := h.front.Registry.Lookup("n8n")
	tools, err := ad.Tools(context.Background(), g)
	if err != nil || len(tools) != 0 {
		t.Fatalf("owner-only tools %v %v", tools, err)
	}
	before := f.count()
	got, err := h.front.call(context.WithValue(context.Background(), grantKey, g), "n8n", "create_workflow", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{}`)}})
	if err != nil || !got.IsError || f.count() != before {
		t.Fatal("existing grant reached management")
	}
	aggregate, err := mcp.NewClient(&mcp.Implementation{Name: "agent", Version: "1"}, nil).Connect(context.Background(),
		&mcp.StreamableClientTransport{Endpoint: h.srv.URL + "/mcp", DisableStandaloneSSE: true,
			HTTPClient: &http.Client{Transport: &bearerRT{token: tok, next: http.DefaultTransport}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer aggregate.Close()
	listed, err := aggregate.ListTools(context.Background(), nil)
	if err != nil || len(listed.Tools) != 0 {
		t.Fatalf("aggregate exposed owner tools: %v %v", listed, err)
	}
	// Even without the flag the instance catalog cannot synthesize management operations.
	spec.OwnerOnly = false
	tools, err = ad.Tools(context.Background(), g)
	if err != nil || len(tools) != 0 {
		t.Fatalf("instance catalog exposed management: %v %v", tools, err)
	}
}

func TestN8nFlowBudgetsAllowlistAndUnknown(t *testing.T) {
	h, f, _, _ := n8nHarness(t)
	caps := flowCaps("flows.run", "flows.write", "flows.send", "flows.spend")
	cap := caps["n8n-flows"]
	cap.Limits["max_per_day"] = 3
	caps["n8n-flows"] = cap
	s := h.connect(t, flowToken(t, h, caps, "run"), "n8n-flows")
	for _, tool := range []string{"fixture_write", "fixture_send", "fixture_spend"} {
		res := callFixture(t, s, tool, map[string]any{"idempotency_key": "fixture-key"})
		if res.IsError {
			t.Fatal(text(res))
		}
	}
	// New grant, same agent/project/day and shared flows.run operation cannot reset quota.
	s2 := h.connect(t, flowToken(t, h, caps, "run-2"), "n8n-flows")
	before := f.count()
	res := callFixture(t, s2, "fixture_read", map[string]any{"idempotency_key": "fixture-key"})
	if !res.IsError || !strings.Contains(text(res), "BROKER_LIMIT_EXCEEDED") || f.count() != before {
		t.Fatal("daily budget bypassed")
	}
	for _, tt := range []struct {
		name   string
		mutate func(auth.ConnectorCapability) auth.ConnectorCapability
		run    string
	}{
		{"missing budget", func(c auth.ConnectorCapability) auth.ConnectorCapability { c.Limits = nil; return c }, "run"},
		{"zero budget", func(c auth.ConnectorCapability) auth.ConnectorCapability {
			c.Limits = map[string]float64{"max_per_day": 0}
			return c
		}, "run"},
		{"missing allowlist", func(c auth.ConnectorCapability) auth.ConnectorCapability { c.Constraints = nil; return c }, "run"},
		{"foreign allowlist", func(c auth.ConnectorCapability) auth.ConnectorCapability {
			c.Constraints = map[string]any{"flow_ids": []any{"other"}}
			return c
		}, "run"},
		{"missing run", func(c auth.ConnectorCapability) auth.ConnectorCapability { return c }, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			c := flowCaps("flows.run")
			c["n8n-flows"] = tt.mutate(c["n8n-flows"])
			tok := flowToken(t, h, c, tt.run)
			g, _ := h.store.Authenticate(context.Background(), tok, time.Now())
			got, err := h.front.call(context.WithValue(context.Background(), grantKey, g), "n8n-flows", "fixture_read", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{}`)}})
			if err != nil || !got.IsError || f.count() != before {
				t.Fatalf("denial bypass: %v %v", got, err)
			}
		})
	}
	// A successful response without execution correlation is explicitly unknown.
	c := flowCaps("flows.run")
	c["n8n-flows"] = auth.ConnectorCapability{Operations: []string{"flows.run"}, Constraints: map[string]any{"flow_ids": []any{"fixture.read"}}, Limits: map[string]float64{"max_per_day": 10}}
	f.mu.Lock()
	f.omitExecution = true
	f.mu.Unlock()
	unknown := h.connect(t, flowToken(t, h, c, "run-unknown"), "n8n-flows")
	res = callFixture(t, unknown, "fixture_read", map[string]any{})
	if !res.IsError || !strings.Contains(text(res), "reconcile") {
		t.Fatal("missing execution id reported success")
	}
	receipts, err := h.audit.List(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range receipts {
		if r.RunID == "run-unknown" && r.State == audit.StateOutcomeUnknown {
			found = true
		}
	}
	if !found {
		t.Fatal("missing outcome_unknown receipt")
	}
	f.mu.Lock()
	f.omitExecution, f.rpcError = false, true
	f.mu.Unlock()
	res = callFixture(t, unknown, "fixture_read", map[string]any{})
	if !res.IsError || !strings.Contains(text(res), "reconcile") {
		t.Fatal("lost dispatch response did not require reconciliation")
	}
	receipts, err = h.audit.List(context.Background(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	unknownCount := 0
	for _, r := range receipts {
		if r.RunID == "run-unknown" && r.State == audit.StateOutcomeUnknown {
			unknownCount++
		}
	}
	if unknownCount != 2 {
		t.Fatalf("want two ambiguous outcomes, got %d", unknownCount)
	}
	// Inputs and inherited authority are not mutated by execution.
	if !reflect.DeepEqual(caps["n8n-flows"].Operations, []string{"flows.run", "flows.write", "flows.send", "flows.spend"}) {
		t.Fatal("grant mutated")
	}
}

func TestN8nEffectReplayDeniedAfterUnknown(t *testing.T) {
	h, f, _, _ := n8nHarness(t)
	f.mu.Lock()
	f.omitExecution = true
	f.mu.Unlock()
	caps := flowCaps("flows.run", "flows.send")
	s := h.connect(t, flowToken(t, h, caps, "first-run"), "n8n-flows")
	args := map[string]any{"idempotency_key": "one-effect"}
	res := callFixture(t, s, "fixture_send", args)
	if !res.IsError || !strings.Contains(text(res), "reconcile") || f.count() != 1 {
		t.Fatalf("first dispatch: %s calls=%d", text(res), f.count())
	}
	// Another grant/run must not redispatch an effect whose outcome is unknown.
	s2 := h.connect(t, flowToken(t, h, caps, "retry-run"), "n8n-flows")
	res = callFixture(t, s2, "fixture_send", args)
	if !res.IsError || !strings.Contains(text(res), "BROKER_DUPLICATE_FLOW_RUN") || f.count() != 1 {
		t.Fatalf("effect replayed: %s calls=%d", text(res), f.count())
	}
}

func TestN8nEffectReplayConcurrentAndOwnerTransparent(t *testing.T) {
	h, f, _, _ := n8nHarness(t)
	caps := flowCaps("flows.run", "flows.send")
	tok := flowToken(t, h, caps, "concurrent-run")
	g, err := h.store.Authenticate(context.Background(), tok, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), grantKey, g)
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			res, err := h.front.call(ctx, "n8n-flows", "fixture_send", &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{"idempotency_key":"concurrent-effect"}`)}})
			if err != nil || (res.IsError && !strings.Contains(text(res), "BROKER_DUPLICATE_FLOW_RUN")) {
				t.Errorf("concurrent dispatch: %v %v", res, err)
			}
		})
	}
	wg.Wait()
	if f.count() != 1 {
		t.Fatalf("concurrent key dispatched %d times", f.count())
	}
	// Owner flow calls may repeat keys and retain their original arguments.
	_, ownerToken, err := h.store.Mint(ctx, auth.MintRequest{Kind: auth.KindClient, Name: "codex", Capabilities: auth.OwnerCapabilities()})
	if err != nil {
		t.Fatal(err)
	}
	s := h.connect(t, ownerToken, "n8n-flows")
	args := map[string]any{"idempotency_key": "concurrent-effect", "verbose": true}
	for range 2 {
		if res := callFixture(t, s, "fixture_send", args); res.IsError {
			t.Fatal(text(res))
		}
	}
	if f.count() != 3 {
		t.Fatal("owner replay path changed")
	}
	f.mu.Lock()
	last := f.calls[len(f.calls)-1]
	f.mu.Unlock()
	sameJSON(t, args, last)
}

func TestN8nReviewManagementPaths(t *testing.T) {
	h, f, spec, _ := n8nHarness(t)
	caps := flowCaps("flows.run", "create_workflow", "update_workflow", "delete_workflow", "execute_workflow", "renamed_management")
	// Model an old grant naming the instance, even though admin minting refuses it.
	caps["n8n"] = caps["n8n-flows"]
	tok := flowToken(t, h, caps, "review-paths")
	g, err := h.store.Authenticate(context.Background(), tok, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), grantKey, g)
	for _, flag := range []bool{true, false} {
		spec.OwnerOnly = flag
		for _, connector := range []string{"n8n", "n8n-flows"} {
			for _, tool := range []string{"create_workflow", "update_workflow", "delete_workflow", "execute_workflow", "renamed_management", "never_discovered"} {
				req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Arguments: json.RawMessage(`{}`)}}
				res, err := h.front.call(ctx, connector, tool, req)
				if err != nil || !res.IsError {
					t.Fatalf("dispatch admitted %s/%s: %v %v", connector, tool, res, err)
				}
				// Bypass discovery's normal owner-only registration as well.
				raw, _ := json.Marshal(discArgs{Connector: connector, Tool: tool, Arguments: json.RawMessage(`{}`)})
				req.Params.Arguments = raw
				res, err = h.front.discover(ctx, "call_tool", map[string]bool{connector: true}, req)
				if err != nil || !res.IsError {
					t.Fatalf("discovery admitted %s/%s: %v %v", connector, tool, res, err)
				}
			}
		}
	}
	spec.OwnerOnly = true
	for _, suffix := range []string{"", "?prefix=none", "?ondemand=all"} {
		s, err := mcp.NewClient(&mcp.Implementation{Name: "review", Version: "1"}, nil).Connect(ctx,
			&mcp.StreamableClientTransport{Endpoint: h.srv.URL + "/mcp" + suffix, DisableStandaloneSSE: true,
				HTTPClient: &http.Client{Transport: &bearerRT{token: tok, next: http.DefaultTransport}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		listed, err := s.ListTools(ctx, nil)
		if err != nil || len(listed.Tools) != 1 || !strings.HasSuffix(listed.Tools[0].Name, "fixture_read") {
			t.Fatalf("aggregate tools: %v %v", listed, err)
		}
		for _, tool := range []string{"n8n__execute_workflow", "n8n-flows__renamed_management", "renamed_management", "call_tool"} {
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{"connector": "n8n", "tool": "execute_workflow"}})
			if err == nil && !res.IsError {
				t.Fatalf("aggregate admitted %s", tool)
			}
		}
		s.Close()
	}
	if f.count() != 0 {
		t.Fatalf("management denials reached upstream %d times", f.count())
	}
}
