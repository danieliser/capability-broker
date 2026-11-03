package memorycleanup

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type fake struct {
	calls       []*mcp.CallToolParams
	records     []map[string]any
	corruptPage bool
	failDelete  bool
}

func (f *fake) CallTool(_ context.Context, p *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	f.calls = append(f.calls, p)
	args := p.Arguments.(map[string]any)
	var body any
	switch p.Name {
	case "recall_memory":
		if id, ok := args["memory_id"].(string); ok {
			for _, r := range f.records {
				if r["memory_id"] == id {
					body = map[string]any{"results": []any{r}}
					break
				}
			}
			if body == nil {
				body = map[string]any{"results": []any{}}
			}
		} else {
			if args["exhaustive"] != true || args["current_only"] != nil {
				return nil, fmt.Errorf("must use exact enumeration without ranked-only params")
			}
			offset := args["offset"].(int)
			end := offset + 200
			if end > len(f.records) {
				end = len(f.records)
			}
			more := end < len(f.records)
			results := f.records[offset:end]
			if f.corruptPage {
				results = nil
				more = true
			}
			body = map[string]any{"mode": "enumeration", "offset": offset, "limit": 200, "has_more": more, "results": results}
		}
	case "delete_memory":
		if len(args) != 1 || args["tags"] != nil {
			return nil, fmt.Errorf("bulk delete is forbidden")
		}
		if f.failDelete {
			return nil, fmt.Errorf("fixture failure")
		}
		id := args["memory_id"].(string)
		for i, r := range f.records {
			if r["memory_id"] == id {
				f.records = append(f.records[:i], f.records[i+1:]...)
				break
			}
		}
		body = map[string]any{"memory_id": id, "message": "Memory deleted"}
	default:
		return nil, fmt.Errorf("unexpected tool")
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}, nil
}
func smoke(id, namespace string) map[string]any {
	return map[string]any{"memory_id": id, "content": "fixture smoke " + id, "tags": []any{Tag, namespace}}
}

func TestInventoryCompleteAndScoped(t *testing.T) {
	f := &fake{}
	for i := range 201 {
		ns := Namespace
		if i == 199 {
			ns = "agent:other"
		}
		f.records = append(f.records, smoke(fmt.Sprint(i), ns))
	}
	m, err := Inventory(context.Background(), f)
	if err != nil || len(m.Memories) != 200 || len(f.calls) != 2 {
		t.Fatalf("inventory=%d calls=%d err=%v", len(m.Memories), len(f.calls), err)
	}
	if f.calls[1].Arguments.(map[string]any)["offset"] != 200 {
		t.Fatal("did not paginate")
	}
	for _, r := range m.Memories {
		if r.ID == "199" {
			t.Fatal("foreign namespace admitted")
		}
	}
}
func TestDryRunAndExactExecute(t *testing.T) {
	f := &fake{records: []map[string]any{smoke("one", Namespace), smoke("foreign", "agent:other")}}
	m, err := Inventory(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(context.Background(), f, m, false); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.calls {
		if c.Name == "delete_memory" {
			t.Fatal("dry run deleted")
		}
	}
	if err := Apply(context.Background(), f, m, true); err != nil {
		t.Fatal(err)
	}
	if len(f.records) != 1 || f.records[0]["memory_id"] != "foreign" {
		t.Fatalf("wrong deletion: %v", f.records)
	}
	deletes := 0
	for _, c := range f.calls {
		if c.Name == "delete_memory" {
			deletes++
			if c.Arguments.(map[string]any)["memory_id"] != "one" {
				t.Fatal("wrong ID")
			}
		}
	}
	if deletes != 1 {
		t.Fatalf("deleted %d", deletes)
	}
}
func TestCleanupRefusesChangedScopeContentAndBadManifest(t *testing.T) {
	for _, change := range []string{"tags", "content", "manifest", "duplicate", "missing", "digest"} {
		t.Run(change, func(t *testing.T) {
			f := &fake{records: []map[string]any{smoke("one", Namespace)}}
			m, err := Inventory(context.Background(), f)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "tags":
				f.records[0]["tags"] = []any{Tag, "agent:other"}
			case "content":
				f.records[0]["content"] = "durable record now"
			case "manifest":
				m.Namespace = "agent:other"
			case "duplicate":
				m.Memories = append(m.Memories, m.Memories[0])
			case "missing":
				f.records = nil
			case "digest":
				m.Memories[0].SHA256 = strings.Repeat("0", 64)
			}
			if err := Apply(context.Background(), f, m, true); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
			for _, c := range f.calls {
				if c.Name == "delete_memory" {
					t.Fatal("deleted despite failed revalidation")
				}
			}
		})
	}
}
func TestInventoryRejectsMalformedAndPartialPages(t *testing.T) {
	f := &fake{records: []map[string]any{smoke("one", Namespace)}, corruptPage: true}
	if _, err := Inventory(context.Background(), f); err == nil {
		t.Fatal("partial page accepted")
	}
	f.corruptPage = false
	f.records[0]["content_truncated"] = true
	if _, err := Inventory(context.Background(), f); err == nil {
		t.Fatal("truncated record accepted")
	}
}
