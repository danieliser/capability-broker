// Package api wires the two listeners: the loopback front (MCP endpoints and a
// credential-free health check) and the admin surface on a Unix socket, which
// is the only place grants are minted, revoked, catalogs pushed and receipts read.
package api

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danieliser/capability-broker/pkg/adapter"
	"github.com/danieliser/capability-broker/pkg/audit"
	"github.com/danieliser/capability-broker/pkg/auth"
	"github.com/danieliser/capability-broker/pkg/buildinfo"
	"github.com/danieliser/capability-broker/pkg/catalog"
	"github.com/danieliser/capability-broker/pkg/mcpfront"
	"github.com/danieliser/capability-broker/pkg/registry"
)

type Admin struct {
	Token    string
	Store    *auth.Store
	Catalog  *catalog.Catalog
	Audit    *audit.Writer
	Front    *mcpfront.Front
	Registry *registry.Registry
	// Reload re-reads connectors.yaml, secrets.json and catalog.d and applies the difference.
	Reload   func(ctx context.Context) (map[string]any, error)
	DB       *sql.DB
	FrontURL string // http://127.0.0.1:8890
	Started  time.Time
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": msg}})
}

func (a *Admin) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, ok := auth.BearerFromHeader(r.Header.Get("Authorization"))
		if !ok || subtle.ConstantTimeCompare([]byte(tok), []byte(a.Token)) != 1 {
			writeErr(w, http.StatusUnauthorized, "ADMIN_UNAUTHENTICATED", "admin token required")
			return
		}
		next(w, r)
	}
}

// Handler builds the admin mux. All routes are versioned under /v1.
func (a *Admin) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/status", a.auth(a.status))
	mux.HandleFunc("POST /v1/grants", a.auth(a.mintGrant))
	mux.HandleFunc("GET /v1/grants", a.auth(a.listGrants))
	mux.HandleFunc("DELETE /v1/grants/{id}", a.auth(a.revokeGrant))
	mux.HandleFunc("DELETE /v1/sessions/{id}/grants", a.auth(a.revokeSession))
	mux.HandleFunc("POST /v1/catalog/queries", a.auth(a.pushQueries))
	mux.HandleFunc("GET /v1/catalog/queries", a.auth(a.listQueries))
	mux.HandleFunc("GET /v1/receipts", a.auth(a.receipts))
	mux.HandleFunc("POST /v1/reload", a.auth(a.reload))
	return mux
}

func (a *Admin) status(w http.ResponseWriter, r *http.Request) {
	health := map[string]string{}
	for name, ad := range a.Registry.Snapshot() {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		if err := ad.Health(ctx); err != nil {
			health[name] = "degraded: " + adapter.Truncate(err.Error(), 120)
		} else {
			health[name] = "ok"
		}
		cancel()
	}
	live, _ := a.Store.List(r.Context(), true)
	n, _ := a.Audit.Count(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"build": buildinfo.Current(), "started_at": a.Started.UTC().Format(time.RFC3339), "front_url": a.FrontURL,
		"policy_version": a.Catalog.PolicyVersion(), "connectors": health, "live_grants": len(live), "receipts": n,
	})
}

type mintRequest struct {
	Kind          string            `json:"kind"`
	Name          string            `json:"name"`
	AgentID       string            `json:"agent_id"`
	ProjectKey    string            `json:"project_key"`
	SessionID     string            `json:"session_id"`
	RunID         string            `json:"run_id"`
	ParentGrantID string            `json:"parent_grant_id"`
	Capabilities  json.RawMessage   `json:"capabilities"`
	TTLSeconds    int               `json:"ttl_seconds"`
	IssuedBy      string            `json:"issued_by"`
	Owner         bool              `json:"owner"` // client tokens: use the owner profile
	Servers       map[string]string `json:"-"`
}

type mintResponse struct {
	GrantID   string            `json:"grant_id"`
	Token     string            `json:"token"`
	ExpiresAt *time.Time        `json:"expires_at,omitempty"`
	Servers   map[string]string `json:"servers"` // connector → front URL
}

func (a *Admin) mintGrant(w http.ResponseWriter, r *http.Request) {
	var req mintRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "ADMIN_BAD_REQUEST", err.Error())
		return
	}
	var caps auth.Capabilities
	if req.Owner {
		caps = auth.OwnerCapabilities()
	} else {
		c, err := auth.ParseCapabilities(req.Capabilities)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "ADMIN_BAD_CAPABILITIES", err.Error())
			return
		}
		for _, name := range c.Connectors() {
			if _, ok := a.Registry.Lookup(name); !ok {
				writeErr(w, http.StatusBadRequest, "ADMIN_BAD_CAPABILITIES", "unknown connector "+name)
				return
			}
			if cap := c[name]; name == "clickhouse" {
				for _, q := range cap.Queries {
					if _, ok := a.Catalog.Query(q); !ok {
						writeErr(w, http.StatusBadRequest, "ADMIN_BAD_CAPABILITIES", "unknown named query "+q+" (push the project's queries first)")
						return
					}
				}
			}
		}
		caps = c
	}
	issuedBy := req.IssuedBy
	if issuedBy == "" {
		issuedBy = "admin"
	}
	g, token, err := a.Store.Mint(r.Context(), auth.MintRequest{Kind: auth.Kind(req.Kind), Name: req.Name, AgentID: req.AgentID,
		ProjectKey: req.ProjectKey, SessionID: req.SessionID, RunID: req.RunID, ParentGrantID: req.ParentGrantID, Capabilities: caps,
		PolicyVersion: a.Catalog.PolicyVersion(), IssuedBy: issuedBy, TTL: time.Duration(req.TTLSeconds) * time.Second})
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ADMIN_MINT_FAILED", err.Error())
		return
	}
	servers := map[string]string{}
	connectors := caps.Connectors()
	if caps.IsOwner() {
		for _, name := range a.Registry.Names() {
			connectors = append(connectors, name)
		}
	}
	for _, name := range connectors {
		servers[name] = a.FrontURL + "/mcp/" + name
	}
	servers["broker"] = a.FrontURL + "/mcp"
	writeJSON(w, http.StatusCreated, mintResponse{GrantID: g.ID, Token: token, ExpiresAt: g.ExpiresAt, Servers: servers})
}

func (a *Admin) listGrants(w http.ResponseWriter, r *http.Request) {
	live := r.URL.Query().Get("live") != "0"
	list, err := a.Store.List(r.Context(), live)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ADMIN_LIST_FAILED", err.Error())
		return
	}
	if list == nil {
		list = []auth.GrantSummary{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": list})
}

func (a *Admin) revokeGrant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "admin"
	}
	if err := a.Store.Revoke(r.Context(), id, reason); err != nil {
		writeErr(w, http.StatusNotFound, "ADMIN_GRANT_NOT_FOUND", err.Error())
		return
	}
	a.Front.Invalidate(id)
	writeJSON(w, http.StatusOK, map[string]any{"revoked": id})
}

func (a *Admin) revokeSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	reason := r.URL.Query().Get("reason")
	if reason == "" {
		reason = "session ended"
	}
	n, err := a.Store.RevokeSession(r.Context(), id, reason)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ADMIN_REVOKE_FAILED", err.Error())
		return
	}
	a.Front.Invalidate("")
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "revoked": n})
}

type pushRequest struct {
	Project  string `json:"project"`
	YAML     string `json:"yaml"`
	PushedBy string `json:"pushed_by"`
}

func (a *Admin) pushQueries(w http.ResponseWriter, r *http.Request) {
	var req pushRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&req); err != nil || strings.TrimSpace(req.Project) == "" {
		writeErr(w, http.StatusBadRequest, "ADMIN_BAD_REQUEST", "project and yaml are required")
		return
	}
	digest, err := a.Catalog.PushQueries(req.Project, []byte(req.YAML))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ADMIN_BAD_CATALOG", err.Error())
		return
	}
	by := req.PushedBy
	if by == "" {
		by = "admin"
	}
	if _, err := a.DB.ExecContext(r.Context(), `INSERT INTO catalog_overlays (connector, project_key, yaml, digest, pushed_by, pushed_at)
		VALUES ('clickhouse', ?, ?, ?, ?, ?) ON CONFLICT(connector, project_key) DO UPDATE SET yaml=excluded.yaml, digest=excluded.digest,
		pushed_by=excluded.pushed_by, pushed_at=excluded.pushed_at`, req.Project, req.YAML, digest, by, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		writeErr(w, http.StatusInternalServerError, "ADMIN_PERSIST_FAILED", err.Error())
		return
	}
	a.Front.Invalidate("")
	writeJSON(w, http.StatusOK, map[string]any{"project": req.Project, "digest": digest, "policy_version": a.Catalog.PolicyVersion()})
}

func (a *Admin) listQueries(w http.ResponseWriter, r *http.Request) {
	rows, err := a.DB.QueryContext(r.Context(), `SELECT project_key, digest, pushed_by, pushed_at FROM catalog_overlays WHERE connector='clickhouse'`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ADMIN_LIST_FAILED", err.Error())
		return
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var p, d, by, at string
		if err := rows.Scan(&p, &d, &by, &at); err == nil {
			out = append(out, map[string]any{"project": p, "digest": d, "pushed_by": by, "pushed_at": at})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"overlays": out, "policy_version": a.Catalog.PolicyVersion()})
}

func (a *Admin) reload(w http.ResponseWriter, r *http.Request) {
	if a.Reload == nil {
		writeErr(w, http.StatusNotImplemented, "ADMIN_NO_RELOAD", "reload is not wired")
		return
	}
	out, err := a.Reload(r.Context())
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ADMIN_RELOAD_FAILED", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *Admin) receipts(w http.ResponseWriter, r *http.Request) {
	after, _ := strconv.ParseInt(r.URL.Query().Get("after"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := a.Audit.List(r.Context(), after, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "ADMIN_LIST_FAILED", err.Error())
		return
	}
	if list == nil {
		list = []audit.Receipt{}
	}
	var cursor int64 = after
	if len(list) > 0 {
		cursor = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"receipts": list, "cursor": cursor})
}

// RestoreOverlays reloads pushed named-query overlays at startup.
func RestoreOverlays(ctx context.Context, db *sql.DB, cat *catalog.Catalog) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT project_key, yaml FROM catalog_overlays WHERE connector='clickhouse'`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var project, y string
		if err := rows.Scan(&project, &y); err != nil {
			return n, err
		}
		if _, err := cat.PushQueries(project, []byte(y)); err != nil {
			return n, fmt.Errorf("overlay %s: %w", project, err)
		}
		n++
	}
	return n, rows.Err()
}
