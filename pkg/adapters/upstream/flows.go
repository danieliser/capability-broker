package upstream

import (
	"strings"

	"github.com/danieliser/capability-broker/pkg/adapter"
)

// A successful MCP response alone does not prove which workflow executed. Keep
// ambiguous results unknown so callers reconcile rather than retry effects.
func validateFlowResult(call adapter.Call, payload any) error {
	f := call.Op.Flow
	m, ok := payload.(map[string]any)
	if ok {
		ok = m["persist_run_id"] == call.Grant.RunID && m["logical_flow_id"] == f.ID &&
			m["git_revision"] == f.GitRevision && m["n8n_workflow_id"] == f.WorkflowID &&
			m["n8n_workflow_version"] == f.WorkflowVersion
		id, _ := m["n8n_execution_id"].(string)
		ok = ok && strings.TrimSpace(id) != ""
	}
	if !ok {
		return &adapter.UpstreamError{Code: adapter.CodeUpstreamError,
			Message: "flow result lacks matching deployment/correlation or execution id; reconcile before retry",
			Outcome: adapter.OutcomeUnknown}
	}
	return nil
}
