// Package memorycleanup inventories exact smoke tags and deletes only a reviewed ID manifest.
// It never uses ranked recall as an inventory, and never sends a bulk-delete request.
package memorycleanup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const Tag = "broker-smoke-test"
const Namespace = "agent:companykit.data-sync-steward"

type Caller interface {
	CallTool(context.Context, *mcp.CallToolParams) (*mcp.CallToolResult, error)
}
type Record struct {
	ID        string   `json:"memory_id"`
	Tags      []string `json:"tags"`
	Content   string   `json:"content"`
	SHA256    string   `json:"content_sha256"`
	CreatedAt string   `json:"created_at,omitempty"`
}
type Manifest struct {
	Version    int      `json:"version"`
	ObservedAt string   `json:"observed_at"`
	Tag        string   `json:"tag"`
	Namespace  string   `json:"namespace"`
	Memories   []Record `json:"memories"`
}

func payload(result *mcp.CallToolResult) (map[string]any, error) {
	if result == nil || result.IsError {
		return nil, fmt.Errorf("memory API refused the call")
	}
	var v any = result.StructuredContent
	if v == nil {
		var text []string
		for _, content := range result.Content {
			if c, ok := content.(*mcp.TextContent); ok {
				text = append(text, c.Text)
			}
		}
		if err := json.Unmarshal([]byte(strings.Join(text, "\n")), &v); err != nil {
			return nil, fmt.Errorf("memory API returned invalid JSON")
		}
	}
	// Some upstream versions put JSON text under a result envelope.
	for range 3 {
		if s, ok := v.(string); ok {
			if err := json.Unmarshal([]byte(s), &v); err != nil {
				break
			}
			continue
		}
		obj, ok := v.(map[string]any)
		if !ok {
			break
		}
		if _, failed := obj["error"]; failed {
			return nil, fmt.Errorf("memory API returned an error")
		}
		if inner, ok := obj["result"]; ok {
			v = inner
			continue
		}
		return obj, nil
	}
	return nil, fmt.Errorf("memory API returned an unexpected shape")
}
func read(ctx context.Context, c Caller, args map[string]any) (map[string]any, error) {
	r, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "recall_memory", Arguments: args})
	if err != nil {
		return nil, fmt.Errorf("recall_memory failed")
	}
	return payload(r)
}
func record(obj map[string]any) (Record, error) {
	outer := obj
	if inner, ok := obj["memory"].(map[string]any); ok {
		obj = inner
	}
	id, _ := outer["memory_id"].(string)
	if id == "" {
		id, _ = outer["id"].(string)
	}
	if id == "" {
		id, _ = obj["memory_id"].(string)
	}
	if id == "" {
		id, _ = obj["id"].(string)
	}
	content, ok := obj["content"].(string)
	if id == "" || !ok {
		return Record{}, fmt.Errorf("memory record lacks ID or full content")
	}
	if truncated, _ := obj["content_truncated"].(bool); truncated {
		return Record{}, fmt.Errorf("memory content is truncated")
	}
	raw, ok := obj["tags"].([]any)
	if !ok {
		return Record{}, fmt.Errorf("memory record lacks tags")
	}
	tags := make([]string, len(raw))
	for i, v := range raw {
		s, ok := v.(string)
		if !ok {
			return Record{}, fmt.Errorf("memory has malformed tags")
		}
		tags[i] = s
	}
	sum := sha256.Sum256([]byte(content))
	created, _ := obj["created_at"].(string)
	return Record{ID: id, Tags: tags, Content: content, SHA256: hex.EncodeToString(sum[:]), CreatedAt: created}, nil
}
func has(tags []string, want string) bool {
	for _, tag := range tags {
		if strings.EqualFold(tag, want) {
			return true
		}
	}
	return false
}
func scoped(r Record) bool { return has(r.Tags, Tag) && has(r.Tags, Namespace) }

// Inventory enumerates exact tags to has_more=false, then intersects the steward namespace locally.
func Inventory(ctx context.Context, c Caller) (Manifest, error) {
	m := Manifest{Version: 1, ObservedAt: time.Now().UTC().Format(time.RFC3339), Tag: Tag, Namespace: Namespace, Memories: []Record{}}
	seen := map[string]bool{}
	for offset := 0; ; offset += 200 {
		obj, err := read(ctx, c, map[string]any{"tags": []string{Tag}, "exhaustive": true, "limit": 200, "offset": offset, "format": "json"})
		if err != nil {
			return Manifest{}, err
		}
		if obj["mode"] != "enumeration" || obj["offset"] != float64(offset) || obj["limit"] != float64(200) {
			return Manifest{}, fmt.Errorf("API did not confirm exact paginated enumeration")
		}
		more, ok := obj["has_more"].(bool)
		if !ok {
			return Manifest{}, fmt.Errorf("API omitted has_more")
		}
		results, ok := obj["results"].([]any)
		if !ok {
			return Manifest{}, fmt.Errorf("API omitted result list")
		}
		if len(results) > 200 || (more && len(results) != 200) {
			return Manifest{}, fmt.Errorf("API returned an incomplete page")
		}
		for _, item := range results {
			obj, ok := item.(map[string]any)
			if !ok {
				return Manifest{}, fmt.Errorf("invalid memory record")
			}
			r, err := record(obj)
			if err != nil {
				return Manifest{}, err
			}
			if seen[r.ID] {
				return Manifest{}, fmt.Errorf("duplicate ID across inventory pages")
			}
			seen[r.ID] = true
			if !has(r.Tags, Tag) {
				return Manifest{}, fmt.Errorf("API returned a record outside the exact tag")
			}
			if scoped(r) {
				m.Memories = append(m.Memories, r)
			}
		}
		if !more {
			return m, nil
		}
	}
}

// Apply revalidates every ID's content and both exact tags before any deletion.
// execute=false (the CLI default) performs only these reads. There is no implicit enumeration-to-delete.
func Apply(ctx context.Context, c Caller, m Manifest, execute bool) error {
	if m.Version != 1 || m.Tag != Tag || m.Namespace != Namespace {
		return fmt.Errorf("manifest is not this exact smoke/steward scope")
	}
	seen := map[string]bool{}
	for _, expected := range m.Memories {
		if expected.ID == "" || seen[expected.ID] || !scoped(expected) {
			return fmt.Errorf("invalid or duplicate manifest ID")
		}
		seen[expected.ID] = true
		sum := sha256.Sum256([]byte(expected.Content))
		if hex.EncodeToString(sum[:]) != expected.SHA256 {
			return fmt.Errorf("manifest content digest is invalid for %s", expected.ID)
		}
		obj, err := read(ctx, c, map[string]any{"memory_id": expected.ID, "format": "json"})
		if err != nil {
			return err
		}
		// ID fetches may wrap one record in a results array.
		if list, ok := obj["results"].([]any); ok {
			if len(list) != 1 {
				return fmt.Errorf("ID lookup did not return one memory")
			}
			obj, ok = list[0].(map[string]any)
			if !ok {
				return fmt.Errorf("invalid ID lookup")
			}
		}
		actual, err := record(obj)
		if err != nil {
			return err
		}
		if actual.ID != expected.ID || !scoped(actual) || actual.SHA256 != expected.SHA256 {
			return fmt.Errorf("memory %s changed or left scope; re-inventory and review", expected.ID)
		}
	}
	if !execute {
		return nil
	}
	for _, r := range m.Memories {
		result, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "delete_memory", Arguments: map[string]any{"memory_id": r.ID}})
		if err != nil {
			return fmt.Errorf("delete failed for %s; earlier IDs may be deleted", r.ID)
		}
		obj, err := payload(result)
		if err != nil {
			return fmt.Errorf("delete refused for %s; earlier IDs may be deleted", r.ID)
		}
		// AutoMem returns status=success; do not mistake a tool transport success for deletion.
		if obj["memory_id"] != r.ID && obj["status"] != "success" && obj["status"] != "deleted" {
			return fmt.Errorf("delete of %s is unconfirmed; re-inventory", r.ID)
		}
	}
	// Verify the exact reviewed IDs are absent from a fresh complete enumeration.
	after, err := Inventory(ctx, c)
	if err != nil {
		return fmt.Errorf("post-delete inventory failed: %w", err)
	}
	for _, r := range after.Memories {
		if seen[r.ID] {
			return fmt.Errorf("memory %s still present after delete", r.ID)
		}
	}
	return nil
}
