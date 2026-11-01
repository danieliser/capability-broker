// Package mcpfront is the transparent MCP face of the broker: one Streamable
// HTTP endpoint per connector, a per-grant server whose tools/list is the
// upstream catalog filtered to the grant, and a tools/call path that resolves,
// authorizes, receipts, executes and shapes every call.
package mcpfront

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/adapter"
	"github.com/danieliser/capability-broker/pkg/audit"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/policy"
)

type ctxKey int

const grantKey ctxKey = 1

// SiteHostSource is implemented by adapters that learn site id → hostname (URL policy).
type SiteHostSource interface{ SiteHosts() map[string]string }

type Front struct {
	Store    *auth.Store
	Catalog  *catalog.Catalog
	Audit    *audit.Writer
	Adapters map[string]adapter.Adapter
	Logger   *slog.Logger
	// SessionTimeout closes idle MCP sessions (legacy stateful mode); zero keeps them.
	SessionTimeout time.Duration
	// Stateless serves the sessionless Streamable HTTP of MCP 2026-07-28 (no Mcp-Session-Id, GET/DELETE 405,
	// one ephemeral session per request). Older clients still negotiate down; the SDK handles both. Every call is
	// authenticated per request anyway, so this is the natural mode for the broker.
	Stateless bool

	mu      sync.Mutex
	servers map[string]*mcp.Server
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

// Handler serves /mcp/<connector>. Authentication happens here on every HTTP
// request; the tool handler re-authenticates from the request header too.
func (f *Front) Handler(connector string) http.Handler {
	ad, ok := f.Adapters[connector]
	if !ok {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeError(w, http.StatusNotFound, "BROKER_UNKNOWN_CONNECTOR", "no connector "+connector)
		})
	}
	mcpHandler := mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		g, _ := r.Context().Value(grantKey).(*auth.Grant)
		if g == nil {
			return nil
		}
		return f.serverFor(r.Context(), connector, g, ad)
	}, &mcp.StreamableHTTPOptions{SessionTimeout: f.SessionTimeout, Logger: f.Logger, Stateless: f.Stateless})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, code, msg := f.authenticate(r)
		if g == nil {
			status := http.StatusUnauthorized
			if code == policy.CodeDeniedConnector {
				status = http.StatusForbidden
			}
			f.Audit.Denied(r.Context(), audit.Call{Principal: "unauthenticated", Connector: connector, Tool: "-"}, code, msg)
			writeError(w, status, code, msg)
			return
		}
		if _, ok := g.Capabilities.For(connector); !ok {
			f.Audit.Denied(r.Context(), audit.Call{Grant: g, Connector: connector, Tool: "-"}, policy.CodeDeniedConnector, "grant does not reach "+connector)
			writeError(w, http.StatusForbidden, policy.CodeDeniedConnector, "grant does not reach connector "+connector)
			return
		}
		mcpHandler.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), grantKey, g)))
	})
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

// serverFor returns the cached per-grant server for a connector, keyed by grant
// and policy version so a catalog push produces a fresh tool list.
func (f *Front) serverFor(ctx context.Context, connector string, g *auth.Grant, ad adapter.Adapter) *mcp.Server {
	key := connector + "|" + g.ID + "|" + f.Catalog.PolicyVersion()
	f.mu.Lock()
	if f.servers == nil {
		f.servers = map[string]*mcp.Server{}
	}
	if s, ok := f.servers[key]; ok {
		f.mu.Unlock()
		return s
	}
	f.mu.Unlock()

	tools, err := ad.Tools(ctx, g)
	if err != nil {
		f.Logger.Warn("tools unavailable", "connector", connector, "grant", g.ID, "err", err.Error())
		tools = nil
	}
	server := mcp.NewServer(&mcp.Implementation{Name: connector, Version: "1"}, &mcp.ServerOptions{Logger: f.Logger})
	for _, t := range tools {
		schema := withVerbose(t.InputSchema)
		raw, err := json.Marshal(schema)
		if err != nil {
			continue
		}
		toolName := t.Name
		server.AddTool(&mcp.Tool{Name: toolName, Description: t.Description, InputSchema: json.RawMessage(raw)},
			func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return f.call(ctx, connector, toolName, ad, req)
			})
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if s, ok := f.servers[key]; ok {
		return s
	}
	f.servers[key] = server
	return server
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
func (f *Front) call(ctx context.Context, connector, tool string, ad adapter.Adapter, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
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
	verbose, _ := args["verbose"].(bool)
	delete(args, "verbose")
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

// Invalidate drops cached servers for a grant (after revocation) or for all when key is empty.
func (f *Front) Invalidate(grantID string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for k := range f.servers {
		if grantID == "" || strings.Contains(k, "|"+grantID+"|") {
			delete(f.servers, k)
		}
	}
}
