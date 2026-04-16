package state

import (
	"context"
	"errors"
	"sync"
	"testing"

	"mha-go/internal/domain"
)

func TestMemoryStoreCreateRun(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	record, err := store.CreateRun(ctx, domain.RunRecord{
		Cluster: "app1",
		Kind:    domain.RunKindFailover,
		Status:  domain.RunStatusRunning,
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if record.ID == "" {
		t.Fatal("CreateRun: expected non-empty ID")
	}
	if record.Cluster != "app1" {
		t.Fatalf("Cluster = %q, want app1", record.Cluster)
	}
	if record.StartedAt.IsZero() {
		t.Fatal("StartedAt should be set")
	}
}

func TestMemoryStoreGetRun(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	created, _ := store.CreateRun(ctx, domain.RunRecord{Cluster: "c1", Kind: domain.RunKindCheckRepl})

	got, err := store.GetRun(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("got ID %q, want %q", got.ID, created.ID)
	}

	_, err = store.GetRun(ctx, "nonexistent")
	if !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("expected ErrRunNotFound, got %v", err)
	}
}

func TestMemoryStoreUpdateRun(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	created, _ := store.CreateRun(ctx, domain.RunRecord{Cluster: "c1", Kind: domain.RunKindMonitor})
	if err := store.UpdateRun(ctx, created.ID, domain.RunStatusSucceeded, "done"); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}

	got, _ := store.GetRun(ctx, created.ID)
	if got.Status != domain.RunStatusSucceeded {
		t.Fatalf("Status = %q, want succeeded", got.Status)
	}
	if got.Summary != "done" {
		t.Fatalf("Summary = %q, want done", got.Summary)
	}

	if err := store.UpdateRun(ctx, "bad-id", domain.RunStatusFailed, ""); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("expected ErrRunNotFound for missing id, got %v", err)
	}
}

func TestMemoryStoreAppendEvent(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	created, _ := store.CreateRun(ctx, domain.RunRecord{Cluster: "c1", Kind: domain.RunKindFailover})
	event := domain.RunEvent{Phase: "discover", Severity: domain.EventSeverityInfo, Message: "found 3 nodes"}
	if err := store.AppendEvent(ctx, created.ID, event); err != nil {
		t.Fatalf("AppendEvent: %v", err)
	}

	got, _ := store.GetRun(ctx, created.ID)
	if len(got.Events) != 1 {
		t.Fatalf("len(Events) = %d, want 1", len(got.Events))
	}
	if got.Events[0].Message != "found 3 nodes" {
		t.Fatalf("Event.Message = %q, want 'found 3 nodes'", got.Events[0].Message)
	}
	if got.Events[0].Sequence != 1 {
		t.Fatalf("Event.Sequence = %d, want 1", got.Events[0].Sequence)
	}

	if err := store.AppendEvent(ctx, "bad-id", event); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("expected ErrRunNotFound, got %v", err)
	}
}

func TestMemoryStoreListRuns(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	store.CreateRun(ctx, domain.RunRecord{Cluster: "app1", Kind: domain.RunKindFailover})
	store.CreateRun(ctx, domain.RunRecord{Cluster: "app1", Kind: domain.RunKindMonitor})
	store.CreateRun(ctx, domain.RunRecord{Cluster: "app2", Kind: domain.RunKindFailover})

	all, err := store.ListRuns(ctx, "", 0)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("len(all) = %d, want 3", len(all))
	}

	app1, _ := store.ListRuns(ctx, "app1", 0)
	if len(app1) != 2 {
		t.Fatalf("len(app1) = %d, want 2", len(app1))
	}

	limited, _ := store.ListRuns(ctx, "app1", 1)
	if len(limited) != 1 {
		t.Fatalf("len(limited) = %d, want 1", len(limited))
	}
}

func TestMemoryStoreGeneratesUniqueIDs(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	seen := make(map[string]bool)
	for i := 0; i < 20; i++ {
		r, _ := store.CreateRun(ctx, domain.RunRecord{Cluster: "c"})
		if seen[r.ID] {
			t.Fatalf("duplicate ID %q", r.ID)
		}
		seen[r.ID] = true
	}
}

func TestMemoryStoreConcurrentAccess(t *testing.T) {
	store := NewMemoryStore()
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, _ := store.CreateRun(ctx, domain.RunRecord{Cluster: "c"})
			_ = store.AppendEvent(ctx, r.ID, domain.RunEvent{Phase: "test", Message: "ok"})
			_ = store.UpdateRun(ctx, r.ID, domain.RunStatusSucceeded, "ok")
		}()
	}
	wg.Wait()

	all, _ := store.ListRuns(ctx, "c", 0)
	if len(all) != 10 {
		t.Fatalf("expected 10 runs, got %d", len(all))
	}
}

func TestLocalLeaseManagerAcquireAndRelease(t *testing.T) {
	mgr := NewLocalLeaseManager()
	ctx := context.Background()

	h, err := mgr.Acquire(ctx, "failover/app1", "manager-1", 0)
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if h.Key() != "failover/app1" {
		t.Fatalf("Key = %q, want failover/app1", h.Key())
	}
	if h.Owner() != "manager-1" {
		t.Fatalf("Owner = %q, want manager-1", h.Owner())
	}

	// Same owner can re-acquire (idempotent).
	_, err = mgr.Acquire(ctx, "failover/app1", "manager-1", 0)
	if err != nil {
		t.Fatalf("re-acquire by same owner: %v", err)
	}

	// Different owner must be rejected.
	_, err = mgr.Acquire(ctx, "failover/app1", "manager-2", 0)
	if err == nil {
		t.Fatal("expected error acquiring lease held by another owner")
	}

	// After release, another owner can take it.
	if err := h.Release(ctx); err != nil {
		t.Fatalf("Release: %v", err)
	}
	_, err = mgr.Acquire(ctx, "failover/app1", "manager-2", 0)
	if err != nil {
		t.Fatalf("Acquire after release: %v", err)
	}
}

func TestLocalLeaseManagerIndependentKeys(t *testing.T) {
	mgr := NewLocalLeaseManager()
	ctx := context.Background()

	_, err1 := mgr.Acquire(ctx, "failover/app1", "m1", 0)
	_, err2 := mgr.Acquire(ctx, "failover/app2", "m2", 0)
	if err1 != nil || err2 != nil {
		t.Fatalf("independent keys should not conflict: %v %v", err1, err2)
	}
}
