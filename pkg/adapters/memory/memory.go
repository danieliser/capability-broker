// Package memory is the AutoMem adapter. AutoMem has one service token and no
// tenancy, so the namespace is enforced here: forced tags on store, scoped tags
// and post-filtering on recall, ownership checks on associate.
package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/danieliser/capability-broker/pkg/adapter"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/config"
)

const AdapterVersion = "1.0"

type Adapter struct {
	cfg    config.Memory
	apiKey string
	cat    *catalog.Catalog
	client *http.Client
}

func New(cfg config.Memory, apiKey string, cat *catalog.Catalog) *Adapter {
	return &Adapter{cfg: cfg, apiKey: apiKey, cat: cat, client: &http.Client{Timeout: cfg.Timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return fmt.Errorf("redirects are refused") }}}
}

func (a *Adapter) Connector() string { return "memory" }

func (a *Adapter) Tools(_ context.Context, g *auth.Grant) ([]catalog.Tool, error) {
	conn, _ := a.cat.Connector("memory")
	cap, _ := g.Capabilities.For("memory")
	owner := g.Capabilities.IsOwner()
	var out []catalog.Tool
	for _, t := range conn.Tools {
		if !owner {
			switch t.Name {
			case "store_memory", "associate_memories":
				if !contains(cap.Operations, "store") {
					continue
				}
			case "recall_memory":
				if !contains(cap.Operations, "recall") {
					continue
				}
			}
		}
		out = append(out, t)
	}
	return out, nil
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func (a *Adapter) Health(ctx context.Context) error {
	_, _, err := a.do(ctx, http.MethodGet, "/health", nil, nil)
	return err
}

func (a *Adapter) Execute(ctx context.Context, call adapter.Call) (*adapter.Result, error) {
	args := call.Decision.Args
	namespaces, _ := call.Decision.Limits["namespaces"].([]string)
	scoped := len(namespaces) > 0 // agent grants only
	switch call.Op.ID {
	case "memory.store":
		body := map[string]any{"content": args["content"], "tags": args["tags"]}
		for _, k := range []string{"importance", "type", "confidence", "metadata"} {
			if v, ok := args[k]; ok {
				body[k] = v
			}
		}
		payload, _, err := a.do(ctx, http.MethodPost, "/memory", nil, body)
		if err != nil {
			return nil, err
		}
		id, _ := payload["memory_id"].(string)
		return &adapter.Result{Compact: map[string]any{"memory_id": id}, Verbose: payload, Outcome: adapter.OutcomeCompleted, UpstreamRequestID: id, OutcomeClass: "ok"}, nil
	case "memory.recall":
		q := url.Values{}
		if s := adapter.ArgString(args, "query"); s != "" {
			q.Set("query", s)
		}
		if qs, ok := args["queries"].([]any); ok {
			for _, x := range qs {
				q.Add("query", fmt.Sprint(x))
			}
		}
		for _, t := range stringList(args["tags"]) {
			q.Add("tags", t)
		}
		if m := adapter.ArgString(args, "tag_mode"); m != "" {
			q.Set("tag_mode", m)
		}
		if tq := adapter.ArgString(args, "time_query"); tq != "" {
			q.Set("time_query", tq)
		}
		limit := 5
		if v, ok := args["limit"].(float64); ok && v > 0 {
			limit = int(v)
		}
		q.Set("limit", fmt.Sprint(limit))
		if scoped {
			q.Set("tag_match", "exact")
			q.Set("expand_respect_tags", "true")
			q.Set("scope_fallback", "false")
		}
		payload, _, err := a.do(ctx, http.MethodGet, "/recall?"+q.Encode(), nil, nil)
		if err != nil {
			return nil, err
		}
		results, _ := payload["results"].([]any)
		kept := make([]any, 0, len(results))
		for _, r := range results {
			obj, _ := r.(map[string]any)
			if obj == nil {
				continue
			}
			// AutoMem nests the record under "memory" (id, final_score at the top level); older
			// shapes are flat. Namespace checks look at whichever carries the tags.
			if scoped && !hasAny(stringList(recordOf(obj)["tags"]), namespaces) {
				continue
			}
			kept = append(kept, obj)
		}
		payload["results"], payload["count"] = kept, len(kept)
		previewN := adapter.IntFromCompact(call.Op, "preview_chars", 200)
		compact := make([]map[string]any, 0, len(kept))
		for _, r := range kept {
			obj := r.(map[string]any)
			rec := recordOf(obj)
			content, _ := rec["content"].(string)
			id := obj["id"]
			if id == nil {
				id = obj["memory_id"]
			}
			compact = append(compact, map[string]any{"memory_id": id, "preview": adapter.Truncate(content, previewN),
				"score": obj["final_score"], "tags": rec["tags"], "timestamp": rec["timestamp"]})
		}
		return &adapter.Result{Compact: map[string]any{"count": len(kept), "results": compact}, Verbose: payload, Outcome: adapter.OutcomeCompleted, OutcomeClass: "ok"}, nil
	case "memory.associate":
		id1, id2 := adapter.ArgString(args, "memory1_id"), adapter.ArgString(args, "memory2_id")
		if scoped {
			for _, id := range []string{id1, id2} {
				m, _, err := a.do(ctx, http.MethodGet, "/memory/"+url.PathEscape(id), nil, nil)
				if err != nil {
					return nil, err
				}
				mem, _ := m["memory"].(map[string]any)
				if mem == nil {
					mem = m
				}
				if !hasAny(stringList(mem["tags"]), namespaces) {
					return nil, adapter.Denied("memory " + id + " is outside the grant's namespaces")
				}
			}
		}
		body := map[string]any{"memory1_id": id1, "memory2_id": id2, "type": args["type"], "strength": args["strength"]}
		payload, _, err := a.do(ctx, http.MethodPost, "/associate", nil, body)
		if err != nil {
			return nil, err
		}
		return &adapter.Result{Compact: map[string]any{"status": payload["status"]}, Verbose: payload, Outcome: adapter.OutcomeCompleted, OutcomeClass: "ok"}, nil
	case "memory.health":
		payload, _, err := a.do(ctx, http.MethodGet, "/health", nil, nil)
		if err != nil {
			return nil, err
		}
		return &adapter.Result{Compact: map[string]any{"status": payload["status"]}, Verbose: payload, Outcome: adapter.OutcomeCompleted, OutcomeClass: "ok"}, nil
	}
	return nil, adapter.Denied("unknown operation " + call.Op.ID)
}

// recordOf returns the memory record inside a recall result, or the result itself when flat.
func recordOf(obj map[string]any) map[string]any {
	if m, ok := obj["memory"].(map[string]any); ok {
		return m
	}
	return obj
}

func hasAny(tags, namespaces []string) bool {
	for _, t := range tags {
		for _, ns := range namespaces {
			if strings.EqualFold(t, ns) {
				return true
			}
		}
	}
	return false
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

func (a *Adapter) do(ctx context.Context, method, path string, _ url.Values, body any) (map[string]any, int, error) {
	if a.apiKey == "" && path != "/health" {
		return nil, 0, adapter.Unavailable("automem credential is not configured on the broker")
	}
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, 0, adapter.Failed(err.Error())
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(a.cfg.URL, "/")+path, reader)
	if err != nil {
		return nil, 0, adapter.Failed(err.Error())
	}
	if a.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+a.apiKey)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client.Do(req)
	if err != nil {
		return nil, 0, adapter.Unavailable(strings.ReplaceAll(err.Error(), a.apiKey, "***"))
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, resp.StatusCode, &adapter.UpstreamError{Code: adapter.CodeUpstreamError, Message: "response read failed", Outcome: adapter.OutcomeUnknown}
	}
	var payload map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			payload = map[string]any{"raw": adapter.Truncate(string(raw), 300)}
		}
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg := fmt.Sprintf("automem %d", resp.StatusCode)
		if e, ok := payload["error"]; ok {
			msg += ": " + adapter.Truncate(fmt.Sprint(e), 200)
		}
		return nil, resp.StatusCode, adapter.Failed(strings.ReplaceAll(msg, a.apiKey, "***"))
	}
	return payload, resp.StatusCode, nil
}

var _ adapter.Adapter = (*Adapter)(nil)
