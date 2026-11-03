package catalog

import (
	"fmt"
	"math"
	"strings"
)

// ArgumentConstraint binds an operation's effective argument to a grant value.
// Key is looked up in connector constraints/limits and operation_constraints[operation ID].
// Paths are dot-separated object keys; no wildcards or array indexes are implicit.
type ArgumentConstraint struct {
	Kind     string   `yaml:"kind"`
	Path     string   `yaml:"path,omitempty"`
	Key      string   `yaml:"key"`
	Required bool     `yaml:"required,omitempty"`
	Default  *float64 `yaml:"default,omitempty"`
}

func (r ArgumentConstraint) Validate() error {
	if r.Key == "" {
		return fmt.Errorf("argument constraint needs a key")
	}
	switch r.Kind {
	case "allowlist", "prefix", "maximum", "row_limit":
		if r.Path == "" {
			return fmt.Errorf("%s constraint needs a path", r.Kind)
		}
		for _, part := range strings.Split(r.Path, ".") {
			if part == "" {
				return fmt.Errorf("invalid argument path %q", r.Path)
			}
		}
	case "max_per_day":
		if r.Path != "" || r.Required {
			return fmt.Errorf("max_per_day has no argument path or required flag")
		}
	default:
		return fmt.Errorf("unknown argument constraint kind %q", r.Kind)
	}
	if r.Default != nil {
		v := *r.Default
		if r.Kind == "allowlist" || r.Kind == "prefix" || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 ||
			((r.Kind == "row_limit" || r.Kind == "max_per_day") && math.Trunc(v) != v) {
			return fmt.Errorf("invalid default for %s", r.Kind)
		}
	}
	return nil
}
