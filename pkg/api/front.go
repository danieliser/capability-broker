package api

import (
	"net/http"

	"github.com/danieliser/capability-broker/pkg/buildinfo"
	"github.com/danieliser/capability-broker/pkg/mcpfront"
)

// FrontHandler is the loopback surface agents and owner clients reach. It has
// no control-plane routes: /v1/* answers 404 here by construction.
func FrontHandler(front *mcpfront.Front, connectors []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "build": buildinfo.Current()})
	})
	for _, name := range connectors {
		mux.Handle("/mcp/"+name, front.Handler(name))
	}
	mux.HandleFunc("/mcp/{connector}", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "BROKER_UNKNOWN_CONNECTOR", "no connector "+r.PathValue("connector"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		writeErr(w, http.StatusNotFound, "BROKER_NOT_FOUND", "not found")
	})
	return mux
}
