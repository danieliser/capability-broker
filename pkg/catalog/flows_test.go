package catalog

import (
	"strings"
	"testing"
)

const validFlowCatalog = `connector: n8n-flows
version: 1
adapter_version: "1.0"
operations:
  - id: flows.run
    tool: fixture_read
    risk: read
    requires_capability: operations:flows.run
    argument_constraints:
      - {kind: max_per_day, key: max_per_day, default: 0}
    flow:
      id: fixture.read
      effect_class: read
      git_revision: 0123456789abcdef0123456789abcdef01234567
      workflow_id: fixture-workflow
      workflow_version: fixture-version
`

func TestFlowCatalogFailsClosed(t *testing.T) {
	for _, tt := range []struct{ name, old, new string }{
		{"missing binding", "    flow:", "    unrecognized_flow:"},
		{"missing quota", "      - {kind: max_per_day, key: max_per_day, default: 0}", "      - {kind: max_per_day, key: max_per_day}"},
		{"nonzero default", "default: 0", "default: 20"},
		{"unknown effect", "effect_class: read", "effect_class: admin"},
		{"short revision", "0123456789abcdef0123456789abcdef01234567", "main"},
		{"missing workflow version", "workflow_version: fixture-version", "workflow_version: ''"},
		{"foreign connector", "connector: n8n-flows", "connector: other"},
		{"instance operation", "connector: n8n-flows", "connector: n8n"},
		{"missing capability", "requires_capability: operations:flows.run", "requires_capability: ''"},
		{"wrong policy key", "id: flows.run", "id: flows.create"},
		{"guard rewrite", "    risk: read", "    risk: read\n    guards: [rewrite]"},
		{"response rewrite", "    risk: read", "    risk: read\n    response: [namespace_filter]"},
		{"lost correlation", "    risk: read", "    risk: read\n    compact: {fields: [data]}"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := strings.Replace(validFlowCatalog, tt.old, tt.new, 1)
			if _, err := parseConnector([]byte(raw), "test"); err == nil {
				t.Fatal("unsafe flow catalog accepted")
			}
		})
	}
	// Same operation/budget, separate logical workflows; duplicate bindings still fail.
	second := strings.SplitN(validFlowCatalog, "operations:\n", 2)[1]
	second = strings.ReplaceAll(second, "fixture_read", "fixture_send")
	second = strings.ReplaceAll(second, "fixture.read", "fixture.send")
	second = strings.ReplaceAll(second, "effect_class: read", "effect_class: send")
	if _, err := parseConnector([]byte(validFlowCatalog+second), "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := parseConnector([]byte(validFlowCatalog+second+second), "test"); err == nil {
		t.Fatal("duplicate flow accepted")
	}
	cat, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"n8n", "n8n-flows"} {
		conn := cat.Ensure(name, []string{"create_workflow", "update_workflow", "delete_workflow", "unreviewed_tool"})
		if len(conn.Operations) != 0 || conn.Source == "synthesized" {
			t.Fatalf("%s synthesized agent operations", name)
		}
	}
}
