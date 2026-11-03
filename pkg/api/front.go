package api

import (
	"net/http"
	"net/url"

	"github.com/danieliser/capability-broker/pkg/buildinfo"
	"github.com/danieliser/capability-broker/pkg/mcpfront"
)

// FrontHandler is the loopback surface agents and owner clients reach. It has
// no control-plane routes: /v1/* answers 404 here by construction.
func FrontHandler(front *mcpfront.Front, oauthCallback http.HandlerFunc) http.Handler {
	mux := http.NewServeMux()
	if oauthCallback != nil {
		// The browser lands here after signing the broker in to a remote server; it carries only the code.
		mux.HandleFunc("GET /oauth/callback", oauthCallback)
	}
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "build": buildinfo.Current()})
	})
	// Connectors are resolved per request, so a connector added by a reload is served immediately.
	mux.HandleFunc("/mcp/{connector}", func(w http.ResponseWriter, r *http.Request) {
		front.ServeConnector(w, r, r.PathValue("connector"))
	})
	mux.HandleFunc("/mcp", front.ServeAggregate)
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusNotFound, "BROKER_NOT_FOUND", "not found")
	})
	return localOrigin(mux)
}

// localOrigin refuses requests a web page on another site sends from a browser (DNS rebinding, as the MCP spec
// warns for local servers). Native MCP clients send no Origin header; a page served from this machine is allowed.
func localOrigin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); o != "" && o != "null" {
			u, err := url.Parse(o)
			// Only web pages (http/https) can mount a rebinding attack; app origins (vscode-file://, app://) pass.
			web := err != nil || u.Scheme == "http" || u.Scheme == "https"
			host := ""
			if err == nil {
				host = u.Hostname()
			}
			if web && host != "localhost" && host != "127.0.0.1" && host != "::1" {
				writeErr(w, http.StatusForbidden, "BROKER_ORIGIN_REFUSED", "requests from web pages on other sites are refused")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}
