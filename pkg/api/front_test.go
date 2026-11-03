package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalOriginRefusesOtherSites(t *testing.T) {
	h := localOrigin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusTeapot) }))
	for origin, want := range map[string]int{"": http.StatusTeapot, "http://localhost:3000": http.StatusTeapot,
		"http://127.0.0.1:8890": http.StatusTeapot, "https://evil.example": http.StatusForbidden, "http://127.0.0.1.evil.example": http.StatusForbidden, "vscode-file://vscode-app": http.StatusTeapot} {
		r := httptest.NewRequest("POST", "/mcp", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want {
			t.Errorf("origin %q: got %d want %d", origin, w.Code, want)
		}
	}
}
