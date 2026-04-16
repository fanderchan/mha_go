package topology

import (
	"context"
	"testing"

	"mha-go/internal/domain"
)

func aliveReplica(id string) domain.NodeState {
	return domain.NodeState{
		ID:     id,
		Role:   domain.NodeRoleReplica,
		Health: domain.NodeHealthAlive,
		Replica: &domain.ReplicaState{
			SourceID:         "db1",
			AutoPosition:     true,
			IOThreadRunning:  true,
			SQLThreadRunning: true,
		},
	}
}

func testView() *domain.ClusterView {
	return &domain.ClusterView{
		ClusterName: "app1",
		PrimaryID:   "db1",
		Nodes: []domain.NodeState{
			{ID: "db1", Role: domain.NodeRolePrimary, Health: domain.NodeHealthAlive},
			aliveReplica("db2"),
			aliveReplica("db3"),
		},
	}
}

// ---- PinnedCandidateSelector ----

func TestPinnedSelectorPicksNamedNode(t *testing.T) {
	sel := NewPinnedCandidateSelector("db3")
	view := testView()
	got, err := sel.SelectFailoverCandidate(context.Background(), domain.ClusterSpec{}, view)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "db3" {
		t.Fatalf("got ID %q, want db3", got.ID)
	}
}

func TestPinnedSelectorRejectsUnknownNode(t *testing.T) {
	sel := NewPinnedCandidateSelector("db99")
	_, err := sel.SelectFailoverCandidate(context.Background(), domain.ClusterSpec{}, testView())
	if err == nil {
		t.Fatal("expected error for unknown node")
	}
}

func TestPinnedSelectorRejectsPrimary(t *testing.T) {
	sel := NewPinnedCandidateSelector("db1") // db1 is the primary
	_, err := sel.SelectFailoverCandidate(context.Background(), domain.ClusterSpec{}, testView())
	if err == nil {
		t.Fatal("expected error when pinning the current primary")
	}
}

func TestPinnedSelectorRejectsDeadNode(t *testing.T) {
	view := testView()
	view.Nodes[1].Health = domain.NodeHealthDead // db2 is dead

	sel := NewPinnedCandidateSelector("db2")
	_, err := sel.SelectFailoverCandidate(context.Background(), domain.ClusterSpec{}, view)
	if err == nil {
		t.Fatal("expected error when pinning a dead node")
	}
}

func TestPinnedSelectorRejectsNoMasterNode(t *testing.T) {
	view := testView()
	view.Nodes[1].NoMaster = true // db2 has no_master=true

	sel := NewPinnedCandidateSelector("db2")
	_, err := sel.SelectFailoverCandidate(context.Background(), domain.ClusterSpec{}, view)
	if err == nil {
		t.Fatal("expected error when pinning a no_master node")
	}
}

// ---- DefaultCandidateSelector ----

func TestDefaultSelectorPicksMostAdvancedReplica(t *testing.T) {
	view := testView()
	// db3 has zero lag; db2 has lag — db3 should score higher.
	view.Nodes[1].Replica.SecondsBehindSource = 10 // db2
	view.Nodes[2].Replica.SecondsBehindSource = 0  // db3

	sel := NewDefaultCandidateSelector()
	got, err := sel.SelectFailoverCandidate(context.Background(), domain.ClusterSpec{}, view)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "db3" {
		t.Fatalf("expected db3 (zero lag), got %q", got.ID)
	}
}

func TestDefaultSelectorErrorsWhenNoReplicas(t *testing.T) {
	view := &domain.ClusterView{
		ClusterName: "app1",
		PrimaryID:   "db1",
		Nodes: []domain.NodeState{
			{ID: "db1", Role: domain.NodeRolePrimary, Health: domain.NodeHealthAlive},
		},
	}
	sel := NewDefaultCandidateSelector()
	_, err := sel.SelectFailoverCandidate(context.Background(), domain.ClusterSpec{}, view)
	if err == nil {
		t.Fatal("expected ErrNoCandidateReplica")
	}
}

func TestDefaultSelectorSkipsDeadReplica(t *testing.T) {
	view := testView()
	view.Nodes[1].Health = domain.NodeHealthDead // db2 dead; only db3 qualifies

	sel := NewDefaultCandidateSelector()
	got, err := sel.SelectFailoverCandidate(context.Background(), domain.ClusterSpec{}, view)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != "db3" {
		t.Fatalf("got %q, want db3", got.ID)
	}
}
