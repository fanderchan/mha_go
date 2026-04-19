package switchover

import (
	"context"
	"errors"
	"testing"
	"time"

	"mha-go/internal/domain"
	"mha-go/internal/hooks"
	"mha-go/internal/obs"
	"mha-go/internal/state"
)

// ---- test doubles ----

type recordingRunner struct {
	called []string
	failOn string
}

func (r *recordingRunner) PrecheckWriterEndpoint(_ context.Context, _ domain.ClusterSpec, _ *domain.SwitchoverPlan) error {
	return r.record("precheck-writer-endpoint")
}
func (r *recordingRunner) LockCandidate(_ context.Context, _ domain.ClusterSpec, _ *domain.SwitchoverPlan) error {
	return r.record("lock-candidate")
}
func (r *recordingRunner) LockOldPrimary(_ context.Context, _ domain.ClusterSpec, _ *domain.SwitchoverPlan) error {
	return r.record("lock-old-primary")
}
func (r *recordingRunner) WaitCandidateCatchUp(_ context.Context, _ domain.ClusterSpec, _ *domain.SwitchoverPlan) error {
	return r.record("wait-candidate-catchup")
}
func (r *recordingRunner) PromoteCandidate(_ context.Context, _ domain.ClusterSpec, _ *domain.SwitchoverPlan) error {
	return r.record("promote-candidate")
}
func (r *recordingRunner) RepointReplicas(_ context.Context, _ domain.ClusterSpec, _ *domain.SwitchoverPlan) error {
	return r.record("repoint-replicas")
}
func (r *recordingRunner) RepointOldPrimary(_ context.Context, _ domain.ClusterSpec, _ *domain.SwitchoverPlan) error {
	return r.record("repoint-old-primary")
}
func (r *recordingRunner) SwitchWriterEndpoint(_ context.Context, _ domain.ClusterSpec, _ *domain.SwitchoverPlan) error {
	return r.record("switch-writer-endpoint")
}
func (r *recordingRunner) VerifyWriterEndpoint(_ context.Context, _ domain.ClusterSpec, _ *domain.SwitchoverPlan) error {
	return r.record("verify-writer-endpoint")
}
func (r *recordingRunner) VerifyCluster(_ context.Context, _ domain.ClusterSpec, _ *domain.SwitchoverPlan) error {
	return r.record("verify")
}
func (r *recordingRunner) record(name string) error {
	r.called = append(r.called, name)
	if r.failOn == name {
		return errors.New("injected failure for " + name)
	}
	return nil
}

type recordingDispatcher struct {
	events []string
}

func (d *recordingDispatcher) Dispatch(_ context.Context, event hooks.Event) error {
	d.events = append(d.events, event.Name)
	return nil
}

type fakeLeases struct{}

func (f *fakeLeases) Acquire(_ context.Context, key, owner string, _ time.Duration) (state.LeaseHandle, error) {
	return &fakeHandle{key: key, owner: owner}, nil
}

type fakeHandle struct{ key, owner string }

func (h *fakeHandle) Key() string                     { return h.key }
func (h *fakeHandle) Owner() string                   { return h.owner }
func (h *fakeHandle) Release(_ context.Context) error { return nil }

// ---- helpers ----

func testSpec() domain.ClusterSpec {
	return domain.ClusterSpec{
		Name: "test",
		Controller: domain.ControllerSpec{
			ID:    "ctrl-1",
			Lease: domain.LeaseSpec{TTL: 15 * time.Second},
		},
		Topology:    domain.TopologySpec{Kind: domain.TopologyMySQLReplicationSinglePrimary},
		Replication: domain.ReplicationSpec{Mode: domain.ReplicationModeGTID},
		Nodes: []domain.NodeSpec{
			{ID: "db1", Host: "10.0.0.1", Port: 3306, ExpectedRole: domain.NodeRolePrimary},
			{ID: "db2", Host: "10.0.0.2", Port: 3306, ExpectedRole: domain.NodeRoleReplica},
			{ID: "db3", Host: "10.0.0.3", Port: 3306, ExpectedRole: domain.NodeRoleReplica},
		},
	}
}

func testPlan(spec domain.ClusterSpec, requiresEndpoint bool) *domain.SwitchoverPlan {
	return &domain.SwitchoverPlan{
		ClusterName:                  spec.Name,
		CreatedAt:                    time.Now(),
		OriginalPrimary:              domain.NodeState{ID: "db1"},
		Candidate:                    domain.NodeState{ID: "db2"},
		RequiresWriterEndpointSwitch: requiresEndpoint,
		Steps:                        buildSwitchoverSteps(spec, "db2", "db1", requiresEndpoint),
	}
}

func newExecutor(runner ActionRunner, dispatcher hooks.Dispatcher) *Executor {
	store := state.NewMemoryStore()
	logger := obs.NewLogger("error")
	return NewExecutor(runner, &fakeLeases{}, store, dispatcher, logger)
}

// ---- tests ----

func TestExecutePlanSucceeds(t *testing.T) {
	runner := &recordingRunner{}
	dispatcher := &recordingDispatcher{}
	exec := newExecutor(runner, dispatcher)

	spec := testSpec()
	plan := testPlan(spec, false) // no endpoint switch, no extra replicas besides db1+db2+db3

	execution, err := exec.ExecutePlan(context.Background(), spec, plan, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !execution.Succeeded {
		t.Fatal("expected execution to succeed")
	}
	if execution.FailedStep != "" {
		t.Fatalf("expected no failed step, got %q", execution.FailedStep)
	}

	// All pending steps must have been called.
	wantCalled := []string{"lock-candidate", "lock-old-primary", "wait-candidate-catchup", "promote-candidate", "repoint-replicas", "repoint-old-primary", "verify"}
	if len(runner.called) != len(wantCalled) {
		t.Fatalf("called steps %v, want %v", runner.called, wantCalled)
	}
	for i, name := range wantCalled {
		if runner.called[i] != name {
			t.Errorf("step[%d] = %q, want %q", i, runner.called[i], name)
		}
	}

	// Hook events: start + complete.
	if len(dispatcher.events) < 2 {
		t.Fatalf("expected at least 2 hook events, got %v", dispatcher.events)
	}
	if dispatcher.events[0] != "switchover.start" {
		t.Errorf("first event = %q, want switchover.start", dispatcher.events[0])
	}
	if dispatcher.events[len(dispatcher.events)-1] != "switchover.complete" {
		t.Errorf("last event = %q, want switchover.complete", dispatcher.events[len(dispatcher.events)-1])
	}
}

func TestExecutePlanSkipsEndpointWhenNotRequired(t *testing.T) {
	runner := &recordingRunner{}
	exec := newExecutor(runner, hooks.NoopDispatcher{})

	spec := testSpec()
	plan := testPlan(spec, false)

	_, err := exec.ExecutePlan(context.Background(), spec, plan, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range runner.called {
		if name == "precheck-writer-endpoint" || name == "switch-writer-endpoint" || name == "verify-writer-endpoint" {
			t.Fatalf("%s should not have been called when endpoint switch is not required", name)
		}
	}
}

func TestExecutePlanIncludesEndpointWhenRequired(t *testing.T) {
	runner := &recordingRunner{}
	exec := newExecutor(runner, hooks.NoopDispatcher{})

	spec := testSpec()
	spec.WriterEndpoint = domain.WriterEndpointSpec{Kind: "vip", Command: "/bin/true"}
	plan := testPlan(spec, true)

	_, err := exec.ExecutePlan(context.Background(), spec, plan, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	foundSwitch := false
	foundPrecheck := false
	foundVerify := false
	for _, name := range runner.called {
		if name == "switch-writer-endpoint" {
			foundSwitch = true
		}
		if name == "precheck-writer-endpoint" {
			foundPrecheck = true
		}
		if name == "verify-writer-endpoint" {
			foundVerify = true
		}
	}
	if !foundPrecheck {
		t.Fatal("expected precheck-writer-endpoint to be called")
	}
	if !foundSwitch {
		t.Fatal("expected switch-writer-endpoint to be called")
	}
	if !foundVerify {
		t.Fatal("expected verify-writer-endpoint to be called")
	}
}

func TestExecutePlanStopsOnStepFailure(t *testing.T) {
	runner := &recordingRunner{failOn: "wait-candidate-catchup"}
	dispatcher := &recordingDispatcher{}
	exec := newExecutor(runner, dispatcher)

	spec := testSpec()
	plan := testPlan(spec, false)

	execution, err := exec.ExecutePlan(context.Background(), spec, plan, false)
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if execution == nil {
		t.Fatal("expected non-nil execution even on failure")
	}
	if execution.FailedStep != "wait-candidate-catchup" {
		t.Errorf("FailedStep = %q, want wait-candidate-catchup", execution.FailedStep)
	}
	if execution.Succeeded {
		t.Fatal("expected Succeeded=false on step failure")
	}
	// Steps after the failure must not have been called.
	for _, name := range runner.called {
		if name == "promote-candidate" || name == "repoint-replicas" {
			t.Errorf("step %q should not have been called after failure", name)
		}
	}
	// Hook: start + abort (no complete).
	hasAbort := false
	for _, e := range dispatcher.events {
		if e == "switchover.complete" {
			t.Fatal("switchover.complete should not fire on failure")
		}
		if e == "switchover.abort" {
			hasAbort = true
		}
	}
	if !hasAbort {
		t.Fatal("expected switchover.abort hook event")
	}
}

func TestExecutePlanDryRun(t *testing.T) {
	runner := &recordingRunner{}
	exec := newExecutor(runner, hooks.NoopDispatcher{})

	spec := testSpec()
	plan := testPlan(spec, false)

	execution, err := exec.ExecutePlan(context.Background(), spec, plan, true)
	if err != nil {
		t.Fatalf("dry-run unexpected error: %v", err)
	}
	if !execution.DryRun {
		t.Fatal("expected DryRun=true")
	}
	if !execution.Succeeded {
		t.Fatal("expected dry-run to succeed")
	}
}
