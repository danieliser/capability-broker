// Package auth owns bearer tokens and grant records. Tokens are random, hashed
// at rest, and bound to a principal and a capability set.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	GrantTokenPrefix  = "cbg_"
	ClientTokenPrefix = "cbc_"
	AdminTokenPrefix  = "cba_"
)

func randomToken(prefix string) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// BearerFromHeader extracts the token from an Authorization header.
func BearerFromHeader(header string) (string, bool) {
	const prefix = "bearer "
	if len(header) <= len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
		return "", false
	}
	tok := strings.TrimSpace(header[len(prefix):])
	return tok, tok != ""
}

// LoadOrCreateAdminToken mirrors agentd's auth.token: 0600 file, hard-linked into
// place so concurrent starts observe one complete value. Never logged.
func LoadOrCreateAdminToken(path string) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", fmt.Errorf("create data dir: %w", err)
	}
	if raw, err := os.ReadFile(path); err == nil {
		tok := strings.TrimSpace(string(raw))
		if tok == "" {
			return "", fmt.Errorf("admin token file %s is empty", path)
		}
		return tok, nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	tok, err := randomToken(AdminTokenPrefix)
	if err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".admin-token-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if _, err := tmp.WriteString(tok + "\n"); err != nil {
		_ = tmp.Close()
		return "", err
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if err := os.Link(tmpPath, path); err != nil {
		if os.IsExist(err) {
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return "", rerr
			}
			return strings.TrimSpace(string(raw)), nil
		}
		return "", err
	}
	return tok, nil
}
