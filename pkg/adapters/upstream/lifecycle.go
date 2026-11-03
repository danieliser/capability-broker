package upstream

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// On-demand start and idle stop. A connector with `on_demand: true` is listed from its last discovery (cached on
// disk) and started by the first call that needs it; `idle_stop` closes a started upstream after a quiet period.
// Listing never starts it, so a client's connect stays instant and costs no process.

type cacheFile struct {
	Instructions string                  `json:"instructions,omitempty"`
	Tools        []*mcp.Tool             `json:"tools"`
	Prompts      []*mcp.Prompt           `json:"prompts,omitempty"`
	Resources    []*mcp.Resource         `json:"resources,omitempty"`
	Templates    []*mcp.ResourceTemplate `json:"templates,omitempty"`
	SavedAt      time.Time               `json:"saved_at"`
}

func (a *Adapter) cachePath() string {
	if a.CacheDir == "" {
		return ""
	}
	return filepath.Join(a.CacheDir, a.spec.Name+".json")
}

func (a *Adapter) touch() {
	a.inflight.Add(1)
	a.lastUsed.Store(time.Now().UnixNano())
}

func (a *Adapter) untouch() {
	a.lastUsed.Store(time.Now().UnixNano())
	a.inflight.Add(-1)
}

// saveCache records the current discovery for on-demand listing; called after every successful connect.
func (a *Adapter) saveCache() {
	p := a.cachePath()
	if p == "" || !(a.spec.OnDemandStart || a.spec.IdleStop > 0) {
		return
	}
	a.mu.Lock()
	tools := make([]*mcp.Tool, 0, len(a.tools))
	for _, t := range a.tools {
		if t.Upstream != nil {
			tools = append(tools, t.Upstream)
		}
	}
	a.mu.Unlock()
	a.extrasMu.RLock()
	c := cacheFile{Instructions: a.Instructions(), Tools: tools, Prompts: a.extras.prompts, Resources: a.extras.resources,
		Templates: a.extras.templates, SavedAt: time.Now()}
	a.extrasMu.RUnlock()
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	tmp := p + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, p)
	}
}

// LoadCache fills tools, prompts, resources and instructions from the last discovery without starting the
// upstream. It reports false when there is no cache yet (the reloader then starts the connector once).
func (a *Adapter) LoadCache() bool {
	p := a.cachePath()
	if p == "" {
		return false
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return false
	}
	var c cacheFile
	if json.Unmarshal(raw, &c) != nil || len(c.Tools) == 0 {
		return false
	}
	tools := make([]catalogTool, 0, len(c.Tools))
	for _, t := range c.Tools {
		tools = append(tools, toCatalogTool(t))
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	conn := a.cat.Ensure(a.spec.Name, names)
	a.mu.Lock()
	if a.session == nil {
		a.tools, a.conn = tools, conn
	}
	a.mu.Unlock()
	in := c.Instructions
	a.instructions.Store(&in)
	a.setExtras(extras{prompts: c.Prompts, resources: c.Resources, templates: c.Templates})
	a.cached.Store(true)
	return true
}

// WatchIdle stops the upstream after spec.IdleStop without calls, until ctx ends. The cached listing stays, so
// clients still see every tool; the next call starts the upstream again.
func (a *Adapter) WatchIdle(ctx context.Context) {
	if a.spec.IdleStop <= 0 {
		return
	}
	every := a.spec.IdleStop / 4
	if every > 30*time.Second {
		every = 30 * time.Second
	}
	if every < time.Second {
		every = time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if a.closed.Load() {
			return
		}
		if !a.ready.Load() || a.inflight.Load() > 0 {
			continue
		}
		last := time.Unix(0, a.lastUsed.Load())
		if a.lastUsed.Load() == 0 || time.Since(last) < a.spec.IdleStop {
			continue
		}
		if !a.cached.Load() && !a.LoadCache() {
			continue // never stop an upstream we could not list again without starting it
		}
		a.mu.Lock()
		if a.session != nil && a.inflight.Load() == 0 {
			_ = a.session.Close()
			a.session = nil
			a.ready.Store(false)
			a.log("connector stopped after idle", "connector", a.spec.Name, "idle", a.spec.IdleStop.String())
		}
		a.mu.Unlock()
	}
}
