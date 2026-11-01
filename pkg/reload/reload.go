// Package reload keeps the running broker in line with its data-root files:
// connectors.yaml, secrets.json and catalog.d/*.yaml. A change is picked up by
// polling (every couple of seconds), SIGHUP, or POST /v1/reload. Added
// connectors start, changed ones restart, removed ones stop, and every live MCP
// server is refreshed in place so connected clients get tools/list_changed.
// A file that fails to parse changes nothing: the last good state keeps serving.
package reload

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/danieliser/capability-broker/pkg/adapters/upstream"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/config"
	"github.com/danieliser/capability-broker/pkg/connectors"
	"github.com/danieliser/capability-broker/pkg/mcpfront"
	"github.com/danieliser/capability-broker/pkg/pins"
	"github.com/danieliser/capability-broker/pkg/registry"
)

type Logger func(msg string, kv ...any)

type Reloader struct {
	Config   config.Config
	Catalog  *catalog.Catalog
	Registry *registry.Registry
	Front    *mcpfront.Front
	Log      Logger
	// Base outlives any single Apply call (an admin request's context ends with its response); children
	// start and run under it.
	Base context.Context
	// Pins swaps npx/uvx commands for pinned installs; nil disables pinning.
	Pins *pins.Manager

	mu        sync.Mutex
	digests   map[string]string // connector → spec digest
	filesSeen string            // fingerprint of the watched files
}

// Result describes what one reconcile did.
type Result struct {
	Added     []string `json:"added"`
	Restarted []string `json:"restarted"`
	Removed   []string `json:"removed"`
	Catalog   bool     `json:"catalog_reloaded"`
	Policy    string   `json:"policy_version"`
}

func (r *Reloader) log(msg string, kv ...any) {
	if r.Log != nil {
		r.Log(msg, kv...)
	}
}

// Apply reads every watched file and reconciles. Safe to call concurrently; calls are serialised.
func (r *Reloader) Apply(ctx context.Context) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.digests == nil {
		r.digests = map[string]string{}
	}
	var res Result
	cfg := r.Config
	cfg.Secrets = map[string]string{}
	if err := cfg.LoadSecrets(); err != nil {
		return res, err
	}
	specs, err := connectors.Load(cfg.ConnectorsPath(), cfg.Secrets)
	if err != nil {
		return res, err
	}
	if r.Pins != nil {
		r.Pins.Apply(specs)
		// Resolve and install missing or stale pins in the background; pins.json changing triggers the next
		// reconcile, which restarts those connectors on the pinned copy.
		raw, _ := connectors.Load(cfg.ConnectorsPath(), cfg.Secrets)
		base := r.Base
		if base == nil {
			base = context.Background()
		}
		go r.Pins.Check(base, raw)
	}
	if err := r.Catalog.Reload(); err != nil {
		return res, fmt.Errorf("catalog: %w", err)
	}
	res.Catalog = true

	want := map[string]*connectors.Spec{}
	for _, s := range specs {
		want[s.Name] = s
	}
	for name, spec := range want {
		d := digest(spec)
		old, had := r.digests[name]
		if had && old == d {
			continue
		}
		ad := upstream.New(spec, r.Catalog, r.Log)
		prev := r.Registry.Set(name, ad)
		r.digests[name] = d
		if prev != nil {
			// Closed in the background: a child still mid-start holds its lock, and a reload must not wait on it.
			if c, ok := prev.(interface{ Close() error }); ok {
				go func() { _ = c.Close() }()
			}
			res.Restarted = append(res.Restarted, name)
		} else {
			res.Added = append(res.Added, name)
		}
		base := r.Base
		if base == nil {
			base = context.Background()
		}
		go r.startWithRetry(base, name, ad)
	}
	for _, name := range r.Registry.Names() {
		if _, ok := want[name]; ok {
			continue
		}
		if prev := r.Registry.Remove(name); prev != nil {
			if c, ok := prev.(interface{ Close() error }); ok {
				go func() { _ = c.Close() }()
			}
		}
		delete(r.digests, name)
		res.Removed = append(res.Removed, name)
	}
	sort.Strings(res.Added)
	sort.Strings(res.Restarted)
	sort.Strings(res.Removed)
	r.Front.Refresh(ctx)
	res.Policy = r.Catalog.PolicyVersion()
	if len(res.Added)+len(res.Restarted)+len(res.Removed) > 0 {
		r.log("connectors reconciled", "added", res.Added, "restarted", res.Restarted, "removed", res.Removed, "policy_version", res.Policy)
	}
	r.filesSeen = r.fingerprint()
	return res, nil
}

// startWithRetry keeps starting a child until it connects, the context ends, or a reload replaces it
// (package runners like npx and uvx can take minutes on a slow network). Every success refreshes live
// servers so clients get tools/list_changed.
func (r *Reloader) startWithRetry(ctx context.Context, name string, ad *upstream.Adapter) {
	backoff := 5 * time.Second
	for attempt := 1; ; attempt++ {
		err := ad.Start(ctx)
		if err == nil {
			r.Front.Refresh(ctx)
			return
		}
		if cur, ok := r.Registry.Lookup(name); !ok || cur != ad {
			return // replaced or removed while starting
		}
		r.log("connector not started; retrying", "connector", name, "attempt", attempt, "retry_in", backoff.String(), "err", err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < time.Minute {
			backoff *= 2
		}
	}
}

// Watch polls the watched files and applies when their fingerprint changes. It returns when ctx ends.
func (r *Reloader) Watch(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			r.mu.Lock()
			seen := r.filesSeen
			r.mu.Unlock()
			if fp := r.fingerprint(); fp != seen {
				if _, err := r.Apply(ctx); err != nil {
					r.log("reload refused; keeping the last good state", "err", err.Error())
					r.mu.Lock()
					r.filesSeen = fp // do not retry the same broken file every tick
					r.mu.Unlock()
				}
			}
		}
	}
}

// fingerprint is the size and mtime of every watched file; contents are hashed only by Apply.
func (r *Reloader) fingerprint() string {
	paths := []string{r.Config.ConnectorsPath(), r.Config.SecretsPath(), filepath.Join(r.Config.DataDir, pins.Filename)}
	matches, _ := filepath.Glob(filepath.Join(r.Config.CatalogDir(), "*.yaml"))
	sort.Strings(matches)
	paths = append(paths, matches...)
	h := sha256.New()
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil {
			fmt.Fprintf(h, "%s|%d|%d;", p, info.Size(), info.ModTime().UnixNano())
		} else {
			fmt.Fprintf(h, "%s|missing;", p)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// digest covers everything that requires a child restart, including resolved secret values. Never logged.
func digest(s *connectors.Spec) string {
	b, _ := json.Marshal(struct {
		Command, Cwd  string
		Args          []string
		Env           map[string]string
		Startup, Call time.Duration
	}{s.Command, s.Cwd, s.Args, s.Env, s.StartupTimeout, s.CallTimeout})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
