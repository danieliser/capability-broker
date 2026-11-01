package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type Kind string

const (
	KindAgent  Kind = "agent"
	KindClient Kind = "client"
)

// Grant is the authority behind one bearer. It is the only identity the broker knows.
type Grant struct {
	ID            string
	Kind          Kind
	Name          string // agent id for agents, client name for owner clients
	AgentID       string
	ProjectKey    string
	SessionID     string
	RunID         string
	ParentGrantID string
	Capabilities  Capabilities
	PolicyVersion string
	IssuedBy      string
	CreatedAt     time.Time
	ExpiresAt     *time.Time
	RevokedAt     *time.Time
}

// Principal is the receipt-facing identity string.
func (g *Grant) Principal() string {
	if g.Kind == KindClient {
		return "client:" + g.Name
	}
	return "agent:" + g.AgentID
}

type MintRequest struct {
	Kind          Kind
	Name          string
	AgentID       string
	ProjectKey    string
	SessionID     string
	RunID         string
	ParentGrantID string
	Capabilities  Capabilities
	PolicyVersion string
	IssuedBy      string
	TTL           time.Duration // zero = no expiry (client tokens only)
}

var (
	ErrUnauthenticated = errors.New("BROKER_UNAUTHENTICATED")
	ErrGrantRevoked    = errors.New("BROKER_GRANT_REVOKED")
	ErrGrantExpired    = errors.New("BROKER_GRANT_EXPIRED")
)

const MaxAgentGrantTTL = 24 * time.Hour

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Mint creates a grant and returns the one-time plaintext token.
func (s *Store) Mint(ctx context.Context, req MintRequest) (*Grant, string, error) {
	if req.Kind != KindAgent && req.Kind != KindClient {
		return nil, "", fmt.Errorf("mint: kind must be agent or client")
	}
	if req.Name == "" {
		return nil, "", fmt.Errorf("mint: name is required")
	}
	if req.Kind == KindAgent {
		if req.AgentID == "" || req.SessionID == "" {
			return nil, "", fmt.Errorf("mint: agent grants need agent_id and session_id")
		}
		if req.TTL <= 0 || req.TTL > MaxAgentGrantTTL {
			req.TTL = MaxAgentGrantTTL
		}
		if req.Capabilities.IsOwner() {
			return nil, "", fmt.Errorf("mint: agent grants cannot carry the owner profile")
		}
	}
	if len(req.Capabilities) == 0 {
		return nil, "", fmt.Errorf("mint: capabilities are required")
	}
	var parentExpiry *time.Time
	if req.ParentGrantID != "" {
		parent, err := s.Get(ctx, req.ParentGrantID)
		if err != nil {
			return nil, "", fmt.Errorf("mint: parent grant: %w", err)
		}
		if err := parent.Check(time.Now()); err != nil {
			return nil, "", fmt.Errorf("mint: parent grant unusable: %w", err)
		}
		req.Capabilities = req.Capabilities.Intersect(parent.Capabilities)
		parentExpiry = parent.ExpiresAt
	}
	prefix := ClientTokenPrefix
	if req.Kind == KindAgent {
		prefix = GrantTokenPrefix
	}
	token, err := randomToken(prefix)
	if err != nil {
		return nil, "", err
	}
	now := time.Now().UTC()
	g := &Grant{
		ID: uuid.NewString(), Kind: req.Kind, Name: req.Name, AgentID: req.AgentID, ProjectKey: req.ProjectKey,
		SessionID: req.SessionID, RunID: req.RunID, ParentGrantID: req.ParentGrantID, Capabilities: req.Capabilities,
		PolicyVersion: req.PolicyVersion, IssuedBy: req.IssuedBy, CreatedAt: now,
	}
	var expires *string
	if req.TTL > 0 || parentExpiry != nil {
		t := now.Add(req.TTL)
		if parentExpiry != nil && (req.TTL <= 0 || t.After(*parentExpiry)) {
			t = *parentExpiry
		}
		g.ExpiresAt = &t
		s := t.Format(time.RFC3339Nano)
		expires = &s
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO grants (id, token_hash, kind, name, agent_id, project_key, session_id, run_id,
		parent_grant_id, capabilities_json, policy_version, issued_by, created_at, expires_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		g.ID, HashToken(token), string(g.Kind), g.Name, nullable(g.AgentID), nullable(g.ProjectKey), nullable(g.SessionID),
		nullable(g.RunID), nullable(g.ParentGrantID), g.Capabilities.JSON(), g.PolicyVersion, g.IssuedBy,
		now.Format(time.RFC3339Nano), expires)
	if err != nil {
		return nil, "", fmt.Errorf("mint: insert: %w", err)
	}
	return g, token, nil
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

const grantColumns = `id, kind, name, COALESCE(agent_id,''), COALESCE(project_key,''), COALESCE(session_id,''), COALESCE(run_id,''),
	COALESCE(parent_grant_id,''), capabilities_json, policy_version, issued_by, created_at, expires_at, revoked_at`

func scanGrant(row interface{ Scan(...any) error }) (*Grant, error) {
	var g Grant
	var kind, caps, created string
	var expires, revoked sql.NullString
	if err := row.Scan(&g.ID, &kind, &g.Name, &g.AgentID, &g.ProjectKey, &g.SessionID, &g.RunID, &g.ParentGrantID,
		&caps, &g.PolicyVersion, &g.IssuedBy, &created, &expires, &revoked); err != nil {
		return nil, err
	}
	g.Kind = Kind(kind)
	c, err := ParseCapabilities([]byte(caps))
	if err != nil {
		return nil, err
	}
	g.Capabilities = c
	g.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if expires.Valid {
		t, _ := time.Parse(time.RFC3339Nano, expires.String)
		g.ExpiresAt = &t
	}
	if revoked.Valid {
		t, _ := time.Parse(time.RFC3339Nano, revoked.String)
		g.RevokedAt = &t
	}
	return &g, nil
}

func (s *Store) Get(ctx context.Context, id string) (*Grant, error) {
	g, err := scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantColumns+` FROM grants WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("grant %s not found", id)
	}
	return g, err
}

// Authenticate resolves a bearer to a usable grant. Revocation and expiry are
// checked on every call; there is no cache.
func (s *Store) Authenticate(ctx context.Context, token string, now time.Time) (*Grant, error) {
	if token == "" {
		return nil, ErrUnauthenticated
	}
	hash := HashToken(token)
	g, err := scanGrant(s.db.QueryRowContext(ctx, `SELECT `+grantColumns+` FROM grants WHERE token_hash=?`, hash))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrUnauthenticated
	}
	if err != nil {
		return nil, err
	}
	// Belt and braces: compare the hash in constant time even though the lookup was keyed by it.
	if subtle.ConstantTimeCompare([]byte(hash), []byte(HashToken(token))) != 1 {
		return nil, ErrUnauthenticated
	}
	if err := g.Check(now); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Grant) Check(now time.Time) error {
	if g.RevokedAt != nil {
		return ErrGrantRevoked
	}
	if g.ExpiresAt != nil && !now.Before(*g.ExpiresAt) {
		return ErrGrantExpired
	}
	return nil
}

func (s *Store) Revoke(ctx context.Context, id, reason string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE grants SET revoked_at=?, revoke_reason=? WHERE id=? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), reason, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("grant %s not found or already revoked", id)
	}
	// Descendants fall with the parent.
	_, err = s.db.ExecContext(ctx, `UPDATE grants SET revoked_at=?, revoke_reason=? WHERE parent_grant_id=? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), "parent revoked: "+reason, id)
	return err
}

// RevokeSession revokes every live grant of a session (PERSIST calls this at terminal state).
func (s *Store) RevokeSession(ctx context.Context, sessionID, reason string) (int64, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE grants SET revoked_at=?, revoke_reason=? WHERE session_id=? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), reason, sessionID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Increment bumps a per-grant counter and returns the new value (for calls_per_run style limits).
func (s *Store) Increment(ctx context.Context, grantID, key string) (int64, error) {
	_, err := s.db.ExecContext(ctx, `INSERT INTO counters (grant_id, key, count) VALUES (?,?,1)
		ON CONFLICT(grant_id, key) DO UPDATE SET count = count + 1`, grantID, key)
	if err != nil {
		return 0, err
	}
	var n int64
	err = s.db.QueryRowContext(ctx, `SELECT count FROM counters WHERE grant_id=? AND key=?`, grantID, key).Scan(&n)
	return n, err
}

func (s *Store) Counter(ctx context.Context, grantID, key string) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE((SELECT count FROM counters WHERE grant_id=? AND key=?),0)`, grantID, key).Scan(&n)
	return n, err
}

type GrantSummary struct {
	ID         string     `json:"id"`
	Kind       Kind       `json:"kind"`
	Name       string     `json:"name"`
	AgentID    string     `json:"agent_id,omitempty"`
	ProjectKey string     `json:"project_key,omitempty"`
	SessionID  string     `json:"session_id,omitempty"`
	Connectors []string   `json:"connectors"`
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

func (s *Store) List(ctx context.Context, liveOnly bool) ([]GrantSummary, error) {
	q := `SELECT ` + grantColumns + ` FROM grants`
	if liveOnly {
		q += ` WHERE revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`
	}
	q += ` ORDER BY created_at DESC`
	var rows *sql.Rows
	var err error
	if liveOnly {
		rows, err = s.db.QueryContext(ctx, q, time.Now().UTC().Format(time.RFC3339Nano))
	} else {
		rows, err = s.db.QueryContext(ctx, q)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GrantSummary
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, GrantSummary{ID: g.ID, Kind: g.Kind, Name: g.Name, AgentID: g.AgentID, ProjectKey: g.ProjectKey,
			SessionID: g.SessionID, Connectors: g.Capabilities.Connectors(), CreatedAt: g.CreatedAt, ExpiresAt: g.ExpiresAt, RevokedAt: g.RevokedAt})
	}
	return out, rows.Err()
}
