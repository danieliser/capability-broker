// Package catalog holds the operation catalog: per connector, the verbatim
// upstream tools the broker exposes and the operations (policy keys) each tool
// call resolves to. Named queries are a pushed overlay keyed by project.
package catalog

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
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
	Name        string         `yaml:"name" json:"name"`
	Description string         `yaml:"description" json:"description"`
	InputSchema map[string]any `yaml:"inputSchema" json:"inputSchema"`
}

// Operation is the stable policy key a (tool, argument pattern) resolves to.
type Operation struct {
	ID                 string            `yaml:"id"`
	Tool               string            `yaml:"tool"`
	Match              map[string]string `yaml:"match,omitempty"` // argument name → regex; all must match
	Risk               Risk              `yaml:"risk"`
	RequiresCapability string            `yaml:"requires_capability,omitempty"` // "sql.select" or "operations:<name>"
	Constraints        []string          `yaml:"constraints,omitempty"`         // policy check names
	Guards             []string          `yaml:"guards,omitempty"`              // adapter-side argument guards
	Approval           string            `yaml:"approval,omitempty"`            // "exact_action" for P2 operations
	Compact            map[string]any    `yaml:"compact,omitempty"`
	Description        string            `yaml:"description,omitempty"`

	compiled map[string]*regexp.Regexp
}

type Connector struct {
	Connector         string       `yaml:"connector"`
	Version           int          `yaml:"version"`
	AdapterVersion    string       `yaml:"adapter_version"`
	ToolsFromUpstream bool         `yaml:"tools_from_upstream,omitempty"` // tools/list is fetched from the upstream child
	ToolsAllow        []string     `yaml:"tools_allow,omitempty"`         // when from upstream: names exposed
	Tools             []Tool       `yaml:"tools,omitempty"`
	Operations        []*Operation `yaml:"operations"`
	digest            string
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
	connectors map[string]*Connector
	queries    map[string]Query  // id → query (overlay, all projects)
	overlays   map[string]string // connector/project → digest
}

var queryIDRe = regexp.MustCompile(`^[a-z][a-z0-9_]*(\.[a-z0-9_]+)+$`)

// Load parses the embedded connector catalogs.
func Load() (*Catalog, error) {
	c := &Catalog{connectors: map[string]*Connector{}, queries: map[string]Query{}, overlays: map[string]string{}}
	entries, err := fs.ReadDir(embedded, "connectors")
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		raw, err := embedded.ReadFile("connectors/" + e.Name())
		if err != nil {
			return nil, err
		}
		var conn Connector
		if err := yaml.Unmarshal(raw, &conn); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", e.Name(), err)
		}
		if err := conn.compile(); err != nil {
			return nil, fmt.Errorf("catalog %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(raw)
		conn.digest = hex.EncodeToString(sum[:8])
		c.connectors[conn.Connector] = &conn
	}
	return c, nil
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

func (c *Catalog) Connector(name string) (*Connector, bool) {
	conn, ok := c.connectors[name]
	return conn, ok
}

func (c *Catalog) Connectors() []string {
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

// ToolExposed reports whether the connector passes this upstream tool through at all.
func (conn *Connector) ToolExposed(name string) bool {
	if conn.ToolsFromUpstream {
		for _, n := range conn.ToolsAllow {
			if n == name {
				return true
			}
		}
		return false
	}
	for _, t := range conn.Tools {
		if t.Name == name {
			return true
		}
	}
	return false
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
	for _, name := range c.Connectors() {
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

// ToolJSON renders a tool's input schema as raw JSON for the MCP server.
func (t Tool) ToolJSON() (json.RawMessage, error) {
	return json.Marshal(t.InputSchema)
}
