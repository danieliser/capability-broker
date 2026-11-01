package audit

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/capability-broker/pkg/durable"
)

func TestIntentOutcomeAppendOnly(t *testing.T) {
	ctx := context.Background()
	db, err := durable.Open(ctx, filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	w := NewWriter(db)
	call := Call{Principal: "agent:x", Connector: "clickhouse", Tool: "run_query", Operation: "clickhouse.named_query",
		Args: map[string]any{"query": "@a.b"}, PolicyVersion: "v"}
	id, err := w.Intent(ctx, call)
	if err != nil || id == "" {
		t.Fatal(err)
	}
	if err := w.Outcome(ctx, id, call, Outcome{State: StateCompleted, UpstreamRequestID: "q1", Duration: 12 * time.Millisecond, ResponseBytes: 10}); err != nil {
		t.Fatal(err)
	}
	w.Denied(ctx, call, "BROKER_DENIED_ARGUMENT", "nope")
	rs, err := w.List(ctx, 0, 10)
	if err != nil || len(rs) != 3 {
		t.Fatalf("list: %d %v", len(rs), err)
	}
	if rs[0].State != StateIntent || rs[1].State != StateCompleted || rs[2].State != StateDenied || rs[1].CallID != id {
		t.Fatalf("rows: %+v", rs)
	}
	if rs[0].ArgsDigest == "" || rs[0].ArgsSummary == "" {
		t.Fatal("digest missing")
	}
	if _, err := db.ExecContext(ctx, `UPDATE receipts SET decision='deny' WHERE id=?`, rs[0].ID); err == nil {
		t.Fatal("update allowed on receipts")
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM receipts WHERE id=?`, rs[0].ID); err == nil {
		t.Fatal("delete allowed on receipts")
	}
}
