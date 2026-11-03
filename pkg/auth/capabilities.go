package auth

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// ConnectorCapability is what a grant may do on one connector. Lists are allowlists;
// an absent list means "nothing of that kind" for agent grants. Limits and
// Constraints are keyed by the names the policy engine knows (see pkg/policy).
type ConnectorCapability struct {
	Operations           []string                  `json:"operations,omitempty"`
	Queries              []string                  `json:"queries,omitempty"`
	Sites                []string                  `json:"sites,omitempty"`
	Namespaces           []string                  `json:"namespaces,omitempty"`
	SQL                  string                    `json:"sql,omitempty"` // "select" grants raw SELECT on clickhouse
	Limits               map[string]float64        `json:"limits,omitempty"`
	Constraints          map[string]any            `json:"constraints,omitempty"`
	OperationConstraints map[string]map[string]any `json:"operation_constraints,omitempty"`
}

// Capabilities maps connector id to its capability. The owner profile is the
// single key "*" with Owner set, used by client tokens.
type Capabilities map[string]ConnectorCapability

const OwnerKey = "*"

func OwnerCapabilities() Capabilities {
	return Capabilities{OwnerKey: ConnectorCapability{SQL: "select"}}
}

func (c Capabilities) IsOwner() bool { _, ok := c[OwnerKey]; return ok }

// Connectors lists the connectors this capability set names, sorted.
func (c Capabilities) Connectors() []string {
	out := make([]string, 0, len(c))
	for k := range c {
		if k != OwnerKey {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// For returns the capability for a connector and whether the grant reaches it at all.
func (c Capabilities) For(connector string) (ConnectorCapability, bool) {
	if c.IsOwner() {
		return c[OwnerKey], true
	}
	cap, ok := c[connector]
	return cap, ok
}

func ParseCapabilities(raw []byte) (Capabilities, error) {
	var c Capabilities
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("capabilities: %w", err)
	}
	if len(c) == 0 {
		return nil, fmt.Errorf("capabilities: at least one connector is required")
	}
	for name, cap := range c {
		if name == "" {
			return nil, fmt.Errorf("capabilities: empty connector name")
		}
		if name != OwnerKey && cap.SQL != "" && cap.SQL != "select" {
			return nil, fmt.Errorf("capabilities: %s.sql must be \"select\"", name)
		}
	}
	return c, nil
}

func (c Capabilities) JSON() string {
	b, _ := json.Marshal(c)
	return string(b)
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// Intersect narrows this capability set by a parent's: a child grant may never
// exceed its parent (beat C §3). Owner parents impose nothing.
func (c Capabilities) Intersect(parent Capabilities) Capabilities {
	if parent.IsOwner() {
		return c
	}
	out := Capabilities{}
	for name, child := range c {
		p, ok := parent[name]
		if !ok {
			continue
		}
		out[name] = ConnectorCapability{
			Operations:           intersectList(child.Operations, p.Operations),
			Queries:              intersectList(child.Queries, p.Queries),
			Sites:                intersectList(child.Sites, p.Sites),
			Namespaces:           intersectList(child.Namespaces, p.Namespaces),
			SQL:                  minSQL(child.SQL, p.SQL),
			Limits:               minLimits(child.Limits, p.Limits),
			Constraints:          intersectConstraints(child.Constraints, p.Constraints),
			OperationConstraints: intersectOperationConstraints(child.OperationConstraints, p.OperationConstraints),
		}
	}
	return out
}

func intersectList(a, b []string) []string {
	var out []string
	for _, x := range a {
		if contains(b, x) {
			out = append(out, x)
		}
	}
	return out
}

func minSQL(a, b string) string {
	if a == "select" && b == "select" {
		return "select"
	}
	return ""
}

func minLimits(a, b map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range a {
		if pv, ok := b[k]; ok && pv < v {
			out[k] = pv
		} else {
			out[k] = v
		}
	}
	for k, v := range b {
		if _, ok := out[k]; !ok {
			out[k] = v
		}
	}
	return out
}

// Retain parent restrictions when a child omits them. Lists intersect, numeric ceilings
// take the minimum, and compatible string prefixes take the narrower prefix. An
// incompatible value becomes null, which declarative checks deny rather than ignore.
func intersectConstraints(child, parent map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range child {
		out[k] = v
	}
	for k, p := range parent {
		c, exists := child[k]
		if !exists {
			out[k] = p
			continue
		}
		if pl, ok := constraintStrings(p); ok {
			cl, valid := constraintStrings(c)
			if valid {
				out[k] = intersectList(cl, pl)
			} else {
				out[k] = nil
			}
		} else if pn, ok := constraintNumber(p); ok {
			cn, valid := constraintNumber(c)
			if valid {
				if cn < pn {
					out[k] = cn
				} else {
					out[k] = pn
				}
			} else {
				out[k] = nil
			}
		} else if ps, ok := p.(string); ok {
			cs, valid := c.(string)
			if valid && strings.HasPrefix(cs, ps) {
				out[k] = cs
			} else if valid && strings.HasPrefix(ps, cs) {
				out[k] = ps
			} else {
				out[k] = nil
			}
		} else {
			out[k] = nil
		}
	}
	return out
}
func intersectOperationConstraints(child, parent map[string]map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	for op, v := range child {
		out[op] = intersectConstraints(v, parent[op])
	}
	for op, v := range parent {
		if _, ok := out[op]; !ok {
			out[op] = intersectConstraints(nil, v)
		}
	}
	return out
}
func constraintStrings(v any) ([]string, bool) {
	switch v := v.(type) {
	case []string:
		return v, true
	case []any:
		out := make([]string, len(v))
		for i, x := range v {
			s, ok := x.(string)
			if !ok {
				return nil, false
			}
			out[i] = s
		}
		return out, true
	}
	return nil, false
}
func constraintNumber(v any) (float64, bool) {
	switch v := v.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	}
	return 0, false
}
