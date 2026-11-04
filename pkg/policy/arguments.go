package policy

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
)

// CloneArgs copies nested objects and lists: preparation must not mutate the request or grant.
func CloneArgs(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = cloneValue(v)
	}
	return out
}
func cloneValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return CloneArgs(x)
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = cloneValue(v)
		}
		return out
	case []string:
		return append([]string{}, x...)
	default:
		return v
	}
}

type DailyCounters interface {
	ReserveDaily(ctx context.Context, scope, operation, day string, limit int64) (bool, error)
}

// EvaluateArguments is the final policy pass, after request enrichment and before intent/execution.
// Every configured value is enforced: operation values can narrow connector values, never replace them.
func EvaluateArguments(ctx context.Context, g *auth.Grant, connector string, op *catalog.Operation, d Decision, env Env, counters Counters) (Decision, error) {
	if !d.Allow || g.Capabilities.IsOwner() {
		return d, nil
	}
	cap, ok := g.Capabilities.For(connector)
	if !ok {
		return deny(CodeDeniedConnector, "grant does not reach connector"), nil
	}
	d.Args = CloneArgs(d.Args)
	d.Limits = CloneArgs(d.Limits)
	if dd := checkFlow(g, cap, op, &d); dd != nil {
		return *dd, nil
	}
	var daily *int64
	for _, r := range op.ArgumentConstraints {
		if err := r.Validate(); err != nil {
			return deny(CodeDeniedOperation, err.Error()), nil
		}
		values := constraintValues(cap, op.ID, r.Key)
		if len(values) == 0 && r.Default != nil {
			values = []any{*r.Default}
		}
		if len(values) == 0 {
			if r.Required {
				return deny(CodeDeniedArgument, "grant must configure "+r.Key), nil
			}
			continue
		}
		if r.Kind == "max_per_day" {
			n, err := numericCeiling(values, true)
			if err != nil {
				return deny(CodeDeniedArgument, r.Key+": "+err.Error()), nil
			}
			v := int64(n)
			if daily == nil || v < *daily {
				daily = &v
			}
			continue
		}
		parent, key, exists, err := argumentAt(d.Args, r.Path, r.Kind == "row_limit" && (r.Default != nil || r.Required))
		if err != nil {
			return deny(CodeDeniedArgument, err.Error()), nil
		}
		if r.Kind == "maximum" || r.Kind == "row_limit" {
			bound, err := numericCeiling(values, r.Kind == "row_limit")
			if err != nil {
				return deny(CodeDeniedArgument, r.Key+": "+err.Error()), nil
			}
			if r.Kind == "row_limit" {
				if bound == 0 {
					return deny(CodeLimitExceeded, "max_rows is zero"), nil
				}
				if old, ok := d.Limits["max_rows"].(float64); !ok || bound < old {
					d.Limits["max_rows"] = bound
				}
			}
			if !exists {
				if r.Kind == "row_limit" && (r.Default != nil || r.Required) {
					parent[key] = bound
					continue
				}
				if r.Required {
					return deny(CodeDeniedArgument, r.Path+" is required"), nil
				}
				continue
			}
			n, valid := finiteNumber(parent[key])
			if !valid || n < 0 || (r.Kind == "row_limit" && (n < 1 || math.Trunc(n) != n)) {
				return deny(CodeDeniedArgument, r.Path+" must be a non-negative finite number (positive integer for rows)"), nil
			}
			if n > bound {
				return deny(CodeDeniedArgument, r.Path+" exceeds "+r.Key), nil
			}
			continue
		}
		if !exists {
			if r.Required {
				return deny(CodeDeniedArgument, r.Path+" is required"), nil
			}
			continue
		}
		for _, value := range values {
			switch r.Kind {
			case "allowlist":
				allowed, valid := strictStrings(value)
				if !valid {
					return deny(CodeDeniedArgument, r.Key+" must be a string allowlist"), nil
				}
				given, valid := strictStrings(parent[key])
				if s, ok := parent[key].(string); ok {
					given, valid = []string{s}, true
				}
				if !valid || len(given) == 0 {
					return deny(CodeDeniedArgument, r.Path+" must be a string or non-empty string list"), nil
				}
				for _, item := range given {
					if item == "" || !contains(allowed, item) {
						return deny(CodeDeniedArgument, r.Path+" is outside "+r.Key), nil
					}
				}
			case "prefix":
				prefix, valid := value.(string)
				s, isString := parent[key].(string)
				if !valid || prefix == "" || !isString || !strings.HasPrefix(s, prefix) {
					return deny(CodeDeniedArgument, r.Path+" must start with "+r.Key), nil
				}
			}
		}
	}
	// Reserve only after all pure checks. Attempts count even if the intent write or provider subsequently fails.
	if daily != nil {
		store, ok := counters.(DailyCounters)
		if !ok {
			return Decision{}, fmt.Errorf("daily counter store is unavailable")
		}
		now := env.Now
		if now.IsZero() {
			now = time.Now()
		}
		scope, _ := json.Marshal([]string{g.Principal(), g.ProjectKey})
		operation, _ := json.Marshal([]string{connector, op.ID})
		allowed, err := store.ReserveDaily(ctx, string(scope), string(operation), now.UTC().Format("2006-01-02"), *daily)
		if err != nil {
			return Decision{}, err
		}
		if !allowed {
			return deny(CodeLimitExceeded, "max_per_day reached for this agent/project/operation (UTC)"), nil
		}
	}
	return d, nil
}

func constraintValues(cap auth.ConnectorCapability, operation, key string) []any {
	var out []any
	if v, ok := cap.Constraints[key]; ok {
		out = append(out, v)
	}
	if v, ok := cap.Limits[key]; ok {
		out = append(out, v)
	}
	if v, ok := cap.OperationConstraints[operation][key]; ok {
		out = append(out, v)
	}
	return out
}

func finiteNumber(v any) (float64, bool) {
	var n float64
	switch v := v.(type) {
	case float64:
		n = v
	case int:
		n = float64(v)
	case int64:
		n = float64(v)
	case json.Number:
		var err error
		n, err = v.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0) && math.Abs(n) <= 9007199254740991
}
func numericCeiling(values []any, integer bool) (float64, error) {
	bound := math.Inf(1)
	for _, v := range values {
		n, ok := finiteNumber(v)
		if !ok || n < 0 || (integer && math.Trunc(n) != n) {
			return 0, fmt.Errorf("ceiling must be a non-negative finite%s number", map[bool]string{true: " integer", false: ""}[integer])
		}
		if n < bound {
			bound = n
		}
	}
	return bound, nil
}
func strictStrings(v any) ([]string, bool) {
	switch v := v.(type) {
	case []string:
		return v, true
	case []any:
		out := make([]string, len(v))
		for i, item := range v {
			s, ok := item.(string)
			if !ok {
				return nil, false
			}
			out[i] = s
		}
		return out, true
	default:
		return nil, false
	}
}

// argumentAt returns a leaf and creates missing parent objects only for a row default. An explicit null or non-object
// parent is refused so nested restrictions cannot be bypassed with malformed containers.
func argumentAt(args map[string]any, path string, create bool) (map[string]any, string, bool, error) {
	parts := strings.Split(path, ".")
	parent := args
	for _, p := range parts[:len(parts)-1] {
		v, ok := parent[p]
		if !ok {
			if !create {
				return nil, parts[len(parts)-1], false, nil
			}
			v = map[string]any{}
			parent[p] = v
		}
		child, ok := v.(map[string]any)
		if !ok {
			return nil, "", false, fmt.Errorf("%s must be an object", p)
		}
		parent = child
	}
	key := parts[len(parts)-1]
	_, exists := parent[key]
	return parent, key, exists, nil
}

// CheckResultRows refuses oversized collection payloads, including verbose results.
// It bounds returned rows, not provider scans or spend; provider settings remain necessary.
func CheckResultRows(payload any, limits map[string]any) error {
	bound, ok := limits["max_rows"].(float64)
	if !ok {
		return nil
	}
	var check func(any) error
	check = func(v any) error {
		switch v := v.(type) {
		case []any:
			if float64(len(v)) > bound {
				return fmt.Errorf("upstream result exceeds max_rows")
			}
		case map[string]any:
			for _, key := range []string{"rows", "data", "results", "items", "posts", "sites", "abilities", "databases", "tables"} {
				if list, ok := v[key].([]any); ok && float64(len(list)) > bound {
					return fmt.Errorf("upstream result exceeds max_rows")
				}
			}
			if inner, ok := v["result"]; ok {
				return check(inner)
			}
		}
		return nil
	}
	return check(payload)
}
