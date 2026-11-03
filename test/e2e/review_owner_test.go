package e2e

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/danieliser/capability-broker/pkg/connectors"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestReviewOwnerUncataloguedWordPressActionIsTransparent(t *testing.T) {
	provider := mcp.NewServer(&mcp.Implementation{Name: "owner-fixture", Version: "1"}, nil)
	for _, name := range []string{"wp_run", "wp_owner_extension"} {
		provider.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(req.Params.Arguments)}}}, nil
		})
	}
	srv := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return provider }, &mcp.StreamableHTTPOptions{Stateless: true}))
	t.Cleanup(srv.Close)
	e := newEnv(t, &connectors.Spec{Name: "wp-mcp-router", URL: srv.URL})
	s := e.connect(t, e.owner(t, "review"), "/mcp/wp-mcp-router", nil)
	args := map[string]any{"site": "owner-site", "ability_name": "owner/private-action", "verbose": true, "arguments": map[string]any{"status": "publish"}}
	for _, tool := range []string{"wp_run", "wp_owner_extension"} {
		res := call(t, s, tool, args)
		var got map[string]any
		if res.IsError || json.Unmarshal([]byte(firstText(res)), &got) != nil || !reflect.DeepEqual(got, args) {
			t.Fatalf("%s owner call changed or denied: %v", tool, firstText(res))
		}
	}
}
