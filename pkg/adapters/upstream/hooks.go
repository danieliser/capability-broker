package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/adapter"
)

// Guards are request hooks named in the catalog. They see the effective
// arguments after policy and may rewrite them; they cannot widen the grant.
func (a *Adapter) guard(ctx context.Context, s *mcp.ClientSession, name string, call adapter.Call, args map[string]any) error {
	switch name {
	case "resolve_named_query":
		q := call.Decision.Query
		if q == nil {
			return adapter.Denied("named query not resolved")
		}
		given, _ := args["params"].(map[string]any)
		params := map[string]any{}
		for pname, p := range q.Params {
			v, ok := given[pname]
			if !ok || v == nil {
				if p.Required {
					return adapter.Denied(fmt.Sprintf("param %s is required", pname))
				}
				if p.Default != "" {
					params[pname] = p.Default
				}
				continue
			}
			params[pname] = v
		}
		for pname := range given {
			if _, declared := q.Params[pname]; !declared {
				return adapter.Denied(fmt.Sprintf("param %s is not declared by query %s", pname, q.ID))
			}
		}
		args["query"] = q.SQL
		if len(params) > 0 {
			args["params"] = params
		} else {
			delete(args, "params")
		}
		return nil
	case "single_statement":
		sql, _ := args["query"].(string)
		if strings.Contains(strings.TrimSuffix(strings.TrimSpace(sql), ";"), ";") {
			return adapter.Denied("one statement per call")
		}
		return nil
	case "select_only":
		sql := strings.TrimSpace(fmt.Sprint(args["query"]))
		if !readOnlyStart.MatchString(sql) {
			return adapter.Denied("only SELECT, WITH, SHOW, DESCRIBE, EXPLAIN and EXISTS statements are permitted")
		}
		upper := strings.ToUpper(sql)
		if strings.Contains(upper, "INTO OUTFILE") || settingsClause.MatchString(upper) {
			return adapter.Denied("INTO OUTFILE and SETTINGS clauses are not permitted")
		}
		return nil
	case "force_draft_status":
		inner, _ := args["arguments"].(map[string]any)
		if inner == nil {
			inner = map[string]any{}
		}
		inner["status"] = "draft"
		args["arguments"] = inner
		return nil
	case "compact_request":
		if !call.Verbose {
			args["compact"] = true
		}
		return nil
	case "single_memory_only":
		// mcp-automem's batch and supersede modes bypass the per-call namespace and counter checks.
		if call.Grant != nil && call.Grant.Capabilities.IsOwner() {
			return nil
		}
		for _, k := range []string{"memories", "supersedes_memory_id"} {
			if _, ok := args[k]; ok {
				return adapter.Denied(k + " is not permitted through the broker; store one memory per call")
			}
		}
		return nil
	case "scoped_recall":
		if _, ok := call.Decision.Limits["namespaces"]; !ok {
			return nil // owner grant
		}
		for _, k := range []string{"memory_id", "exhaustive", "scope_fallback", "expand_respect_tags", "tag_match", "current_only", "state_mode"} {
			delete(args, k)
		}
		args["tag_match"] = "exact"
		args["expand_respect_tags"] = true
		args["scope_fallback"] = false
		return nil
	case "memory_ownership":
		namespaces, _ := call.Decision.Limits["namespaces"].([]string)
		if len(namespaces) == 0 {
			return nil
		}
		keys := []string{"memory1_id", "memory2_id"}
		if call.Tool != "associate_memories" {
			keys = []string{"memory_id"}
		}
		for _, k := range keys {
			id, _ := args[k].(string)
			if id == "" {
				return adapter.Denied(k + " is required")
			}
			res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "recall_memory", Arguments: map[string]any{"memory_id": id}})
			if err != nil || res == nil || res.IsError {
				return adapter.Failed("could not verify ownership of memory " + id)
			}
			if !recordHasNamespace(payloadOf(res), namespaces) {
				return adapter.Denied("memory " + id + " is outside the grant's namespaces")
			}
		}
		if _, ok := args["associations"]; ok {
			return adapter.Denied("batch associations are not permitted through the broker")
		}
		return nil
	case "no_bulk_delete":
		if call.Grant != nil && call.Grant.Capabilities.IsOwner() {
			return nil
		}
		if _, ok := args["tags"]; ok {
			return adapter.Denied("bulk delete by tag is not permitted through the broker")
		}
		return nil
	}
	return adapter.Denied("catalog names an unknown guard " + name)
}

var readOnlyStart = regexp.MustCompile(`(?is)^\s*(SELECT|WITH|SHOW|DESCRIBE|DESC|EXPLAIN|EXISTS)\b`)
var settingsClause = regexp.MustCompile(`\bSETTINGS\b`)

// respond applies a response hook named in the catalog.
func (a *Adapter) respond(name string, call adapter.Call, payload any) (any, error) {
	switch name {
	case "namespace_filter":
		namespaces, _ := call.Decision.Limits["namespaces"].([]string)
		if len(namespaces) == 0 {
			return payload, nil
		}
		m, ok := payload.(map[string]any)
		if !ok {
			return payload, nil
		}
		results, _ := m["results"].([]any)
		kept := make([]any, 0, len(results))
		for _, r := range results {
			if recordHasNamespace(r, namespaces) {
				kept = append(kept, r)
			}
		}
		m["results"], m["count"] = kept, len(kept)
		return m, nil
	case "site_result":
		// wp-mcp-router wraps the site's MCP result: {site, result: {content, isError, structuredContent}}.
		outer, ok := payload.(map[string]any)
		if !ok {
			return payload, nil
		}
		inner, ok := outer["result"].(map[string]any)
		if !ok {
			return payload, nil
		}
		if _, wrapped := inner["content"].([]any); !wrapped {
			return payload, nil
		}
		if isErr, _ := inner["isError"].(bool); isErr {
			msg, _ := json.Marshal(map[string]any{"site": outer["site"], "error": unwrapResult(inner)})
			return nil, adapter.Failed(string(msg))
		}
		return map[string]any{"site": outer["site"], "result": unwrapResult(inner)}, nil
	}
	return nil, adapter.Denied("catalog names an unknown response hook " + name)
}

// recordHasNamespace checks a memory record (flat, or nested under "memory") for a namespace tag.
func recordHasNamespace(v any, namespaces []string) bool {
	obj, ok := v.(map[string]any)
	if !ok {
		return false
	}
	if m, ok := obj["memory"].(map[string]any); ok {
		obj = m
	}
	tags, _ := obj["tags"].([]any)
	for _, t := range tags {
		s, _ := t.(string)
		for _, ns := range namespaces {
			if strings.EqualFold(s, ns) {
				return true
			}
		}
	}
	return false
}

// unwrapResult turns a site's MCP result into its payload: structuredContent when present,
// otherwise the text content parsed as JSON, otherwise the joined text. A `{success, data}` envelope is opened.
func unwrapResult(inner map[string]any) any {
	if sc, ok := inner["structuredContent"]; ok && sc != nil {
		return openEnvelope(sc)
	}
	var texts []string
	if content, ok := inner["content"].([]any); ok {
		for _, c := range content {
			if cm, ok := c.(map[string]any); ok {
				if s, ok := cm["text"].(string); ok {
					texts = append(texts, s)
				}
			}
		}
	}
	joined := strings.Join(texts, "\n")
	var parsed any
	if err := json.Unmarshal([]byte(joined), &parsed); err == nil {
		return openEnvelope(parsed)
	}
	return joined
}

func openEnvelope(v any) any {
	if m, ok := v.(map[string]any); ok {
		if data, ok := m["data"]; ok && len(m) <= 2 {
			return data
		}
	}
	return v
}
