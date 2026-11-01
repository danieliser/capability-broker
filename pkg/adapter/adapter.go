// Package adapter defines the contract a connector adapter implements and the
// shared result/error shapes the front turns into MCP responses.
package adapter

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/policy"
)

// Call is one authorized, policy-evaluated tool invocation.
type Call struct {
	Grant          *auth.Grant
	Op             *catalog.Operation
	Tool           string
	Decision       policy.Decision // effective args, limits, named query
	Verbose        bool
	IdempotencyKey string
	CallID         string
	// Session is the calling client's server-side session (nil in stateless mode or tests);
	// ProgressToken is the token the client attached, if any. Both feed the relay.
	Session       *mcp.ServerSession
	ProgressToken any
}

type Outcome string

const (
	OutcomeCompleted Outcome = "completed"
	OutcomeFailed    Outcome = "failed"
	OutcomeUnknown   Outcome = "outcome_unknown"
)

// Result is what an adapter returns. Compact is the default response body;
// Verbose the full upstream payload (already redacted).
type Result struct {
	Compact           any
	Verbose           any
	Outcome           Outcome
	UpstreamRequestID string
	OutcomeClass      string
	Account           string
}

// UpstreamError is a failure the agent should see, with a stable code.
type UpstreamError struct {
	Code    string
	Message string
	Outcome Outcome
}

func (e *UpstreamError) Error() string { return e.Code + ": " + e.Message }

const (
	CodeUpstreamUnavailable = "BROKER_UPSTREAM_UNAVAILABLE"
	CodeUpstreamError       = "BROKER_UPSTREAM_ERROR"
)

func Unavailable(msg string) error {
	return &UpstreamError{Code: CodeUpstreamUnavailable, Message: Truncate(msg, 300), Outcome: OutcomeFailed}
}

func Failed(msg string) error {
	return &UpstreamError{Code: CodeUpstreamError, Message: Truncate(msg, 600), Outcome: OutcomeFailed}
}

func Denied(msg string) error {
	return &UpstreamError{Code: policy.CodeDeniedArgument, Message: Truncate(msg, 300), Outcome: OutcomeFailed}
}

func Truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func AsUpstream(err error) (*UpstreamError, bool) {
	var ue *UpstreamError
	if errors.As(err, &ue) {
		return ue, true
	}
	return nil, false
}

// Adapter is implemented once per connector.
type Adapter interface {
	Connector() string
	// Tools returns the upstream tool definitions this grant may see.
	Tools(ctx context.Context, g *auth.Grant) ([]catalog.Tool, error)
	// Execute performs the call; the broker's credential is injected at the outbound edge and nowhere else.
	Execute(ctx context.Context, call Call) (*Result, error)
	// Health reports whether the upstream is reachable, without credentials in the message.
	Health(ctx context.Context) error
}
