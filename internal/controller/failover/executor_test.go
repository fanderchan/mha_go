package failover

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

// recordingDispatcher captures dispatched event names for assertions.
type recordingDispatcher struct{ events []string }

func (d *recordingDispatcher) Dispatch(_ context.Context, e hooks.Event) error {
	d.events = append(d.events, e.Name)
	return nil
}

// failingRunner returns an error for a specific step, succeeds for all others.
type failingRunner struct {
	failStep string
}

func (r *failingRunner) PrecheckWriterEndpoint(_ context.Context, _ domain.ClusterSpec, _ *domain.FailoverPlan) error {
	return r.mayFail("precheck-writer-endpoint")
}
func (r *failingRunner) FenceOldPrimary(_ context.Context, _ domain.ClusterSpec, _ *domain.FailoverPlan) error {
	return r.mayFail("fence-old-primary")
}
func (r *failingRunner) ApplySalvageAction(_ context.Context, _ domain.ClusterSpec, _ *domain.FailoverPlan, action domain.SalvageAction) error {
	return r.mayFail(action.Kind)
}
func (r *failingRunner) PromoteCandidate(_ context.Context, _ domain.ClusterSpec, _ *domain.FailoverPlan) error {
	return r.mayFail("promote-candidate")
}
func (r *failingRunner) RepointReplicas(_ context.Context, _ domain.ClusterSpec, _ *domain.FailoverPlan) error {
	return r.mayFail("repoint-replicas")
}
func (r *failingRunner) SwitchWriterEndpoint(_ context.Context, _ domain.ClusterSpec, _ *domain.FailoverPlan) error {
	return r.mayFail("switch-writer-endpoint")
}
func (r *failingRunner) VerifyWriterEndpoint(_ context.Context, _ domain.ClusterSpec, _ *domain.FailoverPlan) error {
	return r.mayFail("verify-writer-endpoint")
}
func (r *failingRunner) VerifyCluster(_ context.Context, _ domain.ClusterSpec, _ *domain.FailoverPlan) error {
	return r.mayFail("verify-cluster")
}
func (r *failingRunner) mayFail(name string) error {
	if name == r.failStep {
		return errors.New("injected failure for " + name)
	}
	return nil
}

func TestExecutorBlocksPlan(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Controller: domain.ControllerSpec{
			Lease: domain.LeaseSpec{TTL: 15 * time.Second},
		},
	}
	plan := &domain.FailoverPlan{
		ClusterName: "app1",
		LeaseKey:    "failover/app1",
		LeaseOwner:  "manager-1",
		Steps: []domain.FailoverStep{
			{Name: "acquire-lease", Status: "completed"},
			{Name: "confirm-primary-dead", Status: "blocked", Blocking: true, Reason: "primary is still reachable"},
			{Name: "promote-candidate", Status: "blocked", Blocking: true, Reason: "primary failure is not confirmed"},
		},
	}

	executor := NewExecutor(NewDryRunActionRunner(obs.NewLogger("error")), nil, state.NewMemoryStore(), nil, obs.NewLogger("error"))
	execution, err := executor.ExecutePlan(context.Background(), spec, plan, true)
	if err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	if !execution.Blocked {
		t.Fatal("execution should be blocked")
	}
	if execution.FailedStep != "confirm-primary-dead" {
		t.Fatalf("failed step = %s, want confirm-primary-dead", execution.FailedStep)
	}
	if len(execution.StepResults) != 3 {
		t.Fatalf("step results = %+v", execution.StepResults)
	}
	if execution.StepResults[1].Status != "blocked" {
		t.Fatalf("step 2 status = %s, want blocked", execution.StepResults[1].Status)
	}
	if execution.StepResults[2].Status != "skipped" {
		t.Fatalf("step 3 status = %s, want skipped", execution.StepResults[2].Status)
	}
}

func TestExecutorRunsDryRunSteps(t *testing.T) {
	spec := domain.ClusterSpec{
		Name: "app1",
		Controller: domain.ControllerSpec{
			Lease: domain.LeaseSpec{TTL: 15 * time.Second},
		},
	}
	plan := &domain.FailoverPlan{
		ClusterName: "app1",
		LeaseKey:    "failover/app1",
		LeaseOwner:  "manager-1",
		OldPrimary:  domain.NodeState{ID: "db1"},
		Candidate:   domain.NodeState{ID: "db2"},
		SalvageActions: []domain.SalvageAction{
			{Kind: "recover-from-old-primary", DonorNodeID: "db1", TargetNodeID: "db2", MissingGTIDSet: "uuid:1-3", Required: true},
		},
		Steps: []domain.FailoverStep{
			{Name: "acquire-lease", Status: "completed"},
			{Name: "confirm-primary-dead", Status: "completed"},
			{Name: "fence-old-primary", Status: "pending"},
			{Name: "recover-from-old-primary", Status: "pending"},
			{Name: "promote-candidate", Status: "pending"},
			{Name: "repoint-replicas", Status: "pending"},
			{Name: "verify-cluster", Status: "pending"},
		},
	}

	dispatcher := &recordingDispatcher{}
	executor := NewExecutor(NewDryRunActionRunner(obs.NewLogger("error")), nil, state.NewMemoryStore(), dispatcher, obs.NewLogger("error"))
	execution, err := executor.ExecutePlan(context.Background(), spec, plan, true)
	if err != nil {
		t.Fatalf("execute plan: %v", err)
	}

	if execution.Blocked {
		t.Fatal("execution should not be blocked")
	}
	if !execution.Succeeded {
		t.Fatal("execution should succeed")
	}
	if len(execution.StepResults) != len(plan.Steps) {
		t.Fatalf("step results = %+v", execution.StepResults)
	}
	for _, result := range execution.StepResults {
		if result.Status != "completed" {
			t.Fatalf("step result %+v is not completed", result)
		}
	}
	// Hook events: start + fence + promote + complete (no endpoint switch in this plan).
	wantEvents := []string{"failover.start", "failover.fence", "failover.promote", "failover.complete"}
	if len(dispatcher.events) != len(wantEvents) {
		t.Fatalf("hook events = %v, want %v", dispatcher.events, wantEvents)
	}
	for i, name := range wantEvents {
		if dispatcher.events[i] != name {
			t.Errorf("event[%d] = %q, want %q", i, dispatcher.events[i], name)
		}
	}
}

func TestExecutorHooksOnFailure(t *testing.T) {
	spec := domain.ClusterSpec{
		Name:       "app1",
		Controller: domain.ControllerSpec{Lease: domain.LeaseSpec{TTL: 15 * time.Second}},
	}
	plan := &domain.FailoverPlan{
		ClusterName: "app1",
		LeaseKey:    "failover/app1",
		LeaseOwner:  "manager-1",
		OldPrimary:  domain.NodeState{ID: "db1"},
		Candidate:   domain.NodeState{ID: "db2"},
		Steps: []domain.FailoverStep{
			{Name: "acquire-lease", Status: "completed"},
			{Name: "confirm-primary-dead", Status: "completed"},
			{Name: "fence-old-primary", Status: "pending"},
			{Name: "promote-candidate", Status: "pending"},
		},
	}

	dispatcher := &recordingDispatcher{}
	runner := &failingRunner{failStep: "fence-old-primary"}
	executor := NewExecutor(runner, nil, state.NewMemoryStore(), dispatcher, obs.NewLogger("error"))
	execution, err := executor.ExecutePlan(context.Background(), spec, plan, false)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if execution.FailedStep != "fence-old-primary" {
		t.Errorf("FailedStep = %q, want fence-old-primary", execution.FailedStep)
	}
	// Must have start + abort, must not have complete.
	hasStart, hasAbort, hasComplete := false, false, false
	for _, e := range dispatcher.events {
		switch e {
		case "failover.start":
			hasStart = true
		case "failover.abort":
			hasAbort = true
		case "failover.complete":
			hasComplete = true
		}
	}
	if !hasStart {
		t.Error("expected failover.start event")
	}
	if !hasAbort {
		t.Error("expected failover.abort event")
	}
	if hasComplete {
		t.Error("failover.complete must not fire on failure")
	}
}

func TestExecutorEndpointPrecheckFailureStopsBeforeFence(t *testing.T) {
	spec := domain.ClusterSpec{
		Name:       "app1",
		Controller: domain.ControllerSpec{Lease: domain.LeaseSpec{TTL: 15 * time.Second}},
	}
	plan := &domain.FailoverPlan{
		ClusterName: "app1",
		LeaseKey:    "failover/app1",
		LeaseOwner:  "manager-1",
		OldPrimary:  domain.NodeState{ID: "db1"},
		Candidate:   domain.NodeState{ID: "db2"},
		Steps: []domain.FailoverStep{
			{Name: "acquire-lease", Status: "completed"},
			{Name: "confirm-primary-dead", Status: "completed"},
			{Name: "precheck-writer-endpoint", Status: "pending"},
			{Name: "fence-old-primary", Status: "pending"},
			{Name: "promote-candidate", Status: "pending"},
		},
	}

	dispatcher := &recordingDispatcher{}
	runner := &failingRunner{failStep: "precheck-writer-endpoint"}
	executor := NewExecutor(runner, nil, state.NewMemoryStore(), dispatcher, obs.NewLogger("error"))
	execution, err := executor.ExecutePlan(context.Background(), spec, plan, false)
	if err == nil {
		t.Fatal("expected endpoint precheck error")
	}
	if execution.FailedStep != "precheck-writer-endpoint" {
		t.Fatalf("failed step = %s, want precheck-writer-endpoint", execution.FailedStep)
	}
	for _, event := range dispatcher.events {
		if event == "failover.fence" {
			t.Fatal("fence hook must not fire after endpoint precheck failure")
		}
	}
}

func TestExecutorAvailabilityFirstContinuesOnSalvageFailure(t *testing.T) {
	spec := domain.ClusterSpec{
		Name:       "app1",
		Controller: domain.ControllerSpec{Lease: domain.LeaseSpec{TTL: 15 * time.Second}},
	}
	plan := &domain.FailoverPlan{
		ClusterName: "app1",
		LeaseKey:    "failover/app1",
		LeaseOwner:  "manager-1",
		OldPrimary:  domain.NodeState{ID: "db1"},
		Candidate:   domain.NodeState{ID: "db2"},
		SalvageActions: []domain.SalvageAction{
			{Kind: "recover-from-old-primary", DonorNodeID: "db1", TargetNodeID: "db2",
				MissingGTIDSet: "uuid:1-5", Required: false},
		},
		Steps: []domain.FailoverStep{
			{Name: "acquire-lease", Status: "completed"},
			{Name: "confirm-primary-dead", Status: "completed"},
			{Name: "fence-old-primary", Status: "pending"},
			// Required=false: failure should warn, not abort.
			{Name: "recover-from-old-primary", Status: "pending", Required: false},
			{Name: "promote-candidate", Status: "pending"},
			{Name: "verify-cluster", Status: "pending"},
		},
	}

	runner := &failingRunner{failStep: "recover-from-old-primary"}
	executor := NewExecutor(runner, nil, state.NewMemoryStore(), nil, obs.NewLogger("error"))
	execution, err := executor.ExecutePlan(context.Background(), spec, plan, false)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !execution.Succeeded {
		t.Fatal("expected execution to succeed despite salvage failure (availability-first)")
	}

	warnedFound := false
	for _, r := range execution.StepResults {
		if r.Name == "recover-from-old-primary" && r.Status == "warned" {
			warnedFound = true
		}
	}
	if !warnedFound {
		t.Errorf("expected 'warned' result for salvage step, got %+v", execution.StepResults)
	}
}
