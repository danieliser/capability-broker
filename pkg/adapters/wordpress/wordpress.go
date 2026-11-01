// Package wordpress hosts the wp-mcp-router as one supervised stdio child and
// forwards authorized tool calls to it. The router's sites.json (application
// passwords) is read by the child only; agents see tool names verbatim.
package wordpress

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/adapter"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/config"
)

const AdapterVersion = "1.0"

type Adapter struct {
	cfg  config.WordPress
	cat  *catalog.Catalog
	conn *catalog.Connector

	mu        sync.Mutex
	session   *mcp.ClientSession
	tools     []catalog.Tool
	siteHosts map[string]string
	connected time.Time
}

func New(cfg config.WordPress, cat *catalog.Catalog) *Adapter {
	conn, _ := cat.Connector("wp-mcp-router")
	return &Adapter{cfg: cfg, cat: cat, conn: conn, siteHosts: map[string]string{}}
}

func (a *Adapter) Connector() string { return "wp-mcp-router" }

// SiteHosts returns site id → hostname, learned from wp_list_sites at connect time.
func (a *Adapter) SiteHosts() map[string]string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := map[string]string{}
	for k, v := range a.siteHosts {
		out[k] = v
	}
	return out
}

// Start connects the child eagerly so the first agent call does not pay the npx cold start.
func (a *Adapter) Start(ctx context.Context) error {
	_, err := a.ensure(ctx)
	return err
}

func (a *Adapter) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		err := a.session.Close()
		a.session = nil
		return err
	}
	return nil
}

func (a *Adapter) ensure(ctx context.Context) (*mcp.ClientSession, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		return a.session, nil
	}
	if a.cfg.Command == "" {
		return nil, adapter.Unavailable("wp-mcp-router command is not configured")
	}
	if _, err := os.Stat(a.cfg.ConfigPath); err != nil {
		return nil, adapter.Unavailable("wp-mcp-router sites config is not readable by the broker")
	}
	cmd := exec.Command(a.cfg.Command, a.cfg.Args...)
	// Minimal environment: the child needs PATH and HOME for npx, and its own config path. Nothing else leaks in.
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"WP_MCP_ROUTER_CONFIG=" + a.cfg.ConfigPath,
	}
	cmd.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "brokerd", Version: "1"}, nil)
	cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	session, err := client.Connect(cctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil)
	if err != nil {
		return nil, adapter.Unavailable("wp-mcp-router did not start: " + adapter.Truncate(err.Error(), 200))
	}
	tools, err := a.discover(cctx, session)
	if err != nil {
		_ = session.Close()
		return nil, err
	}
	a.session, a.tools, a.connected = session, tools, time.Now()
	a.learnSites(cctx, session)
	return session, nil
}

func (a *Adapter) discover(ctx context.Context, s *mcp.ClientSession) ([]catalog.Tool, error) {
	var out []catalog.Tool
	cursor := ""
	for {
		res, err := s.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, adapter.Unavailable("wp-mcp-router tools/list failed: " + adapter.Truncate(err.Error(), 200))
		}
		for _, t := range res.Tools {
			if !a.conn.ToolExposed(t.Name) {
				continue
			}
			schema := map[string]any{}
			if raw, err := json.Marshal(t.InputSchema); err == nil {
				_ = json.Unmarshal(raw, &schema)
			}
			out = append(out, catalog.Tool{Name: t.Name, Description: t.Description, InputSchema: schema})
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
	}
	return out, nil
}

// learnSites fills siteHosts from wp_list_sites so URL policy can check hosts. Best effort.
func (a *Adapter) learnSites(ctx context.Context, s *mcp.ClientSession) {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "wp_list_sites", Arguments: map[string]any{}})
	if err != nil || res == nil || res.IsError {
		return
	}
	var payload any
	if res.StructuredContent != nil {
		payload = res.StructuredContent
	} else {
		for _, c := range res.Content {
			if tc, ok := c.(*mcp.TextContent); ok {
				_ = json.Unmarshal([]byte(tc.Text), &payload)
				break
			}
		}
	}
	walk(payload, func(obj map[string]any) {
		id, _ := obj["id"].(string)
		raw, _ := obj["url"].(string)
		if id == "" || raw == "" {
			return
		}
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			a.siteHosts[id] = strings.ToLower(u.Hostname())
		}
	})
}

func walk(v any, fn func(map[string]any)) {
	switch t := v.(type) {
	case map[string]any:
		fn(t)
		for _, x := range t {
			walk(x, fn)
		}
	case []any:
		for _, x := range t {
			walk(x, fn)
		}
	}
}

// Tools returns the router's definitions, with the `site` enum narrowed to the grant's sites.
func (a *Adapter) Tools(ctx context.Context, g *auth.Grant) ([]catalog.Tool, error) {
	if _, err := a.ensure(ctx); err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	cap, _ := g.Capabilities.For("wp-mcp-router")
	owner := g.Capabilities.IsOwner()
	out := make([]catalog.Tool, 0, len(a.tools))
	for _, t := range a.tools {
		tool := t
		if !owner {
			tool.InputSchema = narrowSites(t.InputSchema, cap.Sites)
		}
		out = append(out, tool)
	}
	return out, nil
}

func narrowSites(schema map[string]any, sites []string) map[string]any {
	raw, _ := json.Marshal(schema)
	var copy map[string]any
	_ = json.Unmarshal(raw, &copy)
	props, _ := copy["properties"].(map[string]any)
	if site, ok := props["site"].(map[string]any); ok {
		enum := make([]any, 0, len(sites))
		for _, s := range sites {
			enum = append(enum, s)
		}
		site["enum"] = enum
	}
	return copy
}

func (a *Adapter) Health(ctx context.Context) error {
	s, err := a.ensure(ctx)
	if err != nil {
		return err
	}
	return s.Ping(ctx, nil)
}

func (a *Adapter) Execute(ctx context.Context, call adapter.Call) (*adapter.Result, error) {
	s, err := a.ensure(ctx)
	if err != nil {
		return nil, err
	}
	args := map[string]any{}
	for k, v := range call.Decision.Args {
		args[k] = v
	}
	for _, g := range call.Op.Guards {
		if g == "force_draft_status" {
			inner, _ := args["arguments"].(map[string]any)
			if inner == nil {
				inner = map[string]any{}
			}
			inner["status"] = "draft"
			args["arguments"] = inner
		}
	}
	if call.Tool == "wp_run" && !call.Verbose {
		args["compact"] = true
	}
	site := adapter.ArgString(args, "site")
	cctx, cancel := context.WithTimeout(ctx, a.cfg.Timeout)
	defer cancel()
	res, err := s.CallTool(cctx, &mcp.CallToolParams{Name: call.Tool, Arguments: args})
	if err != nil {
		if cctx.Err() != nil {
			return nil, &adapter.UpstreamError{Code: adapter.CodeUpstreamError, Message: "wp-mcp-router call timed out", Outcome: adapter.OutcomeUnknown}
		}
		a.dropSession()
		return nil, adapter.Unavailable("wp-mcp-router call failed: " + adapter.Truncate(err.Error(), 200))
	}
	payload := payloadOf(res)
	if res.IsError {
		msg, _ := json.Marshal(payload)
		return nil, adapter.Failed(string(msg))
	}
	compact := shapeCompact(payload, adapter.FieldsFromCompact(call.Op))
	return &adapter.Result{Compact: compact, Verbose: payload, Outcome: adapter.OutcomeCompleted, OutcomeClass: "ok", Account: site}, nil
}

func (a *Adapter) dropSession() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		_ = a.session.Close()
		a.session = nil
	}
}

func payloadOf(res *mcp.CallToolResult) any {
	if res.StructuredContent != nil {
		return res.StructuredContent
	}
	var texts []string
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			texts = append(texts, tc.Text)
		}
	}
	joined := strings.Join(texts, "\n")
	var parsed any
	if err := json.Unmarshal([]byte(joined), &parsed); err == nil {
		return parsed
	}
	return joined
}

// shapeCompact keeps the declared fields of an object, or of each object in a list.
func shapeCompact(payload any, fields []string) any {
	if len(fields) == 0 {
		return payload
	}
	switch t := payload.(type) {
	case map[string]any:
		if inner, ok := t["result"]; ok {
			return map[string]any{"result": shapeCompact(inner, fields), "site": t["site"]}
		}
		// Only post-like objects (those carrying an id) are reduced to the declared fields;
		// anything else (site info, taxonomies, settings) passes through unchanged.
		if _, isPost := t["id"]; !isPost {
			return t
		}
		return adapter.PickFields(t, fields)
	case []any:
		out := make([]any, 0, len(t))
		for _, x := range t {
			out = append(out, shapeCompact(x, fields))
		}
		return out
	}
	return payload
}

var _ adapter.Adapter = (*Adapter)(nil)
