package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
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

// Adopting moves MCP servers out of every client config and into the broker. The clients keep a restorable
// copy in place (a `mcpServersBackup` map in JSON configs, `enabled = false` in Codex and OpenCode), and the
// broker keeps the original entries in <data-dir>/clients/adopted.json, so `brokerd client restore <name>`
// puts a server back in seconds.

const backupKey = "mcpServersBackup"

// adoptRecord is one adopted server: where it came from and the raw entries removed from each client.
type adoptRecord struct {
	Connector string                     `json:"connector"`
	Source    string                     `json:"source"`
	AdoptedAt time.Time                  `json:"adopted_at"`
	Entries   map[string]json.RawMessage `json:"entries"` // client → original entry (Codex: TOML text as a JSON string)
}

func adoptedPath(cfg config.Config) string {
	return filepath.Join(cfg.DataDir, "clients", "adopted.json")
}

func loadAdopted(cfg config.Config) map[string]*adoptRecord {
	out := map[string]*adoptRecord{}
	if raw, err := os.ReadFile(adoptedPath(cfg)); err == nil {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func saveAdopted(cfg config.Config, m map[string]*adoptRecord) error {
	b, _ := json.MarshalIndent(m, "", "  ")
	_ = os.MkdirAll(filepath.Dir(adoptedPath(cfg)), 0o700)
	return atomicWrite(adoptedPath(cfg), b)
}

// runClientAdopt: brokerd client adopt <server>... [--owner-only] [--dry-run]
func runClientAdopt(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("client adopt", flag.ContinueOnError)
	ownerOnly := fs.Bool("owner-only", false, "serve these connectors to owner client tokens only, never to agent grants")
	dry := fs.Bool("dry-run", false, "print the connector definitions without changing anything")
	var names []string
	for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		names, args = append(names, args[0]), args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return fail("%v", err)
	}
	names = append(names, fs.Args()...)
	if len(names) == 0 {
		return fail("usage: brokerd client adopt <server>... [--owner-only] [--dry-run]")
	}
	cfg := config.FromEnv()
	home, _ := os.UserHomeDir()
	adopted := loadAdopted(cfg)
	secrets := map[string]string{}
	if raw, err := os.ReadFile(cfg.SecretsPath()); err == nil {
		_ = json.Unmarshal(raw, &secrets)
	}
	existing := map[string]bool{}
	if raw, err := os.ReadFile(cfg.ConnectorsPath()); err == nil {
		var f connectors.File
		_ = yaml.Unmarshal(raw, &f)
		for k := range f.Connectors {
			existing[k] = true
		}
	}
	failed := 0
	for _, arg := range names {
		// client-name=connector-name renames on the way in (connector names are lowercase).
		name, conn, _ := strings.Cut(arg, "=")
		if conn == "" {
			conn = strings.ToLower(name)
		}
		src, entry := findSource(home, name)
		if entry == nil {
			if existing[conn] {
				fmt.Printf("%-24s already in the broker; removing any client copies\n", name)
			} else {
				fmt.Printf("%-24s not found in any client config\n", name)
				failed++
				continue
			}
		}
		if entry != nil && !existing[conn] {
			spec, newSecrets, err := toSpec(ctx, conn, entry)
			if err != nil {
				fmt.Printf("%-24s cannot adopt: %v\n", name, err)
				failed++
				continue
			}
			spec.OwnerOnly = *ownerOnly
			out, _ := yaml.Marshal(map[string]*connectors.Spec{conn: spec})
			if *dry {
				fmt.Printf("# %s (from %s)\n%s", name, src, indent(string(out)))
				for k := range newSecrets {
					fmt.Printf("#   secret %s → secrets.json\n", k)
				}
				continue
			}
			for k, v := range newSecrets {
				secrets[k] = v
			}
			b, _ := json.MarshalIndent(secrets, "", "  ")
			if err := atomicWrite(cfg.SecretsPath(), b); err != nil {
				return fail("write secrets: %v", err)
			}
			if err := appendConnector(cfg.ConnectorsPath(), string(out), src); err != nil {
				return fail("write connectors.yaml: %v", err)
			}
			existing[conn] = true
			fmt.Printf("%-24s added to the broker as %s (from %s)\n", name, conn, src)
		}
		if *dry {
			continue
		}
		rec := adopted[name]
		if rec == nil {
			rec = &adoptRecord{Connector: conn, Source: src, AdoptedAt: time.Now(), Entries: map[string]json.RawMessage{}}
			adopted[name] = rec
		}
		for _, c := range allClients() {
			path := c.path(home)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			orig, _ := os.ReadFile(path)
			removed, err := c.park(path, name)
			if err != nil {
				fmt.Printf("%-24s %s: %v\n", name, c.name, err)
				failed++
				continue
			}
			if removed == nil {
				continue
			}
			_ = os.WriteFile(path+".pre-adopt-"+time.Now().Format("20060102T150405"), orig, 0o600)
			if _, have := rec.Entries[c.name]; !have {
				rec.Entries[c.name] = removed
			}
			fmt.Printf("%-24s parked in %s (restorable)\n", name, c.name)
		}
		if err := saveAdopted(cfg, adopted); err != nil {
			return fail("%v", err)
		}
	}
	if !*dry {
		if ac, _, err := adminClient(); err == nil {
			var out map[string]any
			_ = ac.Do(ctx, "POST", "/v1/reload", nil, &out)
		}
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// runClientRestore: brokerd client restore <server>... — puts the client entries back and disables the
// connector in the broker so tools are not offered twice.
func runClientRestore(ctx context.Context, args []string) int {
	if len(args) == 0 {
		return fail("usage: brokerd client restore <server>...")
	}
	cfg := config.FromEnv()
	home, _ := os.UserHomeDir()
	adopted := loadAdopted(cfg)
	failed := 0
	for _, name := range args {
		for _, c := range allClients() {
			path := c.path(home)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			ok, err := c.unpark(path, name)
			if err == nil && !ok && c.format == "json" {
				// Some clients (Claude Desktop) rewrite their config and drop unknown keys, taking the in-file
				// backup with them; the broker's own record still has the original entry.
				if rec := adopted[name]; rec != nil && rec.Entries[c.name] != nil {
					ok, err = reinsertJSON(path, c.key, name, rec.Entries[c.name])
				}
			}
			if err != nil {
				fmt.Printf("%-24s %s: %v\n", name, c.name, err)
				failed++
			} else if ok {
				fmt.Printf("%-24s restored in %s\n", name, c.name)
			}
		}
		conn := strings.ToLower(name)
		if rec := adopted[name]; rec != nil && rec.Connector != "" {
			conn = rec.Connector
		}
		if err := setDisabled(cfg.ConnectorsPath(), conn, true); err != nil {
			fmt.Printf("%-24s broker: %v\n", name, err)
		} else {
			fmt.Printf("%-24s disabled in the broker (connectors.yaml keeps it; set disabled: false to re-adopt)\n", name)
		}
		delete(adopted, name)
	}
	_ = saveAdopted(cfg, adopted)
	if ac, _, err := adminClient(); err == nil {
		var out map[string]any
		_ = ac.Do(ctx, "POST", "/v1/reload", nil, &out)
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// findSource returns the first client config holding a non-broker entry for name, as a generic JSON entry.
// JSON configs are preferred (they carry headers and env verbatim).
func findSource(home, name string) (string, map[string]any) {
	for _, c := range allClients() {
		if c.format != "json" {
			continue
		}
		doc, err := readJSONC(c.path(home))
		if err != nil {
			continue
		}
		servers, _ := doc[c.key].(map[string]any)
		if e, ok := servers[name].(map[string]any); ok && !isBrokerEntry(e) {
			if c.name == "opencode" {
				e = fromOpencode(e)
			}
			return c.name, e
		}
	}
	return "", nil
}

func fromOpencode(e map[string]any) map[string]any {
	out := map[string]any{}
	if cmd, ok := e["command"].([]any); ok && len(cmd) > 0 {
		out["command"] = cmd[0]
		out["args"] = cmd[1:]
	}
	if env, ok := e["environment"]; ok {
		out["env"] = env
	}
	if u, ok := e["url"]; ok {
		out["url"] = u
		out["headers"] = e["headers"]
	}
	return out
}

var headerArg = regexp.MustCompile(`^([^:]+):\s*(.*)$`)
var envVarRef = regexp.MustCompile(`\$\{([A-Za-z0-9_]+)\}`)

// toSpec converts a client entry to a connector. Env and header values that are not paths or URLs move to
// secrets.json under <NAME>_<KEY>. A stdio `mcp-remote <url>` wrapper becomes a native url connector.
func toSpec(ctx context.Context, name string, e map[string]any) (*connectors.Spec, map[string]string, error) {
	spec := &connectors.Spec{}
	secrets := map[string]string{}
	env := stringMap(e["env"])
	secretKey := func(k string) string {
		norm := func(x string) string {
			return strings.ToUpper(regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(x, "_"))
		}
		if strings.HasPrefix(norm(k), norm(name)+"_") {
			return norm(k) // DATAFORSEO_PASSWORD, not DATAFORSEO_DATAFORSEO_PASSWORD
		}
		return norm(name + "_" + k)
	}
	keep := func(k, v string) string {
		if v == "" || strings.HasPrefix(v, "/") || strings.HasPrefix(v, "~/") || strings.HasPrefix(v, "http://") || strings.HasPrefix(v, "https://") || v == "true" || v == "false" || regexp.MustCompile(`^[0-9]+$`).MatchString(v) {
			return v
		}
		sk := secretKey(k)
		secrets[sk] = v
		return "${secret:" + sk + "}"
	}
	cmd, _ := e["command"].(string)
	args := stringSlice(e["args"])
	if i := remoteArg(args); cmd != "" && i >= 0 && i+1 < len(args) {
		// mcp-remote wrapper: url plus --header K:V with ${VAR} from env.
		spec.URL = args[i+1]
		for j := i + 2; j < len(args); j++ {
			if args[j] == "--header" && j+1 < len(args) {
				if m := headerArg.FindStringSubmatch(args[j+1]); m != nil {
					v := envVarRef.ReplaceAllStringFunc(m[2], func(s string) string { return env[envVarRef.FindStringSubmatch(s)[1]] })
					if spec.Headers == nil {
						spec.Headers = map[string]string{}
					}
					spec.Headers[m[1]] = keep("header_"+m[1], v)
				}
				j++
			}
		}
	} else if u, _ := e["url"].(string); u != "" {
		spec.URL = u
		for k, v := range stringMap(e["headers"]) {
			if spec.Headers == nil {
				spec.Headers = map[string]string{}
			}
			spec.Headers[k] = keep("header_"+k, v)
		}
		if o, ok := e["oauth"].(map[string]any); ok {
			spec.OAuth = &connectors.OAuth{}
			if sc, _ := o["scopes"].(string); sc != "" {
				spec.OAuth.Scopes = strings.Fields(sc)
			}
		}
	} else if cmd != "" {
		spec.Command, spec.Args = cmd, args
		if cwd, _ := e["cwd"].(string); cwd != "" {
			spec.Cwd = cwd
		}
		for k, v := range env {
			if spec.Env == nil {
				spec.Env = map[string]string{}
			}
			spec.Env[k] = keep(k, v)
		}
	} else {
		return nil, nil, fmt.Errorf("entry has neither command nor url")
	}
	if spec.URL != "" && spec.OAuth == nil && len(spec.Headers) == 0 && wantsOAuth(ctx, spec.URL) {
		spec.OAuth = &connectors.OAuth{}
	}
	return spec, secrets, nil
}

func remoteArg(args []string) int {
	for i, a := range args {
		if strings.Contains(a, "mcp-remote") {
			return i
		}
	}
	return -1
}

// wantsOAuth probes the server unauthenticated: a 401 means it expects a bearer token from a sign-in.
func wantsOAuth(ctx context.Context, url string) bool {
	body := strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"brokerd-probe","version":"1"}}}`)
	req, err := http.NewRequestWithContext(ctx, "POST", url, body)
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusUnauthorized
}

func appendConnector(path, block, src string) error {
	raw, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(raw)
	if strings.TrimSpace(text) == "" {
		text = "connectors:\n"
	}
	var b strings.Builder
	b.WriteString(strings.TrimRight(text, "\n") + "\n")
	fmt.Fprintf(&b, "  # adopted from %s on %s\n", src, time.Now().Format("2006-01-02"))
	b.WriteString(indent(block))
	// Validate before writing: a broken file would be refused by the reloader anyway, but fail loudly here.
	var f connectors.File
	if err := yaml.Unmarshal([]byte(b.String()), &f); err != nil {
		return err
	}
	return atomicWrite(path, []byte(b.String()))
}

// setDisabled flips a connector's disabled flag, keeping the rest of the file (comments included).
func setDisabled(path, name string, disabled bool) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return err
	}
	root := doc.Content[0]
	var conns *yaml.Node
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == "connectors" {
			conns = root.Content[i+1]
		}
	}
	if conns == nil {
		return fmt.Errorf("no connectors map")
	}
	for i := 0; i+1 < len(conns.Content); i += 2 {
		if conns.Content[i].Value != name {
			continue
		}
		m := conns.Content[i+1]
		for j := 0; j+1 < len(m.Content); j += 2 {
			if m.Content[j].Value == "disabled" {
				m.Content[j+1].Value = fmt.Sprint(disabled)
				goto write
			}
		}
		m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "disabled"}, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: fmt.Sprint(disabled)})
		goto write
	}
	return fmt.Errorf("%s is not in connectors.yaml", name)
write:
	out, err := yaml.Marshal(&doc)
	if err != nil {
		return err
	}
	return atomicWrite(path, out)
}

func indent(s string) string {
	var b strings.Builder
	for _, l := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		b.WriteString("  " + l + "\n")
	}
	return b.String()
}

func stringMap(v any) map[string]string {
	out := map[string]string{}
	if m, ok := v.(map[string]any); ok {
		for k, x := range m {
			if s, ok := x.(string); ok {
				out[k] = s
			}
		}
	}
	return out
}

func stringSlice(v any) []string {
	var out []string
	if a, ok := v.([]any); ok {
		for _, x := range a {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func readJSONC(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	text := regexp.MustCompile(`(?m)^\s*//.*$`).ReplaceAllString(string(raw), "")
	var doc map[string]any
	if err := json.Unmarshal([]byte(text), &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return doc, nil
}

// parkJSON moves servers[name] into the config's backup map (or, for OpenCode, whose schema rejects unknown
// keys, disables it in place). Returns the original entry, or nil when the client had none.
func parkJSON(key string, inPlace bool) func(path, name string) (json.RawMessage, error) {
	return func(path, name string) (json.RawMessage, error) {
		before, _ := os.Stat(path)
		doc, err := readJSONC(path)
		if err != nil {
			return nil, err
		}
		servers, _ := doc[key].(map[string]any)
		cur, ok := servers[name].(map[string]any)
		if !ok || isBrokerEntry(cur) {
			return nil, nil
		}
		if inPlace {
			if en, has := cur["enabled"].(bool); has && !en {
				return nil, nil // already parked
			}
		}
		orig, _ := json.Marshal(cur)
		if inPlace {
			cur["enabled"] = false
		} else {
			bk, _ := doc[backupKey].(map[string]any)
			if bk == nil {
				bk = map[string]any{}
			}
			bk[name] = cur
			doc[backupKey] = bk
			delete(servers, name)
		}
		if err := writeIfUnchanged(path, before, doc); err != nil {
			return nil, err
		}
		return orig, nil
	}
}

func unparkJSON(key string, inPlace bool) func(path, name string) (bool, error) {
	return func(path, name string) (bool, error) {
		before, _ := os.Stat(path)
		doc, err := readJSONC(path)
		if err != nil {
			return false, err
		}
		servers, _ := doc[key].(map[string]any)
		if servers == nil {
			servers = map[string]any{}
		}
		if inPlace {
			cur, ok := servers[name].(map[string]any)
			if !ok {
				return false, nil
			}
			if en, has := cur["enabled"].(bool); has && en {
				return false, nil
			}
			cur["enabled"] = true
		} else {
			bk, _ := doc[backupKey].(map[string]any)
			e, ok := bk[name]
			if !ok {
				return false, nil
			}
			servers[name] = e
			delete(bk, name)
			if len(bk) == 0 {
				delete(doc, backupKey)
			}
			doc[key] = servers
		}
		return true, writeIfUnchanged(path, before, doc)
	}
}

// reinsertJSON puts an adopted entry back into a JSON client's server map, unless the client already has one.
func reinsertJSON(path, key, name string, raw json.RawMessage) (bool, error) {
	before, _ := os.Stat(path)
	doc, err := readJSONC(path)
	if err != nil {
		return false, err
	}
	servers, _ := doc[key].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	if _, has := servers[name]; has {
		return false, nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return false, err
	}
	servers[name] = v
	doc[key] = servers
	return true, writeIfUnchanged(path, before, doc)
}

func writeIfUnchanged(path string, before os.FileInfo, doc map[string]any) error {
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	if after, err := os.Stat(path); err == nil && before != nil && !after.ModTime().Equal(before.ModTime()) {
		return fmt.Errorf("%s changed while being edited; try again", path)
	}
	return atomicWrite(path, out)
}

// parkCodex sets `enabled = false` on [mcp_servers.<name>], leaving the table (and its tool approvals) in place.
func parkCodex(path, name string) (json.RawMessage, error) {
	return setCodexEnabled(path, name, false)
}

func unparkCodex(path, name string) (bool, error) {
	r, err := setCodexEnabled(path, name, true)
	return r != nil, err
}

var enabledLine = regexp.MustCompile(`(?m)^enabled[ \t]*=[ \t]*(true|false)[ \t]*$`)

func setCodexEnabled(path, name string, enabled bool) (json.RawMessage, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	blocks := splitTOML(string(raw))
	var orig string
	changed := false
	for i, b := range blocks {
		if b.header != "[mcp_servers."+name+"]" && b.header != `[mcp_servers."`+name+`"]` {
			continue
		}
		if strings.Contains(b.text, `url = "http://127.0.0.1:`) {
			return nil, nil // the broker's own entry
		}
		orig = b.text
		want := fmt.Sprintf("enabled = %v", enabled)
		if m := enabledLine.FindStringSubmatch(b.text); m != nil {
			if m[1] == fmt.Sprint(enabled) {
				return nil, nil
			}
			blocks[i].text = enabledLine.ReplaceAllString(b.text, want)
		} else {
			if enabled {
				return nil, nil
			}
			lines := strings.SplitAfterN(b.text, "\n", 2)
			blocks[i].text = lines[0] + want + "\n" + strings.Join(lines[1:], "")
		}
		changed = true
	}
	if !changed {
		return nil, nil
	}
	var out strings.Builder
	for _, b := range blocks {
		out.WriteString(b.text)
	}
	if err := atomicWrite(path, []byte(out.String())); err != nil {
		return nil, err
	}
	j, _ := json.Marshal(orig)
	return j, nil
}

func allClients() []client {
	out := append([]client{}, clients...)
	sort.SliceStable(out, func(i, j int) bool { return clientOrder(out[i].name) < clientOrder(out[j].name) })
	return out
}

func clientOrder(n string) int {
	for i, x := range []string{"claude-code", "claude-desktop", "cursor", "opencode", "codex"} {
		if x == n {
			return i
		}
	}
	return 99
}
