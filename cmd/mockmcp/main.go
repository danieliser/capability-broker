// mockmcp is a stdio MCP server that exercises every protocol feature the broker must pass through: tool
// annotations, output schemas, structured and image results, upstream errors, instructions, prompts,
// resources, resource templates and runtime list changes. It backs the end-to-end tests and live checks:
//
//	connectors:
//	  mock:
//	    command: /path/to/mockmcp
package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"strings"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Instructions = "mockmcp: test server. Use echo to echo text."

// a 1x1 PNG
var pixel, _ = base64.StdEncoding.DecodeString("iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==")

type echoIn struct {
	Text string `json:"text" jsonschema:"text to echo"`
}
type echoOut struct {
	Echo   string `json:"echo"`
	Length int    `json:"length"`
}

func main() {
	s := mcp.NewServer(&mcp.Implementation{Name: "mockmcp", Version: "1"}, &mcp.ServerOptions{Instructions: Instructions,
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true},
			Prompts: &mcp.PromptCapabilities{ListChanged: true}, Resources: &mcp.ResourceCapabilities{ListChanged: true}}})
	f := false
	mcp.AddTool(s, &mcp.Tool{Name: "echo", Title: "Echo", Description: "Echo text back with its length.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, DestructiveHint: &f}, Meta: mcp.Meta{"mock/kind": "echo"}},
		func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, echoOut, error) {
			return nil, echoOut{Echo: in.Text, Length: len(in.Text)}, nil
		})
	s.AddTool(&mcp.Tool{Name: "markdown", Description: "Return a markdown document.", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "# Title\n\n- one\n- two\n"}}}, nil
		})
	s.AddTool(&mcp.Tool{Name: "pixel", Description: "Return an image.", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "a pixel"}, &mcp.ImageContent{Data: pixel, MIMEType: "image/png"}}}, nil
		})
	s.AddTool(&mcp.Tool{Name: "fail", Description: "Always fails with a tool error.", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "mock failure: as requested"}}}, nil
		})
	s.AddTool(&mcp.Tool{Name: "noisy", Description: "Return JSON with noise keys.", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: `{"keep":1,"debug":{"trace":"x"},"_links":{"self":"/x"},"long":"` + strings.Repeat("y", 2000) + `"}`}}}, nil
		})
	var added atomic.Int32
	s.AddTool(&mcp.Tool{Name: "grow", Description: "Add a new tool, prompt and resource at runtime (announced with list_changed).", InputSchema: map[string]any{"type": "object"}},
		func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			n := added.Add(1)
			name := fmt.Sprintf("added_%d", n)
			s.AddTool(&mcp.Tool{Name: name, Description: "Added at runtime.", InputSchema: map[string]any{"type": "object"}},
				func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name + " ran"}}}, nil
				})
			s.AddPrompt(&mcp.Prompt{Name: "prompt_" + name, Description: "Added at runtime."}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
				return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: name}}}}, nil
			})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "added " + name}}}, nil
		})
	s.AddPrompt(&mcp.Prompt{Name: "greet", Title: "Greet", Description: "Greet someone.", Arguments: []*mcp.PromptArgument{{Name: "name", Required: true}}},
		func(_ context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			return &mcp.GetPromptResult{Description: "greeting", Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "Say hello to " + req.Params.Arguments["name"] + "."}}}}, nil
		})
	s.AddResource(&mcp.Resource{URI: "mock://readme", Name: "readme", MIMEType: "text/markdown", Description: "Mock readme."},
		func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "mock://readme", MIMEType: "text/markdown", Text: "# Mock readme"}}}, nil
		})
	s.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "mock://items/{id}", Name: "item", MIMEType: "application/json"},
		func(_ context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			id := strings.TrimPrefix(req.Params.URI, "mock://items/")
			return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, MIMEType: "application/json", Text: `{"id":"` + id + `"}`}}}, nil
		})
	if err := s.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		log.Fatal(err)
	}
}
