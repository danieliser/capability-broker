package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/danieliser/capability-broker/pkg/durable"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	db, err := durable.Open(context.Background(), filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return NewStore(db)
}

func agentCaps() Capabilities {
	return Capabilities{"clickhouse": {Queries: []string{"a", "b"}, Limits: map[string]float64{"max_rows": 100}}}
}

func TestMintAuthenticateRevoke(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	g, tok, err := s.Mint(ctx, MintRequest{Kind: KindAgent, Name: "x.steward", AgentID: "x.steward", SessionID: "s1",
		Capabilities: agentCaps(), PolicyVersion: "v1", IssuedBy: "test", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if tok[:4] != GrantTokenPrefix {
		t.Fatalf("token prefix %q", tok[:4])
	}
	got, err := s.Authenticate(ctx, tok, time.Now())
	if err != nil || got.ID != g.ID {
		t.Fatalf("authenticate: %v %v", got, err)
	}
	if _, err := s.Authenticate(ctx, tok+"x", time.Now()); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("tampered token accepted: %v", err)
	}
	if _, err := s.Authenticate(ctx, tok, time.Now().Add(2*time.Hour)); !errors.Is(err, ErrGrantExpired) {
		t.Fatalf("expired token accepted: %v", err)
	}
	if err := s.Revoke(ctx, g.ID, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, tok, time.Now()); !errors.Is(err, ErrGrantRevoked) {
		t.Fatalf("revoked token accepted: %v", err)
	}
}

func TestAgentGrantCannotBeOwner(t *testing.T) {
	s := newStore(t)
	_, _, err := s.Mint(context.Background(), MintRequest{Kind: KindAgent, Name: "a", AgentID: "a", SessionID: "s",
		Capabilities: OwnerCapabilities(), IssuedBy: "t"})
	if err == nil {
		t.Fatal("owner profile minted for an agent")
	}
}

func TestChildGrantOnlyNarrows(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	parent, _, err := s.Mint(ctx, MintRequest{Kind: KindAgent, Name: "p", AgentID: "p", SessionID: "s",
		Capabilities: agentCaps(), IssuedBy: "t", TTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	child, tok, err := s.Mint(ctx, MintRequest{Kind: KindAgent, Name: "c", AgentID: "c", SessionID: "s2", ParentGrantID: parent.ID,
		Capabilities: Capabilities{
			"clickhouse": {Queries: []string{"b", "zzz"}, SQL: "select", Limits: map[string]float64{"max_rows": 1000}},
			"memory":     {Namespaces: []string{"n"}},
		}, IssuedBy: "t", TTL: 48 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ch := child.Capabilities["clickhouse"]
	if len(ch.Queries) != 1 || ch.Queries[0] != "b" || ch.SQL != "" || ch.Limits["max_rows"] != 100 {
		t.Fatalf("child not narrowed: %+v", ch)
	}
	if _, ok := child.Capabilities["memory"]; ok {
		t.Fatal("child gained a connector the parent lacks")
	}
	if child.ExpiresAt.After(*parent.ExpiresAt) {
		t.Fatal("child outlives parent")
	}
	if err := s.Revoke(ctx, parent.ID, "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, tok, time.Now()); !errors.Is(err, ErrGrantRevoked) {
		t.Fatalf("child survived parent revocation: %v", err)
	}
}

func TestCounters(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)
	g, _, _ := s.Mint(ctx, MintRequest{Kind: KindClient, Name: "cc", Capabilities: OwnerCapabilities(), IssuedBy: "t"})
	for i := 1; i <= 3; i++ {
		n, err := s.Increment(ctx, g.ID, "calls")
		if err != nil || n != int64(i) {
			t.Fatalf("increment %d: %d %v", i, n, err)
		}
	}
}

func TestConstraintInheritanceCannotWiden(t *testing.T) {
	parent := Capabilities{"x": {Constraints: map[string]any{"post_types": []any{"post"}, "max_importance": 0.5}, OperationConstraints: map[string]map[string]any{"x.send": {"to": []any{"a", "b"}, "subject_prefix": "[Fleet] ", "max_amount": 10.0, "max_per_day": 2.0}}}}
	child := Capabilities{"x": {OperationConstraints: map[string]map[string]any{"x.send": {"to": []any{"b", "c"}, "subject_prefix": "[Fleet] report ", "max_amount": 100.0}}}}
	narrowed := child.Intersect(parent)["x"]
	if narrowed.Constraints["max_importance"] != 0.5 {
		t.Fatal("parent constraint omitted")
	}
	op := narrowed.OperationConstraints["x.send"]
	list, ok := constraintStrings(op["to"])
	if !ok || len(list) != 1 || list[0] != "b" || op["subject_prefix"] != "[Fleet] report " || op["max_amount"] != 10.0 || op["max_per_day"] != 2.0 {
		t.Fatalf("widened: %+v", op)
	}
	child["x"] = ConnectorCapability{OperationConstraints: map[string]map[string]any{"x.send": {"subject_prefix": "other"}}}
	if child.Intersect(parent)["x"].OperationConstraints["x.send"]["subject_prefix"] != nil {
		t.Fatal("incompatible prefix did not fail closed")
	}
}
