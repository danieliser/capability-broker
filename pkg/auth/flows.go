package auth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"
)

// ReserveFlowRun admits a key once, independently of grants, runs and UTC days.
// Keep reservations even when dispatch or receipt writes fail: absence of a
// successful response cannot prove that an effect did not happen.
func (s *Store) ReserveFlowRun(ctx context.Context, scope, key string) (bool, error) {
	hash := sha256.Sum256([]byte(key))
	var admitted int
	err := s.db.QueryRowContext(ctx, `INSERT INTO flow_run_keys (scope, key_hash, created_at)
 VALUES (?,?,?) ON CONFLICT(scope, key_hash) DO NOTHING RETURNING 1`,
		scope, hex.EncodeToString(hash[:]), time.Now().UTC().Format(time.RFC3339Nano)).Scan(&admitted)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}
