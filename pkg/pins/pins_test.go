package pins

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/capability-broker/pkg/connectors"
)

func TestDetect(t *testing.T) {
	cases := []struct {
		cmd                 string
		args                []string
		kind, pkg, ver, bin string
		rest                int
		ok                  bool
	}{
		{"/x/npx", []string{"-y", "wp-mcp-router@latest"}, "npm", "wp-mcp-router", "latest", "", 0, true},
		{"npx", []string{"-y", "@scope/srv@1.2.3", "--flag"}, "npm", "@scope/srv", "1.2.3", "", 1, true},
		{"npx", []string{"-p", "a", "b"}, "", "", "", "", 0, false},
		{"/x/uvx", []string{"--from", "mcp-clickhouse", "mcp-clickhouse"}, "uv", "mcp-clickhouse", "", "mcp-clickhouse", 0, true},
		{"uvx", []string{"mcp-server-time", "--local-timezone", "UTC"}, "uv", "mcp-server-time", "", "mcp-server-time", 2, true},
		{"uvx", []string{"pkg==0.4.1"}, "uv", "pkg", "0.4.1", "pkg", 0, true},
		{"/usr/bin/node", []string{"x.js"}, "", "", "", "", 0, false},
	}
	for _, c := range cases {
		got, ok := detect(&connectors.Spec{Command: c.cmd, Args: c.args})
		if ok != c.ok || (ok && (got.kind != c.kind || got.pkg != c.pkg || got.version != c.ver || got.bin != c.bin || len(got.rest) != c.rest)) {
			t.Errorf("%s %v → %+v ok=%v", c.cmd, c.args, got, ok)
		}
	}
}

func TestApplySwapsOnlyMatchingValidPins(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "srv")
	_ = os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o700)
	m := &Manager{DataDir: dir}
	spec := &connectors.Spec{Name: "a", Command: "npx", Args: []string{"-y", "srv@latest"}}
	f := File{Pins: map[string]*Pin{"a": {Key: keyFor(spec), Command: bin, Args: []string{"--x"}, Package: "srv", Version: "1.0.0", CheckedAt: time.Now()}}}
	raw, _ := json.Marshal(f)
	_ = os.WriteFile(filepath.Join(dir, Filename), raw, 0o600)

	specs := []*connectors.Spec{spec}
	m.Apply(specs)
	if spec.Command != bin || spec.PinnedVersion != "srv@1.0.0" || spec.Args[0] != "--x" {
		t.Fatalf("pin not applied: %+v", spec)
	}
	edited := &connectors.Spec{Name: "a", Command: "npx", Args: []string{"-y", "srv@2.0.0"}}
	m.Apply([]*connectors.Spec{edited})
	if edited.Command != "npx" {
		t.Fatal("a pin for a different command line was applied")
	}
	off := &connectors.Spec{Name: "a", Command: "npx", Args: []string{"-y", "srv@latest"}, NoPin: true}
	m.Apply([]*connectors.Spec{off})
	if off.Command != "npx" {
		t.Fatal("pin: false was ignored")
	}
	_ = os.Remove(bin)
	gone := &connectors.Spec{Name: "a", Command: "npx", Args: []string{"-y", "srv@latest"}}
	m.Apply([]*connectors.Spec{gone})
	if gone.Command != "npx" {
		t.Fatal("pin with a missing install was applied")
	}
}
