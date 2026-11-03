package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/danieliser/capability-broker/pkg/connectors"
	"github.com/danieliser/capability-broker/pkg/memorycleanup"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Run the built cleanup command through the real broker memory front. All records
// and tokens belong to this isolated fixture; no live AutoMem endpoint is used.
func TestCleanupCommandDefaultsToDryRun(t *testing.T) {
	var mu sync.Mutex
	deleted := false
	writes := 0
	record := map[string]any{"memory_id": "fixture-smoke-1", "content": "local fixture only", "tags": []any{memorycleanup.Tag, memorycleanup.Namespace}}
	server := mcp.NewServer(&mcp.Implementation{Name: "cleanup-fixture", Version: "1"}, nil)
	for _, tool := range []string{"recall_memory", "delete_memory"} {
		server.AddTool(&mcp.Tool{Name: tool, InputSchema: map[string]any{"type": "object", "additionalProperties": true}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			mu.Lock()
			defer mu.Unlock()
			var args map[string]any
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			var body any
			if req.Params.Name == "delete_memory" {
				if args["memory_id"] != "fixture-smoke-1" || len(args) != 1 {
					t.Error("wrong delete scope")
				}
				deleted = true
				writes++
				body = map[string]any{"memory_id": "fixture-smoke-1", "message": "Memory deleted"}
			} else {
				results := []any{}
				if !deleted {
					results = append(results, record)
				}
				if args["memory_id"] != nil {
					body = map[string]any{"results": results}
				} else {
					if args["exhaustive"] != true || args["offset"] != 0.0 {
						t.Error("wrong enumeration request")
					}
					body = map[string]any{"mode": "enumeration", "limit": 200, "offset": 0, "has_more": false, "results": results}
				}
			}
			raw, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
		})
	}
	provider := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(provider.Close)
	e := newEnv(t, &connectors.Spec{Name: "memory", URL: provider.URL})
	token := e.owner(t, "cleanup-fixture")
	run := func(extra ...string) memorycleanup.Manifest {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		args := append([]string{"-url", e.srv.URL + "/mcp/memory"}, extra...)
		cmd := exec.CommandContext(ctx, cleanupBin, args...)
		cmd.Env = append(os.Environ(), "BROKER_TOKEN="+token)
		// Stdout is the raw reviewable manifest; stderr contains only dry-run status/errors.
		raw, err := cmd.Output()
		if err != nil {
			t.Fatalf("cleanup command failed: %v", err)
		}
		var m memorycleanup.Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	manifest := run()
	if len(manifest.Memories) != 1 || manifest.Memories[0].ID != "fixture-smoke-1" {
		t.Fatalf("wrong manifest: %+v", manifest)
	}
	path := filepath.Join(t.TempDir(), "reviewed.json")
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	run("-manifest", path)
	mu.Lock()
	if writes != 0 {
		t.Error("default command deleted a record")
	}
	mu.Unlock()
	run("-manifest", path, "-execute")
	mu.Lock()
	defer mu.Unlock()
	if writes != 1 || !deleted {
		t.Fatalf("explicit execute failed: writes=%d", writes)
	}
	t.Log("built CLI: inventory and manifest modes made no writes; explicit execute deleted one local fixture ID and verified absence")
}
