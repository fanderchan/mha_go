package failover

import (
	"context"
	"fmt"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
)

type ActionRunner interface {
	FenceOldPrimary(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error
	ApplySalvageAction(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan, action domain.SalvageAction) error
	PromoteCandidate(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error
	RepointReplicas(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error
	SwitchWriterEndpoint(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error
	VerifyCluster(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error
}

type DryRunActionRunner struct {
	logger *obs.Logger
}

func NewDryRunActionRunner(logger *obs.Logger) *DryRunActionRunner {
	return &DryRunActionRunner{logger: logger}
}

func (r *DryRunActionRunner) FenceOldPrimary(_ context.Context, _ domain.ClusterSpec, plan *domain.FailoverPlan) error {
	r.logger.Info("dry-run fence old primary", "old_primary", plan.OldPrimary.ID)
	return nil
}

func (r *DryRunActionRunner) ApplySalvageAction(_ context.Context, _ domain.ClusterSpec, _ *domain.FailoverPlan, action domain.SalvageAction) error {
	r.logger.Info("dry-run salvage action", "kind", action.Kind, "donor", action.DonorNodeID, "target", action.TargetNodeID, "missing_gtids", action.MissingGTIDSet)
	return nil
}

func (r *DryRunActionRunner) PromoteCandidate(_ context.Context, _ domain.ClusterSpec, plan *domain.FailoverPlan) error {
	r.logger.Info("dry-run promote candidate", "candidate", plan.Candidate.ID)
	return nil
}

func (r *DryRunActionRunner) RepointReplicas(_ context.Context, _ domain.ClusterSpec, plan *domain.FailoverPlan) error {
	r.logger.Info("dry-run repoint replicas", "candidate", plan.Candidate.ID)
	return nil
}

func (r *DryRunActionRunner) SwitchWriterEndpoint(_ context.Context, _ domain.ClusterSpec, plan *domain.FailoverPlan) error {
	r.logger.Info("dry-run switch writer endpoint", "candidate", plan.Candidate.ID)
	return nil
}

func (r *DryRunActionRunner) VerifyCluster(_ context.Context, _ domain.ClusterSpec, plan *domain.FailoverPlan) error {
	r.logger.Info("dry-run verify cluster", "candidate", plan.Candidate.ID)
	return nil
}

func stepLabel(step domain.FailoverStep) string {
	if step.Reason == "" {
		return step.Name
	}
	return fmt.Sprintf("%s (%s)", step.Name, step.Reason)
}
