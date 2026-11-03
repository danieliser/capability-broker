package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToSpecMcpRemoteWrapper(t *testing.T) {
	e := map[string]any{
		"command": "/opt/homebrew/bin/node",
		"args":    []any{"/x/node_modules/mcp-remote/dist/proxy.js", "https://mcp.example.com/mcp/", "--header", "X-Key:${H0}", "--header", "X-Host:fixed"},
		"env":     map[string]any{"H0": "s3cret"},
	}
	spec, secrets, err := toSpec(context.Background(), "example", e)
	if err != nil {
		t.Fatal(err)
	}
	if spec.URL != "https://mcp.example.com/mcp/" || spec.Command != "" {
		t.Fatalf("want url connector, got %+v", spec)
	}
	if spec.Headers["X-Key"] != "${secret:EXAMPLE_HEADER_X_KEY}" || secrets["EXAMPLE_HEADER_X_KEY"] != "s3cret" {
		t.Fatalf("header secret not moved: %v %v", spec.Headers, secrets)
	}
}

func TestToSpecStdioSecrets(t *testing.T) {
	e := map[string]any{"command": "npx", "args": []any{"-y", "pkg"}, "env": map[string]any{"PKG_PASSWORD": "pw", "PKG_URL": "https://x", "CFG": "/etc/x"}}
	spec, secrets, err := toSpec(context.Background(), "pkg", e)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Env["PKG_PASSWORD"] != "${secret:PKG_PASSWORD}" || secrets["PKG_PASSWORD"] != "pw" {
		t.Fatalf("secret: %v %v", spec.Env, secrets)
	}
	if spec.Env["PKG_URL"] != "https://x" || spec.Env["CFG"] != "/etc/x" {
		t.Fatalf("plain values should stay inline: %v", spec.Env)
	}
}

func TestParkUnparkJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	_ = os.WriteFile(path, []byte(`{"mcpServers":{"a":{"command":"x"},"broker":{"url":"http://127.0.0.1:8890/mcp"}},"other":1}`), 0o600)
	orig, err := parkJSON("mcpServers", false)(path, "a")
	if err != nil || orig == nil {
		t.Fatalf("park: %v %s", err, orig)
	}
	doc, _ := readJSONC(path)
	if _, ok := doc["mcpServers"].(map[string]any)["a"]; ok {
		t.Fatal("a still active")
	}
	if _, ok := doc[backupKey].(map[string]any)["a"]; !ok {
		t.Fatal("a not in backup")
	}
	if again, _ := parkJSON("mcpServers", false)(path, "a"); again != nil {
		t.Fatal("second park should be a no-op")
	}
	if ok, err := unparkJSON("mcpServers", false)(path, "a"); !ok || err != nil {
		t.Fatalf("unpark: %v %v", ok, err)
	}
	doc, _ = readJSONC(path)
	if _, ok := doc["mcpServers"].(map[string]any)["a"]; !ok || doc[backupKey] != nil || doc["other"] != float64(1) {
		t.Fatalf("restore wrong: %v", doc)
	}
	b, _ := json.Marshal(doc)
	if !strings.Contains(string(b), "broker") {
		t.Fatal("broker entry lost")
	}
}

func TestParkCodex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	src := "model = \"x\"\n\n[mcp_servers.a]\ncommand = \"x\"\nenabled = true\n\n[mcp_servers.a.tools.t]\napproval_mode = \"prompt\"\n\n[mcp_servers.b]\ncommand = \"y\"\n"
	_ = os.WriteFile(path, []byte(src), 0o600)
	if r, err := parkCodex(path, "a"); err != nil || r == nil {
		t.Fatalf("park a: %v", err)
	}
	if r, err := parkCodex(path, "b"); err != nil || r == nil {
		t.Fatalf("park b: %v", err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "[mcp_servers.a]\ncommand = \"x\"\nenabled = false") || !strings.Contains(string(got), "[mcp_servers.b]\nenabled = false\ncommand") {
		t.Fatalf("park result:\n%s", got)
	}
	if !strings.Contains(string(got), "approval_mode = \"prompt\"") {
		t.Fatal("tool approvals lost")
	}
	if ok, err := unparkCodex(path, "a"); !ok {
		g, _ := os.ReadFile(path)
		t.Fatalf("unpark a: %v\n%s", err, g)
	}
	got, _ = os.ReadFile(path)
	if !strings.Contains(string(got), "[mcp_servers.a]\ncommand = \"x\"\nenabled = true") {
		t.Fatalf("unpark result:\n%s", got)
	}
}

func TestSetDisabled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "connectors.yaml")
	_ = os.WriteFile(path, []byte("connectors:\n  # keep me\n  a:\n    command: x\n"), 0o600)
	if err := setDisabled(path, "a", true); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "disabled: true") || !strings.Contains(string(got), "# keep me") {
		t.Fatalf("got:\n%s", got)
	}
}

func TestReinsertWhenBackupStripped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "d.json")
	_ = os.WriteFile(path, []byte(`{"mcpServers":{"broker":{"command":"node","args":["p.js","http://127.0.0.1:8890/mcp"]}}}`), 0o600)
	ok, err := reinsertJSON(path, "mcpServers", "a", json.RawMessage(`{"command":"x"}`))
	if !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	doc, _ := readJSONC(path)
	if _, ok := doc["mcpServers"].(map[string]any)["a"]; !ok {
		t.Fatal("not reinserted")
	}
	if !isBrokerEntry(doc["mcpServers"].(map[string]any)["broker"]) {
		t.Fatal("stdio bridge should count as a broker entry")
	}
}

func TestApplyCodexKeepsBrokerToolApprovals(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	src := "[mcp_servers.broker]\nurl = \"http://127.0.0.1:8890/mcp?prefix=none\"\nenabled = true\n\n[mcp_servers.broker.tools.\"cloudflare-api__execute\"]\napproval_mode = \"prompt\"\n"
	_ = os.WriteFile(path, []byte(src), 0o600)
	if _, err := applyCodex(path, []entry{{Name: "broker", URL: "http://127.0.0.1:8890/mcp?prefix=none", Token: "t"}}, nil); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "[mcp_servers.broker.tools.\"cloudflare-api__execute\"]\napproval_mode = \"prompt\"") {
		t.Fatalf("approval lost:\n%s", got)
	}
	if strings.Count(string(got), "[mcp_servers.broker]") != 1 {
		t.Fatalf("broker table duplicated:\n%s", got)
	}
}

func TestReinsertCodexReplacesBrokerEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	_ = os.WriteFile(path, []byte("model = \"x\"\n\n[mcp_servers.mock]\nurl = \"http://127.0.0.1:8890/mcp/mock\"\nenabled = true\n\n[mcp_servers.mock.tools.t]\napproval_mode = \"prompt\"\n"), 0o600)
	ok, err := reinsertCodex(path, "mock", "[mcp_servers.mock]\ncommand = \"x\"\nenabled = false\n")
	if !ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	got, _ := os.ReadFile(path)
	s := string(got)
	if strings.Contains(s, "127.0.0.1") || !strings.Contains(s, "command = \"x\"\nenabled = true") || !strings.Contains(s, "approval_mode = \"prompt\"") || !strings.Contains(s, "model = \"x\"") {
		t.Fatalf("got:\n%s", s)
	}
}
