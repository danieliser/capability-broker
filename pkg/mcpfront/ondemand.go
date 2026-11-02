package mcpfront

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/connectors"
)

// Connectors with `expose: broker` stay inside the broker for owner clients: their tools are not preloaded
// into the client's context but found and run through three discovery tools (progressive disclosure).

const discoveryOwner = "\x00discovery"

var discoveryTools = []string{"search_tools", "describe_tool", "call_tool"}

func (f *Front) specOf(name string) *connectors.Spec {
	ad, ok := f.Registry.Lookup(name)
	if !ok {
		return nil
	}
	if s, ok := ad.(interface{ Spec() *connectors.Spec }); ok {
		return s.Spec()
	}
	return nil
}

// shapeTool removes what the connector strips from a tool definition.
func (f *Front) shapeTool(connector string, t mcp.Tool) mcp.Tool {
	sp := f.specOf(connector)
	if sp == nil {
		return t
	}
	if sp.Strips("outputSchema") || sp.Response != nil {
		// A reshaped result may no longer match the declared schema; clients validate against it.
		t.OutputSchema = nil
	}
	if sp.Strips("annotations") {
		t.Annotations = nil
	}
	if sp.Strips("title") {
		t.Title = ""
	}
	if sp.Strips("icons") {
		t.Icons = nil
	}
	if sp.Strips("meta") {
		t.Meta = nil
	}
	return t
}

func discoveryInstructions(onDemand []string) string {
	return "More tools are available on demand from: " + strings.Join(onDemand, ", ") +
		". They are not listed up front; use search_tools to find one (by words or by connector), describe_tool to see its arguments, and call_tool to run it."
}

// syncDiscovery adds or removes the discovery tools on a live server.
func (f *Front) syncDiscovery(ls *liveServer, onDemand []string, desired map[string]def) {
	if len(onDemand) == 0 {
		var remove []string
		for name, owner := range ls.toolOwner {
			if owner == discoveryOwner {
				remove = append(remove, name)
			}
		}
		if len(remove) > 0 {
			ls.server.RemoveTools(remove...)
			for _, n := range remove {
				delete(ls.toolOwner, n)
				delete(ls.toolHash, n)
			}
		}
		return
	}
	allowed := map[string]bool{}
	for _, n := range onDemand {
		allowed[n] = true
	}
	names := strings.Join(onDemand, ", ")
	defs := map[string]*mcp.Tool{
		"search_tools": {Name: "search_tools", Description: "Find tools from the on-demand connectors (" + names + "). Give words describing the task, a connector name, or both; with neither, lists each connector and its tools.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"words to match against tool names and descriptions"},"connector":{"type":"string","description":"limit to one connector"}}}`),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		"describe_tool": {Name: "describe_tool", Description: "Show one on-demand tool's full definition, including its input schema.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"connector":{"type":"string"},"tool":{"type":"string"}},"required":["connector","tool"]}`),
			Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true}},
		"call_tool": {Name: "call_tool", Description: "Run an on-demand tool found with search_tools. Returns the tool's own result.",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"connector":{"type":"string"},"tool":{"type":"string"},"arguments":{"type":"object"}},"required":["connector","tool"]}`)},
	}
	for _, base := range discoveryTools {
		name := base
		if _, clash := desired[name]; clash {
			name = "broker__" + base
		}
		t := *defs[base]
		t.Name = name
		h := "discovery:" + names
		if ls.toolHash[name] == h {
			continue
		}
		kind := base
		ls.server.AddTool(&t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return f.discover(ctx, kind, allowed, req)
		})
		ls.toolHash[name] = h
		ls.toolOwner[name] = discoveryOwner
	}
}

type discArgs struct {
	Query     string          `json:"query"`
	Connector string          `json:"connector"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
}

func (f *Front) discover(ctx context.Context, kind string, allowed map[string]bool, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	var a discArgs
	if len(req.Params.Arguments) > 0 {
		_ = json.Unmarshal(req.Params.Arguments, &a)
	}
	g, _ := ctx.Value(grantKey).(*auth.Grant)
	if g == nil {
		return toolError("BROKER_UNAUTHENTICATED", "no grant on this call"), nil
	}
	if a.Connector != "" && !allowed[a.Connector] {
		return toolError("BROKER_UNKNOWN_CONNECTOR", "no on-demand connector "+a.Connector), nil
	}
	switch kind {
	case "call_tool":
		args := a.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		inner := &mcp.CallToolRequest{Session: req.Session, Extra: req.Extra,
			Params: &mcp.CallToolParamsRaw{Name: a.Tool, Arguments: args, Meta: req.Params.Meta}}
		if !f.hasTool(ctx, a.Connector, a.Tool, g) {
			return toolError("BROKER_UNKNOWN_TOOL", a.Connector+" has no tool "+a.Tool+"; use search_tools"), nil
		}
		return f.call(ctx, a.Connector, a.Tool, inner)
	case "describe_tool":
		for _, t := range f.toolsOf(ctx, a.Connector, g) {
			if t.Name == a.Tool {
				def := f.shapeTool(a.Connector, *t.Upstream)
				return jsonResult(def), nil
			}
		}
		return toolError("BROKER_UNKNOWN_TOOL", a.Connector+" has no tool "+a.Tool), nil
	}
	// search_tools
	var conns []string
	for n := range allowed {
		if a.Connector == "" || a.Connector == n {
			conns = append(conns, n)
		}
	}
	sort.Strings(conns)
	words := strings.Fields(strings.ToLower(a.Query))
	type hit struct {
		Connector   string `json:"connector"`
		Tool        string `json:"tool"`
		Title       string `json:"title,omitempty"`
		Summary     string `json:"summary"`
		ReadOnly    bool   `json:"read_only,omitempty"`
		Destructive bool   `json:"destructive,omitempty"`
		score       int
	}
	if len(words) == 0 {
		out := map[string][]string{}
		for _, c := range conns {
			for _, t := range f.toolsOf(ctx, c, g) {
				out[c] = append(out[c], t.Name)
			}
		}
		return jsonResult(map[string]any{"connectors": out, "next": "search_tools with words, then describe_tool and call_tool"}), nil
	}
	var hits []hit
	for _, c := range conns {
		for _, t := range f.toolsOf(ctx, c, g) {
			hay := strings.ToLower(t.Name + " " + t.Description)
			title := ""
			if t.Upstream != nil {
				title = t.Upstream.Title
				hay += " " + strings.ToLower(title)
			}
			score := 0
			for _, w := range words {
				if strings.Contains(hay, w) {
					score++
					if strings.Contains(strings.ToLower(t.Name), w) {
						score++
					}
				}
			}
			if score == 0 {
				continue
			}
			h := hit{Connector: c, Tool: t.Name, Title: title, Summary: firstSentence(t.Description), score: score}
			if t.Upstream != nil && t.Upstream.Annotations != nil {
				h.ReadOnly = t.Upstream.Annotations.ReadOnlyHint
				if d := t.Upstream.Annotations.DestructiveHint; d != nil {
					h.Destructive = *d
				}
			}
			hits = append(hits, h)
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > 20 {
		hits = hits[:20]
	}
	return jsonResult(map[string]any{"tools": hits}), nil
}

func (f *Front) toolsOf(ctx context.Context, connector string, g *auth.Grant) []catalog.Tool {
	ad, ok := f.Registry.Lookup(connector)
	if !ok {
		return nil
	}
	tools, err := ad.Tools(ctx, g)
	if err != nil {
		return nil
	}
	out := tools[:0:0]
	for _, t := range tools {
		if t.Upstream != nil {
			out = append(out, t)
		}
	}
	return out
}

func (f *Front) hasTool(ctx context.Context, connector, tool string, g *auth.Grant) bool {
	for _, t := range f.toolsOf(ctx, connector, g) {
		if t.Name == tool {
			return true
		}
	}
	return false
}

func firstSentence(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if i := strings.Index(s, ". "); i > 0 && i < 200 {
		return s[:i+1]
	}
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

func jsonResult(v any) *mcp.CallToolResult {
	b, err := json.Marshal(v)
	if err != nil {
		b = []byte(fmt.Sprintf(`{"error":%q}`, err.Error()))
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(b)}}}
}
