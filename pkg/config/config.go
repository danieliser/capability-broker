// Package config loads brokerd's configuration once, at startup, from flags,
// environment and the data-root secrets file. No other package reads os.Getenv.
package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	DefaultPort        = 8890
	DefaultHost        = "127.0.0.1"
	SecretsFilename    = "secrets.json"
	AdminSocketName    = "admin.sock"
	AdminTokenFilename = "admin.token"
	DatabaseFilename   = "broker.db"
)

// Secrets are the upstream credentials the broker holds on the agents' behalf.
// The file is owner-only JSON; values are never logged.
type Secrets struct {
	ClickHousePassword string `json:"clickhouse_password"`
	AutoMemAPIKey      string `json:"automem_api_key"`
	// PersistAppToken is the broker's own tier-2 application token for judgement hand-off (P2).
	PersistAppToken string `json:"persist_app_token,omitempty"`
}

type ClickHouse struct {
	URL      string        // http://localhost:18123
	User     string        // dedicated read-only user
	Timeout  time.Duration // per request
	Database string
}

type WordPress struct {
	// Command and Args launch the wp-mcp-router as a stdio child (one shared process).
	Command string
	Args    []string
	// ConfigPath is the router's sites.json, passed through WP_MCP_ROUTER_CONFIG.
	ConfigPath string
	Timeout    time.Duration
}

type Memory struct {
	URL     string // https://memory-service.up.railway.app
	Timeout time.Duration
}

type Config struct {
	Host       string
	Port       int
	DataDir    string
	PersistURL string // for receipts pull hand-shake and judgement (P2); informational in P1
	ClickHouse ClickHouse
	WordPress  WordPress
	Memory     Memory
	Secrets    Secrets
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
func (c Config) FrontAddr() string       { return fmt.Sprintf("%s:%d", c.Host, c.Port) }

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
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
	home, _ := os.UserHomeDir()
	return Config{
		Host:       envOr("BROKERD_HOST", DefaultHost),
		Port:       port,
		DataDir:    envOr("BROKERD_DATA_DIR", DefaultDataDir()),
		PersistURL: envOr("BROKERD_PERSIST_URL", "http://127.0.0.1:8803"),
		ClickHouse: ClickHouse{
			URL:      envOr("BROKERD_CLICKHOUSE_URL", "http://localhost:18123"),
			User:     envOr("BROKERD_CLICKHOUSE_USER", "broker_reader"),
			Database: envOr("BROKERD_CLICKHOUSE_DATABASE", "companykit"),
			Timeout:  envDuration("BROKERD_CLICKHOUSE_TIMEOUT", 90*time.Second),
		},
		WordPress: WordPress{
			Command:    envOr("BROKERD_WP_ROUTER_COMMAND", "npx"),
			Args:       []string{"-y", envOr("BROKERD_WP_ROUTER_PACKAGE", "wp-mcp-router@latest")},
			ConfigPath: envOr("BROKERD_WP_ROUTER_CONFIG", filepath.Join(home, ".config", "wp-mcp-router", "sites.json")),
			Timeout:    envDuration("BROKERD_WP_TIMEOUT", 120*time.Second),
		},
		Memory: Memory{
			URL:     envOr("BROKERD_AUTOMEM_URL", "https://memory-service.up.railway.app"),
			Timeout: envDuration("BROKERD_AUTOMEM_TIMEOUT", 30*time.Second),
		},
	}
}

// LoadSecrets reads the owner-only secrets file. A missing file is not an error:
// adapters whose credential is absent report BROKER_UPSTREAM_UNAVAILABLE.
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
	if err := json.Unmarshal(raw, &c.Secrets); err != nil {
		return fmt.Errorf("parse secrets: %w", err)
	}
	return nil
}
