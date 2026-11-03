package upstream

import (
	"context"
	"reflect"
	"testing"

	"github.com/danieliser/capability-broker/pkg/auth"
)

func TestReviewOwnerArgumentsRemainVerbatim(t *testing.T) {
	for _, tt := range []struct {
		name, connector, tool string
		args                  map[string]any
	}{
		{"raw write SQL", "clickhouse", "run_query", map[string]any{"query": "CREATE TABLE fixture (n UInt8)"}},
		{"unregistered named query", "clickhouse", "run_query", map[string]any{"query": "@owner.private"}},
		{"explicit row count", "clickhouse", "run_query", map[string]any{"query": "SELECT 1", "max_rows": 1000000.0}},
		{"published post", "wp-mcp-router", "wp_run", map[string]any{"site": "a", "ability_name": "core/create-post", "arguments": map[string]any{"status": "publish"}}},
		{"read refs", "wp-mcp-router", "wp_run", map[string]any{"site": "a", "ability_name": "gk-block-mcp/get-page-blocks", "arguments": map[string]any{"persist_refs": true}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			child := &fakeChild{reply: func(_ string, args map[string]any) any { return args }}
			a, cat := adapterWith(t, tt.connector, child, tt.tool)
			g := &auth.Grant{Kind: auth.KindClient, Capabilities: auth.OwnerCapabilities()}
			res, err := run(t, a, cat, g, tt.tool, tt.args, false)
			if err != nil {
				t.Fatal(err)
			}
			if res.Raw == nil || !reflect.DeepEqual(child.last, tt.args) {
				t.Fatalf("owner args changed: got %v want %v", child.last, tt.args)
			}
		})
	}
}

func TestReviewOwnerExposurePreservesExplicitDenials(t *testing.T) {
	child := &fakeChild{reply: func(_ string, args map[string]any) any { return args }}
	a, cat := adapterWith(t, "wp-mcp-router", child, "wp_run", "wp_owner_extension", "wp_hidden")
	conn, _ := cat.Connector("wp-mcp-router")
	conn.ToolsDeny = []string{"wp_hidden"}
	owner := &auth.Grant{Kind: auth.KindClient, Capabilities: auth.OwnerCapabilities()}
	tools, err := a.Tools(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	if !names["wp_owner_extension"] || names["wp_hidden"] {
		t.Fatalf("owner exposure bypassed explicit denial or lost extension: %v", names)
	}
	tools, err = a.Tools(context.Background(), agent(auth.Capabilities{"wp-mcp-router": {Operations: []string{"content.read"}}}))
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range tools {
		if tool.Name == "wp_owner_extension" || tool.Name == "wp_hidden" {
			t.Fatalf("agent reached %s", tool.Name)
		}
	}
}
