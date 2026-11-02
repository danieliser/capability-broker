package api

import (
	"net/http"

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
	return mux
}
