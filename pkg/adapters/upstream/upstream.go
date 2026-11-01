// Package upstream hosts any stdio MCP server as a broker connector: one
// supervised child per connector, tools discovered and exported verbatim,
// policy from the connector's catalog, credentials only in the child's env.
// Request hooks (guards), response hooks and tool transforms are small named
// functions the catalog refers to; a connector with no catalog gets a
// synthesized one where every tool is its own operation.
package upstream

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/adapter"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/compact"
	"github.com/danieliser/capability-broker/pkg/connectors"
)

type Adapter struct {
	spec  *connectors.Spec
	cat   *catalog.Catalog
	relay *relay
	log   func(msg string, kv ...any)

	mu        sync.Mutex // guards session, tools, conn, connected; held while the child starts
	session   *mcp.ClientSession
	tools     []catalog.Tool
	conn      *catalog.Connector
	connected time.Time

	hostsMu   sync.RWMutex // never held across the child's startup
	siteHosts map[string]string
}

func New(spec *connectors.Spec, cat *catalog.Catalog, log func(msg string, kv ...any)) *Adapter {
	if log == nil {
		log = func(string, ...any) {}
	}
	return &Adapter{spec: spec, cat: cat, relay: newRelay(), log: log, siteHosts: map[string]string{}}
}

func (a *Adapter) Connector() string { return a.spec.Name }

// SiteHosts returns site id → hostname learned from the catalog's site_hosts_tool.
func (a *Adapter) SiteHosts() map[string]string {
	a.hostsMu.RLock()
	defer a.hostsMu.RUnlock()
	out := make(map[string]string, len(a.siteHosts))
	for k, v := range a.siteHosts {
		out[k] = v
	}
	return out
}

// Start connects eagerly so the first call does not pay the child's cold start.
func (a *Adapter) Start(ctx context.Context) error {
	_, _, err := a.ensure(ctx)
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

func (a *Adapter) ensure(ctx context.Context) (*mcp.ClientSession, *catalog.Connector, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		return a.session, a.conn, nil
	}
	cmd := exec.Command(a.spec.Command, a.spec.Args...)
	// The child's environment is exactly what the connector declares, plus PATH and HOME so package
	// runners (npx, uvx) work. Nothing else from the broker's environment leaks in.
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME")}
	for k, v := range a.spec.Env {
		env = append(env, k+"="+v)
	}
	cmd.Env = env
	if a.spec.Cwd != "" {
		cmd.Dir = a.spec.Cwd
	}
	cmd.Stderr = os.Stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "brokerd", Version: "1"}, a.relay.clientOptions())
	cctx, cancel := context.WithTimeout(ctx, a.spec.StartupTimeout)
	defer cancel()
	session, err := client.Connect(cctx, &mcp.CommandTransport{Command: cmd, TerminateDuration: 5 * time.Second}, nil)
	if err != nil {
		return nil, nil, adapter.Unavailable(a.spec.Name + " did not start: " + adapter.Truncate(err.Error(), 200))
	}
	tools, err := discover(cctx, session)
	if err != nil {
		_ = session.Close()
		return nil, nil, adapter.Unavailable(a.spec.Name + " tools/list failed: " + adapter.Truncate(err.Error(), 200))
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	conn := a.cat.Ensure(a.spec.Name, names)
	a.session, a.tools, a.conn, a.connected = session, tools, conn, time.Now()
	a.log("connector connected", "connector", a.spec.Name, "tools", len(tools), "catalog", conn.Source)
	if conn.SiteHostsTool != "" {
		a.learnSites(cctx, session, conn.SiteHostsTool)
	}
	return session, conn, nil
}

func discover(ctx context.Context, s *mcp.ClientSession) ([]catalog.Tool, error) {
	var out []catalog.Tool
	cursor := ""
	for {
		res, err := s.ListTools(ctx, &mcp.ListToolsParams{Cursor: cursor})
		if err != nil {
			return nil, err
		}
		for _, t := range res.Tools {
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

func (a *Adapter) learnSites(ctx context.Context, s *mcp.ClientSession, tool string) {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: map[string]any{}})
	if err != nil || res == nil || res.IsError {
		return
	}
	hosts := map[string]string{}
	walk(payloadOf(res), func(obj map[string]any) {
		id, _ := obj["id"].(string)
		raw, _ := obj["url"].(string)
		if id == "" || raw == "" {
			return
		}
		if u, err := url.Parse(raw); err == nil && u.Hostname() != "" {
			hosts[id] = strings.ToLower(u.Hostname())
		}
	})
	a.hostsMu.Lock()
	a.siteHosts = hosts
	a.hostsMu.Unlock()
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

func (a *Adapter) dropSession() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.session != nil {
		_ = a.session.Close()
		a.session = nil
	}
}

// Health lists tools rather than pinging: some upstreams (FastMCP) reject a ping whose _meta lacks
// the newest protocol fields, and tools/list is what every call depends on anyway.
func (a *Adapter) Health(ctx context.Context) error {
	s, _, err := a.ensure(ctx)
	if err != nil {
		return err
	}
	_, err = s.ListTools(ctx, &mcp.ListToolsParams{})
	return err
}

// Tools returns the exported upstream tools this grant can reach: not denied by
// the catalog, and bound to at least one operation the grant's capabilities
// could satisfy. Owner grants see every exported tool. Transforms and
// description hooks from the catalog apply per grant.
func (a *Adapter) Tools(ctx context.Context, g *auth.Grant) ([]catalog.Tool, error) {
	if _, _, err := a.ensure(ctx); err != nil {
		return nil, err
	}
	a.mu.Lock()
	tools, conn := a.tools, a.conn
	a.mu.Unlock()
	cap, _ := g.Capabilities.For(a.spec.Name)
	owner := g.Capabilities.IsOwner()
	out := make([]catalog.Tool, 0, len(tools))
	for _, t := range tools {
		if !conn.ToolExposed(t.Name) {
			continue
		}
		if !owner && !reachable(conn.OperationsFor(t.Name), cap) {
			continue
		}
		tool := t
		if hook, ok := conn.Describe[t.Name]; ok && !owner {
			tool.Description = a.describe(hook, t.Description, cap)
		}
		for _, tr := range conn.ToolTransforms {
			tool = a.transform(tr, tool, cap, owner)
		}
		out = append(out, tool)
	}
	return out, nil
}

// reachable: at least one operation on the tool has a capability requirement this grant meets.
func reachable(ops []*catalog.Operation, cap auth.ConnectorCapability) bool {
	for _, op := range ops {
		switch {
		case op.RequiresCapability == "":
			return true
		case op.RequiresCapability == "sql.select":
			if cap.SQL == "select" {
				return true
			}
		case strings.HasPrefix(op.RequiresCapability, "operations:"):
			if contains(cap.Operations, strings.TrimPrefix(op.RequiresCapability, "operations:")) {
				return true
			}
		}
	}
	return false
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (a *Adapter) describe(hook, base string, cap auth.ConnectorCapability) string {
	switch hook {
	case "named_queries":
		var b strings.Builder
		if cap.SQL == "select" {
			b.WriteString(base)
			b.WriteString(" ")
		} else {
			b.WriteString("Run one of this grant's named queries by passing query=\"@id\" and params. Ad-hoc SQL is not permitted for this grant. ")
		}
		qs := a.cat.Queries(cap.Queries)
		if len(qs) > 0 {
			b.WriteString("Named queries: ")
			for i, q := range qs {
				if i > 0 {
					b.WriteString("; ")
				}
				b.WriteString("@" + q.ID)
				if len(q.Params) > 0 {
					b.WriteString("(")
					first := true
					for name, p := range q.Params {
						if !first {
							b.WriteString(", ")
						}
						first = false
						b.WriteString(name + ":" + p.Type)
						if p.Required {
							b.WriteString("!")
						}
					}
					b.WriteString(")")
				}
				if q.Summary != "" {
					b.WriteString(" — " + q.Summary)
				}
			}
			b.WriteString(".")
		}
		return b.String()
	}
	return base
}

func (a *Adapter) transform(name string, t catalog.Tool, cap auth.ConnectorCapability, owner bool) catalog.Tool {
	switch name {
	case "narrow_site_enum":
		if owner {
			return t
		}
		raw, _ := json.Marshal(t.InputSchema)
		var schema map[string]any
		_ = json.Unmarshal(raw, &schema)
		props, _ := schema["properties"].(map[string]any)
		if site, ok := props["site"].(map[string]any); ok {
			enum := make([]any, 0, len(cap.Sites))
			for _, s := range cap.Sites {
				enum = append(enum, s)
			}
			site["enum"] = enum
		}
		t.InputSchema = schema
	}
	return t
}

// Execute runs the guards, forwards the call to the child with a progress token
// for the relay, then runs the response hooks and shapes the result.
func (a *Adapter) Execute(ctx context.Context, call adapter.Call) (*adapter.Result, error) {
	s, _, err := a.ensure(ctx)
	if err != nil {
		return nil, err
	}
	args := map[string]any{}
	for k, v := range call.Decision.Args {
		args[k] = v
	}
	for _, g := range call.Op.Guards {
		if err := a.guard(ctx, s, g, call, args); err != nil {
			return nil, err
		}
	}
	callID := call.CallID
	if callID == "" {
		callID = uuid.NewString()
	}
	a.relay.begin(callID, call.Session, call.ProgressToken)
	defer a.relay.end(callID)
	cctx, cancel := context.WithTimeout(ctx, a.spec.CallTimeout)
	defer cancel()
	params := &mcp.CallToolParams{Name: call.Tool, Arguments: args, Meta: mcp.Meta{"progressToken": callID}}
	res, err := s.CallTool(cctx, params)
	if err != nil {
		if cctx.Err() != nil {
			return nil, &adapter.UpstreamError{Code: adapter.CodeUpstreamError, Message: a.spec.Name + " call timed out", Outcome: adapter.OutcomeUnknown}
		}
		a.dropSession()
		return nil, adapter.Unavailable(a.spec.Name + " call failed: " + adapter.Truncate(err.Error(), 200))
	}
	payload := payloadOf(res)
	if res.IsError {
		msg, _ := json.Marshal(payload)
		return nil, adapter.Failed(string(msg))
	}
	for _, h := range call.Op.Response {
		payload, err = a.respond(h, call, payload)
		if err != nil {
			return nil, err
		}
	}
	return &adapter.Result{
		Compact: shape(payload, call.Op), Verbose: payload, Outcome: adapter.OutcomeCompleted, OutcomeClass: "ok",
		Account: accountOf(args), UpstreamRequestID: upstreamID(payload),
	}, nil
}

func accountOf(args map[string]any) string {
	if s, ok := args["site"].(string); ok {
		return s
	}
	return ""
}

func upstreamID(payload any) string {
	if m, ok := payload.(map[string]any); ok {
		for _, k := range []string{"query_id", "memory_id", "id", "request_id"} {
			if s, ok := m[k].(string); ok {
				return s
			}
		}
	}
	return ""
}

// payloadOf turns a tool result into a value: structured content first, then the
// text content parsed as JSON, then the joined text.
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

// shape applies the operation's compact spec: explicit fields when declared,
// otherwise generic trimming with the spec's limits.
func shape(payload any, op *catalog.Operation) any {
	fields := fieldsOf(op)
	opts := compact.Options{}
	if v, ok := numberOf(op.Compact["max_items"]); ok {
		opts.MaxItems = v
	}
	if v, ok := numberOf(op.Compact["max_string"]); ok {
		opts.MaxString = v
	}
	if len(fields) > 0 {
		return compact.Trim(pickDeep(payload, fields), opts)
	}
	return compact.Trim(payload, opts)
}

// pickDeep keeps declared fields on post-like objects (those carrying an id), at the top level or
// under a `result` wrapper; everything else passes through.
func pickDeep(payload any, fields []string) any {
	switch t := payload.(type) {
	case map[string]any:
		if inner, ok := t["result"]; ok {
			return map[string]any{"result": pickDeep(inner, fields), "site": t["site"]}
		}
		if _, isPost := t["id"]; isPost {
			return compact.Pick(t, fields)
		}
		return t
	case []any:
		out := make([]any, 0, len(t))
		for _, x := range t {
			out = append(out, pickDeep(x, fields))
		}
		return out
	}
	return payload
}

func fieldsOf(op *catalog.Operation) []string {
	raw, _ := op.Compact["fields"].([]any)
	out := make([]string, 0, len(raw))
	for _, f := range raw {
		if s, ok := f.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func numberOf(v any) (int, bool) {
	switch t := v.(type) {
	case int:
		return t, true
	case float64:
		return int(t), true
	}
	return 0, false
}
