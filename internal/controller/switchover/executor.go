package switchover

import (
	"context"
	"fmt"
	"time"

	"mha-go/internal/domain"
	"mha-go/internal/hooks"
	"mha-go/internal/obs"
	"mha-go/internal/state"
)

// Executor carries out a SwitchoverPlan step by step.
type Executor struct {
	runner     ActionRunner
	leases     state.LeaseManager
	store      state.RunStore
	dispatcher hooks.Dispatcher
	logger     *obs.Logger
}

func NewExecutor(runner ActionRunner, leases state.LeaseManager, store state.RunStore, dispatcher hooks.Dispatcher, logger *obs.Logger) *Executor {
	if leases == nil {
		leases = state.NewLocalLeaseManager()
	}
	if dispatcher == nil {
		dispatcher = hooks.NoopDispatcher{}
	}
	return &Executor{
		runner:     runner,
		leases:     leases,
		store:      store,
		dispatcher: dispatcher,
		logger:     logger,
	}
}

// ExecutePlan executes the switchover plan step by step.
// It acquires a lease to prevent concurrent switchovers, records all steps to the store,
// and dispatches hook events at key milestones.
func (e *Executor) ExecutePlan(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan, dryRun bool) (*domain.SwitchoverExecution, error) {
	run, err := e.store.CreateRun(ctx, domain.RunRecord{
		Cluster: spec.Name,
		Kind:    domain.RunKindSwitch,
		Status:  domain.RunStatusRunning,
		Summary: fmt.Sprintf("executing switchover: %s → %s", plan.OriginalPrimary.ID, plan.Candidate.ID),
	})
	if err != nil {
		return nil, err
	}

	leaseKey := "switchover/" + spec.Name
	lease, err := e.leases.Acquire(ctx, leaseKey, spec.Controller.ID, spec.Controller.Lease.TTL)
	if err != nil {
		_ = e.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, err.Error())
		return nil, fmt.Errorf("acquire switchover lease %q: %w", leaseKey, err)
	}
	defer lease.Release(ctx)

	execution := &domain.SwitchoverExecution{
		ClusterName: spec.Name,
		DryRun:      dryRun,
		StartedAt:   time.Now(),
		Plan:        *plan,
	}

	_ = e.dispatch(ctx, spec, hooks.Event{
		Name:    "switchover.start",
		Cluster: spec.Name,
		RunKind: domain.RunKindSwitch,
		NodeID:  plan.OriginalPrimary.ID,
		Data: map[string]string{
			"original_primary": plan.OriginalPrimary.ID,
			"candidate":        plan.Candidate.ID,
		},
	})

	for _, step := range plan.Steps {
		if step.Status == "skipped" {
			execution.StepResults = append(execution.StepResults, domain.SwitchoverStepResult{
				Name:    step.Name,
				Status:  "skipped",
				Message: "step skipped during planning",
			})
			continue
		}

		e.logger.Info("executing switchover step", "step", step.Name, "dry_run", dryRun)
		stepErr := e.executeStep(ctx, spec, plan, step.Name)
		if stepErr != nil {
			execution.FailedStep = step.Name
			execution.StepResults = append(execution.StepResults, domain.SwitchoverStepResult{
				Name:    step.Name,
				Status:  "failed",
				Message: stepErr.Error(),
			})
			_ = e.store.AppendEvent(ctx, run.ID, domain.RunEvent{
				Phase:    step.Name,
				Severity: domain.EventSeverityError,
				Message:  stepErr.Error(),
			})
			_ = e.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, stepErr.Error())
			execution.FinishedAt = time.Now()

			_ = e.dispatch(ctx, spec, hooks.Event{
				Name:    "switchover.abort",
				Cluster: spec.Name,
				RunKind: domain.RunKindSwitch,
				Data:    map[string]string{"failed_step": step.Name, "error": stepErr.Error()},
			})
			return execution, stepErr
		}

		execution.StepResults = append(execution.StepResults, domain.SwitchoverStepResult{
			Name:    step.Name,
			Status:  "completed",
			Message: "step executed successfully",
		})
		_ = e.store.AppendEvent(ctx, run.ID, domain.RunEvent{
			Phase:    step.Name,
			Severity: domain.EventSeverityInfo,
			Message:  "step executed successfully",
		})
	}

	execution.FinishedAt = time.Now()
	execution.Succeeded = execution.FailedStep == ""
	_ = e.store.UpdateRun(ctx, run.ID, domain.RunStatusSucceeded,
		fmt.Sprintf("switchover completed: %s → %s", plan.OriginalPrimary.ID, plan.Candidate.ID))

	_ = e.dispatch(ctx, spec, hooks.Event{
		Name:    "switchover.complete",
		Cluster: spec.Name,
		RunKind: domain.RunKindSwitch,
		NodeID:  plan.Candidate.ID,
		Data: map[string]string{
			"new_primary":      plan.Candidate.ID,
			"original_primary": plan.OriginalPrimary.ID,
		},
	})
	return execution, nil
}

func (e *Executor) executeStep(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan, name string) error {
	switch name {
	case "precheck-writer-endpoint":
		return e.runner.PrecheckWriterEndpoint(ctx, spec, plan)
	case "lock-old-primary":
		return e.runner.LockOldPrimary(ctx, spec, plan)
	case "wait-candidate-catchup":
		return e.runner.WaitCandidateCatchUp(ctx, spec, plan)
	case "promote-candidate":
		return e.runner.PromoteCandidate(ctx, spec, plan)
	case "repoint-replicas":
		return e.runner.RepointReplicas(ctx, spec, plan)
	case "repoint-old-primary":
		return e.runner.RepointOldPrimary(ctx, spec, plan)
	case "switch-writer-endpoint":
		return e.runner.SwitchWriterEndpoint(ctx, spec, plan)
	case "verify-writer-endpoint":
		return e.runner.VerifyWriterEndpoint(ctx, spec, plan)
	case "verify":
		return e.runner.VerifyCluster(ctx, spec, plan)
	default:
		return fmt.Errorf("unknown switchover step %q", name)
	}
}

func (e *Executor) dispatch(ctx context.Context, _ domain.ClusterSpec, event hooks.Event) error {
	if err := e.dispatcher.Dispatch(ctx, event); err != nil {
		e.logger.Warn("hook dispatch failed", "event", event.Name, "error", err)
	}
	return nil
}
