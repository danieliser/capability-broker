package policy

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/durable"
)

func TestReviewChildCannotRepairDeniedParentPrefix(t *testing.T) {
	parent := auth.Capabilities{"x": {Constraints: map[string]any{"prefix": ""}}}
	child := auth.Capabilities{"x": {Constraints: map[string]any{"prefix": "approved:"}}}.Intersect(parent)
	op := &catalog.Operation{ID: "x.send", ArgumentConstraints: []catalog.ArgumentConstraint{{Kind: "prefix", Path: "subject", Key: "prefix", Required: true}}}
	for name, caps := range map[string]auth.Capabilities{"parent": parent, "child": child} {
		d, err := EvaluateArguments(context.Background(), &auth.Grant{Capabilities: caps}, "x", op, Decision{Allow: true, Args: map[string]any{"subject": "approved: test"}}, Env{}, nil)
		if err != nil || d.Allow {
			t.Errorf("%s admitted denied parent: %+v %v", name, d, err)
		}
	}
}

func TestReviewDeclarativeExactStringsAndNestedTypes(t *testing.T) {
	for _, tt := range []struct {
		name, kind, path string
		constraint       any
		args             map[string]any
	}{
		{"unicode allowlist", "allowlist", "to", []string{"ops@example.test"}, map[string]any{"to": "opſ@example.test"}},
		{"combining prefix", "prefix", "subject", "é:", map[string]any{"subject": "e\u0301: report"}},
		{"case prefix", "prefix", "subject", "Approved:", map[string]any{"subject": "approved: report"}},
		{"array parent", "allowlist", "message.to", []string{"a"}, map[string]any{"message": []any{map[string]any{"to": "a"}}}},
		{"nested mixed list", "allowlist", "message.to", []string{"a"}, map[string]any{"message": map[string]any{"to": []any{"a", []any{"a"}}}}},
		{"numeric object", "maximum", "payment.amount", 10.0, map[string]any{"payment": map[string]any{"amount": map[string]any{"value": 1.0}}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			g := &auth.Grant{Capabilities: auth.Capabilities{"x": {Constraints: map[string]any{"rule": tt.constraint}}}}
			op := &catalog.Operation{ID: "x.send", ArgumentConstraints: []catalog.ArgumentConstraint{{Kind: tt.kind, Path: tt.path, Key: "rule", Required: true}}}
			d, err := EvaluateArguments(context.Background(), g, "x", op, Decision{Allow: true, Args: tt.args}, Env{}, nil)
			if err != nil || d.Allow {
				t.Fatalf("restriction bypassed: %+v %v", d, err)
			}
		})
	}
}

func TestReviewDailyAtomicAcrossIndependentConnections(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "shared.db")
	var stores []*auth.Store
	for range 3 {
		db, err := durable.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { db.Close() })
		stores = append(stores, auth.NewStore(db))
	}
	var wg sync.WaitGroup
	var admitted atomic.Int32
	for i := range 30 {
		wg.Go(func() {
			ok, err := stores[i%len(stores)].ReserveDaily(ctx, "scope", "operation", "2026-11-03", 5)
			if err != nil {
				t.Error(err)
			}
			if ok {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 5 {
		t.Fatalf("admitted %d attempts, want 5", admitted.Load())
	}
	n, err := stores[0].ReserveDaily(ctx, "scope", "operation", "2026-11-03", 5)
	if err != nil || n {
		t.Fatalf("persisted counter did not deny: %v %v", n, err)
	}
}
