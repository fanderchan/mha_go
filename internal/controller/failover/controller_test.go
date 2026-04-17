package failover

import (
	"context"
	"testing"
	"time"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
	"mha-go/internal/state"
	"mha-go/internal/topology"
)

type fakeDiscoverer struct {
	view *domain.ClusterView
	err  error
}

func (f fakeDiscoverer) Discover(context.Context, domain.ClusterSpec) (*domain.ClusterView, error) {
	return f.view, f.err
}

func TestBuildPlanBlocksWhenPrimaryStillAlive(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Controller: domain.ControllerSpec{
			ID: "manager-1",
			Lease: domain.LeaseSpec{
				Backend: "local-memory",
				TTL:     15 * time.Second,
			},
		},
		Topology: domain.TopologySpec{
			Kind:         domain.TopologyAsyncSinglePrimary,
			SingleWriter: true,
		},
		Replication: domain.ReplicationSpec{
			Mode: domain.ReplicationModeGTID,
			SemiSync: domain.SemiSyncSpec{
				Policy: domain.SemiSyncPreferred,
			},
			Salvage: domain.SalvageSpec{
				Policy: domain.SalvageIfPossible,
			},
		},
		Nodes: []domain.NodeSpec{
			{ID: "db1", ExpectedRole: domain.NodeRolePrimary, VersionSeries: "8.4"},
			{ID: "db2", ExpectedRole: domain.NodeRoleReplica, VersionSeries: "8.4", CandidatePriority: 100},
		},
	}

	view := &domain.ClusterView{
		ClusterName: spec.Name,
		PrimaryID:   "db1",
		Nodes: []domain.NodeState{
			{
				ID:             "db1",
				Role:           domain.NodeRolePrimary,
				Health:         domain.NodeHealthAlive,
				GTIDExecuted:   "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10",
				ReadOnly:       false,
				SuperReadOnly:  false,
				SemiSyncSource: true,
			},
			{
				ID:                "db2",
				Role:              domain.NodeRoleReplica,
				Health:            domain.NodeHealthAlive,
				CandidatePriority: 100,
				GTIDExecuted:      "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10",
				ReadOnly:          true,
				SuperReadOnly:     true,
				Replica: &domain.ReplicaState{
					SourceID:            "db1",
					AutoPosition:        true,
					IOThreadRunning:     true,
					SQLThreadRunning:    true,
					SecondsBehindSource: 0,
					SemiSyncReplica:     true,
				},
			},
		},
	}

	controller := NewController(
		fakeDiscoverer{view: view},
		topology.NewDefaultCandidateSelector(),
		nil,
		state.NewMemoryStore(),
		obs.NewLogger("error"),
	)
	plan, err := controller.BuildPlan(context.Background(), spec)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	if plan.PrimaryFailureConfirmed {
		t.Fatal("primary failure should not be confirmed")
	}
	if plan.ExecutionPermitted {
		t.Fatal("execution should be blocked while primary is alive")
	}
	if len(plan.BlockingReasons) == 0 {
		t.Fatal("blocking reasons should not be empty")
	}
}

func TestBuildPlanIncludesSalvageActions(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Controller: domain.ControllerSpec{
			ID: "manager-1",
			Lease: domain.LeaseSpec{
				Backend: "local-memory",
				TTL:     15 * time.Second,
			},
		},
		Topology: domain.TopologySpec{
			Kind:         domain.TopologyAsyncSinglePrimary,
			SingleWriter: true,
		},
		Replication: domain.ReplicationSpec{
			Mode: domain.ReplicationModeGTID,
			SemiSync: domain.SemiSyncSpec{
				Policy: domain.SemiSyncPreferred,
			},
			Salvage: domain.SalvageSpec{
				Policy: domain.SalvageIfPossible,
			},
		},
		Nodes: []domain.NodeSpec{
			{ID: "db1", ExpectedRole: domain.NodeRolePrimary, VersionSeries: "8.4"},
			{ID: "db2", ExpectedRole: domain.NodeRoleReplica, VersionSeries: "8.4", CandidatePriority: 90},
			{ID: "db3", ExpectedRole: domain.NodeRoleReplica, VersionSeries: "8.4", CandidatePriority: 100},
		},
	}

	view := &domain.ClusterView{
		ClusterName: spec.Name,
		PrimaryID:   "db1",
		Nodes: []domain.NodeState{
			{
				ID:           "db1",
				Role:         domain.NodeRolePrimary,
				Health:       domain.NodeHealthDead,
				GTIDExecuted: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-15",
			},
			{
				ID:                "db2",
				Role:              domain.NodeRoleReplica,
				Health:            domain.NodeHealthAlive,
				CandidatePriority: 90,
				GTIDExecuted:      "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-12",
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
			{
				ID:                "db3",
				Role:              domain.NodeRoleReplica,
				Health:            domain.NodeHealthAlive,
				CandidatePriority: 100,
				GTIDExecuted:      "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10",
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

	controller := NewController(
		fakeDiscoverer{view: view},
		topology.NewDefaultCandidateSelector(),
		nil,
		state.NewMemoryStore(),
		obs.NewLogger("error"),
	)
	plan, err := controller.BuildPlan(context.Background(), spec)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	if !plan.PrimaryFailureConfirmed {
		t.Fatal("primary failure should be confirmed")
	}
	if !plan.ExecutionPermitted {
		t.Fatalf("execution should be permitted, blocking reasons: %+v", plan.BlockingReasons)
	}
	if !plan.ShouldAttemptSalvage {
		t.Fatal("salvage should be attempted")
	}
	if len(plan.SalvageActions) == 0 {
		t.Fatal("salvage actions should not be empty")
	}
	if plan.SalvageActions[0].DonorNodeID != "db1" {
		t.Fatalf("first salvage donor = %s, want db1", plan.SalvageActions[0].DonorNodeID)
	}
	if len(plan.Steps) == 0 {
		t.Fatal("steps should not be empty")
	}
}

func TestBuildPlanBlocksWritableCandidatePromotion(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Controller: domain.ControllerSpec{
			ID: "manager-1",
			Lease: domain.LeaseSpec{
				Backend: "local-memory",
				TTL:     15 * time.Second,
			},
		},
		Topology: domain.TopologySpec{
			Kind:         domain.TopologyAsyncSinglePrimary,
			SingleWriter: true,
		},
		Replication: domain.ReplicationSpec{
			Mode: domain.ReplicationModeGTID,
			SemiSync: domain.SemiSyncSpec{
				Policy: domain.SemiSyncPreferred,
			},
			Salvage: domain.SalvageSpec{
				Policy: domain.SalvageIfPossible,
			},
		},
		Nodes: []domain.NodeSpec{
			{ID: "db1", ExpectedRole: domain.NodeRolePrimary, VersionSeries: "8.4"},
			{ID: "db2", ExpectedRole: domain.NodeRoleReplica, VersionSeries: "8.4", CandidatePriority: 100},
		},
	}

	view := &domain.ClusterView{
		ClusterName: spec.Name,
		PrimaryID:   "db1",
		Nodes: []domain.NodeState{
			{
				ID:           "db1",
				Role:         domain.NodeRolePrimary,
				Health:       domain.NodeHealthDead,
				GTIDExecuted: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10",
			},
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

	controller := NewController(
		fakeDiscoverer{view: view},
		topology.NewDefaultCandidateSelector(),
		nil,
		state.NewMemoryStore(),
		obs.NewLogger("error"),
	)
	plan, err := controller.BuildPlan(context.Background(), spec)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}

	if plan.PromoteReadinessConfirmed {
		t.Fatal("promote readiness should be blocked for writable candidate")
	}
	if plan.ExecutionPermitted {
		t.Fatal("execution should not be permitted when candidate is writable")
	}
	found := false
	for _, reason := range plan.PromoteReadinessReasons {
		if reason == "candidate db2 is writable before promotion" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected writable candidate reason, got %+v", plan.PromoteReadinessReasons)
	}
}

func TestBuildPlanSkipsDeadOrdinaryReplicaForRepoint(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Controller: domain.ControllerSpec{
			ID:    "manager-1",
			Lease: domain.LeaseSpec{Backend: "local-memory", TTL: 15 * time.Second},
		},
		Topology: domain.TopologySpec{
			Kind:         domain.TopologyAsyncSinglePrimary,
			SingleWriter: true,
		},
		Replication: domain.ReplicationSpec{
			Mode:     domain.ReplicationModeGTID,
			SemiSync: domain.SemiSyncSpec{Policy: domain.SemiSyncPreferred},
			Salvage:  domain.SalvageSpec{Policy: domain.SalvageIfPossible},
		},
		Nodes: []domain.NodeSpec{
			{ID: "db1", ExpectedRole: domain.NodeRolePrimary, VersionSeries: "8.4"},
			{ID: "db2", ExpectedRole: domain.NodeRoleReplica, VersionSeries: "8.4", CandidatePriority: 100},
			{ID: "db3", ExpectedRole: domain.NodeRoleReplica, VersionSeries: "8.4", CandidatePriority: 90},
		},
	}

	view := &domain.ClusterView{
		ClusterName: spec.Name,
		PrimaryID:   "db1",
		Nodes: []domain.NodeState{
			{ID: "db1", Role: domain.NodeRolePrimary, Health: domain.NodeHealthDead, GTIDExecuted: "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10"},
			{
				ID:                "db2",
				Role:              domain.NodeRoleReplica,
				Health:            domain.NodeHealthAlive,
				CandidatePriority: 100,
				GTIDExecuted:      "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-10",
				ReadOnly:          true,
				SuperReadOnly:     true,
				Replica: &domain.ReplicaState{
					SourceID:            "db1",
					AutoPosition:        true,
					IOThreadRunning:     false,
					SQLThreadRunning:    true,
					SecondsBehindSource: 0,
				},
			},
			{ID: "db3", Role: domain.NodeRoleReplica, Health: domain.NodeHealthDead},
		},
	}

	controller := NewController(
		fakeDiscoverer{view: view},
		topology.NewDefaultCandidateSelector(),
		nil,
		state.NewMemoryStore(),
		obs.NewLogger("error"),
	)
	plan, err := controller.BuildPlan(context.Background(), spec)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	if len(plan.RepointReplicaIDs) != 0 {
		t.Fatalf("repoint IDs = %+v, want none; old primary and db3 are dead", plan.RepointReplicaIDs)
	}
	if len(plan.SkippedReplicaIDs) != 2 {
		t.Fatalf("skipped IDs = %+v, want db1 and db3", plan.SkippedReplicaIDs)
	}
}
