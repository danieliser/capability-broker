// Package durable opens the broker's SQLite database and applies its schema.
// Receipts are insert-only, enforced by triggers rather than application trust.
package durable

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS grants (
  id TEXT PRIMARY KEY,
  token_hash TEXT NOT NULL UNIQUE,
  kind TEXT NOT NULL CHECK (kind IN ('agent','client')),
  name TEXT NOT NULL,
  agent_id TEXT,
  project_key TEXT,
  session_id TEXT,
  run_id TEXT,
  parent_grant_id TEXT REFERENCES grants(id),
  capabilities_json TEXT NOT NULL,
  policy_version TEXT NOT NULL,
  issued_by TEXT NOT NULL,
  created_at TEXT NOT NULL,
  expires_at TEXT,
  revoked_at TEXT,
  revoke_reason TEXT
);
CREATE INDEX IF NOT EXISTS idx_grants_session ON grants(session_id);
CREATE INDEX IF NOT EXISTS idx_grants_agent ON grants(agent_id, created_at);

CREATE TABLE IF NOT EXISTS counters (
  grant_id TEXT NOT NULL REFERENCES grants(id),
  key TEXT NOT NULL,
  count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (grant_id, key)
);

CREATE TABLE IF NOT EXISTS daily_counters (
  scope TEXT NOT NULL,
  operation TEXT NOT NULL,
  day TEXT NOT NULL,
  count INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (scope, operation, day)
);

CREATE TABLE IF NOT EXISTS flow_run_keys (
  scope TEXT NOT NULL,
  key_hash TEXT NOT NULL,
  created_at TEXT NOT NULL,
  PRIMARY KEY (scope, key_hash)
);

CREATE TABLE IF NOT EXISTS receipts (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  ts TEXT NOT NULL,
  call_id TEXT NOT NULL,
  phase TEXT NOT NULL CHECK (phase IN ('intent','outcome')),
  grant_id TEXT,
  grant_kind TEXT,
  principal TEXT NOT NULL,
  agent_id TEXT,
  project_key TEXT,
  session_id TEXT,
  run_id TEXT,
  parent_grant_id TEXT,
  connector TEXT NOT NULL,
  tool TEXT NOT NULL,
  operation TEXT,
  adapter_version TEXT,
  account TEXT,
  args_digest TEXT,
  args_summary TEXT,
  policy_version TEXT,
  decision TEXT NOT NULL,
  deny_reason TEXT,
  approval_id TEXT,
  idempotency_key TEXT,
  state TEXT NOT NULL,
  upstream_request_id TEXT,
  outcome_class TEXT,
  duration_ms INTEGER,
  request_bytes INTEGER NOT NULL DEFAULT 0,
  response_bytes INTEGER NOT NULL DEFAULT 0,
  verbose INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_receipts_ts ON receipts(ts);
CREATE INDEX IF NOT EXISTS idx_receipts_call ON receipts(call_id);
CREATE INDEX IF NOT EXISTS idx_receipts_grant ON receipts(grant_id, ts);
CREATE TRIGGER IF NOT EXISTS receipts_no_update BEFORE UPDATE ON receipts
  BEGIN SELECT RAISE(ABORT, 'receipts are append-only'); END;
CREATE TRIGGER IF NOT EXISTS receipts_no_delete BEFORE DELETE ON receipts
  BEGIN SELECT RAISE(ABORT, 'receipts are append-only'); END;

CREATE TABLE IF NOT EXISTS approvals (
  id TEXT PRIMARY KEY,
  grant_id TEXT NOT NULL REFERENCES grants(id),
  connector TEXT NOT NULL,
  operation TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  action_json TEXT NOT NULL,
  action_digest TEXT NOT NULL,
  policy_version TEXT NOT NULL,
  state TEXT NOT NULL CHECK (state IN ('pending','approved','denied','expired','consumed')),
  judgement_question_id TEXT,
  decided_by TEXT,
  decided_at TEXT,
  created_at TEXT NOT NULL,
  expires_at TEXT NOT NULL,
  UNIQUE (grant_id, idempotency_key)
);

CREATE TABLE IF NOT EXISTS catalog_overlays (
  connector TEXT NOT NULL,
  project_key TEXT NOT NULL,
  yaml TEXT NOT NULL,
  digest TEXT NOT NULL,
  pushed_by TEXT NOT NULL,
  pushed_at TEXT NOT NULL,
  PRIMARY KEY (connector, project_key)
);
`

// Open creates the data root (0700) if needed and opens the database with WAL
// and foreign keys on. The schema is idempotent.
func Open(ctx context.Context, path string) (*sql.DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil && !os.IsNotExist(err) {
		_ = db.Close()
		return nil, fmt.Errorf("secure database: %w", err)
	}
	return db, nil
}
