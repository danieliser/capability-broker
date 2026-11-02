package connectors

import "testing"

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
