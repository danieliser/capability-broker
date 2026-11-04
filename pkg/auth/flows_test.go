package auth

import (
	"context"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/danieliser/capability-broker/pkg/durable"
)

func TestFlowReplayGuardAtomicAndPersistent(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "flows.db")
	db1, err := durable.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	db2, err := durable.Open(ctx, path)
	if err != nil {
		db1.Close()
		t.Fatal(err)
	}
	stores := []*Store{NewStore(db1), NewStore(db2)}
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			ok, err := stores[i%2].ReserveFlowRun(ctx, "project/flow", "same-key")
			if err != nil {
				t.Error(err)
			}
			if ok {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	db1.Close()
	db2.Close()
	if admitted.Load() != 1 {
		t.Fatalf("admitted %d duplicate effects", admitted.Load())
	}
	db, err := durable.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := NewStore(db)
	if ok, err := s.ReserveFlowRun(ctx, "project/flow", "same-key"); err != nil || ok {
		t.Fatalf("reservation lost on reopen: %v %v", ok, err)
	}
	for _, tt := range []struct{ scope, key string }{{"project/flow", "new-key"}, {"project/other-flow", "same-key"}} {
		if ok, err := s.ReserveFlowRun(ctx, tt.scope, tt.key); err != nil || !ok {
			t.Fatalf("independent key refused: %v %v", ok, err)
		}
	}
	db.Close()
	if ok, err := s.ReserveFlowRun(ctx, "project/flow", "unavailable"); err == nil || ok {
		t.Fatalf("unavailable store admitted effect: %v %v", ok, err)
	}
}
