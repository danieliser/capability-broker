package catalog

import (
	"fmt"
	"regexp"
	"strings"
)

// Flow is an owner-reviewed binding from a workflow-only MCP tool to a deployment.
// Publishing/updating workflows remains outside the agent connector.
type Flow struct {
	ID              string `yaml:"id"`
	EffectClass     string `yaml:"effect_class"`
	GitRevision     string `yaml:"git_revision"`
	WorkflowID      string `yaml:"workflow_id"`
	WorkflowVersion string `yaml:"workflow_version"`
}

var flowID = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,127}$`)
var gitRevision = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)

func validateFlow(connector string, op *Operation) error {
	if connector == "n8n" {
		return fmt.Errorf("n8n instance management is owner-only; its agent catalog must stay empty")
	}
	if op.Flow == nil {
		if connector == "n8n-flows" {
			return fmt.Errorf("n8n-flows operations require a reviewed flow binding")
		}
		return nil
	}
	f := op.Flow
	if connector != "n8n-flows" || op.ID != "flows.run" || op.RequiresCapability != "operations:flows.run" ||
		len(op.Match) != 0 || len(op.Guards) != 0 || len(op.Response) != 0 || len(op.Compact) != 0 {
		return fmt.Errorf("flow tools must bind n8n-flows/flows.run with operations:flows.run and no match/guards/response/compact hooks")
	}
	if !flowID.MatchString(f.ID) || !gitRevision.MatchString(f.GitRevision) ||
		strings.TrimSpace(f.WorkflowID) == "" || strings.TrimSpace(f.WorkflowVersion) == "" {
		return fmt.Errorf("flow binding needs a logical id, full git revision, workflow id and version")
	}
	switch f.EffectClass {
	case "read", "write", "send", "spend":
	default:
		return fmt.Errorf("flow %s has an invalid effect_class", f.ID)
	}
	for _, r := range op.ArgumentConstraints {
		if r.Kind == "max_per_day" && r.Key == "max_per_day" && r.Default != nil && *r.Default == 0 {
			return nil
		}
	}
	return fmt.Errorf("flow %s requires max_per_day with default: 0", f.ID)
}
