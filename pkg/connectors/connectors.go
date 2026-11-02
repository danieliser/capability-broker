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

// Spec is one hosted upstream MCP server: a stdio child (command) or a remote
// Streamable HTTP server (url), optionally signed in with OAuth.
type Spec struct {
	Name    string `yaml:"-"`
	Command string `yaml:"command,omitempty"`
	// URL is a remote MCP server; Headers ("${secret:name}" allowed) go on every request.
	URL     string            `yaml:"url,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty"`
	// OAuth signs the broker in to a remote server (`brokerd auth <name>`); tokens are kept in the data dir.
	OAuth *OAuth `yaml:"oauth,omitempty"`
	// OwnerOnly connectors are served to owner client tokens only; agent grants may not name them.
	OwnerOnly      bool              `yaml:"owner_only,omitempty"`
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
	// NoPin (`pin: false`) runs an npx/uvx command as written instead of a pinned local install.
	NoPin bool  `yaml:"-"`
	Pin   *bool `yaml:"pin,omitempty"`
	// PinnedVersion is set when the command was swapped for a pinned install (package@version).
	PinnedVersion string `yaml:"-"`
}

// OAuth configures sign-in to a remote server. Empty means discover everything (protected resource metadata,
// dynamic client registration); scopes or a pre-registered client override discovery.
type OAuth struct {
	Scopes       []string `yaml:"scopes,omitempty"`
	ClientID     string   `yaml:"client_id,omitempty"`
	ClientSecret string   `yaml:"client_secret,omitempty"`
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
		if (strings.TrimSpace(spec.Command) == "") == (strings.TrimSpace(spec.URL) == "") {
			return nil, fmt.Errorf("%s: connector %s needs exactly one of command or url", path, name)
		}
		if spec.URL != "" && !strings.HasPrefix(spec.URL, "https://") && !strings.HasPrefix(spec.URL, "http://127.0.0.1") && !strings.HasPrefix(spec.URL, "http://localhost") {
			return nil, fmt.Errorf("%s: connector %s url must be https (plain http only on loopback)", path, name)
		}
		spec.Name = name
		spec.NoPin = spec.Pin != nil && !*spec.Pin
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
		var err error
		if spec.Env, err = resolveMap(spec.Env, secrets, path, name, "env"); err != nil {
			return nil, err
		}
		if spec.Headers, err = resolveMap(spec.Headers, secrets, path, name, "header"); err != nil {
			return nil, err
		}
		if spec.OAuth != nil && spec.OAuth.ClientSecret != "" {
			m, err := resolveMap(map[string]string{"client_secret": spec.OAuth.ClientSecret}, secrets, path, name, "oauth")
			if err != nil {
				return nil, err
			}
			spec.OAuth.ClientSecret = m["client_secret"]
		}
		out = append(out, spec)
	}
	return out, nil
}

// resolveMap substitutes ${secret:x} and ${env:X} in every value.
func resolveMap(in map[string]string, secrets map[string]string, path, name, what string) (map[string]string, error) {
	if in == nil {
		return nil, nil
	}
	resolved := map[string]string{}
	for k, v := range in {
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
			return nil, fmt.Errorf("%s: connector %s %s %s references secret %q which is not in secrets.json", path, name, what, k, missing)
		}
		v = envRef.ReplaceAllStringFunc(v, func(m string) string {
			return os.Getenv(envRef.FindStringSubmatch(m)[1])
		})
		resolved[k] = expandHome(v)
	}
	return resolved, nil
}

func expandHome(s string) string {
	if strings.HasPrefix(s, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, s[2:])
		}
	}
	return s
}
