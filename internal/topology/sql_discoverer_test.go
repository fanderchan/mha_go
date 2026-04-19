package topology

import (
	"context"
	"errors"
	"testing"
	"time"

	"mha-go/internal/domain"
	sqltransport "mha-go/internal/transport/sql"
)

type fakeInspector struct {
	results map[string]*sqltransport.Inspection
	errs    map[string]error
}

func (f fakeInspector) Inspect(_ context.Context, node domain.NodeSpec) (*sqltransport.Inspection, error) {
	if err, ok := f.errs[node.ID]; ok {
		return nil, err
	}
	if inspection, ok := f.results[node.ID]; ok {
		return inspection, nil
	}
	return nil, errors.New("missing inspection")
}

func TestSQLDiscovererDiscover(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Topology: domain.TopologySpec{
			Kind:         domain.TopologyMySQLReplicationSinglePrimary,
			SingleWriter: true,
		},
		Controller: domain.ControllerSpec{
			Monitor: domain.MonitorSpec{
				Interval:         time.Second,
				FailureThreshold: 3,
				ReconfirmTimeout: time.Second,
			},
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
			{
				ID:            "db1",
				Host:          "10.0.0.11",
				Port:          3306,
				VersionSeries: "8.4",
				ExpectedRole:  domain.NodeRolePrimary,
			},
			{
				ID:                "db2",
				Host:              "10.0.0.12",
				Port:              3306,
				VersionSeries:     "8.4",
				ExpectedRole:      domain.NodeRoleReplica,
				CandidatePriority: 100,
			},
			{
				ID:                "db3",
				Host:              "10.0.0.13",
				Port:              3306,
				VersionSeries:     "8.4",
				ExpectedRole:      domain.NodeRoleReplica,
				CandidatePriority: 90,
			},
		},
	}

	discoverer := NewSQLDiscoverer(fakeInspector{
		results: map[string]*sqltransport.Inspection{
			"db1": {
				NodeID:                    "db1",
				Address:                   "10.0.0.11:3306",
				ServerUUID:                "uuid-db1",
				Version:                   "8.4.0",
				VersionSeries:             "8.4",
				GTIDMode:                  "ON",
				GTIDExecuted:              "uuid-db1:1-100",
				SemiSyncSourceEnabled:     true,
				SemiSyncSourceOperational: true,
			},
			"db2": {
				NodeID:                     "db2",
				Address:                    "10.0.0.12:3306",
				ServerUUID:                 "uuid-db2",
				Version:                    "8.4.0",
				VersionSeries:              "8.4",
				GTIDMode:                   "ON",
				GTIDExecuted:               "uuid-db1:1-100,uuid-db2:1-10",
				ReadOnly:                   true,
				SuperReadOnly:              true,
				SemiSyncReplicaOperational: true,
				ReplicaChannels: []sqltransport.ReplicaChannelStatus{
					{
						SourceHost:          "10.0.0.11",
						SourcePort:          3306,
						SourceUUID:          "uuid-db1",
						IOThreadRunning:     true,
						SQLThreadRunning:    true,
						SecondsBehindSource: 0,
					},
				},
			},
			"db3": {
				NodeID:                     "db3",
				Address:                    "10.0.0.13:3306",
				ServerUUID:                 "uuid-db3",
				Version:                    "8.4.0",
				VersionSeries:              "8.4",
				GTIDMode:                   "ON",
				GTIDExecuted:               "uuid-db1:1-100,uuid-db3:1-5",
				ReadOnly:                   true,
				SuperReadOnly:              true,
				SemiSyncReplicaOperational: true,
				ReplicaChannels: []sqltransport.ReplicaChannelStatus{
					{
						SourceHost:          "10.0.0.11",
						SourcePort:          3306,
						SourceUUID:          "uuid-db1",
						IOThreadRunning:     true,
						SQLThreadRunning:    true,
						SecondsBehindSource: 0,
					},
				},
			},
		},
	})

	view, err := discoverer.Discover(context.Background(), spec)
	if err != nil {
		t.Fatalf("discover: %v", err)
	}

	if view.PrimaryID != "db1" {
		t.Fatalf("primary = %q, want db1", view.PrimaryID)
	}
	if len(view.Warnings) != 0 {
		t.Fatalf("warnings = %v, want none", view.Warnings)
	}

	primary, ok := view.PrimaryNode()
	if !ok {
		t.Fatal("primary node missing")
	}
	if primary.Role != domain.NodeRolePrimary {
		t.Fatalf("primary role = %s, want %s", primary.Role, domain.NodeRolePrimary)
	}

	replicas := view.ReplicaNodes()
	if len(replicas) != 2 {
		t.Fatalf("replica count = %d, want 2", len(replicas))
	}
	for _, replica := range replicas {
		if replica.Replica == nil {
			t.Fatalf("replica %s has nil replica state", replica.ID)
		}
		if replica.Replica.SourceID != "db1" {
			t.Fatalf("replica %s source_id = %q, want db1", replica.ID, replica.Replica.SourceID)
		}
		if replica.Health != domain.NodeHealthAlive {
			t.Fatalf("replica %s health = %s, want alive", replica.ID, replica.Health)
		}
	}
}
