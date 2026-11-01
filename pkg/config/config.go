// Package config loads brokerd's configuration once, at startup, from flags,
// environment and the data-root files. No other package reads os.Getenv.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

const (
	DefaultPort        = 8890
	DefaultHost        = "127.0.0.1"
	SecretsFilename    = "secrets.json"
	AdminSocketName    = "admin.sock"
	AdminTokenFilename = "admin.token"
	DatabaseFilename   = "broker.db"
	ConnectorsFilename = "connectors.yaml"
	CatalogDirname     = "catalog.d"
)

type Config struct {
	Host       string
	Port       int
	DataDir    string
	PersistURL string // for judgement hand-off (P2); informational in P1
	// Secrets is the owner-only secrets file as a flat map; connectors reference keys as ${secret:key}.
	Secrets map[string]string
}

func DefaultDataDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".brokerd"
	}
	return filepath.Join(home, ".brokerd")
}

func (c Config) AdminSocketPath() string { return filepath.Join(c.DataDir, AdminSocketName) }
func (c Config) AdminTokenPath() string  { return filepath.Join(c.DataDir, AdminTokenFilename) }
func (c Config) DatabasePath() string    { return filepath.Join(c.DataDir, DatabaseFilename) }
func (c Config) SecretsPath() string     { return filepath.Join(c.DataDir, SecretsFilename) }
func (c Config) ConnectorsPath() string  { return filepath.Join(c.DataDir, ConnectorsFilename) }
func (c Config) CatalogDir() string      { return filepath.Join(c.DataDir, CatalogDirname) }
func (c Config) FrontAddr() string       { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// FromEnv builds the configuration from BROKERD_* variables with documented defaults.
// Flags in cmd/brokerd override Host, Port and DataDir afterwards.
func FromEnv() Config {
	port := DefaultPort
	if v := os.Getenv("BROKERD_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			port = p
		}
	}
	return Config{
		Host:       envOr("BROKERD_HOST", DefaultHost),
		Port:       port,
		DataDir:    envOr("BROKERD_DATA_DIR", DefaultDataDir()),
		PersistURL: envOr("BROKERD_PERSIST_URL", "http://127.0.0.1:8803"),
		Secrets:    map[string]string{},
	}
}

// LoadSecrets reads the owner-only secrets file (flat JSON object of strings). A
// missing file is not an error: a connector referencing a secret then fails to load.
func (c *Config) LoadSecrets() error {
	raw, err := os.ReadFile(c.SecretsPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read secrets: %w", err)
	}
	info, err := os.Stat(c.SecretsPath())
	if err == nil && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("secrets file %s must be mode 0600 (is %04o)", c.SecretsPath(), info.Mode().Perm())
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("parse secrets: %w", err)
	}
	for k, v := range m {
		if s, ok := v.(string); ok {
			c.Secrets[k] = s
		}
	}
	return nil
}
