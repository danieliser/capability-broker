package mcpfront

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Owner clients also get each upstream's prompts, resources and resource templates, as if connected directly.
// Agents do not: their grants cover tools only.

func serverCaps(owner bool) *mcp.ServerCapabilities {
	c := &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}}
	if owner {
		// Declared up front so prompts or resources that appear later can be announced.
		c.Prompts = &mcp.PromptCapabilities{ListChanged: true}
		c.Resources = &mcp.ResourceCapabilities{ListChanged: true}
	}
	return c
}

type extrasSource interface {
	Prompts() []*mcp.Prompt
	Resources() []*mcp.Resource
	ResourceTemplates() []*mcp.ResourceTemplate
	GetPrompt(ctx context.Context, name string, args map[string]string) (*mcp.GetPromptResult, error)
	ReadResource(ctx context.Context, uri string) (*mcp.ReadResourceResult, error)
}

func defHash(v any) string {
	b, _ := json.Marshal(v)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// syncExtras edits the live server's prompts, resources and templates in place. keep holds connectors that
// are still starting: their current entries stay.
func (f *Front) syncExtras(ctx context.Context, ls *liveServer, connectors []string, keep map[string]bool) {
	type promptDef struct {
		connector, upstream string
		p                   *mcp.Prompt
	}
	prompts := map[string]promptDef{}
	resources := map[string]string{} // uri → connector
	resDefs := map[string]*mcp.Resource{}
	templates := map[string]string{}
	tmplDefs := map[string]*mcp.ResourceTemplate{}
	promptCount := map[string]int{}
	for _, name := range connectors {
		ad, ok := f.Registry.Lookup(name)
		if !ok {
			continue
		}
		src, ok := ad.(extrasSource)
		if !ok {
			continue
		}
		sp := f.specOf(name)
		if sp == nil || !sp.Strips("prompts") {
			for _, p := range src.Prompts() {
				promptCount[p.Name]++
			}
		}
	}
	for _, name := range connectors {
		ad, ok := f.Registry.Lookup(name)
		if !ok {
			continue
		}
		src, ok := ad.(extrasSource)
		if !ok {
			continue
		}
		sp := f.specOf(name)
		if sp == nil || !sp.Strips("prompts") {
			for _, p := range src.Prompts() {
				exported := p.Name
				if ls.endpoint == AggregateEndpoint && (!ls.plain || promptCount[p.Name] > 1) {
					exported = name + "__" + p.Name
				}
				prompts[exported] = promptDef{connector: name, upstream: p.Name, p: p}
			}
		}
		if sp == nil || !sp.Strips("resources") {
			for _, r := range src.Resources() {
				if _, taken := resources[r.URI]; !taken {
					resources[r.URI], resDefs[r.URI] = name, r
				}
			}
			for _, t := range src.ResourceTemplates() {
				if _, taken := templates[t.URITemplate]; !taken {
					templates[t.URITemplate], tmplDefs[t.URITemplate] = name, t
				}
			}
		}
	}
	// Remove what is gone (unless its connector is still starting).
	var rmPrompts, rmRes, rmTmpl []string
	for key, owner := range ls.extOwner() {
		if keep[owner] {
			continue
		}
		kind, id := key[:2], key[2:]
		switch kind {
		case "p:":
			if _, ok := prompts[id]; !ok {
				rmPrompts = append(rmPrompts, id)
			}
		case "r:":
			if _, ok := resources[id]; !ok {
				rmRes = append(rmRes, id)
			}
		case "t:":
			if _, ok := templates[id]; !ok {
				rmTmpl = append(rmTmpl, id)
			}
		}
	}
	if len(rmPrompts) > 0 {
		ls.server.RemovePrompts(rmPrompts...)
	}
	if len(rmRes) > 0 {
		ls.server.RemoveResources(rmRes...)
	}
	if len(rmTmpl) > 0 {
		ls.server.RemoveResourceTemplates(rmTmpl...)
	}
	for _, id := range rmPrompts {
		delete(ls.extHash, "p:"+id)
	}
	for _, id := range rmRes {
		delete(ls.extHash, "r:"+id)
	}
	for _, id := range rmTmpl {
		delete(ls.extHash, "t:"+id)
	}
	for exported, d := range prompts {
		p := *d.p
		p.Name = exported
		h := d.connector + "|" + defHash(&p)
		if ls.extHash["p:"+exported] == h {
			continue
		}
		connector, upstream := d.connector, d.upstream
		ls.server.AddPrompt(&p, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
			ad, ok := f.Registry.Lookup(connector)
			if !ok {
				return nil, errNoConnector(connector)
			}
			return ad.(extrasSource).GetPrompt(ctx, upstream, req.Params.Arguments)
		})
		ls.extHash["p:"+exported] = h
	}
	for uri, connector := range resources {
		r := *resDefs[uri]
		h := connector + "|" + defHash(&r)
		if ls.extHash["r:"+uri] == h {
			continue
		}
		conn := connector
		ls.server.AddResource(&r, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			ad, ok := f.Registry.Lookup(conn)
			if !ok {
				return nil, errNoConnector(conn)
			}
			return ad.(extrasSource).ReadResource(ctx, req.Params.URI)
		})
		ls.extHash["r:"+uri] = h
	}
	for tpl, connector := range templates {
		t := *tmplDefs[tpl]
		h := connector + "|" + defHash(&t)
		if ls.extHash["t:"+tpl] == h {
			continue
		}
		conn := connector
		ls.server.AddResourceTemplate(&t, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
			ad, ok := f.Registry.Lookup(conn)
			if !ok {
				return nil, errNoConnector(conn)
			}
			return ad.(extrasSource).ReadResource(ctx, req.Params.URI)
		})
		ls.extHash["t:"+tpl] = h
	}
}

// extOwner maps each tracked extra to its connector (stored as the hash prefix).
func (ls *liveServer) extOwner() map[string]string {
	out := make(map[string]string, len(ls.extHash))
	for k, h := range ls.extHash {
		if i := strings.Index(h, "|"); i > 0 {
			out[k] = h[:i]
		}
	}
	return out
}

type connectorGone string

func (e connectorGone) Error() string { return "connector " + string(e) + " is no longer configured" }

func errNoConnector(name string) error { return connectorGone(name) }
