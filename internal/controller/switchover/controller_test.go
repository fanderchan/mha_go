package switchover

import (
	"context"
	"strings"
	"testing"
	"time"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
	"mha-go/internal/state"
	"mha-go/internal/topology"
)

type controllerFakeDiscoverer struct {
	view *domain.ClusterView
	err  error
}

func (f controllerFakeDiscoverer) Discover(context.Context, domain.ClusterSpec) (*domain.ClusterView, error) {
	return f.view, f.err
}

func TestBuildPlanRejectsErrantCandidateGTID(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Controller: domain.ControllerSpec{
			ID:    "manager-1",
			Lease: domain.LeaseSpec{TTL: 15 * time.Second},
		},
		Topology:    domain.TopologySpec{Kind: domain.TopologyMySQLReplicationSinglePrimary, SingleWriter: true},
		Replication: domain.ReplicationSpec{Mode: domain.ReplicationModeGTID},
		Nodes: []domain.NodeSpec{
			{ID: "db1", ExpectedRole: domain.NodeRolePrimary, VersionSeries: "8.4"},
			{ID: "db2", ExpectedRole: domain.NodeRoleReplica, VersionSeries: "8.4", CandidatePriority: 100},
		},
	}
	view := &domain.ClusterView{
		ClusterName: spec.Name,
		PrimaryID:   "db1",
		Nodes: []domain.NodeState{
			{ID: "db1", Role: domain.NodeRolePrimary, Health: domain.NodeHealthAlive, GTIDExecuted: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10", ReadOnly: false, SuperReadOnly: false},
			{
				ID:                "db2",
				Role:              domain.NodeRoleReplica,
				Health:            domain.NodeHealthAlive,
				CandidatePriority: 100,
				GTIDExecuted:      "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-11",
				ReadOnly:          false,
				SuperReadOnly:     false,
				Replica: &domain.ReplicaState{
					SourceID:            "db1",
					AutoPosition:        true,
					IOThreadRunning:     true,
					SQLThreadRunning:    true,
					SecondsBehindSource: 0,
				},
			},
		},
	}

	ctrl := NewController(
		controllerFakeDiscoverer{view: view},
		topology.NewDefaultCandidateSelector(),
		state.NewMemoryStore(),
		obs.NewLogger("error"),
	)
	_, err := ctrl.BuildPlan(context.Background(), spec)
	if err == nil {
		t.Fatal("expected errant candidate GTID to block switchover planning")
	}
	if !strings.Contains(err.Error(), "errant GTIDs") {
		t.Fatalf("error = %v, want errant GTID message", err)
	}
}

func TestBuildPlanAllowsWritableCandidateWithoutErrantGTID(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Controller: domain.ControllerSpec{
			ID:    "manager-1",
			Lease: domain.LeaseSpec{TTL: 15 * time.Second},
		},
		Topology:    domain.TopologySpec{Kind: domain.TopologyMySQLReplicationSinglePrimary, SingleWriter: true},
		Replication: domain.ReplicationSpec{Mode: domain.ReplicationModeGTID},
		Nodes: []domain.NodeSpec{
			{ID: "db1", ExpectedRole: domain.NodeRolePrimary, VersionSeries: "8.4"},
			{ID: "db2", ExpectedRole: domain.NodeRoleReplica, VersionSeries: "8.4", CandidatePriority: 100},
		},
	}
	view := &domain.ClusterView{
		ClusterName: spec.Name,
		PrimaryID:   "db1",
		Nodes: []domain.NodeState{
			{ID: "db1", Role: domain.NodeRolePrimary, Health: domain.NodeHealthAlive, GTIDExecuted: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10", ReadOnly: false, SuperReadOnly: false},
			{
				ID:                "db2",
				Role:              domain.NodeRoleReplica,
				Health:            domain.NodeHealthAlive,
				CandidatePriority: 100,
				GTIDExecuted:      "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10",
				ReadOnly:          false,
				SuperReadOnly:     false,
				Replica: &domain.ReplicaState{
					SourceID:            "db1",
					AutoPosition:        true,
					IOThreadRunning:     true,
					SQLThreadRunning:    true,
					SecondsBehindSource: 0,
				},
			},
		},
	}

	ctrl := NewController(
		controllerFakeDiscoverer{view: view},
		topology.NewDefaultCandidateSelector(),
		state.NewMemoryStore(),
		obs.NewLogger("error"),
	)
	plan, err := ctrl.BuildPlan(context.Background(), spec)
	if err != nil {
		t.Fatalf("BuildPlan returned error for writable candidate without errant GTIDs: %v", err)
	}
	if plan.Candidate.ID != "db2" {
		t.Fatalf("candidate = %s, want db2", plan.Candidate.ID)
	}
}

func TestBuildPlanAllowsReadOnlyCandidateWithLocalGTID(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Controller: domain.ControllerSpec{
			ID:    "manager-1",
			Lease: domain.LeaseSpec{TTL: 15 * time.Second},
		},
		Topology:    domain.TopologySpec{Kind: domain.TopologyMySQLReplicationSinglePrimary, SingleWriter: true},
		Replication: domain.ReplicationSpec{Mode: domain.ReplicationModeGTID},
		Nodes: []domain.NodeSpec{
			{ID: "db1", ExpectedRole: domain.NodeRolePrimary, VersionSeries: "8.4"},
			{ID: "db2", ExpectedRole: domain.NodeRoleReplica, VersionSeries: "8.4", CandidatePriority: 100},
		},
	}
	view := &domain.ClusterView{
		ClusterName: spec.Name,
		PrimaryID:   "db1",
		Nodes: []domain.NodeState{
			{ID: "db1", Role: domain.NodeRolePrimary, Health: domain.NodeHealthAlive, GTIDExecuted: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10", ReadOnly: false, SuperReadOnly: false},
			{
				ID:                "db2",
				Role:              domain.NodeRoleReplica,
				Health:            domain.NodeHealthAlive,
				CandidatePriority: 100,
				GTIDExecuted:      "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10,bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb:1-5",
				ReadOnly:          true,
				SuperReadOnly:     true,
				Replica: &domain.ReplicaState{
					SourceID:            "db1",
					AutoPosition:        true,
					IOThreadRunning:     true,
					SQLThreadRunning:    true,
					SecondsBehindSource: 0,
				},
			},
		},
	}

	ctrl := NewController(
		controllerFakeDiscoverer{view: view},
		topology.NewDefaultCandidateSelector(),
		state.NewMemoryStore(),
		obs.NewLogger("error"),
	)
	if _, err := ctrl.BuildPlan(context.Background(), spec); err != nil {
		t.Fatalf("BuildPlan returned error for read-only candidate with local GTIDs: %v", err)
	}
}
