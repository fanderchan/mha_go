package failover

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mha-go/internal/domain"
	"mha-go/internal/hooks"
	"mha-go/internal/obs"
	"mha-go/internal/state"
)

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

func (e *Executor) ExecutePlan(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan, dryRun bool) (*domain.FailoverExecution, error) {
	run, err := e.store.CreateRun(ctx, domain.RunRecord{
		Cluster: spec.Name,
		Kind:    domain.RunKindFailover,
		Status:  domain.RunStatusRunning,
		Summary: "executing failover plan",
	})
	if err != nil {
		return nil, err
	}

	lease, err := e.leases.Acquire(ctx, plan.LeaseKey, plan.LeaseOwner, spec.Controller.Lease.TTL)
	if err != nil {
		_ = e.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, err.Error())
		return nil, fmt.Errorf("acquire execution lease %q: %w", plan.LeaseKey, err)
	}
	defer lease.Release(ctx)

	execution := &domain.FailoverExecution{
		ClusterName: spec.Name,
		DryRun:      dryRun,
		StartedAt:   time.Now(),
		Plan:        *plan,
	}

	_ = e.dispatcher.Dispatch(ctx, hooks.Event{
		Name:    "failover.start",
		Cluster: spec.Name,
		RunKind: domain.RunKindFailover,
		NodeID:  plan.Candidate.ID,
		Data: map[string]string{
			"old_primary": plan.OldPrimary.ID,
			"candidate":   plan.Candidate.ID,
		},
	})

	salvageIndex := 0
	blocked := false
	for i, step := range plan.Steps {
		if blocked {
			execution.StepResults = append(execution.StepResults, domain.FailoverStepResult{
				Name:    step.Name,
				Status:  "skipped",
				Message: "skipped because a previous step blocked execution",
			})
			continue
		}

		if step.Blocking {
			blocked = true
			execution.Blocked = true
			execution.FailedStep = step.Name
			execution.StepResults = append(execution.StepResults, domain.FailoverStepResult{
				Name:    step.Name,
				Status:  "blocked",
				Message: firstNonEmpty(step.Reason, "step is blocked"),
			})
			_ = e.store.AppendEvent(ctx, run.ID, domain.RunEvent{
				Phase:    step.Name,
				Severity: domain.EventSeverityWarn,
				Message:  firstNonEmpty(step.Reason, "step is blocked"),
			})
			for _, remaining := range plan.Steps[i+1:] {
				execution.StepResults = append(execution.StepResults, domain.FailoverStepResult{
					Name:    remaining.Name,
					Status:  "skipped",
					Message: "skipped because a previous step blocked execution",
				})
			}
			break
		}

		if step.Status == "completed" {
			execution.StepResults = append(execution.StepResults, domain.FailoverStepResult{
				Name:    step.Name,
				Status:  "completed",
				Message: "precondition already satisfied during planning",
			})
			continue
		}

		if step.Status != "pending" {
			execution.StepResults = append(execution.StepResults, domain.FailoverStepResult{
				Name:    step.Name,
				Status:  "skipped",
				Message: fmt.Sprintf("step status %q is not executable", step.Status),
			})
			continue
		}

		stepErr := e.executeStep(ctx, spec, plan, step, &salvageIndex)
		if stepErr != nil {
			// availability-first salvage steps: warn and continue rather than abort.
			if strings.HasPrefix(step.Name, "recover-from-") && !step.Required {
				e.logger.Warn("salvage step failed, continuing (availability-first policy)",
					"step", step.Name, "error", stepErr)
				execution.StepResults = append(execution.StepResults, domain.FailoverStepResult{
					Name:    step.Name,
					Status:  "warned",
					Message: "salvage failed (availability-first): " + stepErr.Error(),
				})
				_ = e.store.AppendEvent(ctx, run.ID, domain.RunEvent{
					Phase:    step.Name,
					Severity: domain.EventSeverityWarn,
					Message:  "salvage step failed, proceeding (availability-first): " + stepErr.Error(),
				})
				continue
			}

			execution.FailedStep = step.Name
			execution.StepResults = append(execution.StepResults, domain.FailoverStepResult{
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
			_ = e.dispatcher.Dispatch(ctx, hooks.Event{
				Name:    "failover.abort",
				Cluster: spec.Name,
				RunKind: domain.RunKindFailover,
				NodeID:  plan.Candidate.ID,
				Data: map[string]string{
					"failed_step": step.Name,
					"error":       stepErr.Error(),
				},
			})
			return execution, stepErr
		}

		execution.StepResults = append(execution.StepResults, domain.FailoverStepResult{
			Name:    step.Name,
			Status:  "completed",
			Message: "step executed successfully",
		})
		_ = e.store.AppendEvent(ctx, run.ID, domain.RunEvent{
			Phase:    step.Name,
			Severity: domain.EventSeverityInfo,
			Message:  "step executed successfully",
		})

		// Per-step hook events.
		switch step.Name {
		case "fence-old-primary":
			_ = e.dispatcher.Dispatch(ctx, hooks.Event{
				Name:    "failover.fence",
				Cluster: spec.Name,
				RunKind: domain.RunKindFailover,
				NodeID:  plan.OldPrimary.ID,
				Data:    map[string]string{"fenced_node": plan.OldPrimary.ID},
			})
		case "promote-candidate":
			_ = e.dispatcher.Dispatch(ctx, hooks.Event{
				Name:    "failover.promote",
				Cluster: spec.Name,
				RunKind: domain.RunKindFailover,
				NodeID:  plan.Candidate.ID,
				Data:    map[string]string{"new_primary": plan.Candidate.ID},
			})
		case "switch-writer-endpoint":
			_ = e.dispatcher.Dispatch(ctx, hooks.Event{
				Name:    "failover.writer_switched",
				Cluster: spec.Name,
				RunKind: domain.RunKindFailover,
				NodeID:  plan.Candidate.ID,
				Data: map[string]string{
					"new_primary": plan.Candidate.ID,
					"old_primary": plan.OldPrimary.ID,
				},
			})
		}
	}

	execution.FinishedAt = time.Now()
	execution.Succeeded = !execution.Blocked && execution.FailedStep == ""
	finalStatus := domain.RunStatusSucceeded
	finalSummary := "failover execution completed"
	if execution.Blocked {
		finalStatus = domain.RunStatusAborted
		finalSummary = fmt.Sprintf("failover execution blocked at %s", execution.FailedStep)
		_ = e.dispatcher.Dispatch(ctx, hooks.Event{
			Name:    "failover.abort",
			Cluster: spec.Name,
			RunKind: domain.RunKindFailover,
			NodeID:  plan.Candidate.ID,
			Data: map[string]string{
				"failed_step": execution.FailedStep,
				"reason":      "blocked",
			},
		})
	} else if execution.FailedStep != "" {
		finalStatus = domain.RunStatusFailed
		finalSummary = fmt.Sprintf("failover execution failed at %s", execution.FailedStep)
	} else {
		_ = e.dispatcher.Dispatch(ctx, hooks.Event{
			Name:    "failover.complete",
			Cluster: spec.Name,
			RunKind: domain.RunKindFailover,
			NodeID:  plan.Candidate.ID,
			Data: map[string]string{
				"new_primary": plan.Candidate.ID,
				"old_primary": plan.OldPrimary.ID,
			},
		})
	}
	_ = e.store.UpdateRun(ctx, run.ID, finalStatus, finalSummary)
	return execution, nil
}

func (e *Executor) executeStep(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan, step domain.FailoverStep, salvageIndex *int) error {
	switch {
	case step.Name == "fence-old-primary":
		return e.runner.FenceOldPrimary(ctx, spec, plan)
	case strings.HasPrefix(step.Name, "recover-from-"):
		if *salvageIndex >= len(plan.SalvageActions) {
			return fmt.Errorf("no salvage action available for step %q", step.Name)
		}
		action := plan.SalvageActions[*salvageIndex]
		*salvageIndex = *salvageIndex + 1
		return e.runner.ApplySalvageAction(ctx, spec, plan, action)
	case step.Name == "promote-candidate":
		return e.runner.PromoteCandidate(ctx, spec, plan)
	case step.Name == "repoint-replicas":
		return e.runner.RepointReplicas(ctx, spec, plan)
	case step.Name == "switch-writer-endpoint":
		return e.runner.SwitchWriterEndpoint(ctx, spec, plan)
	case step.Name == "verify-cluster":
		return e.runner.VerifyCluster(ctx, spec, plan)
	default:
		return nil
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
