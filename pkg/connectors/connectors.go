// Package connectors loads the upstream MCP servers the broker hosts, from
// <data-dir>/connectors.yaml. Each connector is any stdio MCP server; its tools
// are discovered at startup and exported verbatim. Credentials reach the child
// only through env values resolved from the broker's secrets file.
package connectors

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const Filename = "connectors.yaml"

// Spec is one hosted upstream MCP server.
type Spec struct {
	Name           string            `yaml:"-"`
	Command        string            `yaml:"command"`
	Args           []string          `yaml:"args,omitempty"`
	Env            map[string]string `yaml:"env,omitempty"` // "${secret:name}" resolves from secrets.json
	Cwd            string            `yaml:"cwd,omitempty"`
	StartupTimeout time.Duration     `yaml:"startup_timeout,omitempty"`
	CallTimeout    time.Duration     `yaml:"call_timeout,omitempty"`
	// Catalog is an optional path to this connector's operation catalog; defaults to
	// <data-dir>/catalog.d/<name>.yaml, then the embedded catalog of the same name, then a
	// synthesized one (every tool an operation of its own name).
	Catalog string `yaml:"catalog,omitempty"`
	// Disabled connectors stay in the file but are not started.
	Disabled bool `yaml:"disabled,omitempty"`
}

type File struct {
	Connectors map[string]*Spec `yaml:"connectors"`
}

var nameRe = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var secretRef = regexp.MustCompile(`\$\{secret:([A-Za-z0-9_]+)\}`)
var envRef = regexp.MustCompile(`\$\{env:([A-Za-z0-9_]+)\}`)

// Load reads the file and resolves secret and env references. A missing file is
// an empty connector set, not an error: the daemon then serves nothing but health.
func Load(path string, secrets map[string]string) ([]*Spec, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var f File
	if err := yaml.Unmarshal(raw, &f); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	var out []*Spec
	for name, spec := range f.Connectors {
		if spec == nil {
			continue
		}
		if !nameRe.MatchString(name) || name == "persist" {
			return nil, fmt.Errorf("%s: connector name %q is invalid", path, name)
		}
		if spec.Disabled {
			continue
		}
		if strings.TrimSpace(spec.Command) == "" {
			return nil, fmt.Errorf("%s: connector %s needs a command", path, name)
		}
		spec.Name = name
		spec.Command = expandHome(spec.Command)
		if spec.Cwd != "" {
			spec.Cwd = expandHome(spec.Cwd)
		}
		if spec.StartupTimeout <= 0 {
			spec.StartupTimeout = 90 * time.Second
		}
		if spec.CallTimeout <= 0 {
			spec.CallTimeout = 120 * time.Second
		}
		resolved := map[string]string{}
		for k, v := range spec.Env {
			var missing string
			v = secretRef.ReplaceAllStringFunc(v, func(m string) string {
				key := secretRef.FindStringSubmatch(m)[1]
				val, ok := secrets[key]
				if !ok || val == "" {
					missing = key
				}
				return val
			})
			if missing != "" {
				return nil, fmt.Errorf("%s: connector %s env %s references secret %q which is not in secrets.json", path, name, k, missing)
			}
			v = envRef.ReplaceAllStringFunc(v, func(m string) string {
				return os.Getenv(envRef.FindStringSubmatch(m)[1])
			})
			resolved[k] = expandHome(v)
		}
		spec.Env = resolved
		out = append(out, spec)
	}
	return out, nil
}

func expandHome(s string) string {
	if strings.HasPrefix(s, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, s[2:])
		}
	}
	return s
}
