package remoteauth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// A refresh must go through the broker's client (HTTP/1.1, broker User-Agent) and be written back to disk.
func TestRefreshUsesBrokerClientAndPersists(t *testing.T) {
	var gotUA, gotProto string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotProto = r.UserAgent(), r.Proto
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "new", "refresh_token": "r2", "token_type": "Bearer", "expires_in": 900})
	}))
	defer srv.Close()
	m := &Manager{DataDir: t.TempDir()}
	h := &handler{m: m, name: "x"}
	c := &oauth2.Config{ClientID: "id", Endpoint: oauth2.Endpoint{TokenURL: srv.URL, AuthStyle: oauth2.AuthStyleInParams}}
	old := &oauth2.Token{AccessToken: "old", RefreshToken: "r1", Expiry: time.Now().Add(-time.Minute)}
	ts := h.persisting(c, old, &stored{ClientID: "id", TokenURL: srv.URL, Token: old})
	tok, err := ts.Token()
	if err != nil {
		t.Fatal(err)
	}
	if tok.AccessToken != "new" || gotUA != UserAgent || gotProto != "HTTP/1.1" {
		t.Fatalf("token %q ua %q proto %q", tok.AccessToken, gotUA, gotProto)
	}
	if s := m.load("x"); s == nil || s.Token.AccessToken != "new" || s.Token.RefreshToken != "r2" {
		t.Fatalf("not persisted: %+v", s)
	}
}
