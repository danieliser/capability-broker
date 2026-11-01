// Package catalog holds the operation catalog: per connector, which upstream
// tools pass through and the operations (policy keys) each tool call resolves
// to. Tools themselves come from the upstream server at runtime; the catalog
// never carries schemas. Sources, in order: <data-dir>/catalog.d/<name>.yaml,
// the embedded catalog of the same name, or a synthesized one (every tool is an
// operation of its own name). Named queries are a pushed overlay keyed by project.
package catalog

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed connectors/*.yaml
var embedded embed.FS

type Risk string

const (
	RiskRead       Risk = "read"
	RiskReversible Risk = "reversible"
	RiskSensitive  Risk = "sensitive" // needs exact-action approval (P2)
)

// Tool is an upstream tool definition passed through verbatim.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// Operation is the stable policy key a (tool, argument pattern) resolves to.
type Operation struct {
	ID                 string            `yaml:"id"`
	Tool               string            `yaml:"tool"`
	Match              map[string]string `yaml:"match,omitempty"` // argument name → regex; all must match
	Risk               Risk              `yaml:"risk"`
	RequiresCapability string            `yaml:"requires_capability,omitempty"` // "sql.select" or "operations:<name>"
	Constraints        []string          `yaml:"constraints,omitempty"`         // policy check names (pkg/policy)
	Guards             []string          `yaml:"guards,omitempty"`              // request hooks (pkg/adapters/upstream)
	Response           []string          `yaml:"response,omitempty"`            // response hooks (pkg/adapters/upstream)
	Approval           string            `yaml:"approval,omitempty"`            // "exact_action" for P2 operations
	Compact            map[string]any    `yaml:"compact,omitempty"`             // fields: [...] | max_items | max_string
	Description        string            `yaml:"description,omitempty"`

	compiled map[string]*regexp.Regexp
}

type Connector struct {
	Connector      string            `yaml:"connector"`
	Version        int               `yaml:"version"`
	AdapterVersion string            `yaml:"adapter_version"`
	ToolsDeny      []string          `yaml:"tools_deny,omitempty"`      // upstream tools never exported
	ToolTransforms []string          `yaml:"tool_transforms,omitempty"` // tools/list hooks, e.g. narrow_site_enum
	Describe       map[string]string `yaml:"describe,omitempty"`        // tool → description hook, e.g. run_query: named_queries
	SiteHostsTool  string            `yaml:"site_hosts_tool,omitempty"` // tool whose result lists sites (id, url)
	Operations     []*Operation      `yaml:"operations"`
	Source         string            `yaml:"-"` // overlay | embedded | synthesized
	digest         string
}

// Query is one named, parameterised, read-only ClickHouse query.
type Query struct {
	ID      string             `yaml:"id" json:"id"`
	Project string             `yaml:"project,omitempty" json:"project,omitempty"`
	Summary string             `yaml:"summary,omitempty" json:"summary,omitempty"`
	SQL     string             `yaml:"sql" json:"sql"`
	Params  map[string]Param   `yaml:"params,omitempty" json:"params,omitempty"`
	Limits  map[string]float64 `yaml:"limits,omitempty" json:"limits,omitempty"`
}

type Param struct {
	Type     string `yaml:"type" json:"type"` // ClickHouse type for {name:Type}
	Required bool   `yaml:"required,omitempty" json:"required,omitempty"`
	Default  string `yaml:"default,omitempty" json:"default,omitempty"`
}

type QueryOverlay struct {
	Queries []Query `yaml:"queries"`
}

type Catalog struct {
	mu         sync.RWMutex
	overlayDir string
	connectors map[string]*Connector
	queries    map[string]Query  // id → query (overlay, all projects)
	overlays   map[string]string // connector/project → digest
}

var queryIDRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)

// Load parses the embedded connector catalogs. overlayDir may be empty.
func Load(overlayDir string) (*Catalog, error) {
	c := &Catalog{overlayDir: overlayDir, connectors: map[string]*Connector{}, queries: map[string]Query{}, overlays: map[string]string{}}
	entries, err := fs.ReadDir(embedded, "connectors")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		raw, err := embedded.ReadFile("connectors/" + e.Name())
		if err != nil {
			return nil, err
		}
		conn, err := parseConnector(raw, "embedded")
		if err != nil {
			return nil, fmt.Errorf("catalog %s: %w", e.Name(), err)
		}
		c.connectors[conn.Connector] = conn
	}
	if overlayDir != "" {
		matches, _ := filepath.Glob(filepath.Join(overlayDir, "*.yaml"))
		for _, path := range matches {
			raw, err := os.ReadFile(path)
			if err != nil {
				return nil, err
			}
			conn, err := parseConnector(raw, "overlay")
			if err != nil {
				return nil, fmt.Errorf("catalog overlay %s: %w", path, err)
			}
			want := strings.TrimSuffix(filepath.Base(path), ".yaml")
			if conn.Connector != want {
				return nil, fmt.Errorf("catalog overlay %s names connector %q; the file name must match", path, conn.Connector)
			}
			c.connectors[conn.Connector] = conn
		}
	}
	return c, nil
}

func parseConnector(raw []byte, source string) (*Connector, error) {
	var conn Connector
	if err := yaml.Unmarshal(raw, &conn); err != nil {
		return nil, err
	}
	if err := conn.compile(); err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	conn.digest = hex.EncodeToString(sum[:8])
	conn.Source = source
	return &conn, nil
}

func (c *Connector) compile() error {
	if c.Connector == "" {
		return fmt.Errorf("connector name is required")
	}
	seen := map[string]bool{}
	for _, op := range c.Operations {
		if op.ID == "" || op.Tool == "" {
			return fmt.Errorf("operation needs id and tool")
		}
		if seen[op.ID] {
			return fmt.Errorf("duplicate operation %s", op.ID)
		}
		seen[op.ID] = true
		if op.Risk == "" {
			return fmt.Errorf("operation %s needs a risk", op.ID)
		}
		op.compiled = map[string]*regexp.Regexp{}
		for arg, pattern := range op.Match {
			re, err := regexp.Compile(pattern)
			if err != nil {
				return fmt.Errorf("operation %s match %s: %w", op.ID, arg, err)
			}
			op.compiled[arg] = re
		}
	}
	return nil
}

// Ensure returns the catalog for a connector, synthesizing one from the
// discovered tool names when neither an overlay nor an embedded catalog exists:
// every tool becomes an operation `<connector>.<tool>` an agent grant must list
// under `operations`; owner grants reach all of them.
func (c *Catalog) Ensure(name string, toolNames []string) *Connector {
	c.mu.Lock()
	defer c.mu.Unlock()
	if conn, ok := c.connectors[name]; ok {
		// A synthesized catalog follows the child's tool set; written catalogs never change here.
		if conn.Source != "synthesized" || len(toolNames) == 0 || sameTools(conn, toolNames) {
			return conn
		}
	}
	conn := &Connector{Connector: name, Version: 1, AdapterVersion: "synth", Source: "synthesized"}
	for _, t := range toolNames {
		conn.Operations = append(conn.Operations, &Operation{
			ID: name + "." + t, Tool: t, Risk: RiskReversible, RequiresCapability: "operations:" + t,
			Constraints: []string{"calls_per_run"},
		})
	}
	_ = conn.compile()
	sum := sha256.Sum256([]byte(name + ":" + strings.Join(toolNames, ",")))
	conn.digest = hex.EncodeToString(sum[:8])
	c.connectors[name] = conn
	return conn
}

func sameTools(conn *Connector, names []string) bool {
	if len(conn.Operations) != len(names) {
		return false
	}
	have := map[string]bool{}
	for _, op := range conn.Operations {
		have[op.Tool] = true
	}
	for _, n := range names {
		if !have[n] {
			return false
		}
	}
	return true
}

// Reload re-reads the embedded catalogs and the overlay directory. Pushed named queries and synthesized
// catalogs for connectors that have no written catalog are kept. A broken overlay leaves the catalog as it was.
func (c *Catalog) Reload() error {
	fresh, err := Load(c.overlayDir)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, conn := range c.connectors {
		if conn.Source == "synthesized" {
			if _, written := fresh.connectors[name]; !written {
				fresh.connectors[name] = conn
			}
		}
	}
	c.connectors = fresh.connectors
	return nil
}

func (c *Catalog) Connector(name string) (*Connector, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	conn, ok := c.connectors[name]
	return conn, ok
}

func (c *Catalog) Connectors() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connectorNamesLocked()
}

func (c *Catalog) connectorNamesLocked() []string {
	out := make([]string, 0, len(c.connectors))
	for k := range c.connectors {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Resolve picks the operation for a tool call: the first operation on that tool
// whose argument patterns all match. Operations without patterns are fallbacks,
// so list specific ones first in the YAML.
func (conn *Connector) Resolve(tool string, args map[string]any) (*Operation, bool) {
	var fallback *Operation
	for _, op := range conn.Operations {
		if op.Tool != tool {
			continue
		}
		if len(op.compiled) == 0 {
			if fallback == nil {
				fallback = op
			}
			continue
		}
		all := true
		for arg, re := range op.compiled {
			v, ok := args[arg]
			s, isStr := v.(string)
			if !ok || !isStr || !re.MatchString(s) {
				all = false
				break
			}
		}
		if all {
			return op, true
		}
	}
	if fallback != nil {
		return fallback, true
	}
	return nil, false
}

// ToolExposed reports whether the connector passes this upstream tool through at
// all: not denied, and reachable by at least one operation.
func (conn *Connector) ToolExposed(name string) bool {
	for _, d := range conn.ToolsDeny {
		if d == name {
			return false
		}
	}
	for _, op := range conn.Operations {
		if op.Tool == name {
			return true
		}
	}
	return false
}

// OperationsFor lists the operations bound to a tool.
func (conn *Connector) OperationsFor(tool string) []*Operation {
	var out []*Operation
	for _, op := range conn.Operations {
		if op.Tool == tool {
			out = append(out, op)
		}
	}
	return out
}

// PushQueries replaces the named-query overlay for one project. Ids must be
// unique across projects; a clash is rejected rather than silently shadowed.
func (c *Catalog) PushQueries(project string, raw []byte) (digest string, err error) {
	var overlay QueryOverlay
	if err := yaml.Unmarshal(raw, &overlay); err != nil {
		return "", fmt.Errorf("queries: %w", err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	fresh := map[string]Query{}
	for _, q := range overlay.Queries {
		if !queryIDRe.MatchString(q.ID) {
			return "", fmt.Errorf("query id %q must look like group.name", q.ID)
		}
		if strings.TrimSpace(q.SQL) == "" {
			return "", fmt.Errorf("query %s has no sql", q.ID)
		}
		if _, dup := fresh[q.ID]; dup {
			return "", fmt.Errorf("duplicate query id %s", q.ID)
		}
		if existing, ok := c.queries[q.ID]; ok && existing.Project != project {
			return "", fmt.Errorf("query id %s already belongs to project %s", q.ID, existing.Project)
		}
		for name, p := range q.Params {
			if p.Type == "" {
				return "", fmt.Errorf("query %s param %s needs a type", q.ID, name)
			}
			if !strings.Contains(q.SQL, "{"+name+":") {
				return "", fmt.Errorf("query %s declares param %s but the sql has no {%s:…} placeholder", q.ID, name, name)
			}
		}
		q.Project = project
		fresh[q.ID] = q
	}
	for id, q := range c.queries {
		if q.Project == project {
			delete(c.queries, id)
		}
	}
	for id, q := range fresh {
		c.queries[id] = q
	}
	sum := sha256.Sum256(raw)
	digest = hex.EncodeToString(sum[:8])
	c.overlays["clickhouse/"+project] = digest
	return digest, nil
}

func (c *Catalog) Query(id string) (Query, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	q, ok := c.queries[id]
	return q, ok
}

func (c *Catalog) Queries(ids []string) []Query {
	c.mu.RLock()
	defer c.mu.RUnlock()
	var out []Query
	for _, id := range ids {
		if q, ok := c.queries[id]; ok {
			out = append(out, q)
		}
	}
	return out
}

// PolicyVersion is the digest of everything in force: connector catalogs plus overlays.
func (c *Catalog) PolicyVersion() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	parts := []string{}
	for _, name := range c.connectorNamesLocked() {
		parts = append(parts, name+"="+c.connectors[name].digest)
	}
	keys := make([]string, 0, len(c.overlays))
	for k := range c.overlays {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts = append(parts, k+"="+c.overlays[k])
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, ";")))
	return hex.EncodeToString(sum[:8])
}
