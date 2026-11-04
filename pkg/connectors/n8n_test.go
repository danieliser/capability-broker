package connectors

import (
	"path/filepath"
	"testing"
)

func TestN8nOwnerExampleResolvesOnlyBrokerSecret(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "examples", "n8n-connectors.yaml")
	specs, issues, err := LoadAll(path, map[string]string{"N8N_MCP_TOKEN": "fixture-only-token"})
	if err != nil || len(issues) != 0 || len(specs) != 1 {
		t.Fatalf("example: %v %v %v", specs, issues, err)
	}
	s := specs[0]
	if s.Name != "n8n" || s.URL != "http://127.0.0.1:5678/mcp-server/http" || !s.OwnerOnly || s.Expose != "passthrough" || s.Headers["Authorization"] != "Bearer fixture-only-token" || s.Tools != nil || len(s.Strip) != 0 || s.Response != nil {
		t.Fatal("owner example violates verbatim/secret contract")
	}
	if !s.ForClient("claude-code") || !s.ForClient("codex") {
		t.Fatal("owner clients missing")
	}
	specs, issues, err = LoadAll(path, nil)
	if err != nil || len(specs) != 0 || issues["n8n"] == "" {
		t.Fatal("missing secret did not fail closed")
	}
}
