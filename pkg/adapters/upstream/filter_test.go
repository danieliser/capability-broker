package upstream

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/connectors"
)

func TestFilterResult(t *testing.T) {
	res := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"a":1,"_links":{"x":1},"n":{"debug":2,"k":3}}`}},
		StructuredContent: map[string]any{"debug": 1, "keep": 2}}
	out, err := filterResult(context.Background(), res, &connectors.ResponseFilter{DropKeys: []string{"_links", "debug"}})
	if err != nil {
		t.Fatal(err)
	}
	txt := out.Content[0].(*mcp.TextContent).Text
	if strings.Contains(txt, "_links") || strings.Contains(txt, "debug") || !strings.Contains(txt, `"k":3`) {
		t.Fatalf("drop_keys: %s", txt)
	}
	if sc := out.StructuredContent.(map[string]any); sc["debug"] != nil || sc["keep"] == nil {
		t.Fatalf("structured: %v", sc)
	}
	if res.Content[0].(*mcp.TextContent).Text == txt {
		t.Fatal("original result was modified in place")
	}

	long := &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: strings.Repeat("x", 50)}}}
	out, _ = filterResult(context.Background(), long, &connectors.ResponseFilter{MaxChars: 10})
	if got := out.Content[0].(*mcp.TextContent).Text; !strings.HasPrefix(got, "xxxxxxxxxx\n…[40 more") {
		t.Fatalf("max_chars: %q", got)
	}

	out, err = filterResult(context.Background(), long, &connectors.ResponseFilter{Command: []string{"tr", "x", "y"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := out.Content[0].(*mcp.TextContent).Text; !strings.Contains(got, "yyyy") {
		t.Fatalf("command: %q", got)
	}
	if _, err := filterResult(context.Background(), long, &connectors.ResponseFilter{Command: []string{"false"}}); err == nil {
		t.Fatal("failing filter must fail the call")
	}
}
