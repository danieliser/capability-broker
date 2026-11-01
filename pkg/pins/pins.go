// Package pins turns connectors that run through a package runner (`npx`, `uvx`) into pinned local installs.
// The broker resolves the version once, installs it under <data-dir>/runtimes, and runs that copy directly, so
// a start never waits on the registry and every restart runs the same code. Pins are rechecked after a TTL
// (default 24h); a newer version is installed beside the old one and swapped in by a normal hot reload.
// Until a pin exists the original command runs unchanged, so pinning never blocks a connector.
package pins

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/danieliser/capability-broker/pkg/connectors"
)

const Filename = "pins.json"

// Pin is one resolved install. Key ties it to the exact original command line, so editing the connector
// invalidates the pin.
type Pin struct {
	Key        string    `json:"key"`
	Connector  string    `json:"connector"`
	Kind       string    `json:"kind"` // npm | uv
	Package    string    `json:"package"`
	Version    string    `json:"version"`
	Command    string    `json:"command"`
	Args       []string  `json:"args"`
	Dir        string    `json:"dir"`
	ResolvedAt time.Time `json:"resolved_at"`
	CheckedAt  time.Time `json:"checked_at"`
}

type File struct {
	Pins map[string]*Pin `json:"pins"` // connector name → pin
}

// target is what a runner command line asks for.
type target struct {
	kind    string // npm | uv
	pkg     string // package name without version
	version string // requested version, "" or "latest" for newest
	bin     string // executable inside the package
	rest    []string
	runner  string // npx or uvx path (to find npm / uv beside it)
}

var npmSpec = regexp.MustCompile(`^(@?[^@\s]+)(?:@(.+))?$`)

// detect parses `npx [-y] pkg[@ver] args...` and `uvx [--from pkg[==ver]] cmd args...` / `uvx pkg[==ver] args...`.
func detect(s *connectors.Spec) (target, bool) {
	base := filepath.Base(s.Command)
	switch base {
	case "npx":
		var t target
		t.kind, t.runner = "npm", s.Command
		args := s.Args
		for i := 0; i < len(args); i++ {
			a := args[i]
			switch {
			case a == "-y" || a == "--yes" || a == "--quiet" || a == "-q":
				continue
			case a == "-p" || a == "--package":
				return target{}, false // multi-package invocations are left alone
			case strings.HasPrefix(a, "-"):
				return target{}, false
			default:
				m := npmSpec.FindStringSubmatch(a)
				if m == nil {
					return target{}, false
				}
				t.pkg, t.version, t.rest = m[1], m[2], append([]string{}, args[i+1:]...)
				return t, true
			}
		}
	case "uvx":
		var t target
		t.kind, t.runner = "uv", s.Command
		args := s.Args
		if len(args) >= 3 && args[0] == "--from" {
			t.pkg, t.version = splitPy(args[1])
			t.bin, t.rest = args[2], append([]string{}, args[3:]...)
			return t, t.pkg != ""
		}
		if len(args) >= 1 && !strings.HasPrefix(args[0], "-") {
			t.pkg, t.version = splitPy(args[0])
			t.bin, t.rest = t.pkg, append([]string{}, args[1:]...)
			return t, t.pkg != ""
		}
	}
	return target{}, false
}

func splitPy(s string) (string, string) {
	if i := strings.Index(s, "=="); i > 0 {
		return s[:i], s[i+2:]
	}
	if i := strings.Index(s, "@"); i > 0 {
		return s[:i], s[i+1:]
	}
	return s, ""
}

// keyFor fingerprints the original command line.
func keyFor(s *connectors.Spec) string {
	b, _ := json.Marshal(struct {
		C string
		A []string
	}{s.Command, s.Args})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

type Manager struct {
	DataDir string
	TTL     time.Duration // default 24h
	Log     func(msg string, kv ...any)
	HTTP    *http.Client

	mu sync.Mutex
}

func (m *Manager) path() string { return filepath.Join(m.DataDir, Filename) }

func (m *Manager) load() File {
	f := File{Pins: map[string]*Pin{}}
	if raw, err := os.ReadFile(m.path()); err == nil {
		_ = json.Unmarshal(raw, &f)
		if f.Pins == nil {
			f.Pins = map[string]*Pin{}
		}
	}
	return f
}

func (m *Manager) save(f File) error {
	raw, _ := json.MarshalIndent(f, "", "  ")
	tmp := m.path() + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, m.path())
}

// Apply rewrites specs that have a valid pin to run the pinned install. Specs with `pin: false`, an unknown
// runner, or no pin yet are returned unchanged.
func (m *Manager) Apply(specs []*connectors.Spec) {
	f := m.load()
	for _, s := range specs {
		if s.NoPin {
			continue
		}
		p, ok := f.Pins[s.Name]
		if !ok || p.Key != keyFor(s) {
			continue
		}
		if _, err := os.Stat(p.Command); err != nil {
			continue // install removed by hand: run the original until the next check reinstalls
		}
		s.Command, s.Args = p.Command, append([]string{}, p.Args...)
		s.PinnedVersion = p.Package + "@" + p.Version
	}
}

// Check resolves and installs what is missing or past its TTL. It reports whether pins.json changed (the
// reloader then restarts those connectors on the pinned copy).
func (m *Manager) Check(ctx context.Context, specs []*connectors.Spec) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	ttl := m.TTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	f := m.load()
	changed := false
	live := map[string]bool{}
	for _, s := range specs {
		live[s.Name] = true
		if s.NoPin {
			continue
		}
		t, ok := detect(s)
		if !ok {
			continue
		}
		key := keyFor(s)
		cur := f.Pins[s.Name]
		fresh := cur != nil && cur.Key == key && time.Since(cur.CheckedAt) < ttl
		if fresh {
			if _, err := os.Stat(cur.Command); err == nil {
				continue
			}
		}
		version := t.version
		if version == "" || version == "latest" {
			v, err := m.latest(ctx, t)
			if err != nil {
				m.log("pin: could not resolve latest; keeping current", "connector", s.Name, "package", t.pkg, "err", err.Error())
				continue
			}
			version = v
		}
		if cur != nil && cur.Key == key && cur.Version == version {
			if _, err := os.Stat(cur.Command); err == nil {
				cur.CheckedAt = time.Now().UTC()
				changed = true // only the timestamp; the command is the same, so no restart follows
				continue
			}
		}
		p, err := m.install(ctx, s.Name, key, t, version)
		if err != nil {
			m.log("pin: install failed; keeping current", "connector", s.Name, "package", t.pkg, "version", version, "err", err.Error())
			continue
		}
		prev := ""
		if cur != nil {
			prev = cur.Version
		}
		f.Pins[s.Name] = p
		changed = true
		m.log("pin: installed", "connector", s.Name, "package", t.pkg, "version", version, "previous", prev)
		m.prune(s.Name, p.Dir)
	}
	for name := range f.Pins {
		if !live[name] {
			delete(f.Pins, name)
			changed = true
		}
	}
	if changed {
		if err := m.save(f); err != nil {
			m.log("pin: could not save pins.json", "err", err.Error())
			return false
		}
	}
	return changed
}

func (m *Manager) log(msg string, kv ...any) {
	if m.Log != nil {
		m.Log(msg, kv...)
	}
}

func (m *Manager) latest(ctx context.Context, t target) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	switch t.kind {
	case "npm":
		out, err := exec.CommandContext(cctx, sibling(t.runner, "npm"), "view", t.pkg, "version").Output()
		if err != nil {
			return "", fmt.Errorf("npm view: %w", err)
		}
		v := strings.TrimSpace(string(out))
		if v == "" {
			return "", fmt.Errorf("npm view returned nothing")
		}
		return v, nil
	case "uv":
		client := m.HTTP
		if client == nil {
			client = &http.Client{Timeout: 30 * time.Second}
		}
		req, _ := http.NewRequestWithContext(cctx, http.MethodGet, "https://pypi.org/pypi/"+t.pkg+"/json", nil)
		resp, err := client.Do(req)
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return "", fmt.Errorf("pypi %d", resp.StatusCode)
		}
		var body struct {
			Info struct {
				Version string `json:"version"`
			} `json:"info"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || body.Info.Version == "" {
			return "", fmt.Errorf("pypi: no version")
		}
		return body.Info.Version, nil
	}
	return "", fmt.Errorf("unknown kind %s", t.kind)
}

var safeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func (m *Manager) install(ctx context.Context, connector, key string, t target, version string) (*Pin, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	dir := filepath.Join(m.DataDir, "runtimes", connector, safeName.ReplaceAllString(t.pkg, "_")+"-"+safeName.ReplaceAllString(version, "_"))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	p := &Pin{Key: key, Connector: connector, Kind: t.kind, Package: t.pkg, Version: version, Dir: dir,
		Args: t.rest, ResolvedAt: time.Now().UTC(), CheckedAt: time.Now().UTC()}
	switch t.kind {
	case "npm":
		cmd := exec.CommandContext(cctx, sibling(t.runner, "npm"), "install", "--prefix", dir, "--no-audit", "--no-fund", "--omit=dev", t.pkg+"@"+version)
		if out, err := cmd.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("npm install: %v: %s", err, tail(out))
		}
		bin, err := npmBin(dir, t.pkg)
		if err != nil {
			return nil, err
		}
		p.Command = filepath.Join(dir, "node_modules", ".bin", bin)
	case "uv":
		uv := sibling(t.runner, "uv")
		if out, err := exec.CommandContext(cctx, uv, "venv", "--quiet", "--allow-existing", filepath.Join(dir, "venv")).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("uv venv: %v: %s", err, tail(out))
		}
		py := filepath.Join(dir, "venv", "bin", "python")
		if out, err := exec.CommandContext(cctx, uv, "pip", "install", "--quiet", "--python", py, t.pkg+"=="+version).CombinedOutput(); err != nil {
			return nil, fmt.Errorf("uv pip install: %v: %s", err, tail(out))
		}
		p.Command = filepath.Join(dir, "venv", "bin", t.bin)
	}
	if _, err := os.Stat(p.Command); err != nil {
		return nil, fmt.Errorf("installed, but %s is missing", p.Command)
	}
	return p, nil
}

// npmBin finds the executable a package declares.
func npmBin(dir, pkg string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "node_modules", pkg, "package.json"))
	if err != nil {
		return "", err
	}
	var meta struct {
		Name string          `json:"name"`
		Bin  json.RawMessage `json:"bin"`
	}
	if err := json.Unmarshal(raw, &meta); err != nil {
		return "", err
	}
	base := pkg[strings.LastIndex(pkg, "/")+1:]
	var single string
	if json.Unmarshal(meta.Bin, &single) == nil && single != "" {
		return base, nil
	}
	var many map[string]string
	if json.Unmarshal(meta.Bin, &many) == nil && len(many) > 0 {
		if _, ok := many[base]; ok {
			return base, nil
		}
		names := make([]string, 0, len(many))
		for k := range many {
			names = append(names, k)
		}
		sort.Strings(names)
		if len(names) == 1 {
			return names[0], nil
		}
		return "", fmt.Errorf("package %s declares several executables %v; pin it by hand", pkg, names)
	}
	return "", fmt.Errorf("package %s declares no executable", pkg)
}

// prune keeps the active install and one previous one per connector, for rollback.
func (m *Manager) prune(connector, keep string) {
	root := filepath.Join(m.DataDir, "runtimes", connector)
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	type d struct {
		path string
		mod  time.Time
	}
	var others []d
	for _, e := range entries {
		p := filepath.Join(root, e.Name())
		if p == keep || !e.IsDir() {
			continue
		}
		if info, err := e.Info(); err == nil {
			others = append(others, d{p, info.ModTime()})
		}
	}
	sort.Slice(others, func(i, j int) bool { return others[i].mod.After(others[j].mod) })
	for i, o := range others {
		if i >= 1 {
			_ = os.RemoveAll(o.path)
		}
	}
}

// sibling finds a tool next to the runner (npm beside npx, uv beside uvx), else on PATH.
func sibling(runner, tool string) string {
	if runner != "" {
		candidate := filepath.Join(filepath.Dir(runner), tool)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	if p, err := exec.LookPath(tool); err == nil {
		return p
	}
	return tool
}

func tail(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[len(s)-300:]
	}
	return s
}
