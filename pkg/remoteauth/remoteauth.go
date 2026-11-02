// Package remoteauth signs the broker in to remote MCP servers with OAuth
// (authorization code + PKCE, discovery and dynamic client registration from the
// Go MCP SDK). One sign-in per connector serves every client and agent; tokens
// live in <data-dir>/oauth/<connector>.json (0600) and refresh on their own.
//
// A daemon must never pop a browser on its own, so a connector without a token
// fails fast with ErrNeedsAuth until the owner runs `brokerd auth <connector>`,
// which marks the connector interactive, gets the authorization URL, opens it,
// and the browser comes back to the broker's /oauth/callback.
package remoteauth

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/danieliser/capability-broker/pkg/connectors"
)

// ErrNeedsAuth means the connector has no usable token and nobody is signing in.
var ErrNeedsAuth = errors.New("needs sign-in")

const CallbackPath = "/oauth/callback"

// UserAgent identifies the broker to remote servers (and keeps bot protection from refusing Go's default).
const UserAgent = "brokerd (capability-broker; +https://github.com/danieliser/capability-broker)"

// HTTP1Transport speaks HTTP/1.1 only: Cloudflare bot management challenges Go's HTTP/2 client fingerprint
// (agents.stackoverflow.com answers client registration with a 403 "Just a moment" page) but not HTTP/1.1.
func HTTP1Transport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ForceAttemptHTTP2 = false
	t.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	// The clone may already offer h2 in ALPN; a server that picks it would then get HTTP/1.1 bytes.
	t.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
	return t
}

func httpClient() *http.Client {
	return &http.Client{Timeout: 30 * time.Second, Transport: uaTransport{base: HTTP1Transport()}}
}

type uaTransport struct{ base http.RoundTripper }

func (t uaTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", UserAgent)
	return t.base.RoundTrip(r)
}

// Manager owns every connector's OAuth state.
type Manager struct {
	DataDir     string
	CallbackURL string // e.g. http://127.0.0.1:8890/oauth/callback
	Log         func(msg string, kv ...any)

	mu       sync.Mutex
	handlers map[string]*handler // connector → handler; kept across adapter restarts
	pending  map[string]*pending // state → pending sign-in
}

type pending struct {
	connector string
	url       string
	result    chan *auth.AuthorizationResult
	created   time.Time
}

// stored is the on-disk record: enough to refresh without a browser and to reuse the registered client.
type stored struct {
	ClientID     string        `json:"client_id"`
	ClientSecret string        `json:"client_secret,omitempty"`
	AuthURL      string        `json:"auth_url"`
	TokenURL     string        `json:"token_url"`
	AuthStyle    int           `json:"auth_style"`
	Scopes       []string      `json:"scopes,omitempty"`
	Token        *oauth2.Token `json:"token"`
	SignedInAt   time.Time     `json:"signed_in_at"`
}

func (m *Manager) path(name string) string { return filepath.Join(m.DataDir, "oauth", name+".json") }

func (m *Manager) log(msg string, kv ...any) {
	if m.Log != nil {
		m.Log(msg, kv...)
	}
}

func (m *Manager) load(name string) *stored {
	raw, err := os.ReadFile(m.path(name))
	if err != nil {
		return nil
	}
	var s stored
	if json.Unmarshal(raw, &s) != nil || s.Token == nil {
		return nil
	}
	return &s
}

func (m *Manager) save(name string, s *stored) {
	_ = os.MkdirAll(filepath.Dir(m.path(name)), 0o700)
	b, _ := json.MarshalIndent(s, "", "  ")
	tmp := m.path(name) + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, m.path(name))
	}
}

// Forget drops a connector's stored token (sign out).
func (m *Manager) Forget(name string) error {
	m.mu.Lock()
	delete(m.handlers, name)
	m.mu.Unlock()
	err := os.Remove(m.path(name))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}

// Status reports "signed_in", "signing_in" or "needs_sign_in" for an OAuth connector.
func (m *Manager) Status(name string) string {
	m.mu.Lock()
	for _, p := range m.pending {
		if p.connector == name {
			m.mu.Unlock()
			return "signing_in"
		}
	}
	m.mu.Unlock()
	if m.load(name) != nil {
		return "signed_in"
	}
	return "needs_sign_in"
}

// Handler returns the connector's OAuth handler, creating it on first use. The same handler survives
// adapter restarts (a reload must not throw away a refreshed token).
func (m *Manager) Handler(spec *connectors.Spec) (auth.OAuthHandler, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handlers == nil {
		m.handlers = map[string]*handler{}
		m.pending = map[string]*pending{}
	}
	if h, ok := m.handlers[spec.Name]; ok && h.oauthSpecKey == specKey(spec) {
		return h, nil
	}
	h := &handler{m: m, name: spec.Name, oauthSpecKey: specKey(spec)}
	if err := h.build(spec); err != nil {
		return nil, err
	}
	m.handlers[spec.Name] = h
	return h, nil
}

func specKey(s *connectors.Spec) string {
	b, _ := json.Marshal(struct {
		URL string
		O   *connectors.OAuth
	}{s.URL, s.OAuth})
	return string(b)
}

// handler wraps the SDK's AuthorizationCodeHandler: it refuses to start a browser flow unless the owner
// asked for one, and persists every token it obtains or refreshes.
type handler struct {
	m            *Manager
	name         string
	oauthSpecKey string

	mu          sync.Mutex
	inner       *auth.AuthorizationCodeHandler
	interactive bool
}

func (h *handler) build(spec *connectors.Spec) error {
	cfg := &auth.AuthorizationCodeHandlerConfig{
		RedirectURL:              h.m.CallbackURL,
		RequestRefreshToken:      true,
		AcceptUnadvertisedIss:    true,
		AuthorizationCodeFetcher: h.fetch,
		Client:                   httpClient(),
		NewTokenSource: func(ctx context.Context, c *oauth2.Config, t *oauth2.Token) (oauth2.TokenSource, error) {
			s := &stored{ClientID: c.ClientID, ClientSecret: c.ClientSecret, AuthURL: c.Endpoint.AuthURL, TokenURL: c.Endpoint.TokenURL,
				AuthStyle: int(c.Endpoint.AuthStyle), Scopes: c.Scopes, Token: t, SignedInAt: time.Now()}
			h.m.save(h.name, s)
			h.m.log("oauth signed in", "connector", h.name)
			return h.persisting(c, t, s), nil
		},
	}
	if spec.OAuth != nil && len(spec.OAuth.Scopes) > 0 {
		scopes := spec.OAuth.Scopes
		cfg.ScopeFilter = func([]string) []string { return scopes }
	}
	prev := h.m.load(h.name)
	switch {
	case spec.OAuth != nil && spec.OAuth.ClientID != "":
		cfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: spec.OAuth.ClientID}
		if spec.OAuth.ClientSecret != "" {
			cfg.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: spec.OAuth.ClientSecret}
		}
	case prev != nil && prev.ClientID != "":
		// Reuse the client registered last time instead of registering a new one on every sign-in.
		cfg.PreregisteredClient = &oauthex.ClientCredentials{ClientID: prev.ClientID}
		if prev.ClientSecret != "" {
			cfg.PreregisteredClient.ClientSecretAuth = &oauthex.ClientSecretAuth{ClientSecret: prev.ClientSecret}
		}
	default:
		cfg.DynamicClientRegistrationConfig = &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			RedirectURIs:            []string{h.m.CallbackURL},
			ClientName:              "brokerd (capability broker)",
			GrantTypes:              []string{"authorization_code", "refresh_token"},
			ResponseTypes:           []string{"code"},
			TokenEndpointAuthMethod: "none",
		}}
	}
	if prev != nil {
		c := &oauth2.Config{ClientID: prev.ClientID, ClientSecret: prev.ClientSecret, Scopes: prev.Scopes,
			Endpoint: oauth2.Endpoint{AuthURL: prev.AuthURL, TokenURL: prev.TokenURL, AuthStyle: oauth2.AuthStyle(prev.AuthStyle)}}
		cfg.InitialTokenSource = h.persisting(c, prev.Token, prev)
	}
	inner, err := auth.NewAuthorizationCodeHandler(cfg)
	if err != nil {
		return err
	}
	h.inner = inner
	return nil
}

// persisting refreshes through the token endpoint and writes each new token back to disk.
func (h *handler) persisting(c *oauth2.Config, t *oauth2.Token, s *stored) oauth2.TokenSource {
	// Refreshes must use the same HTTP/1.1 client as sign-in: the default client's HTTP/2 fingerprint is
	// challenged by Cloudflare-fronted token endpoints (403 "Just a moment").
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, httpClient())
	base := oauth2.ReuseTokenSource(t, c.TokenSource(ctx, t))
	return &savingSource{base: base, last: t.AccessToken, save: func(nt *oauth2.Token) {
		cp := *s
		cp.Token = nt
		h.m.save(h.name, &cp)
	}}
}

type savingSource struct {
	mu   sync.Mutex
	base oauth2.TokenSource
	last string
	save func(*oauth2.Token)
}

func (s *savingSource) Token() (*oauth2.Token, error) {
	t, err := s.base.Token()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if t.AccessToken != s.last {
		s.last = t.AccessToken
		s.save(t)
	}
	return t, nil
}

func (h *handler) TokenSource(ctx context.Context) (oauth2.TokenSource, error) {
	return h.inner.TokenSource(ctx)
}

func (h *handler) Authorize(ctx context.Context, req *http.Request, resp *http.Response) error {
	h.mu.Lock()
	interactive := h.interactive
	h.mu.Unlock()
	if !interactive {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return fmt.Errorf("%s: %w (run: brokerd auth %s)", h.name, ErrNeedsAuth, h.name)
	}
	return h.inner.Authorize(ctx, req, resp)
}

// fetch publishes the authorization URL for `brokerd auth` and waits for the browser to come back.
func (h *handler) fetch(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
	state := stateOf(args.URL)
	p := &pending{connector: h.name, url: args.URL, result: make(chan *auth.AuthorizationResult, 1), created: time.Now()}
	h.m.mu.Lock()
	h.m.pending[state] = p
	h.m.mu.Unlock()
	defer func() {
		h.m.mu.Lock()
		delete(h.m.pending, state)
		h.m.mu.Unlock()
		h.mu.Lock()
		h.interactive = false
		h.mu.Unlock()
	}()
	h.m.log("oauth waiting for browser sign-in", "connector", h.name)
	select {
	case r := <-p.result:
		return r, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(10 * time.Minute):
		return nil, fmt.Errorf("%s: sign-in not completed within 10 minutes", h.name)
	}
}

// Begin marks the connector interactive; the caller then (re)connects it and polls PendingURL.
func (m *Manager) Begin(spec *connectors.Spec) error {
	hh, err := m.Handler(spec)
	if err != nil {
		return err
	}
	h := hh.(*handler)
	h.mu.Lock()
	h.interactive = true
	h.mu.Unlock()
	return nil
}

// PendingURL is the authorization URL waiting for the owner's browser, if any.
func (m *Manager) PendingURL(name string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.pending {
		if p.connector == name {
			return p.url
		}
	}
	return ""
}

// Callback receives the browser redirect on the front listener.
func (m *Manager) Callback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	state := q.Get("state")
	m.mu.Lock()
	p := m.pending[state]
	m.mu.Unlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if p == nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "<h2>No sign-in is waiting for this response.</h2><p>Run <code>brokerd auth &lt;connector&gt;</code> again.</p>")
		return
	}
	if e := q.Get("error"); e != "" {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintf(w, "<h2>Sign-in for %s was refused: %s</h2>", htmlEscape(p.connector), htmlEscape(e+" "+q.Get("error_description")))
		return
	}
	p.result <- &auth.AuthorizationResult{Code: q.Get("code"), State: state, Iss: q.Get("iss")}
	fmt.Fprintf(w, "<h2>brokerd is signed in to %s.</h2><p>You can close this tab.</p>", htmlEscape(p.connector))
}

func stateOf(u string) string {
	if pu, err := url.Parse(u); err == nil && pu.Query().Get("state") != "" {
		return pu.Query().Get("state")
	}
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
