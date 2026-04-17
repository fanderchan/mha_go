package fencing

import (
	"context"
	"errors"
	"testing"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
)

func TestCoordinatorDefaultsToReadOnlyFence(t *testing.T) {
	called := false
	coordinator := NewCoordinator(obs.NewLogger("error"))
	err := coordinator.FenceOldPrimary(
		context.Background(),
		domain.ClusterSpec{Name: "app1"},
		domain.NodeState{ID: "db1"},
		domain.NodeState{ID: "db2"},
		func(context.Context) error {
			called = true
			return nil
		},
	)
	if err != nil {
		t.Fatalf("FenceOldPrimary: %v", err)
	}
	if !called {
		t.Fatal("default read-only fence was not called")
	}
}

func TestCoordinatorRequiredFenceFailureAborts(t *testing.T) {
	coordinator := NewCoordinator(obs.NewLogger("error"))
	err := coordinator.FenceOldPrimary(
		context.Background(),
		domain.ClusterSpec{Name: "app1"},
		domain.NodeState{ID: "db1"},
		domain.NodeState{ID: "db2"},
		func(context.Context) error { return errors.New("sql down") },
	)
	if err == nil {
		t.Fatal("expected required read-only fence failure")
	}
}

func TestCoordinatorOptionalFenceFailureContinues(t *testing.T) {
	coordinator := NewCoordinator(obs.NewLogger("error"))
	spec := domain.ClusterSpec{
		Name: "app1",
		Fencing: domain.FencingSpec{Steps: []domain.FencingStepSpec{
			{Kind: KindReadOnly, Required: false},
		}},
	}
	err := coordinator.FenceOldPrimary(
		context.Background(),
		spec,
		domain.NodeState{ID: "db1"},
		domain.NodeState{ID: "db2"},
		func(context.Context) error { return errors.New("sql down") },
	)
	if err != nil {
		t.Fatalf("optional fence failure should not abort: %v", err)
	}
}
