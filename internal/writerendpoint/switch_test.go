package writerendpoint

import (
	"context"
	"testing"

	"mha-go/internal/domain"
)

func TestPrecheckRequiresSwitchCommandForVIP(t *testing.T) {
	spec := domain.ClusterSpec{
		Name:           "app1",
		WriterEndpoint: domain.WriterEndpointSpec{Kind: "vip", Target: "192.0.2.10"},
	}
	err := PrecheckWithNodes(context.Background(), spec,
		domain.NodeState{ID: "db2", Address: "db2:3306"},
		domain.NodeState{ID: "db1", Address: "db1:3306"},
	)
	if err == nil {
		t.Fatal("expected precheck to fail without switch command")
	}
}

func TestPrecheckRunsConfiguredCommand(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		WriterEndpoint: domain.WriterEndpointSpec{
			Kind:            "vip",
			Target:          "192.0.2.10",
			Command:         "true",
			PrecheckCommand: `test "$MHA_WRITER_ENDPOINT_ACTION" = precheck && test "$MHA_NEW_PRIMARY_HOST" = db2`,
		},
	}
	err := PrecheckWithNodes(context.Background(), spec,
		domain.NodeState{ID: "db2", Address: "db2:3306"},
		domain.NodeState{ID: "db1", Address: "db1:3306"},
	)
	if err != nil {
		t.Fatalf("PrecheckWithNodes: %v", err)
	}
}

func TestVerifyWithoutCommandIsNoop(t *testing.T) {
	spec := domain.ClusterSpec{
		Name:           "app1",
		WriterEndpoint: domain.WriterEndpointSpec{Kind: "proxy", Command: "true"},
	}
	if err := VerifyWithNodes(context.Background(), spec,
		domain.NodeState{ID: "db2", Address: "db2:3306"},
		domain.NodeState{ID: "db1", Address: "db1:3306"},
	); err != nil {
		t.Fatalf("VerifyWithNodes should be no-op without verify command: %v", err)
	}
}
