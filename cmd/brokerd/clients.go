package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/danieliser/capability-broker/pkg/config"
	"github.com/danieliser/capability-broker/pkg/connectors"
)

// A client is an MCP host whose user config the broker can take over: per-connector http entries for the
// connectors it routes today (tool names unchanged), plus one `broker` entry on the aggregate endpoint that
// excludes them, so any connector added to the broker later appears in an open chat without a restart.
type client struct {
	name   string
	path   func(home string) string
	apply  func(path string, entries []entry, revert map[string]json.RawMessage) (backup map[string]json.RawMessage, err error)
	format string
	key    string // JSON server map key
	// park removes a server the broker has adopted, keeping a restorable copy in the file; unpark restores it.
	park   func(path, name string) (json.RawMessage, error)
	unpark func(path, name string) (bool, error)
}

type entry struct {
	Name  string
	URL   string
	Token string
}

var clients = []client{
	{name: "claude-code", format: "json", key: "mcpServers", path: func(h string) string { return filepath.Join(h, ".claude.json") },
		apply: applyJSONServers("mcpServers", claudeEntry), park: parkJSON("mcpServers", false), unpark: unparkJSON("mcpServers", false)},
	{name: "claude-desktop", format: "json", key: "mcpServers", path: func(h string) string {
		return filepath.Join(h, "Library", "Application Support", "Claude", "claude_desktop_config.json")
	}, apply: applyJSONServers("mcpServers", desktopEntry), park: parkJSON("mcpServers", false), unpark: unparkJSON("mcpServers", false)},
	{name: "cursor", format: "json", key: "mcpServers", path: func(h string) string { return filepath.Join(h, ".cursor", "mcp.json") },
		apply: applyJSONServers("mcpServers", cursorEntry), park: parkJSON("mcpServers", false), unpark: unparkJSON("mcpServers", false)},
	{name: "opencode", format: "json", key: "mcp", path: func(h string) string { return filepath.Join(h, ".config", "opencode", "opencode.jsonc") },
		apply: applyJSONServers("mcp", opencodeEntry), park: parkJSON("mcp", true), unpark: unparkJSON("mcp", true)},
	{name: "codex", format: "toml", path: func(h string) string { return filepath.Join(h, ".codex", "config.toml") }, apply: applyCodex,
		park: parkCodex, unpark: unparkCodex},
}

// desktopEntry: Claude Desktop's config takes stdio servers only, so it reaches the broker through mcp-remote.
// The token travels in the child's env, not its argv.
func desktopEntry(e entry) any {
	home, _ := os.UserHomeDir()
	node := "/opt/homebrew/bin/node"
	for _, p := range []string{"/opt/homebrew/bin/node", "/usr/local/bin/node"} {
		if _, err := os.Stat(p); err == nil {
			node = p
			break
		}
	}
	proxy := filepath.Join(home, ".local", "share", "mcp-clients", "node_modules", "mcp-remote", "dist", "proxy.js")
	return map[string]any{"command": node, "args": []any{proxy, e.URL, "--transport", "http-only", "--header", "Authorization:${BROKER_AUTHORIZATION}"},
		"env": map[string]any{"BROKER_AUTHORIZATION": "Bearer " + e.Token}}
}

func claudeEntry(e entry) any {
	return map[string]any{"type": "http", "url": e.URL, "headers": map[string]any{"Authorization": "Bearer " + e.Token}}
}
func cursorEntry(e entry) any {
	return map[string]any{"url": e.URL, "headers": map[string]any{"Authorization": "Bearer " + e.Token}}
}
func opencodeEntry(e entry) any {
	return map[string]any{"type": "remote", "url": e.URL, "enabled": true, "headers": map[string]any{"Authorization": "Bearer " + e.Token}}
}

// runClientInstall: brokerd client install <claude-code|codex|cursor|opencode|all> [--connectors a,b] [--revert]
func runClientInstall(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("client install", flag.ContinueOnError)
	only := fs.String("connectors", "", "connectors to route as their own servers (default: every connector the broker runs)")
	revert := fs.Bool("revert", false, "restore the entries the broker replaced and remove its own")
	mode := fs.String("mode", "single", "single: one broker entry carrying every connector; servers: one entry per connector plus broker for later ones")
	prefix := fs.String("prefix", "none", "single mode tool names: none (upstream names, prefixed only on a clash) or connector (<connector>__<tool>)")
	target := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil || target == "" || (*mode != "single" && *mode != "servers") || (*prefix != "connector" && *prefix != "none") {
		return fail("usage: brokerd client install <claude-code|claude-desktop|codex|cursor|opencode|all> [--connectors a,b] [--revert]")
	}
	cfg := config.FromEnv()
	home, _ := os.UserHomeDir()
	var selected []client
	for _, c := range clients {
		if target == "all" || target == c.name {
			selected = append(selected, c)
		}
	}
	if len(selected) == 0 {
		return fail("unknown client %q", target)
	}
	var connectors []string
	if *only != "" {
		for _, c := range strings.Split(*only, ",") {
			if c = strings.TrimSpace(c); c != "" {
				connectors = append(connectors, c)
			}
		}
	} else if !*revert {
		ac, _, err := adminClient()
		if err != nil {
			return fail("%v", err)
		}
		var st struct {
			Connectors map[string]string `json:"connectors"`
		}
		if err := ac.Do(ctx, "GET", "/v1/status", nil, &st); err != nil {
			return fail("%v", err)
		}
		for name := range st.Connectors {
			connectors = append(connectors, name)
		}
	}
	sort.Strings(connectors)
	failed := 0
	for _, c := range selected {
		path := c.path(home)
		if _, err := os.Stat(path); err != nil {
			fmt.Printf("%-12s skipped: %s not found\n", c.name, path)
			continue
		}
		backupPath := filepath.Join(cfg.DataDir, "clients", c.name+".backup.json")
		saved := map[string]json.RawMessage{}
		if raw, err := os.ReadFile(backupPath); err == nil {
			_ = json.Unmarshal(raw, &saved)
		}
		var entries []entry
		if !*revert {
			token, err := clientToken(ctx, cfg, c.name)
			if err != nil {
				fmt.Printf("%-12s failed: %v\n", c.name, err)
				failed++
				continue
			}
			entries = buildEntries(cfg, c.name, connectors, token, *mode, *prefix)
		}
		orig, err := os.ReadFile(path)
		if err != nil {
			fmt.Printf("%-12s failed: %v\n", c.name, err)
			failed++
			continue
		}
		var revertMap map[string]json.RawMessage
		if *revert {
			revertMap = saved
		}
		backup, err := c.apply(path, entries, revertMap)
		if err != nil {
			fmt.Printf("%-12s failed: %v\n", c.name, err)
			failed++
			continue
		}
		stamp := time.Now().Format("20060102T150405")
		_ = os.WriteFile(path+".pre-broker-"+stamp, orig, 0o600)
		managed := filepath.Join(cfg.DataDir, "clients", c.name+".managed")
		if *revert {
			_ = os.Remove(backupPath)
			_ = os.Remove(managed)
			fmt.Printf("%-12s restored (%d entries) in %s\n", c.name, len(saved), path)
			continue
		}
		for k, v := range backup {
			if _, have := saved[k]; !have { // keep the oldest original across repeated installs
				saved[k] = v
			}
		}
		raw, _ := json.MarshalIndent(saved, "", "  ")
		_ = os.MkdirAll(filepath.Dir(backupPath), 0o700)
		_ = os.WriteFile(backupPath, raw, 0o600)
		// Managed: the daemon keeps this client's entries in step with the connectors it runs.
		_ = os.WriteFile(managed, []byte(*mode+" "+*prefix+"\n"), 0o600)
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name)
		}
		fmt.Printf("%-12s routed through the broker: %s (file backup %s.pre-broker-%s)\n", c.name, strings.Join(names, ", "), path, stamp)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// clientToken returns the client's owner token, minting one the first time.
func clientToken(ctx context.Context, cfg config.Config, name string) (string, error) {
	path := filepath.Join(cfg.DataDir, "clients", name+".token")
	if raw, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(raw)) != "" {
		return strings.TrimSpace(string(raw)), nil
	}
	ac, _, err := adminClient()
	if err != nil {
		return "", err
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := ac.Do(ctx, "POST", "/v1/grants", map[string]any{"kind": "client", "name": name, "owner": true, "issued_by": "brokerd client install"}, &out); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	return out.Token, os.WriteFile(path, []byte(out.Token+"\n"), 0o600)
}

// applyJSONServers edits a JSON config's server map (`mcpServers` or OpenCode's `mcp`). Comment lines are
// stripped (the file is rewritten as JSON). Returns the entries it replaced.
func applyJSONServers(key string, render func(entry) any) func(string, []entry, map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	return func(path string, entries []entry, revert map[string]json.RawMessage) (map[string]json.RawMessage, error) {
		before, _ := os.Stat(path)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		text := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(string(raw), "")
		var doc map[string]any
		if err := json.Unmarshal([]byte(text), &doc); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		delete(doc, "mcpServersBrokerBackup") // left by the 0.1.0 installer
		servers, _ := doc[key].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		backup := map[string]json.RawMessage{}
		if revert != nil {
			for name, cur := range servers {
				if isBrokerEntry(cur) {
					delete(servers, name)
				}
			}
			for name, orig := range revert {
				if string(orig) == "null" {
					continue
				}
				var v any
				_ = json.Unmarshal(orig, &v)
				servers[name] = v
			}
		} else {
			want := map[string]bool{}
			for _, e := range entries {
				want[e.Name] = true
			}
			for name, cur := range servers { // a connector the broker no longer runs
				if isBrokerEntry(cur) && !want[name] {
					delete(servers, name)
				}
			}
			for _, e := range entries {
				if cur, ok := servers[e.Name]; ok && !isBrokerEntry(cur) {
					b, _ := json.Marshal(cur)
					backup[e.Name] = b
				} else if !ok {
					backup[e.Name] = json.RawMessage("null")
				}
				servers[e.Name] = render(e)
			}
		}
		doc[key] = servers
		out, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return nil, err
		}
		var orig map[string]any
		_ = json.Unmarshal([]byte(text), &orig)
		if a, b := mustJSON(orig), mustJSON(doc); a == b {
			return backup, nil // nothing to change: do not touch a file the client itself writes to
		}
		// Clients write their own config (Claude Code keeps session state in ~/.claude.json). If the file moved
		// under us, start over rather than overwrite their change.
		if after, err := os.Stat(path); err == nil && before != nil && !after.ModTime().Equal(before.ModTime()) {
			return nil, fmt.Errorf("%s changed while being edited; try again", path)
		}
		return backup, atomicWrite(path, out)
	}
}

func isBrokerEntry(v any) bool {
	m, _ := v.(map[string]any)
	u, _ := m["url"].(string)
	if u == "" { // stdio bridge (Claude Desktop): the broker URL is an argument
		for _, a := range stringSlice(m["args"]) {
			if strings.HasPrefix(a, "http://127.0.0.1:") {
				u = a
			}
		}
	}
	return strings.HasPrefix(u, "http://127.0.0.1:") && strings.Contains(u, "/mcp")
}

// applyCodex rewrites [mcp_servers.<name>] tables (and their sub-tables) textually, so the rest of
// config.toml, comments included, is left byte-for-byte.
func applyCodex(path string, entries []entry, revert map[string]json.RawMessage) (map[string]json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blocks := splitTOML(string(raw))
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name] = true
	}
	if revert != nil {
		for name := range revert {
			names[name] = true
		}
		names["broker"] = true
	}
	backup := map[string]json.RawMessage{}
	var kept []string
	removed := map[string]string{}
	for _, b := range blocks {
		if name := codexServer(b.header); name != "" && revert == nil && !names[name] && strings.Contains(b.text, "url = \"http://127.0.0.1:") && strings.Contains(b.text, "/mcp") {
			continue // a connector the broker no longer runs
		}
		if name := codexServer(b.header); name != "" && names[name] {
			// Per-tool settings under a broker entry ([mcp_servers.broker.tools."x"] approval_mode) belong to
			// the owner, not the installer: keep them across syncs unless the broker entry itself is reverted.
			if revert == nil && b.header != "[mcp_servers."+name+"]" && strings.HasPrefix(b.header, "[mcp_servers."+name+".tools.") {
				kept = append(kept, b.text)
				continue
			}
			removed[name] += b.text
			continue
		}
		kept = append(kept, b.text)
	}
	var add strings.Builder
	if revert != nil {
		for name, orig := range revert {
			var text string
			if json.Unmarshal(orig, &text) == nil && text != "" {
				add.WriteString(text)
			}
			_ = name
		}
	} else {
		for _, e := range entries {
			if old, ok := removed[e.Name]; ok && !strings.Contains(old, "url = \"http://127.0.0.1:") {
				b, _ := json.Marshal(old)
				backup[e.Name] = b
			} else if !ok {
				backup[e.Name] = json.RawMessage(`""`)
			}
			fmt.Fprintf(&add, "[mcp_servers.%s]\nurl = %q\nhttp_headers = { Authorization = %q }\nenabled = true\n\n", e.Name, e.URL, "Bearer "+e.Token)
		}
	}
	out := strings.TrimRight(strings.Join(kept, ""), "\n") + "\n\n" + add.String()
	if strings.TrimSpace(out) == strings.TrimSpace(string(raw)) {
		return backup, nil
	}
	return backup, atomicWrite(path, []byte(out))
}

type tomlBlock struct{ header, text string }

// splitTOML cuts a file at table headers; the first block (before any header) has an empty header.
func splitTOML(s string) []tomlBlock {
	var out []tomlBlock
	cur := tomlBlock{}
	for _, line := range strings.SplitAfter(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && !strings.HasPrefix(t, "[[") || strings.HasPrefix(t, "[[") {
			out = append(out, cur)
			cur = tomlBlock{header: t}
		}
		cur.text += line
	}
	return append(out, cur)
}

var codexHeader = regexp.MustCompile(`^\[\[?mcp_servers\.("?)([A-Za-z0-9_-]+)("?)(\..*)?\]\]?$`)

func codexServer(header string) string {
	m := codexHeader.FindStringSubmatch(header)
	if m == nil {
		return ""
	}
	return m[2]
}

func atomicWrite(path string, data []byte) error {
	info, err := os.Stat(path)
	mode := os.FileMode(0o600)
	if err == nil {
		mode = info.Mode().Perm()
	}
	tmp := path + ".broker-tmp"
	if err := os.WriteFile(tmp, data, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// syncManagedClients is called by the daemon when the set of connectors changes. Every client installed with
// `brokerd client install` gets one entry per connector (tool names exactly as the upstream server's), and its
// broker entry excludes them all. A chat already open keeps reaching a new connector through `broker`; the next
// chat sees it as its own server.
func syncManagedClients(cfg config.Config, connectors []string, log func(msg string, kv ...any)) {
	home, _ := os.UserHomeDir()
	sort.Strings(connectors)
	for _, c := range clients {
		dir := filepath.Join(cfg.DataDir, "clients")
		marker, err := os.ReadFile(filepath.Join(dir, c.name+".managed"))
		if err != nil {
			continue
		}
		mode, prefix := "servers", "connector" // markers written before modes existed
		if f := strings.Fields(string(marker)); len(f) == 2 {
			mode, prefix = f[0], f[1]
		}
		raw, err := os.ReadFile(filepath.Join(dir, c.name+".token"))
		if err != nil {
			continue
		}
		token := strings.TrimSpace(string(raw))
		path := c.path(home)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		entries := buildEntries(cfg, c.name, connectors, token, mode, prefix)
		var backup map[string]json.RawMessage
		for attempt := 0; attempt < 3; attempt++ {
			if backup, err = c.apply(path, entries, nil); err == nil {
				break
			}
			time.Sleep(200 * time.Millisecond)
		}
		if err != nil {
			log("client sync failed", "client", c.name, "err", err.Error())
			continue
		}
		backupPath := filepath.Join(dir, c.name+".backup.json")
		saved := map[string]json.RawMessage{}
		if b, err := os.ReadFile(backupPath); err == nil {
			_ = json.Unmarshal(b, &saved)
		}
		for k, v := range backup {
			if _, have := saved[k]; !have {
				saved[k] = v
			}
		}
		if b, err := json.MarshalIndent(saved, "", "  "); err == nil {
			_ = os.WriteFile(backupPath, b, 0o600)
		}
		log("client synced", "client", c.name, "connectors", connectors)
	}
}

// buildEntries renders a client's broker entries. single: one `broker` entry on the aggregate endpoint, so every
// connector, including ones added later, arrives through it with tool names passed through. servers: one entry per
// connector (the client's own server names) plus `broker` excluding them.
// specsFor reads connectors.yaml for the client-facing fields only (no secrets are resolved).
func specsFor(cfg config.Config) map[string]*connectors.Spec {
	out := map[string]*connectors.Spec{}
	raw, err := os.ReadFile(cfg.ConnectorsPath())
	if err != nil {
		return out
	}
	var f connectors.File
	if yaml.Unmarshal(raw, &f) == nil {
		for k, v := range f.Connectors {
			if v != nil {
				out[k] = v
			}
		}
	}
	return out
}

func buildEntries(cfg config.Config, client string, all []string, token, mode, prefix string) []entry {
	// Connectors this client does not get are dropped; on-demand (expose: broker) ones get no entry of their
	// own and stay reachable through the broker's discovery tools.
	specs := specsFor(cfg)
	var connectors []string
	for _, n := range all {
		if sp := specs[n]; sp != nil {
			if !sp.ForClient(client) {
				continue
			}
			if sp.OnDemand() && mode != "single" {
				continue
			}
		}
		connectors = append(connectors, n)
	}
	base := "http://" + cfg.FrontAddr()
	if mode == "single" {
		url := base + "/mcp"
		if prefix == "none" {
			url += "?prefix=none"
		}
		return []entry{{Name: "broker", URL: url, Token: token}}
	}
	// A server adopted under a different connector name (openaiDeveloperDocs → openai-docs) keeps the name the
	// client knew it by, so tool names and every rule keyed to them stay as they were.
	alias := map[string]string{}
	for orig, rec := range loadAdopted(cfg) {
		if rec != nil && rec.Connector != "" && rec.Connector != orig {
			alias[rec.Connector] = orig
		}
	}
	var entries []entry
	for _, name := range connectors {
		clientName := name
		if a, ok := alias[name]; ok {
			clientName = a
		}
		entries = append(entries, entry{Name: clientName, URL: base + "/mcp/" + name, Token: token})
	}
	return append(entries, entry{Name: "broker", URL: base + "/mcp?exclude=" + strings.Join(connectors, ","), Token: token})
}
