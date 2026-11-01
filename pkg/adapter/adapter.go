// Package adapter defines the contract every connector adapter implements and
// the shared result/error shapes the front turns into MCP responses.
package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
}

type Outcome string

const (
	OutcomeCompleted Outcome = "completed"
	OutcomeFailed    Outcome = "failed"
	OutcomeUnknown   Outcome = "outcome_unknown"
)

// Result is what an adapter returns. Compact is the default response body;
// Verbose the full upstream payload (already redacted). Receipt carries the
// upstream request id and sizes for the audit row.
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
	Code    string // BROKER_UPSTREAM_UNAVAILABLE, BROKER_UPSTREAM_ERROR, BROKER_DENIED_ARGUMENT
	Message string // redacted, truncated
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
	return &UpstreamError{Code: CodeUpstreamError, Message: Truncate(msg, 300), Outcome: OutcomeFailed}
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

// AsUpstream unwraps an adapter error.
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
	// Tools returns the verbatim upstream tool definitions this grant may see.
	Tools(ctx context.Context, g *auth.Grant) ([]catalog.Tool, error)
	// Execute performs the call with the broker's credential injected at the
	// outbound request and nowhere else.
	Execute(ctx context.Context, call Call) (*Result, error)
	// Health reports whether the upstream is reachable, without credentials in the message.
	Health(ctx context.Context) error
}

// ArgString reads a string argument.
func ArgString(args map[string]any, key string) string {
	s, _ := args[key].(string)
	return s
}

// ArgObject reads an object argument.
func ArgObject(args map[string]any, key string) map[string]any {
	m, _ := args[key].(map[string]any)
	return m
}

// PickFields keeps the listed keys from an object (compact shaping).
func PickFields(obj map[string]any, fields []string) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if v, ok := obj[f]; ok {
			out[f] = v
		}
	}
	return out
}

// FieldsFromCompact reads the `fields:` list of an operation's compact spec.
func FieldsFromCompact(op *catalog.Operation) []string {
	raw, _ := op.Compact["fields"].([]any)
	out := make([]string, 0, len(raw))
	for _, f := range raw {
		if s, ok := f.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// IntFromCompact reads an integer from an operation's compact spec.
func IntFromCompact(op *catalog.Operation, key string, def int) int {
	switch v := op.Compact[key].(type) {
	case int:
		return v
	case float64:
		return int(v)
	}
	return def
}

// JSONSize measures a payload for receipts.
func JSONSize(v any) int64 {
	b, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	return int64(len(b))
}

// Must is for adapter construction errors that are configuration bugs.
func Must[T any](v T, err error) T {
	if err != nil {
		panic(fmt.Sprintf("adapter init: %v", err))
	}
	return v
}
