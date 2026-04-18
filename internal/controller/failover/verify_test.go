package failover

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
	sqltransport "mha-go/internal/transport/sql"
)

type fakeVerifyInspector map[string]*sqltransport.Inspection

func (f fakeVerifyInspector) Inspect(_ context.Context, node domain.NodeSpec) (*sqltransport.Inspection, error) {
	in, ok := f[node.ID]
	if !ok {
		return nil, fmt.Errorf("missing inspection for %s", node.ID)
	}
	return in, nil
}

func TestVerifyPostFailoverRejectsReplicaWithoutChannel(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Nodes: []domain.NodeSpec{
			{ID: "db1", Host: "db1", Port: 3306, ExpectedRole: domain.NodeRolePrimary},
			{ID: "db2", Host: "db2", Port: 3306, ExpectedRole: domain.NodeRoleReplica},
			{ID: "db3", Host: "db3", Port: 3306, ExpectedRole: domain.NodeRoleReplica},
		},
	}
	plan := &domain.FailoverPlan{
		ClusterName: spec.Name,
		OldPrimary:  domain.NodeState{ID: "db1", Health: domain.NodeHealthDead},
		Candidate:   domain.NodeState{ID: "db2"},
	}
	inspector := fakeVerifyInspector{
		"db2": &sqltransport.Inspection{ReadOnly: false, SuperReadOnly: false},
		"db3": &sqltransport.Inspection{ReadOnly: true, SuperReadOnly: true},
	}

	err := VerifyPostFailover(context.Background(), inspector, spec, plan, obs.NewLogger("error"))
	if err == nil {
		t.Fatal("expected verification to fail for replica without channel")
	}
	if !strings.Contains(err.Error(), "has no replica channel") {
		t.Fatalf("error = %v, want missing channel message", err)
	}
}
