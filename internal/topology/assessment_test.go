package topology

import (
	"context"
	"testing"

	"mha-go/internal/domain"
)

func TestAssessReplicationHealthy(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Topology: domain.TopologySpec{
			Kind:                   domain.TopologyAsyncSinglePrimary,
			SingleWriter:           true,
			AllowCascadingReplicas: true,
		},
		Replication: domain.ReplicationSpec{
			Mode: domain.ReplicationModeGTID,
			SemiSync: domain.SemiSyncSpec{
				Policy:              domain.SemiSyncPreferred,
				WaitForReplicaCount: 1,
			},
		},
		Nodes: []domain.NodeSpec{
			{ID: "db1", VersionSeries: "8.4", ExpectedRole: domain.NodeRolePrimary},
			{ID: "db2", VersionSeries: "8.4", ExpectedRole: domain.NodeRoleReplica},
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
				VersionSeries:  "8.4",
				ReadOnly:       false,
				SuperReadOnly:  false,
				SemiSyncSource: true,
			},
			{
				ID:            "db2",
				Role:          domain.NodeRoleReplica,
				Health:        domain.NodeHealthAlive,
				VersionSeries: "8.4",
				ReadOnly:      true,
				SuperReadOnly: true,
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

	assessment := AssessReplication(spec, view)
	if assessment.HasErrors() {
		t.Fatalf("unexpected errors: %+v", assessment.Errors())
	}
	if len(assessment.Warnings()) != 0 {
		t.Fatalf("unexpected warnings: %+v", assessment.Warnings())
	}
}

func TestAssessReplicationFindsGTIDAndSourceIssues(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Topology: domain.TopologySpec{
			Kind:                   domain.TopologyAsyncSinglePrimary,
			SingleWriter:           true,
			AllowCascadingReplicas: false,
		},
		Replication: domain.ReplicationSpec{
			Mode: domain.ReplicationModeGTID,
			SemiSync: domain.SemiSyncSpec{
				Policy: domain.SemiSyncRequired,
			},
		},
		Nodes: []domain.NodeSpec{
			{ID: "db1", VersionSeries: "8.4", ExpectedRole: domain.NodeRolePrimary},
			{ID: "db2", VersionSeries: "8.4", ExpectedRole: domain.NodeRoleReplica},
		},
	}

	view := &domain.ClusterView{
		ClusterName: spec.Name,
		PrimaryID:   "db1",
		Nodes: []domain.NodeState{
			{
				ID:            "db1",
				Role:          domain.NodeRolePrimary,
				Health:        domain.NodeHealthAlive,
				VersionSeries: "8.4",
				ReadOnly:      false,
				SuperReadOnly: false,
			},
			{
				ID:            "db2",
				Role:          domain.NodeRoleReplica,
				Health:        domain.NodeHealthAlive,
				VersionSeries: "8.4",
				ReadOnly:      false,
				SuperReadOnly: false,
				Replica: &domain.ReplicaState{
					SourceID:            "db3",
					AutoPosition:        false,
					IOThreadRunning:     false,
					SQLThreadRunning:    false,
					SecondsBehindSource: 45,
				},
			},
		},
	}

	assessment := AssessReplication(spec, view)
	if !assessment.HasErrors() {
		t.Fatal("expected assessment errors")
	}

	wantCodes := map[string]bool{
		"auto_position_disabled":         false,
		"replica_sql_thread_down":        false,
		"replica_io_thread_down":         false,
		"cascading_not_allowed":          false,
		"semisync_required_but_disabled": false,
	}
	for _, finding := range assessment.Errors() {
		if _, ok := wantCodes[finding.Code]; ok {
			wantCodes[finding.Code] = true
		}
	}
	for code, seen := range wantCodes {
		if !seen {
			t.Fatalf("expected error code %s in %+v", code, assessment.Errors())
		}
	}
}

func TestDefaultCandidateSelectorUsesReplicaState(t *testing.T) {
	view := &domain.ClusterView{
		PrimaryID: "db1",
		Nodes: []domain.NodeState{
			{
				ID:     "db1",
				Role:   domain.NodeRolePrimary,
				Health: domain.NodeHealthAlive,
			},
			{
				ID:                "db2",
				Role:              domain.NodeRoleReplica,
				Health:            domain.NodeHealthAlive,
				CandidatePriority: 100,
				ReadOnly:          true,
				SuperReadOnly:     true,
				Replica: &domain.ReplicaState{
					SourceID:            "db1",
					AutoPosition:        true,
					IOThreadRunning:     true,
					SQLThreadRunning:    true,
					SecondsBehindSource: 35,
				},
			},
			{
				ID:                "db3",
				Role:              domain.NodeRoleReplica,
				Health:            domain.NodeHealthAlive,
				CandidatePriority: 90,
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

	selector := NewDefaultCandidateSelector()
	candidate, err := selector.SelectFailoverCandidate(context.Background(), domain.ClusterSpec{}, view)
	if err != nil {
		t.Fatalf("select candidate: %v", err)
	}
	if candidate.ID != "db3" {
		t.Fatalf("candidate = %s, want db3", candidate.ID)
	}
}
