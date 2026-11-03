package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/audit"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/connectors"
)

// TestStewardBrokerAcceptance starts an isolated broker HTTP front with the real upstream
// adapter and local MCP provider fixtures. No live grants, databases or AutoMem writes.
func TestStewardBrokerAcceptance(t *testing.T) {
	ctx := context.Background()
	const namespace = "agent:companykit.data-sync-steward"
	const sql = "SELECT source, freshness FROM fixture WHERE source = {source:String}"
	var mu sync.Mutex
	var stored, queryArgs, recallArgs map[string]any
	var writer *audit.Writer
	observedIntent := func(operation string) {
		t.Helper()
		rows, err := writer.List(ctx, 0, 100)
		if err != nil {
			t.Error(err)
			return
		}
		for _, row := range rows {
			if row.Operation == operation && row.State == audit.StateIntent {
				return
			}
		}
		t.Errorf("provider ran without an intent for %s", operation)
	}
	provider := func(handlers map[string]func(map[string]any) any) *httptest.Server {
		server := mcp.NewServer(&mcp.Implementation{Name: "steward-fixture", Version: "1"}, nil)
		for tool, handle := range handlers {
			server.AddTool(&mcp.Tool{Name: tool, InputSchema: map[string]any{"type": "object", "additionalProperties": true}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				var args map[string]any
				if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
					return nil, err
				}
				body, err := json.Marshal(handle(args))
				if err != nil {
					return nil, err
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(body)}}}, nil
			})
		}
		srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true}))
		t.Cleanup(srv.Close)
		return srv
	}
	ch := provider(map[string]func(map[string]any) any{"run_query": func(args map[string]any) any {
		observedIntent("clickhouse.named_query")
		mu.Lock()
		defer mu.Unlock()
		queryArgs = args
		return map[string]any{"query_id": "fixture-query-1", "rows": []any{map[string]any{"source": "telemetry", "freshness": "fixture-fresh"}}}
	}})
	mem := provider(map[string]func(map[string]any) any{
		"store_memory": func(args map[string]any) any {
			observedIntent("memory.store")
			mu.Lock()
			defer mu.Unlock()
			stored = args
			return map[string]any{"memory_id": "fixture-memory-1", "status": "stored"}
		},
		"recall_memory": func(args map[string]any) any {
			observedIntent("memory.recall")
			mu.Lock()
			defer mu.Unlock()
			recallArgs = args
			return map[string]any{"results": []any{
				map[string]any{"memory_id": "fixture-memory-1", "memory": stored},
				map[string]any{"memory_id": "foreign-memory", "memory": map[string]any{"tags": []any{"agent:other"}, "content": "foreign"}},
			}, "count": 2}
		},
	})
	e := newEnv(t, &connectors.Spec{Name: "clickhouse", URL: ch.URL}, &connectors.Spec{Name: "memory", URL: mem.URL})
	writer = e.front.Audit
	conn, _ := e.front.Catalog.Connector("clickhouse")
	conn.Operations[0].ArgumentConstraints = append(conn.Operations[0].ArgumentConstraints, catalog.ArgumentConstraint{Kind: "prefix", Path: "query", Key: "query_prefix", Required: true})
	_, err := e.front.Catalog.PushQueries("companykit", []byte("queries:\n  - id: scheduler.freshness\n    sql: \""+sql+"\"\n    params: {source: {type: String, required: true}}\n    limits: {max_rows: 1}\n"))
	if err != nil {
		t.Fatal(err)
	}
	grant, token, err := e.store.Mint(ctx, auth.MintRequest{Kind: auth.KindAgent, Name: "companykit.data-sync-steward", AgentID: "companykit.data-sync-steward", ProjectKey: "companykit", SessionID: "acceptance-session", RunID: "acceptance-run", TTL: time.Hour, IssuedBy: "test", PolicyVersion: e.front.Catalog.PolicyVersion(), Capabilities: auth.Capabilities{
		"clickhouse": {Queries: []string{"scheduler.freshness"}, Constraints: map[string]any{"query_prefix": "SELECT source"}, Limits: map[string]float64{"max_rows": 2, "calls_per_run": 5, "max_per_day": 5}},
		"memory":     {Operations: []string{"store", "recall"}, Namespaces: []string{namespace}, Limits: map[string]float64{"store_per_run": 1, "max_rows": 2, "max_per_day": 5}, Constraints: map[string]any{"max_importance": 0.8}},
	}})
	if err != nil {
		t.Fatal(err)
	}
	query := e.connect(t, token, "/mcp/clickhouse", nil)
	memory := e.connect(t, token, "/mcp/memory", nil)
	read := call(t, query, "run_query", map[string]any{"query": "@scheduler.freshness", "params": map[string]any{"source": "telemetry"}, "verbose": true})
	if read.IsError || !strings.Contains(firstText(read), "fixture-fresh") {
		t.Fatalf("named read failed: %s", firstText(read))
	}
	content := "Steward observed via broker: " + firstText(read)
	write := call(t, memory, "store_memory", map[string]any{"content": content, "tags": []any{"broker-acceptance-fixture"}, "importance": 0.99})
	if write.IsError || !strings.Contains(firstText(write), "fixture-memory-1") {
		t.Fatalf("broker memory write failed: %s", firstText(write))
	}
	recalled := call(t, memory, "recall_memory", map[string]any{"query": "Steward observed", "verbose": true})
	if recalled.IsError || !strings.Contains(firstText(recalled), "fixture-memory-1") || strings.Contains(firstText(recalled), "foreign-memory") {
		t.Fatalf("scoped recall: %s", firstText(recalled))
	}
	if !call(t, query, "run_query", map[string]any{"query": "SELECT 1"}).IsError {
		t.Fatal("ad-hoc SQL admitted")
	}
	if !call(t, memory, "store_memory", map[string]any{"content": "x", "tags": []any{"agent:other"}}).IsError {
		t.Fatal("foreign namespace admitted")
	}
	mu.Lock()
	if queryArgs["query"] != sql || queryArgs["params"].(map[string]any)["source"] != "telemetry" {
		t.Errorf("query was not resolved/bound: %v", queryArgs)
	}
	if stored["content"] != content || stored["importance"] != 0.8 || !reflect.DeepEqual(stored["tags"], []any{"broker-acceptance-fixture", namespace}) {
		t.Errorf("store not enriched: %v", stored)
	}
	if recallArgs["tag_match"] != "exact" || recallArgs["scope_fallback"] != false || recallArgs["limit"] != 2.0 {
		t.Errorf("recall not constrained: %v", recallArgs)
	}
	mu.Unlock()
	// Outcomes are batched: wait for actual persisted rows, never infer success from process exit.
	var receipts []audit.Receipt
	deadline := time.Now().Add(3 * time.Second)
	for {
		receipts, err = writer.List(ctx, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if len(receipts) == 8 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("want 8 persisted receipts, got %d", len(receipts))
		}
		time.Sleep(10 * time.Millisecond)
	}
	calls := map[string][]audit.Receipt{}
	denied := 0
	for _, r := range receipts {
		if r.GrantID != grant.ID || r.Principal != grant.Principal() || r.AgentID != grant.AgentID || r.ProjectKey != grant.ProjectKey || r.SessionID != grant.SessionID || r.RunID != grant.RunID || r.PolicyVersion != e.front.Catalog.PolicyVersion() || r.AdapterVersion != "2.0" {
			t.Fatalf("incomplete receipt identity: %+v", r)
		}
		if r.State == audit.StateDenied {
			denied++
			continue
		}
		calls[r.CallID] = append(calls[r.CallID], r)
	}
	if denied != 2 || len(calls) != 3 {
		t.Fatalf("receipts: denials=%d pairs=%d", denied, len(calls))
	}
	for _, pair := range calls {
		if len(pair) != 2 || pair[0].State != audit.StateIntent || pair[1].State != audit.StateCompleted || pair[0].Decision != "allow" || pair[1].OutcomeClass != "ok" || pair[1].ResponseBytes == 0 || pair[0].ArgsDigest != pair[1].ArgsDigest {
			t.Fatalf("bad receipt pair: %+v", pair)
		}
		var args map[string]any
		switch pair[0].Operation {
		case "clickhouse.named_query":
			args = queryArgs
		case "memory.store":
			args = stored
		case "memory.recall":
			args = recallArgs
		default:
			t.Fatalf("unexpected operation %s", pair[0].Operation)
		}
		digest, _ := audit.Digest(args)
		if digest != pair[0].ArgsDigest {
			t.Errorf("intent digest differs from effective provider args for %s", pair[0].Operation)
		}
		if pair[0].Operation == "memory.store" && pair[1].UpstreamRequestID != "fixture-memory-1" {
			t.Fatal("missing memory ID receipt")
		}
	}
	t.Logf("broker acceptance: named read -> broker memory store -> scoped recall; %d persisted receipts (3 intent/outcome pairs, 2 denials), grant %s", len(receipts), grant.ID)
}
