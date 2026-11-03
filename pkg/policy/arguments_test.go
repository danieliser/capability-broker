package policy

import (
	"context"
	"math"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/durable"
)

func TestDeclarativeArgumentKinds(t *testing.T) {
	tests := []struct {
		name, kind, path, key string
		value                 any
		args                  map[string]any
		allowed               bool
	}{
		{"to allowed", "allowlist", "to", "to", []any{"ops@example.test"}, map[string]any{"to": "ops@example.test"}, true},
		{"all recipients checked", "allowlist", "recipient", "recipients", []string{"ops@example.test"}, map[string]any{"recipient": []any{"ops@example.test", "other@example.test"}}, false},
		{"comma not parsed", "allowlist", "to", "to", []string{"ops@example.test"}, map[string]any{"to": "ops@example.test, other@example.test"}, false},
		{"case exact", "allowlist", "to", "to", []string{"ops@example.test"}, map[string]any{"to": "OPS@example.test"}, false},
		{"empty recipients", "allowlist", "to", "to", []string{"ops@example.test"}, map[string]any{"to": []any{}}, false},
		{"mixed recipients", "allowlist", "to", "to", []string{"ops@example.test"}, map[string]any{"to": []any{"ops@example.test", 1}}, false},
		{"invalid allowlist", "allowlist", "to", "to", "ops@example.test", map[string]any{"to": "ops@example.test"}, false},
		{"prefix allowed", "prefix", "message.subject", "subject_prefix", "[Fleet] ", map[string]any{"message": map[string]any{"subject": "[Fleet] summary"}}, true},
		{"prefix denied", "prefix", "subject", "subject_prefix", "[Fleet] ", map[string]any{"subject": "other [Fleet] summary"}, false},
		{"empty prefix", "prefix", "subject", "subject_prefix", "", map[string]any{"subject": "anything"}, false},
		{"amount equal", "maximum", "amount", "max_amount", 25.0, map[string]any{"amount": 25.0}, true},
		{"amount above", "maximum", "amount", "max_amount", 25.0, map[string]any{"amount": 25.01}, false},
		{"amount negative", "maximum", "amount", "max_amount", 25.0, map[string]any{"amount": -1.0}, false},
		{"amount string", "maximum", "amount", "max_amount", 25.0, map[string]any{"amount": "1"}, false},
		{"amount nan", "maximum", "amount", "max_amount", 25.0, map[string]any{"amount": math.NaN()}, false},
		{"amount inf", "maximum", "amount", "max_amount", 25.0, map[string]any{"amount": math.Inf(1)}, false},
		{"ceiling invalid", "maximum", "amount", "max_amount", -1.0, map[string]any{"amount": 0}, false},
		{"zero ceiling", "maximum", "amount", "max_amount", 0.0, map[string]any{"amount": 0}, true},
		{"required absent", "maximum", "amount", "max_amount", 25.0, map[string]any{}, false},
		{"row equal", "row_limit", "limit", "max_rows", 2.0, map[string]any{"limit": 2.0}, true},
		{"row above", "row_limit", "limit", "max_rows", 2.0, map[string]any{"limit": 3.0}, false},
		{"row fractional", "row_limit", "limit", "max_rows", 2.0, map[string]any{"limit": 1.5}, false},
		{"row cap zero", "row_limit", "limit", "max_rows", 0.0, map[string]any{}, false},
		{"row zero", "row_limit", "limit", "max_rows", 2.0, map[string]any{"limit": 0.0}, false},
		{"row ceiling fractional", "row_limit", "limit", "max_rows", 2.5, map[string]any{"limit": 1.0}, false},
		{"parent null", "prefix", "message.subject", "subject_prefix", "[Fleet]", map[string]any{"message": nil}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &auth.Grant{Capabilities: auth.Capabilities{"example": {Constraints: map[string]any{tt.key: tt.value}}}}
			op := &catalog.Operation{ID: "example.send", ArgumentConstraints: []catalog.ArgumentConstraint{{Kind: tt.kind, Path: tt.path, Key: tt.key, Required: true}}}
			d, err := EvaluateArguments(context.Background(), g, "example", op, Decision{Allow: true, Args: tt.args}, Env{}, nil)
			if err != nil || d.Allow != tt.allowed {
				t.Fatalf("allow=%v want=%v err=%v decision=%+v", d.Allow, tt.allowed, err, d)
			}
		})
	}
}

func TestOperationConstraintsOnlyNarrowAndArgsAreCopied(t *testing.T) {
	g := &auth.Grant{Capabilities: auth.Capabilities{"x": {
		Constraints:          map[string]any{"to": []any{"a", "b"}, "subject_prefix": "[Fleet]"},
		Limits:               map[string]float64{"max_rows": 3},
		OperationConstraints: map[string]map[string]any{"x.send": {"to": []any{"b", "c"}, "max_rows": 10}},
	}}}
	op := &catalog.Operation{ID: "x.send", ArgumentConstraints: []catalog.ArgumentConstraint{
		{Kind: "allowlist", Path: "to", Key: "to", Required: true},
		{Kind: "row_limit", Path: "options.limit", Key: "max_rows", Required: true},
	}}
	in := map[string]any{"to": "b", "options": map[string]any{}}
	d, err := EvaluateArguments(context.Background(), g, "x", op, Decision{Allow: true, Args: in}, Env{}, nil)
	if err != nil || !d.Allow || d.Args["options"].(map[string]any)["limit"] != 3.0 {
		t.Fatalf("%+v %v", d, err)
	}
	if len(in["options"].(map[string]any)) != 0 {
		t.Fatal("mutated input")
	}
	for _, recipient := range []string{"a", "c"} {
		in["to"] = recipient
		d, _ = EvaluateArguments(context.Background(), g, "x", op, Decision{Allow: true, Args: in}, Env{}, nil)
		if d.Allow {
			t.Fatal("operation or connector restriction bypassed")
		}
	}
	owner := &auth.Grant{Capabilities: auth.OwnerCapabilities()}
	d, err = EvaluateArguments(context.Background(), owner, "x", op, Decision{Allow: true, Args: in}, Env{}, nil)
	if err != nil || !d.Allow {
		t.Fatal("owner changed", err)
	}
	g.Capabilities["x"] = auth.ConnectorCapability{}
	d, _ = EvaluateArguments(context.Background(), g, "x", op, Decision{Allow: true, Args: in}, Env{}, nil)
	if d.Allow {
		t.Fatal("required restriction can be omitted")
	}
}

func TestDailyBudgetPersistsAcrossGrantsRestartAndUTC(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daily.db")
	db, err := durable.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db)
	g := &auth.Grant{ID: "run1", Kind: auth.KindAgent, AgentID: "steward", ProjectKey: "p", Capabilities: auth.Capabilities{"x": {Limits: map[string]float64{"max_per_day": 1}}}}
	op := &catalog.Operation{ID: "x.send", ArgumentConstraints: []catalog.ArgumentConstraint{{Kind: "max_per_day", Key: "max_per_day"}}}
	evaluate := func(now time.Time) Decision {
		d, err := EvaluateArguments(context.Background(), g, "x", op, Decision{Allow: true}, Env{Now: now}, store)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	now := time.Date(2026, 11, 3, 23, 59, 59, 0, time.UTC)
	if !evaluate(now).Allow {
		t.Fatal("first denied")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = durable.Open(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store = auth.NewStore(db)
	g.ID = "run2"
	if d := evaluate(now.In(time.FixedZone("local", -5*3600))); d.Allow || d.Code != CodeLimitExceeded {
		t.Fatalf("rotation/reset: %+v", d)
	}
	if !evaluate(now.Add(time.Second)).Allow {
		t.Fatal("new UTC day denied")
	}
	g.AgentID = "other"
	if !evaluate(now).Allow {
		t.Fatal("unrelated principal consumed budget")
	}
}

func TestDailyBudgetConcurrentAdmissionAndInvalidLimits(t *testing.T) {
	f := setup(t)
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			ok, err := f.store.ReserveDaily(context.Background(), "scope", "operation", "2026-11-03", 3)
			if err != nil {
				t.Error(err)
			}
			if ok {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 3 {
		t.Fatalf("admitted %d", admitted.Load())
	}
	op := &catalog.Operation{ID: "x", ArgumentConstraints: []catalog.ArgumentConstraint{{Kind: "max_per_day", Key: "max_per_day"}}}
	for _, limit := range []any{-1.0, 1.5, "1", math.Inf(1), nil} {
		g := &auth.Grant{Capabilities: auth.Capabilities{"x": {Constraints: map[string]any{"max_per_day": limit}}}}
		d, err := EvaluateArguments(context.Background(), g, "x", op, Decision{Allow: true}, Env{}, f.store)
		if err != nil || d.Allow {
			t.Fatalf("invalid limit %v: %+v %v", limit, d, err)
		}
	}
}

func TestResultRowsAndDefaults(t *testing.T) {
	for _, payload := range []any{[]any{1, 2}, map[string]any{"results": []any{1, 2}}, map[string]any{"result": map[string]any{"rows": []any{1, 2}}}} {
		if CheckResultRows(payload, map[string]any{"max_rows": 1.0}) == nil {
			t.Fatalf("oversized payload accepted: %v", payload)
		}
	}
	g := &auth.Grant{Capabilities: auth.Capabilities{"x": {}}}
	n := 50.0
	op := &catalog.Operation{ID: "x", ArgumentConstraints: []catalog.ArgumentConstraint{{Kind: "row_limit", Path: "limit", Key: "max_rows", Default: &n}}}
	d, err := EvaluateArguments(context.Background(), g, "x", op, Decision{Allow: true}, Env{}, nil)
	if err != nil || !d.Allow || d.Args["limit"] != 50.0 {
		t.Fatalf("default: %+v %v", d, err)
	}
}
