package connectors

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolFilter(t *testing.T) {
	f := &ToolFilter{Include: []string{"sofa_*"}, Exclude: []string{"*delete*", "sofa_vote"}}
	for name, want := range map[string]bool{"sofa_search": true, "sofa_delete_post": false, "sofa_vote": false, "other": false} {
		if got := f.Allows(name); got != want {
			t.Errorf("%s: got %v want %v", name, got, want)
		}
	}
	var nilF *ToolFilter
	if !nilF.Allows("x") {
		t.Error("nil filter must allow")
	}
}

func TestForClient(t *testing.T) {
	cases := []struct {
		clients []string
		client  string
		want    bool
	}{
		{nil, "codex", true},
		{[]string{"claude-code"}, "codex", false},
		{[]string{"claude-code", "codex"}, "codex", true},
		{[]string{"!cursor"}, "cursor", false},
		{[]string{"!cursor"}, "codex", true},
	}
	for _, c := range cases {
		if got := (&Spec{Clients: c.clients}).ForClient(c.client); got != c.want {
			t.Errorf("%v %s: got %v", c.clients, c.client, got)
		}
	}
}

func TestLoadAllSkipsBadConnectors(t *testing.T) {
	p := filepath.Join(t.TempDir(), "connectors.yaml")
	_ = os.WriteFile(p, []byte("connectors:\n  good:\n    command: echo\n  nosecret:\n    command: echo\n    env: { K: \"${secret:MISSING}\" }\n  both:\n    command: echo\n    url: https://x\n"), 0o600)
	specs, issues, err := LoadAll(p, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 1 || specs[0].Name != "good" {
		t.Fatalf("specs: %+v", specs)
	}
	if !strings.Contains(issues["nosecret"], "MISSING") || issues["both"] == "" {
		t.Fatalf("issues: %v", issues)
	}
}

func TestExampleConfigLoadsEmpty(t *testing.T) {
	specs, issues, err := LoadAll("../../connectors.example.yaml", map[string]string{})
	if err != nil || len(specs) != 0 || len(issues) != 0 {
		t.Fatalf("example must load clean with no connectors: %v %v %v", specs, issues, err)
	}
}
