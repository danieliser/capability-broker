package upstream

import (
	"context"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/danieliser/capability-broker/pkg/adapter"
)

// extras are an upstream's prompts, resources and resource templates, passed through to owner clients.
type extras struct {
	prompts   []*mcp.Prompt
	resources []*mcp.Resource
	templates []*mcp.ResourceTemplate
}

func listExtras(ctx context.Context, s *mcp.ClientSession) extras {
	var e extras
	ir := s.InitializeResult()
	if ir == nil || ir.Capabilities == nil {
		return e
	}
	c := ir.Capabilities
	if c.Prompts != nil {
		cursor := ""
		for {
			r, err := s.ListPrompts(ctx, &mcp.ListPromptsParams{Cursor: cursor})
			if err != nil {
				break
			}
			e.prompts = append(e.prompts, r.Prompts...)
			if cursor = r.NextCursor; cursor == "" {
				break
			}
		}
	}
	if c.Resources != nil {
		cursor := ""
		for {
			r, err := s.ListResources(ctx, &mcp.ListResourcesParams{Cursor: cursor})
			if err != nil {
				break
			}
			e.resources = append(e.resources, r.Resources...)
			if cursor = r.NextCursor; cursor == "" {
				break
			}
		}
		cursor = ""
		for {
			r, err := s.ListResourceTemplates(ctx, &mcp.ListResourceTemplatesParams{Cursor: cursor})
			if err != nil {
				break
			}
			e.templates = append(e.templates, r.ResourceTemplates...)
			if cursor = r.NextCursor; cursor == "" {
				break
			}
		}
	}
	return e
}

func (a *Adapter) setExtras(e extras) {
	a.extrasMu.Lock()
	a.extras = e
	a.extrasMu.Unlock()
}

// Prompts, Resources and ResourceTemplates are the upstream's current lists (empty until connected).
func (a *Adapter) Prompts() []*mcp.Prompt {
	a.extrasMu.RLock()
	defer a.extrasMu.RUnlock()
	return a.extras.prompts
}

func (a *Adapter) Resources() []*mcp.Resource {
	a.extrasMu.RLock()
	defer a.extrasMu.RUnlock()
	return a.extras.resources
}

func (a *Adapter) ResourceTemplates() []*mcp.ResourceTemplate {
	a.extrasMu.RLock()
	defer a.extrasMu.RUnlock()
	return a.extras.templates
}

// GetPrompt and ReadResource forward to the upstream unchanged.
func (a *Adapter) GetPrompt(ctx context.Context, name string, args map[string]string) (*mcp.GetPromptResult, error) {
	a.touch()
	defer a.untouch()
	s, _, err := a.ensure(ctx)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, a.spec.CallTimeout)
	defer cancel()
	return s.GetPrompt(cctx, &mcp.GetPromptParams{Name: name, Arguments: args})
}

func (a *Adapter) ReadResource(ctx context.Context, uri string) (*mcp.ReadResourceResult, error) {
	a.touch()
	defer a.untouch()
	s, _, err := a.ensure(ctx)
	if err != nil {
		return nil, err
	}
	cctx, cancel := context.WithTimeout(ctx, a.spec.CallTimeout)
	defer cancel()
	return s.ReadResource(cctx, &mcp.ReadResourceParams{URI: uri})
}

// rediscover re-reads tools, prompts and resources after the upstream announced a change.
func (a *Adapter) rediscover() {
	a.mu.Lock()
	s := a.session
	a.mu.Unlock()
	if s == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	tools, err := discover(ctx, s)
	if err != nil {
		a.log("rediscover failed", "connector", a.spec.Name, "err", adapter.Truncate(err.Error(), 200))
		return
	}
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		names = append(names, t.Name)
	}
	conn := a.cat.Ensure(a.spec.Name, names)
	a.mu.Lock()
	if a.session == s {
		a.tools, a.conn = tools, conn
	}
	a.mu.Unlock()
	a.setExtras(listExtras(ctx, s))
	a.log("upstream lists changed", "connector", a.spec.Name, "tools", len(tools))
	if a.OnChange != nil {
		a.OnChange()
	}
}
