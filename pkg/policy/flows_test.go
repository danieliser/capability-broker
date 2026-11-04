package policy

import (
	"context"
	"reflect"
	"testing"

	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
)

func TestFlowInheritedAuthorityAndCorrelation(t *testing.T) {
	parent := auth.Capabilities{"n8n-flows": {Operations: []string{"flows.run", "flows.send"}, Constraints: map[string]any{"flow_ids": []any{"approved"}}, OperationConstraints: map[string]map[string]any{"flows.run": {"flow_ids": []any{"approved"}}}}}
	child := auth.Capabilities{"n8n-flows": {Operations: []string{"flows.run", "flows.send", "flows.spend"}, Constraints: map[string]any{"flow_ids": []any{"approved", "foreign"}}, OperationConstraints: map[string]map[string]any{"flows.run": {"flow_ids": []any{"approved", "foreign"}}}}}
	caps := child.Intersect(parent)
	op := &catalog.Operation{ID: "flows.run", Tool: "send", RequiresCapability: "operations:flows.run", Flow: &catalog.Flow{ID: "approved", EffectClass: "send", GitRevision: "reviewed"}}
	cap := caps["n8n-flows"]
	if err := FlowAllowed(cap, op); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ id, effect string }{{"foreign", "send"}, {"approved", "spend"}} {
		other := *op
		binding := *op.Flow
		other.Flow = &binding
		other.Flow.ID = tt.id
		other.Flow.EffectClass = tt.effect
		if err := FlowAllowed(cap, &other); err == nil {
			t.Fatalf("child widened %v", tt)
		}
	}
	g := &auth.Grant{Kind: auth.KindAgent, RunID: "trusted-run", Capabilities: caps}
	input := map[string]any{"persist_run_id": "spoof", "logical_flow_id": "spoof", "git_revision": "spoof", "idempotency_key": "fixture-key"}
	before := CloneArgs(input)
	d, err := Evaluate(context.Background(), g, "n8n-flows", op, input, Env{}, nil)
	if err != nil || !d.Allow {
		t.Fatalf("%v %+v", err, d)
	}
	if d.Args["persist_run_id"] != "trusted-run" || d.Args["logical_flow_id"] != "approved" || d.Args["git_revision"] != "reviewed" {
		t.Fatal("caller controlled correlation")
	}
	if !reflect.DeepEqual(input, before) {
		t.Fatal("input mutated")
	}
	delete(input, "idempotency_key")
	d, err = Evaluate(context.Background(), g, "n8n-flows", op, input, Env{}, nil)
	if err != nil || d.Allow {
		t.Fatal("effect ran without idempotency key")
	}
	// Operation constraints cannot widen connector allowlists, even in a root grant.
	cap.OperationConstraints = map[string]map[string]any{"flows.run": {"flow_ids": []any{"foreign"}}}
	if err := FlowAllowed(cap, op); err == nil {
		t.Fatal("operation allowlist replaced connector allowlist")
	}
}
