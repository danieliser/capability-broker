// Package mcpfront is the transparent MCP face of the broker: one Streamable
// HTTP endpoint per connector, a per-grant server whose tools/list is the
// upstream catalog filtered to the grant, and a tools/call path that resolves,
// authorizes, receipts, executes and shapes every call.
package mcpfront

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/adapter"
	"github.com/danieliser/capability-broker/pkg/audit"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/policy"
	"github.com/danieliser/capability-broker/pkg/registry"
)

type ctxKey int

const grantKey ctxKey = 1

// SiteHostSource is implemented by adapters that learn site id → hostname (URL policy).
type SiteHostSource interface{ SiteHosts() map[string]string }

type Front struct {
	Store    *auth.Store
	Catalog  *catalog.Catalog
	Audit    *audit.Writer
	Registry *registry.Registry
	Logger   *slog.Logger
	// SessionTimeout closes idle MCP sessions (legacy stateful mode); zero keeps them.
	SessionTimeout time.Duration
	// Stateless serves the sessionless Streamable HTTP of MCP 2026-07-28. Older clients negotiate down.
	Stateless bool

	inflight atomic.Int64 // tool calls in progress; shutdown waits on these, not on idle streams

	mu       sync.Mutex
	handlers map[string]http.Handler // endpoint key → SDK handler (one session store per endpoint)
	servers  map[string]*liveServer  // endpoint key + grant → stable server, tools edited in place
}

// AggregateEndpoint is the single endpoint that carries every connector the grant reaches, with tool names
// prefixed `<connector>__`. A connector added to connectors.yaml appears there without the client
// reconnecting: the server sends tools/list_changed.
const AggregateEndpoint = "*"

// liveServer is one grant's view of one endpoint. Its tools are refreshed in place on reload so connected
// sessions (legacy or 2026-07-28 subscriptions/listen) receive tools/list_changed instead of going stale.
type liveServer struct {
	mu          sync.Mutex
	server      *mcp.Server
	grant       *auth.Grant
	endpoint    string          // connector name, or AggregateEndpoint
	exclude     map[string]bool // aggregate only: connectors the client already has as their own servers
	plain       bool            // aggregate only: upstream tool names unprefixed, except where two connectors clash
	hint        bool            // prepend a one-line "live connection" note to each tool description (clients that drop server instructions)
	toolHash    map[string]string
	toolOwner   map[string]string // exported name → connector
	allOnDemand bool              // aggregate only (?ondemand=all): every connector reached through discovery
	extHash     map[string]string // "p:"+prompt name | "r:"+uri | "t:"+uri template → definition hash
	lastUsed    atomic.Int64      // unix nanos; idle servers are dropped by SweepIdle
}

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, msg
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(b)
}

// ServeConnector serves /mcp/<connector>; ServeAggregate serves /mcp. Authentication happens on every HTTP
// request; the tool handler re-authenticates from the request header too.
func (f *Front) ServeConnector(w http.ResponseWriter, r *http.Request, connector string) {
	if _, ok := f.Registry.Lookup(connector); !ok {
		writeError(w, http.StatusNotFound, "BROKER_UNKNOWN_CONNECTOR", "no connector "+connector)
		return
	}
	g := f.gate(w, r, connector)
	if g == nil {
		return
	}
	if sp := f.specOf(connector); sp != nil && g.Capabilities.IsOwner() && !sp.ForClient(g.Name) {
		writeError(w, http.StatusNotFound, "BROKER_UNKNOWN_CONNECTOR", "no connector "+connector+" for "+g.Name)
		return
	}
	f.handler(connector).ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantKey, g)))
}

func (f *Front) ServeAggregate(w http.ResponseWriter, r *http.Request) {
	g := f.gate(w, r, "")
	if g == nil {
		return
	}
	f.handler(AggregateEndpoint).ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantKey, g)))
}

func (f *Front) gate(w http.ResponseWriter, r *http.Request, connector string) *auth.Grant {
	label := connector
	if label == "" {
		label = "broker"
	}
	g, code, msg := f.authenticate(r)
	if g == nil {
		f.Audit.Denied(r.Context(), audit.Call{Principal: "unauthenticated", Connector: label, Tool: "-"}, code, msg)
		writeError(w, http.StatusUnauthorized, code, msg)
		return nil
	}
	if connector != "" {
		if _, ok := g.Capabilities.For(connector); !ok {
			f.Audit.Denied(r.Context(), audit.Call{Grant: g, Connector: connector, Tool: "-"}, policy.CodeDeniedConnector, "grant does not reach "+connector)
			writeError(w, http.StatusForbidden, policy.CodeDeniedConnector, "grant does not reach connector "+connector)
			return nil
		}
	}
	return g
}

func (f *Front) handler(endpoint string) http.Handler {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.handlers == nil {
		f.handlers = map[string]http.Handler{}
	}
	if h, ok := f.handlers[endpoint]; ok {
		return h
	}
	h := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		g, _ := r.Context().Value(grantKey).(*auth.Grant)
		if g == nil {
			return nil
		}
		var exclude []string
		if endpoint == AggregateEndpoint {
			for _, e := range strings.Split(r.URL.Query().Get("exclude"), ",") {
				if e = strings.TrimSpace(e); e != "" {
					exclude = append(exclude, e)
				}
			}
		}
		q := r.URL.Query()
		return f.serverFor(r.Context(), endpoint, g, exclude, q.Get("prefix") == "none", q.Get("instructions") == "none", q.Get("hint") == "live", q.Get("ondemand") == "all")
	}, &mcp.StreamableHTTPOptions{SessionTimeout: f.SessionTimeout, Logger: f.Logger, Stateless: f.Stateless})
	f.handlers[endpoint] = h
	return h
}

func (f *Front) authenticate(r *http.Request) (*auth.Grant, string, string) {
	tok, ok := auth.BearerFromHeader(r.Header.Get("Authorization"))
	if !ok {
		return nil, "BROKER_UNAUTHENTICATED", "bearer token required"
	}
	g, err := f.Store.Authenticate(r.Context(), tok, time.Now())
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrGrantRevoked):
			return nil, "BROKER_GRANT_REVOKED", "grant has been revoked"
		case errors.Is(err, auth.ErrGrantExpired):
			return nil, "BROKER_GRANT_EXPIRED", "grant has expired"
		case errors.Is(err, auth.ErrUnauthenticated):
			return nil, "BROKER_UNAUTHENTICATED", "unknown token"
		}
		return nil, "BROKER_UNAUTHENTICATED", "authentication failed"
	}
	return g, "", ""
}

// serverFor returns the stable server for (endpoint, grant, exclude), creating and populating it once.
func (f *Front) serverFor(ctx context.Context, endpoint string, g *auth.Grant, exclude []string, plain, noInstructions, hint, allOnDemand bool) *mcp.Server {
	sort.Strings(exclude)
	instructions := ""
	if !noInstructions {
		instructions = f.instructions(endpoint, g, exclude, allOnDemand)
	}
	// Instructions are fixed per MCP server, and upstreams supply theirs only once connected: a change (a
	// connector finished starting, or was added) gets a fresh server for new sessions instead of stale text.
	sum := sha256.Sum256([]byte(instructions))
	key := endpoint + "|" + g.ID + "|" + strings.Join(exclude, ",") + fmt.Sprintf("|plain=%v|noinst=%v|hint=%v|od=%v|ins=%x", plain, noInstructions, hint, allOnDemand, sum[:6])
	f.mu.Lock()
	if f.servers == nil {
		f.servers = map[string]*liveServer{}
	}
	ls, ok := f.servers[key]
	if !ok {
		name := endpoint
		if endpoint == AggregateEndpoint {
			name = "broker"
		}
		ls = &liveServer{server: mcp.NewServer(&mcp.Implementation{Name: name, Version: "1"}, &mcp.ServerOptions{Logger: f.sdkLogger(),
			Instructions: instructions,
			// Declared up front: a server that starts with no tools (an aggregate whose connectors are all
			// excluded, or still starting) must still tell the client it has tools and will announce changes.
			Capabilities: serverCaps(g.Capabilities.IsOwner())}),
			grant: g, endpoint: endpoint, exclude: map[string]bool{}, plain: plain && endpoint == AggregateEndpoint, hint: hint, allOnDemand: allOnDemand && endpoint == AggregateEndpoint,
			toolHash: map[string]string{}, toolOwner: map[string]string{}, extHash: map[string]string{}}
		for _, e := range exclude {
			ls.exclude[e] = true
		}
		f.servers[key] = ls
	}
	f.mu.Unlock()
	ls.lastUsed.Store(time.Now().UnixNano())
	if !ok {
		f.refreshOne(ctx, ls, true)
	}
	return ls.server
}

// instructions is the server's MCP `instructions`, which clients put in the model's context. Low-effort models
// otherwise answer "I have no access" without looking at their tools (measured: GPT-6 Luna low skipped tools in
// most failed runs). It names the live systems behind this server and says to use them.
func (f *Front) instructions(endpoint string, g *auth.Grant, exclude []string, allOnDemand bool) string {
	skip := map[string]bool{}
	for _, e := range exclude {
		skip[e] = true
	}
	var names []string
	if endpoint == AggregateEndpoint {
		for _, n := range f.Registry.Names() {
			if _, ok := g.Capabilities.For(n); ok && !skip[n] {
				names = append(names, n)
			}
		}
	} else {
		names = []string{endpoint}
	}
	if g.Capabilities.IsOwner() {
		return f.ownerInstructions(g, endpoint == AggregateEndpoint, names, allOnDemand)
	}
	if len(names) == 0 {
		return "Live connectors added to this server later appear as new tools; check your tool list when a task needs a system you do not see."
	}
	about := map[string]string{
		"clickhouse":    "the ClickHouse data warehouse (SQL: databases, tables, counts)",
		"memory":        "long-term memory (recall what was stored earlier, store new facts)",
		"wp-mcp-router": "the WordPress sites (list sites, read and draft content)",
	}
	var parts []string
	for _, n := range names {
		if d, ok := about[n]; ok {
			parts = append(parts, n+": "+d)
		} else {
			parts = append(parts, n)
		}
	}
	return "These tools are live connections to: " + strings.Join(parts, "; ") +
		". When a question needs data, remembered facts or site information, call these tools instead of answering from the conversation; your workspace being read-only does not limit them."
}

// ownerInstructions passes each upstream server's own instructions through, as the client would have shown
// them with the server connected directly. Nothing of the broker's own is added.
func (f *Front) ownerInstructions(g *auth.Grant, aggregate bool, names []string, allOnDemand bool) string {
	var parts []string
	var onDemand []string
	var direct []string
	for _, n := range names {
		sp := f.specOf(n)
		if sp != nil && !sp.ForClient(g.Name) {
			continue
		}
		if aggregate && ((sp != nil && sp.OnDemand()) || allOnDemand) {
			onDemand = append(onDemand, n)
			continue
		}
		direct = append(direct, n)
	}
	if len(onDemand) > 0 {
		parts = append(parts, discoveryInstructions(onDemand))
	}
	for _, n := range direct {
		if sp := f.specOf(n); sp != nil && sp.Strips("instructions") {
			continue
		}
		ad, ok := f.Registry.Lookup(n)
		if !ok {
			continue
		}
		ins, ok := ad.(interface{ Instructions() string })
		if !ok {
			continue
		}
		text := strings.TrimSpace(ins.Instructions())
		if text == "" {
			continue
		}
		if len(names) == 1 {
			return text
		}
		parts = append(parts, "## "+n+"\n"+text)
	}
	return strings.Join(parts, "\n\n")
}

func liveAbout(connector string) string {
	switch connector {
	case "clickhouse":
		return "use it for warehouse data, tables and counts"
	case "memory":
		return "use it to recall or store long-term memory"
	case "wp-mcp-router":
		return "use it for the WordPress sites"
	}
	return "use it when a task needs " + connector
}

// Refresh re-derives every live server's tool list from the current registry and catalog and edits it in
// place. Connectors that are still starting keep the tools they had; removed connectors lose theirs.
func (f *Front) Refresh(ctx context.Context) {
	f.mu.Lock()
	all := make([]*liveServer, 0, len(f.servers))
	for _, ls := range f.servers {
		all = append(all, ls)
	}
	f.mu.Unlock()
	for _, ls := range all {
		f.refreshOne(ctx, ls, false)
	}
}

type readiness interface{ Ready() bool }

func (f *Front) refreshOne(ctx context.Context, ls *liveServer, initial bool) {
	ls.mu.Lock()
	defer ls.mu.Unlock()
	var connectors, onDemand []string
	owner := ls.grant.Capabilities.IsOwner()
	if ls.endpoint == AggregateEndpoint {
		for _, name := range f.Registry.Names() {
			if ls.exclude[name] {
				continue
			}
			if _, ok := ls.grant.Capabilities.For(name); !ok {
				continue
			}
			if sp := f.specOf(name); owner && sp != nil {
				if !sp.ForClient(ls.grant.Name) {
					continue
				}
				if sp.OnDemand() || ls.allOnDemand {
					// Kept inside the broker: reached through the discovery tools, not preloaded.
					onDemand = append(onDemand, name)
					continue
				}
			}
			connectors = append(connectors, name)
		}
	} else {
		connectors = []string{ls.endpoint}
	}
	desired := map[string]def{}
	keep := map[string]bool{} // connectors not ready yet: leave their current tools alone
	for _, name := range connectors {
		ad, ok := f.Registry.Lookup(name)
		if !ok {
			continue
		}
		if r, ok := ad.(readiness); ok && !r.Ready() {
			// Never hold a client's connect on a cold child (npx starts take up to a minute; clients give up at
			// 30s). Wait briefly on first population, then serve what is ready: the reloader refreshes when the
			// child connects and the client gets tools/list_changed.
			if initial {
				deadline := time.Now().Add(5 * time.Second)
				for !r.Ready() && time.Now().Before(deadline) {
					time.Sleep(100 * time.Millisecond)
				}
			}
			if !r.Ready() {
				keep[name] = true
				continue
			}
		}
		tools, err := ad.Tools(ctx, ls.grant)
		if err != nil {
			f.Logger.Warn("tools unavailable", "connector", name, "grant", ls.grant.ID, "err", err.Error())
			keep[name] = true
			continue
		}
		for _, t := range tools {
			exported := t.Name
			if ls.endpoint == AggregateEndpoint {
				exported = name + "__" + t.Name
			}
			desired[exported] = def{connector: name, upstream: t.Name, tool: t}
		}
	}
	if ls.plain {
		// Unprefixed: each tool keeps its upstream name; only names two connectors share stay prefixed.
		count := map[string]int{}
		for _, d := range desired {
			count[d.upstream]++
		}
		renamed := map[string]def{}
		for exported, d := range desired {
			if count[d.upstream] == 1 {
				renamed[d.upstream] = d
			} else {
				renamed[exported] = d
			}
		}
		desired = renamed
	}
	var remove []string
	for exported, owner := range ls.toolOwner {
		if _, still := desired[exported]; still || keep[owner] || owner == discoveryOwner {
			continue // discovery tools are managed by syncDiscovery
		}
		remove = append(remove, exported)
	}
	if len(remove) > 0 {
		ls.server.RemoveTools(remove...)
		for _, n := range remove {
			delete(ls.toolHash, n)
			delete(ls.toolOwner, n)
		}
	}
	f.syncDiscovery(ls, onDemand, desired)
	if owner {
		f.syncExtras(ctx, ls, connectors, keep)
	}
	for exported, d := range desired {
		if owner && d.tool.Upstream != nil {
			// Owner clients see the upstream tool exactly as declared (title, annotations, output schema,
			// icons, _meta), under its exported name and with no broker arguments, minus what the connector strips.
			t := f.shapeTool(d.connector, *d.tool.Upstream)
			t.Name = exported
			raw, _ := json.Marshal(&t)
			sum := sha256.Sum256(raw)
			h := hex.EncodeToString(sum[:])
			if ls.toolHash[exported] == h {
				continue
			}
			connector, upstream := d.connector, d.upstream
			ls.server.AddTool(&t, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return f.call(ctx, connector, upstream, req)
			})
			ls.toolHash[exported] = h
			ls.toolOwner[exported] = connector
			continue
		}
		if ls.hint {
			d.tool.Description = "Live " + d.connector + " connection: " + liveAbout(d.connector) + ". " + d.tool.Description
		}
		raw, err := json.Marshal(withVerbose(d.tool.InputSchema))
		if err != nil {
			continue
		}
		sum := sha256.Sum256(append([]byte(d.tool.Description+"\x00"), raw...))
		h := hex.EncodeToString(sum[:])
		if ls.toolHash[exported] == h {
			continue
		}
		connector, upstream := d.connector, d.upstream
		ls.server.AddTool(&mcp.Tool{Name: exported, Description: d.tool.Description, InputSchema: json.RawMessage(raw)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return f.call(ctx, connector, upstream, req)
			})
		ls.toolHash[exported] = h
		ls.toolOwner[exported] = connector
	}
}

type def struct {
	connector, upstream string
	tool                catalog.Tool
}

// sdkLogger passes the MCP SDK's warnings and errors through but not its per-request info lines (a session
// connect and disconnect for every sessionless request).
func (f *Front) sdkLogger() *slog.Logger {
	if f.Logger == nil {
		return nil
	}
	return slog.New(minLevel{f.Logger.Handler(), slog.LevelWarn})
}

type minLevel struct {
	slog.Handler
	min slog.Level
}

func (m minLevel) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= m.min && m.Handler.Enabled(ctx, l)
}

func (m minLevel) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevel{m.Handler.WithAttrs(attrs), m.min}
}

func (m minLevel) WithGroup(name string) slog.Handler { return minLevel{m.Handler.WithGroup(name), m.min} }

// SweepIdle drops cached servers nobody has used for maxIdle (a new request rebuilds one in milliseconds). Each
// app, grant and option set gets its own server, and changed instructions create new ones, so without this the
// cache only grows.
func (f *Front) SweepIdle(ctx context.Context, maxIdle time.Duration) {
	t := time.NewTicker(maxIdle / 4)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		cutoff := time.Now().Add(-maxIdle).UnixNano()
		f.mu.Lock()
		for k, ls := range f.servers {
			if ls.lastUsed.Load() < cutoff {
				delete(f.servers, k)
			}
		}
		f.mu.Unlock()
	}
}

// Invalidate drops cached servers for a grant (after revocation), or every server when grantID is empty.
func (f *Front) Invalidate(grantID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, ls := range f.servers {
		if grantID == "" || ls.grant.ID == grantID {
			delete(f.servers, k)
		}
	}
}

// withVerbose adds the one broker-specific argument to a verbatim schema.
func withVerbose(schema map[string]any) map[string]any {
	raw, _ := json.Marshal(schema)
	var copy map[string]any
	_ = json.Unmarshal(raw, &copy)
	if copy == nil {
		copy = map[string]any{"type": "object"}
	}
	props, _ := copy["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
		copy["properties"] = props
	}
	props["verbose"] = map[string]any{"type": "boolean", "description": "Return the full upstream payload instead of the compact shape."}
	return copy
}

func toolError(code, msg string) *mcp.CallToolResult {
	var b errorBody
	b.Error.Code, b.Error.Message = code, msg
	raw, _ := json.Marshal(b)
	return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
}

// call is the single tools/call path.
// InFlight reports how many tool calls are running.
func (f *Front) InFlight() int64 { return f.inflight.Load() }

func (f *Front) call(ctx context.Context, connector, tool string, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	f.inflight.Add(1)
	defer f.inflight.Add(-1)
	// The adapter is resolved at call time: a reload may have replaced or removed it.
	ad, ok := f.Registry.Lookup(connector)
	if !ok {
		return toolError("BROKER_UNKNOWN_CONNECTOR", "connector "+connector+" is no longer configured"), nil
	}
	// Re-authenticate from the current request header: revocation is checked on every call.
	var g *auth.Grant
	if req.Extra != nil && req.Extra.Header != nil {
		if tok, ok := auth.BearerFromHeader(req.Extra.Header.Get("Authorization")); ok {
			if gg, err := f.Store.Authenticate(ctx, tok, time.Now()); err == nil {
				g = gg
			} else if errors.Is(err, auth.ErrGrantRevoked) {
				return toolError("BROKER_GRANT_REVOKED", "grant has been revoked"), nil
			} else if errors.Is(err, auth.ErrGrantExpired) {
				return toolError("BROKER_GRANT_EXPIRED", "grant has expired"), nil
			}
		}
	}
	if g == nil {
		if gg, ok := ctx.Value(grantKey).(*auth.Grant); ok {
			g = gg
		}
	}
	if g == nil {
		return toolError("BROKER_UNAUTHENTICATED", "no grant on this call"), nil
	}
	if err := g.Check(time.Now()); err != nil {
		return toolError(strings.TrimPrefix(err.Error(), ""), "grant is no longer usable"), nil
	}

	args := map[string]any{}
	if len(req.Params.Arguments) > 0 {
		if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
			return toolError(policy.CodeDeniedArgument, "arguments must be a JSON object"), nil
		}
	}
	// `verbose` is the broker's own argument, offered to agents only; an owner's arguments go upstream as sent.
	verbose := false
	if !g.Capabilities.IsOwner() {
		verbose, _ = args["verbose"].(bool)
		delete(args, "verbose")
	}
	idem, _ := args["idempotency_key"].(string)
	requestBytes := int64(len(req.Params.Arguments))

	conn, _ := f.Catalog.Connector(connector)
	base := audit.Call{Grant: g, Connector: connector, Tool: tool, Args: args, PolicyVersion: f.Catalog.PolicyVersion(),
		IdempotencyKey: idem, Verbose: verbose, RequestBytes: requestBytes}
	if conn == nil || !conn.ToolExposed(tool) {
		f.Audit.Denied(ctx, base, policy.CodeDeniedOperation, "tool not exposed")
		return toolError(policy.CodeDeniedOperation, fmt.Sprintf("tool %s is not available through this connector", tool)), nil
	}
	op, ok := conn.Resolve(tool, args)
	if !ok {
		f.Audit.Denied(ctx, base, policy.CodeDeniedOperation, "no operation matches these arguments")
		return toolError(policy.CodeDeniedOperation, "this call does not map to a permitted operation (check the ability or query name)"), nil
	}
	base.Operation = op.ID

	env := policy.Env{Catalog: f.Catalog}
	if src, ok := ad.(SiteHostSource); ok {
		env.SiteHosts = src.SiteHosts()
	}
	decision, err := policy.Evaluate(ctx, g, connector, op, args, env, f.Store)
	if err != nil {
		f.Logger.Error("policy evaluation failed", "err", err.Error())
		return toolError("BROKER_INTERNAL", "policy evaluation failed"), nil
	}
	if !decision.Allow {
		f.Audit.Denied(ctx, base, decision.Code, decision.Reason)
		return toolError(decision.Code, decision.Reason), nil
	}
	base.Args = decision.Args

	callID, err := f.Audit.Intent(ctx, base)
	if err != nil {
		// Audit write failure denies the call (design §6).
		f.Logger.Error("intent write failed", "err", err.Error())
		return toolError("BROKER_AUDIT_UNAVAILABLE", "the broker cannot record this call and refuses to run it"), nil
	}
	started := time.Now()
	var progressToken any
	if req.Params != nil && req.Params.Meta != nil {
		progressToken = req.Params.Meta["progressToken"]
	}
	res, err := ad.Execute(ctx, adapter.Call{Grant: g, Op: op, Tool: tool, Decision: decision, Verbose: verbose, IdempotencyKey: idem,
		CallID: callID, Session: req.Session, ProgressToken: progressToken})
	if err != nil {
		ue, ok := adapter.AsUpstream(err)
		code, msg, state := adapter.CodeUpstreamError, "upstream call failed", audit.StateFailed
		if ok {
			code, msg = ue.Code, ue.Message
			if ue.Outcome == adapter.OutcomeUnknown {
				state = audit.StateOutcomeUnknown
			}
		}
		_ = f.Audit.Outcome(ctx, callID, base, audit.Outcome{State: state, OutcomeClass: code, Duration: time.Since(started)})
		return toolError(code, msg), nil
	}
	if res.Raw != nil {
		raw, _ := json.Marshal(res.Raw)
		state := audit.StateCompleted
		_ = f.Audit.Outcome(ctx, callID, base, audit.Outcome{State: state, OutcomeClass: res.OutcomeClass,
			Duration: time.Since(started), ResponseBytes: int64(len(raw))})
		return res.Raw, nil
	}
	body := res.Compact
	if verbose {
		body = res.Verbose
	}
	raw, err := json.Marshal(body)
	if err != nil {
		raw = []byte(`{"error":{"code":"BROKER_INTERNAL","message":"result not serialisable"}}`)
	}
	base.Account = res.Account
	state := audit.StateCompleted
	if res.Outcome == adapter.OutcomeUnknown {
		state = audit.StateOutcomeUnknown
	}
	_ = f.Audit.Outcome(ctx, callID, base, audit.Outcome{State: state, UpstreamRequestID: res.UpstreamRequestID,
		OutcomeClass: res.OutcomeClass, Duration: time.Since(started), ResponseBytes: int64(len(raw))})
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
}
