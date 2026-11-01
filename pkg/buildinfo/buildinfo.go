// Package buildinfo carries the release identity stamped at build time via
// -ldflags "-X github.com/danieliser/capability-broker/pkg/buildinfo.Version=… -X …Commit=…".
package buildinfo

import (
	"fmt"
	"strings"
)

var (
	Version = "dev"
	Commit  = "unknown"
)

type Identity struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

func Current() Identity { return Identity{Version: Version, Commit: Commit} }

// Verify checks an exact "version@commit" identity, mirroring agentd --require-build.
func (i Identity) Verify(expected string) error {
	parts := strings.SplitN(expected, "@", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return fmt.Errorf("build identity must be version@commit, got %q", expected)
	}
	if i.Version != parts[0] || i.Commit != parts[1] {
		return fmt.Errorf("build identity mismatch: have %s@%s, want %s", i.Version, i.Commit, expected)
	}
	return nil
}
