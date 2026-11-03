// Package audit writes receipts: an intent row before any upstream request and
// an outcome row after. The table is append-only (triggers in pkg/durable).
// A failed intent write denies the call.
package audit

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/danieliser/capability-broker/pkg/auth"
)

type State string

const (
	StateIntent         State = "intent"
	StateDenied         State = "denied"
	StateCompleted      State = "completed"
	StateFailed         State = "failed"
	StateOutcomeUnknown State = "outcome_unknown"
)

type Receipt struct {
	ID                int64             `json:"id"`
	TS                string            `json:"ts"`
	CallID            string            `json:"call_id"`
	Phase             string            `json:"phase"`
	GrantID           string            `json:"grant_id,omitempty"`
	GrantKind         string            `json:"grant_kind,omitempty"`
	Principal         string            `json:"principal"`
	AgentID           string            `json:"agent_id,omitempty"`
	ProjectKey        string            `json:"project_key,omitempty"`
	SessionID         string            `json:"session_id,omitempty"`
	RunID             string            `json:"run_id,omitempty"`
	ParentGrantID     string            `json:"parent_grant_id,omitempty"`
	Connector         string            `json:"connector"`
	Tool              string            `json:"tool"`
	Operation         string            `json:"operation,omitempty"`
	AdapterVersion    string            `json:"adapter_version,omitempty"`
	Account           string            `json:"account,omitempty"`
	ArgsDigest        string            `json:"args_digest,omitempty"`
	ArgsSummary       string            `json:"args_summary,omitempty"`
	PolicyVersion     string            `json:"policy_version,omitempty"`
	Decision          string            `json:"decision"`
	DenyReason        string            `json:"deny_reason,omitempty"`
	ApprovalID        string            `json:"approval_id,omitempty"`
	IdempotencyKey    string            `json:"idempotency_key,omitempty"`
	State             State             `json:"state"`
	UpstreamRequestID string            `json:"upstream_request_id,omitempty"`
	OutcomeClass      string            `json:"outcome_class,omitempty"`
	DurationMs        int64             `json:"duration_ms"`
	RequestBytes      int64             `json:"request_bytes"`
	ResponseBytes     int64             `json:"response_bytes"`
	Verbose           bool              `json:"verbose"`
	Extra             map[string]string `json:"-"`
}

type Writer struct {
	db *sql.DB
	// Outcome and denial rows are queued and written in batches off the call path; the intent row that must exist
	// before an upstream call is still written synchronously (a failed intent write denies the call).
	queue chan Receipt
	done  chan struct{}
}

func NewWriter(db *sql.DB) *Writer {
	w := &Writer{db: db, queue: make(chan Receipt, 512), done: make(chan struct{})}
	go w.drain()
	return w
}

// drain writes queued rows in one transaction per batch.
func (w *Writer) drain() {
	defer close(w.done)
	for r := range w.queue {
		batch := []Receipt{r}
	more:
		for len(batch) < 256 {
			select {
			case x, ok := <-w.queue:
				if !ok {
					break more
				}
				batch = append(batch, x)
			default:
				break more
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if tx, err := w.db.BeginTx(ctx, nil); err == nil {
			ok := true
			for _, b := range batch {
				if err := insertWith(ctx, tx, b); err != nil {
					ok = false
					break
				}
			}
			if ok {
				_ = tx.Commit()
			} else {
				_ = tx.Rollback()
				for _, b := range batch { // fall back row by row so one bad row loses nothing else
					_ = insertWith(ctx, w.db, b)
				}
			}
		}
		cancel()
	}
}

// Flush waits until every queued row is written (tests and receipts reads), up to a few seconds.
func (w *Writer) Flush() {
	deadline := time.Now().Add(5 * time.Second)
	for len(w.queue) > 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(5 * time.Millisecond) // the batch in hand
}

// Close drains the queue; the daemon calls it on shutdown.
func (w *Writer) Close() {
	close(w.queue)
	select {
	case <-w.done:
	case <-time.After(5 * time.Second):
	}
}

func (w *Writer) enqueue(r Receipt) {
	select {
	case w.queue <- r:
	default:
		_ = w.insert(context.Background(), r) // queue full: write inline rather than drop a receipt
	}
}

// Digest hashes the normalised arguments; the summary is a redacted, truncated rendering.
func Digest(args map[string]any) (digest, summary string) {
	b, _ := json.Marshal(args)
	sum := sha256.Sum256(b)
	s := string(b)
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return hex.EncodeToString(sum[:]), s
}

// Call is what the front hands the writer at intent time.
type Call struct {
	Grant          *auth.Grant
	Principal      string // for unauthenticated denials
	Connector      string
	Tool           string
	Operation      string
	AdapterVersion string
	Account        string
	Args           map[string]any
	PolicyVersion  string
	IdempotencyKey string
	Verbose        bool
	RequestBytes   int64
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func (w *Writer) insert(ctx context.Context, r Receipt) error { return insertWith(ctx, w.db, r) }

func insertWith(ctx context.Context, db execer, r Receipt) error {
	_, err := db.ExecContext(ctx, `INSERT INTO receipts (ts, call_id, phase, grant_id, grant_kind, principal, agent_id, project_key,
		session_id, run_id, parent_grant_id, connector, tool, operation, adapter_version, account, args_digest, args_summary,
		policy_version, decision, deny_reason, approval_id, idempotency_key, state, upstream_request_id, outcome_class,
		duration_ms, request_bytes, response_bytes, verbose)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.TS, r.CallID, r.Phase, nz(r.GrantID), nz(r.GrantKind), r.Principal, nz(r.AgentID), nz(r.ProjectKey), nz(r.SessionID),
		nz(r.RunID), nz(r.ParentGrantID), r.Connector, r.Tool, nz(r.Operation), nz(r.AdapterVersion), nz(r.Account), nz(r.ArgsDigest),
		nz(r.ArgsSummary), nz(r.PolicyVersion), r.Decision, nz(r.DenyReason), nz(r.ApprovalID), nz(r.IdempotencyKey), string(r.State),
		nz(r.UpstreamRequestID), nz(r.OutcomeClass), r.DurationMs, r.RequestBytes, r.ResponseBytes, boolInt(r.Verbose))
	if err != nil {
		return fmt.Errorf("audit write: %w", err)
	}
	return nil
}

func nz(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func base(c Call, decision string) Receipt {
	r := Receipt{TS: time.Now().UTC().Format(time.RFC3339Nano), CallID: uuid.NewString(), Principal: c.Principal, Connector: c.Connector,
		Tool: c.Tool, Operation: c.Operation, AdapterVersion: c.AdapterVersion, Account: c.Account, PolicyVersion: c.PolicyVersion,
		Decision: decision, IdempotencyKey: c.IdempotencyKey, Verbose: c.Verbose, RequestBytes: c.RequestBytes}
	if c.Grant != nil {
		r.GrantID, r.GrantKind, r.Principal = c.Grant.ID, string(c.Grant.Kind), c.Grant.Principal()
		r.AgentID, r.ProjectKey, r.SessionID, r.RunID, r.ParentGrantID = c.Grant.AgentID, c.Grant.ProjectKey, c.Grant.SessionID, c.Grant.RunID, c.Grant.ParentGrantID
	}
	if c.Args != nil {
		r.ArgsDigest, r.ArgsSummary = Digest(c.Args)
	}
	return r
}

// Intent records that an allowed call is about to reach the upstream. It returns the call id.
func (w *Writer) Intent(ctx context.Context, c Call) (string, error) {
	r := base(c, "allow")
	r.Phase, r.State = "intent", StateIntent
	if err := w.insert(ctx, r); err != nil {
		return "", err
	}
	return r.CallID, nil
}

// Denied records a refusal (policy, auth or validation). Best effort: a denial that
// cannot be written is still a denial.
func (w *Writer) Denied(ctx context.Context, c Call, code, reason string) {
	r := base(c, "deny")
	r.Phase, r.State, r.DenyReason, r.OutcomeClass = "outcome", StateDenied, reason, code
	w.enqueue(r)
}

type Outcome struct {
	State             State
	UpstreamRequestID string
	OutcomeClass      string
	Duration          time.Duration
	ResponseBytes     int64
}

// Outcome appends the result row for a call id.
func (w *Writer) Outcome(ctx context.Context, callID string, c Call, o Outcome) error {
	r := base(c, "allow")
	r.CallID, r.Phase, r.State = callID, "outcome", o.State
	r.UpstreamRequestID, r.OutcomeClass, r.DurationMs, r.ResponseBytes = o.UpstreamRequestID, o.OutcomeClass, o.Duration.Milliseconds(), o.ResponseBytes
	w.enqueue(r)
	return nil
}

// List returns receipts after a cursor id, oldest first, for PERSIST's pull.
func (w *Writer) List(ctx context.Context, afterID int64, limit int) ([]Receipt, error) {
	w.Flush()
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := w.db.QueryContext(ctx, `SELECT id, ts, call_id, phase, COALESCE(grant_id,''), COALESCE(grant_kind,''), principal,
		COALESCE(agent_id,''), COALESCE(project_key,''), COALESCE(session_id,''), COALESCE(run_id,''), COALESCE(parent_grant_id,''),
		connector, tool, COALESCE(operation,''), COALESCE(adapter_version,''), COALESCE(account,''), COALESCE(args_digest,''),
		COALESCE(args_summary,''), COALESCE(policy_version,''), decision, COALESCE(deny_reason,''), COALESCE(approval_id,''),
		COALESCE(idempotency_key,''), state, COALESCE(upstream_request_id,''), COALESCE(outcome_class,''), COALESCE(duration_ms,0),
		request_bytes, response_bytes, verbose FROM receipts WHERE id > ? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Receipt
	for rows.Next() {
		var r Receipt
		var state string
		var verbose int
		if err := rows.Scan(&r.ID, &r.TS, &r.CallID, &r.Phase, &r.GrantID, &r.GrantKind, &r.Principal, &r.AgentID, &r.ProjectKey,
			&r.SessionID, &r.RunID, &r.ParentGrantID, &r.Connector, &r.Tool, &r.Operation, &r.AdapterVersion, &r.Account, &r.ArgsDigest,
			&r.ArgsSummary, &r.PolicyVersion, &r.Decision, &r.DenyReason, &r.ApprovalID, &r.IdempotencyKey, &state, &r.UpstreamRequestID,
			&r.OutcomeClass, &r.DurationMs, &r.RequestBytes, &r.ResponseBytes, &verbose); err != nil {
			return nil, err
		}
		r.State = State(state)
		r.Verbose = verbose == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// Count is used by health and tests.
func (w *Writer) Count(ctx context.Context) (int64, error) {
	w.Flush()
	var n int64
	err := w.db.QueryRowContext(ctx, `SELECT count(*) FROM receipts`).Scan(&n)
	return n, err
}
