// Package policy decides whether a resolved operation may run under a grant and
// with which effective arguments. It runs after the adapter has normalised the
// call, so an adapter cannot widen what was approved. Checks are named by the
// strings used in the catalog's `constraints:` lists.
package policy

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
)

const (
	CodeDeniedConnector = "BROKER_DENIED_CONNECTOR"
	CodeDeniedOperation = "BROKER_DENIED_OPERATION"
	CodeDeniedArgument  = "BROKER_DENIED_ARGUMENT"
	CodeLimitExceeded   = "BROKER_LIMIT_EXCEEDED"
	CodeApprovalNeeded  = "BROKER_APPROVAL_REQUIRED"

	DefaultMaxRows          = 10000
	DefaultMaxExecutionSecs = 60
	DefaultAgentCallsPerRun = 200
)

// Counters is the per-grant counter store (pkg/auth.Store satisfies it).
type Counters interface {
	Counter(ctx context.Context, grantID, key string) (int64, error)
	Increment(ctx context.Context, grantID, key string) (int64, error)
}

// Env is what the engine knows about the world beyond the grant: site → host for
// URL checks, and the catalog for named queries.
type Env struct {
	Catalog   *catalog.Catalog
	SiteHosts map[string]string // wp site id → hostname
	Now       time.Time         // zero uses the current UTC time
}

type Decision struct {
	Allow  bool
	Code   string
	Reason string
	// Args are the effective arguments after enrichment and clamping.
	Args map[string]any
	// Effective limits the adapter must honour (max_rows, max_execution_seconds, forced_tags…).
	Limits map[string]any
	// Query is set for named queries.
	Query *catalog.Query
}

func deny(code, reason string) Decision { return Decision{Allow: false, Code: code, Reason: reason} }

// Evaluate is the single authorization point. It mutates nothing it was given.
func Evaluate(ctx context.Context, g *auth.Grant, connector string, op *catalog.Operation, in map[string]any, env Env, counters Counters) (Decision, error) {
	cap, ok := g.Capabilities.For(connector)
	if !ok {
		return deny(CodeDeniedConnector, fmt.Sprintf("grant does not reach connector %s", connector)), nil
	}
	owner := g.Capabilities.IsOwner()
	args := CloneArgs(in)
	d := Decision{Allow: true, Args: args, Limits: map[string]any{}}

	if op.RequiresCapability != "" && !owner {
		switch {
		case op.RequiresCapability == "sql.select":
			if cap.SQL != "select" {
				return deny(CodeDeniedOperation, "grant has no sql.select capability; use a named query"), nil
			}
		case strings.HasPrefix(op.RequiresCapability, "operations:"):
			want := strings.TrimPrefix(op.RequiresCapability, "operations:")
			if !contains(cap.Operations, want) {
				return deny(CodeDeniedOperation, fmt.Sprintf("operation %s is not in the grant", want)), nil
			}
		default:
			return deny(CodeDeniedOperation, "unknown capability requirement "+op.RequiresCapability), nil
		}
	}
	if op.Approval == "exact_action" && !owner {
		// P2: build the action record and hand it to judgement. P1 has no such operation.
		return deny(CodeApprovalNeeded, "operation requires exact-action approval (not available in phase 1)"), nil
	}

	for _, check := range op.Constraints {
		var err error
		var dd *Decision
		switch check {
		case "queries":
			dd, err = checkQueries(&d, cap, owner, env)
		case "max_rows":
			d.Limits["max_rows"] = limit(cap.Limits, "max_rows", DefaultMaxRows, owner, d.Query)
		case "max_execution_seconds":
			d.Limits["max_execution_seconds"] = limit(cap.Limits, "max_execution_seconds", DefaultMaxExecutionSecs, owner, nil)
		case "calls_per_run":
			dd, err = checkCounter(ctx, g, counters, "calls", cap.Limits["calls_per_run"], DefaultAgentCallsPerRun, owner)
		case "sites":
			dd = checkSite(&d, cap, owner)
		case "sites_filter":
			dd = checkSitesFilter(&d, cap, owner)
		case "sites_url":
			dd = checkSiteURL(&d, cap, owner, env)
		case "post_types":
			dd = checkPostType(&d, cap, owner)
		case "max_drafts_per_run":
			dd, err = checkCounter(ctx, g, counters, "drafts", cap.Limits["max_drafts_per_run"], 0, owner)
		case "store_per_run":
			dd, err = checkCounter(ctx, g, counters, "stores", cap.Limits["store_per_run"], 0, owner)
		case "namespaces":
			dd = checkNamespaces(&d, cap, owner, op)
		case "max_importance":
			dd = clampImportance(&d, cap, owner, op.Tool == "store_memory")
		default:
			return deny(CodeDeniedOperation, "catalog names an unknown constraint "+check), nil
		}
		if err != nil {
			return Decision{}, err
		}
		if dd != nil {
			return *dd, nil
		}
	}
	return d, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func limit(limits map[string]float64, key string, def float64, owner bool, q *catalog.Query) float64 {
	v := def
	if owner {
		v = def * 10
	}
	if l, ok := limits[key]; ok && l > 0 && l < v {
		v = l
	}
	if q != nil {
		if ql, ok := q.Limits[key]; ok && ql > 0 && ql < v {
			v = ql
		}
	}
	return v
}

func checkQueries(d *Decision, cap auth.ConnectorCapability, owner bool, env Env) (*Decision, error) {
	raw, _ := d.Args["query"].(string)
	id := strings.TrimPrefix(raw, "@")
	q, ok := env.Catalog.Query(id)
	if !ok {
		dd := deny(CodeDeniedArgument, fmt.Sprintf("named query %s does not exist", id))
		return &dd, nil
	}
	if !owner && !contains(cap.Queries, id) {
		dd := deny(CodeDeniedArgument, fmt.Sprintf("named query %s is not in the grant", id))
		return &dd, nil
	}
	d.Query = &q
	return nil, nil
}

func checkCounter(ctx context.Context, g *auth.Grant, counters Counters, key string, capLimit, def float64, owner bool) (*Decision, error) {
	if owner {
		return nil, nil
	}
	lim := def
	if capLimit > 0 {
		lim = capLimit
	}
	if lim <= 0 {
		return nil, nil
	}
	n, err := counters.Increment(ctx, g.ID, key)
	if err != nil {
		return nil, err
	}
	if float64(n) > lim {
		dd := deny(CodeLimitExceeded, fmt.Sprintf("%s limit of %d per run reached", key, int(lim)))
		return &dd, nil
	}
	return nil, nil
}

func checkSite(d *Decision, cap auth.ConnectorCapability, owner bool) *Decision {
	site, _ := d.Args["site"].(string)
	if owner {
		return nil
	}
	if site == "" {
		if len(cap.Sites) == 1 {
			d.Args["site"] = cap.Sites[0]
			return nil
		}
		dd := deny(CodeDeniedArgument, "site is required; grant allows "+strings.Join(cap.Sites, ", "))
		return &dd
	}
	if !contains(cap.Sites, site) {
		dd := deny(CodeDeniedArgument, fmt.Sprintf("site %s is not in the grant", site))
		return &dd
	}
	return nil
}

func checkSitesFilter(d *Decision, cap auth.ConnectorCapability, owner bool) *Decision {
	if owner {
		return nil
	}
	if len(cap.Sites) == 0 {
		dd := deny(CodeDeniedArgument, "grant lists no sites")
		return &dd
	}
	if raw, ok := d.Args["sites"]; ok && raw != nil {
		list, ok := raw.([]any)
		if !ok || len(list) == 0 {
			dd := deny(CodeDeniedArgument, "sites must be a non-empty list")
			return &dd
		}
		for _, s := range list {
			str, _ := s.(string)
			if !contains(cap.Sites, str) {
				dd := deny(CodeDeniedArgument, fmt.Sprintf("site %s is not in the grant", str))
				return &dd
			}
		}
		return nil
	}
	sites := make([]any, 0, len(cap.Sites))
	for _, s := range cap.Sites {
		sites = append(sites, s)
	}
	d.Args["sites"] = sites
	return nil
}

func checkSiteURL(d *Decision, cap auth.ConnectorCapability, owner bool, env Env) *Decision {
	if owner {
		return nil
	}
	raw, _ := d.Args["url"].(string)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		dd := deny(CodeDeniedArgument, "url must be absolute")
		return &dd
	}
	for _, site := range cap.Sites {
		if host, ok := env.SiteHosts[site]; ok && strings.EqualFold(host, u.Hostname()) {
			return nil
		}
	}
	dd := deny(CodeDeniedArgument, fmt.Sprintf("host %s is not one of the grant's sites", u.Hostname()))
	return &dd
}

func checkPostType(d *Decision, cap auth.ConnectorCapability, owner bool) *Decision {
	if owner {
		return nil
	}
	allowed := stringList(cap.Constraints["post_types"])
	if len(allowed) == 0 {
		dd := deny(CodeDeniedArgument, "grant lists no post_types for drafts")
		return &dd
	}
	inner, _ := d.Args["arguments"].(map[string]any)
	if inner == nil {
		inner = map[string]any{}
		d.Args["arguments"] = inner
	}
	pt, _ := inner["post_type"].(string)
	if pt == "" {
		pt = "post"
		inner["post_type"] = pt
	}
	if !contains(allowed, pt) {
		dd := deny(CodeDeniedArgument, fmt.Sprintf("post_type %s is not in the grant", pt))
		return &dd
	}
	return nil
}

func checkNamespaces(d *Decision, cap auth.ConnectorCapability, owner bool, op *catalog.Operation) *Decision {
	if owner {
		return nil
	}
	if len(cap.Namespaces) == 0 {
		dd := deny(CodeDeniedArgument, "grant lists no memory namespaces")
		return &dd
	}
	d.Limits["namespaces"] = cap.Namespaces
	switch op.Tool {
	case "store_memory", "update_memory":
		if op.Tool == "update_memory" {
			if _, supplied := d.Args["tags"]; !supplied {
				return nil
			}
		}
		tags := stringList(d.Args["tags"])
		for _, t := range tags {
			if strings.HasPrefix(strings.ToLower(t), "agent:") || strings.HasPrefix(strings.ToLower(t), "project:") {
				if !contains(cap.Namespaces, t) {
					dd := deny(CodeDeniedArgument, fmt.Sprintf("tag %s is outside the grant's namespaces", t))
					return &dd
				}
			}
		}
		if !contains(tags, cap.Namespaces[0]) {
			tags = append(tags, cap.Namespaces[0])
		}
		out := make([]any, 0, len(tags))
		for _, t := range tags {
			out = append(out, t)
		}
		d.Args["tags"] = out
	case "recall_memory":
		tags := stringList(d.Args["tags"])
		var scoped []any
		for _, t := range tags {
			if strings.HasPrefix(strings.ToLower(t), "agent:") || strings.HasPrefix(strings.ToLower(t), "project:") {
				if !contains(cap.Namespaces, t) {
					dd := deny(CodeDeniedArgument, fmt.Sprintf("tag %s is outside the grant's namespaces", t))
					return &dd
				}
			}
			scoped = append(scoped, t)
		}
		if len(scoped) == 0 {
			for _, ns := range cap.Namespaces {
				scoped = append(scoped, ns)
			}
		}
		d.Args["tags"] = scoped
	}
	return nil
}

func clampImportance(d *Decision, cap auth.ConnectorCapability, owner, storing bool) *Decision {
	if owner {
		return nil
	}
	raw, exists := cap.Constraints["max_importance"]
	if !exists {
		return nil
	}
	max, ok := finiteNumber(raw)
	if !ok || max < 0 || max > 1 {
		dd := deny(CodeDeniedArgument, "max_importance must be between 0 and 1")
		return &dd
	}
	if v, supplied := d.Args["importance"]; supplied {
		n, valid := finiteNumber(v)
		if !valid || n < 0 || n > 1 {
			dd := deny(CodeDeniedArgument, "importance must be between 0 and 1")
			return &dd
		}
		if n > max {
			d.Args["importance"] = max
		}
	} else if storing && max < 0.5 {
		d.Args["importance"] = max
	}
	return nil
}

func stringList(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}
