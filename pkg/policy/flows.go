package policy

import (
	"fmt"
	"strings"

	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/catalog"
)

// FlowAllowed also filters discovery. Every inherited allowlist must admit the
// catalog's logical ID; a grant cannot downgrade a tool's reviewed effect class.
func FlowAllowed(cap auth.ConnectorCapability, op *catalog.Operation) error {
	if op.Flow == nil {
		return nil
	}
	if !contains(cap.Operations, "flows.run") {
		return fmt.Errorf("grant needs flows.run")
	}
	values := constraintValues(cap, op.ID, "flow_ids")
	if len(values) == 0 {
		return fmt.Errorf("grant must configure flow_ids")
	}
	for _, v := range values {
		ids, ok := strictStrings(v)
		if !ok || !contains(ids, op.Flow.ID) {
			return fmt.Errorf("logical flow is outside flow_ids")
		}
	}
	if effect := op.Flow.EffectClass; effect != "read" && !contains(cap.Operations, "flows."+effect) {
		return fmt.Errorf("grant needs flows.%s for this flow", effect)
	}
	return nil
}

func checkFlow(g *auth.Grant, cap auth.ConnectorCapability, op *catalog.Operation, d *Decision) *Decision {
	if op.Flow == nil {
		return nil
	}
	if err := FlowAllowed(cap, op); err != nil {
		dd := deny(CodeDeniedOperation, err.Error())
		return &dd
	}
	if strings.TrimSpace(g.RunID) == "" {
		dd := deny(CodeDeniedArgument, "flow runs require a PERSIST run_id in the grant")
		return &dd
	}
	if key, _ := d.Args["idempotency_key"].(string); op.Flow.EffectClass != "read" && strings.TrimSpace(key) == "" {
		dd := deny(CodeDeniedArgument, "effectful flow runs require idempotency_key")
		return &dd
	}
	// Reserved correlation fields always come from the grant and reviewed catalog.
	d.Args["persist_run_id"] = g.RunID
	d.Args["logical_flow_id"] = op.Flow.ID
	d.Args["git_revision"] = op.Flow.GitRevision
	return nil
}
