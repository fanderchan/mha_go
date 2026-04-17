package failover

import (
	"context"
	"fmt"
	"strings"
	"time"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
	"mha-go/internal/replication"
	"mha-go/internal/state"
	"mha-go/internal/topology"
)

type Controller struct {
	discoverer topology.Discoverer
	selector   topology.CandidateSelector
	leases     state.LeaseManager
	store      state.RunStore
	logger     *obs.Logger
}

func NewController(discoverer topology.Discoverer, selector topology.CandidateSelector, leases state.LeaseManager, store state.RunStore, logger *obs.Logger) *Controller {
	if leases == nil {
		leases = state.NewLocalLeaseManager()
	}
	return &Controller{
		discoverer: discoverer,
		selector:   selector,
		leases:     leases,
		store:      store,
		logger:     logger,
	}
}

func (c *Controller) BuildPlan(ctx context.Context, spec domain.ClusterSpec) (*domain.FailoverPlan, error) {
	run, err := c.store.CreateRun(ctx, domain.RunRecord{
		Cluster: spec.Name,
		Kind:    domain.RunKindFailover,
		Status:  domain.RunStatusRunning,
	})
	if err != nil {
		return nil, err
	}

	leaseKey := fmt.Sprintf("failover/%s", spec.Name)
	lease, err := c.leases.Acquire(ctx, leaseKey, spec.Controller.ID, spec.Controller.Lease.TTL)
	if err != nil {
		_ = c.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, err.Error())
		return nil, fmt.Errorf("acquire failover lease %q: %w", leaseKey, err)
	}
	defer lease.Release(ctx)
	_ = c.store.AppendEvent(ctx, run.ID, domain.RunEvent{
		Phase:    "lease",
		Severity: domain.EventSeverityInfo,
		Message:  "acquired failover lease",
		Metadata: map[string]string{
			"lease_key":   lease.Key(),
			"lease_owner": lease.Owner(),
		},
	})

	view, err := c.discoverer.Discover(ctx, spec)
	if err != nil {
		_ = c.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, err.Error())
		return nil, err
	}
	assessment := topology.AssessReplication(spec, view)
	for _, finding := range assessment.Findings {
		_ = c.store.AppendEvent(ctx, run.ID, domain.RunEvent{
			Phase:    "assess",
			Severity: finding.Severity,
			Message:  finding.Message,
			Metadata: map[string]string{
				"code":    finding.Code,
				"node_id": finding.NodeID,
			},
		})
	}

	oldPrimary, ok := view.PrimaryNode()
	if !ok {
		err = fmt.Errorf("cluster %q has no primary in the discovered view", spec.Name)
		_ = c.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, err.Error())
		return nil, err
	}

	candidate, err := c.selector.SelectFailoverCandidate(ctx, spec, view)
	if err != nil {
		_ = c.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, err.Error())
		return nil, err
	}

	recovery := replication.BuildRecoverySummary(view, *oldPrimary, *candidate, spec.Replication.Salvage.Policy)
	primaryFailureConfirmed, primaryFailureReason := confirmPrimaryFailure(*oldPrimary)
	promoteReadinessReasons := evaluatePromoteReadiness(spec, *oldPrimary, *candidate, recovery)
	blockingReasons := buildBlockingReasons(spec, *oldPrimary, *candidate, assessment, recovery, primaryFailureConfirmed, promoteReadinessReasons)
	plan := &domain.FailoverPlan{
		ClusterName:                  spec.Name,
		CreatedAt:                    time.Now(),
		OldPrimary:                   *oldPrimary,
		Candidate:                    *candidate,
		LeaseKey:                     lease.Key(),
		LeaseOwner:                   lease.Owner(),
		PrimaryFailureConfirmed:      primaryFailureConfirmed,
		PrimaryFailureReason:         primaryFailureReason,
		PromoteReadinessConfirmed:    len(promoteReadinessReasons) == 0,
		PromoteReadinessReasons:      promoteReadinessReasons,
		ExecutionPermitted:           len(blockingReasons) == 0,
		BlockingReasons:              blockingReasons,
		AssessmentErrors:             len(assessment.Errors()),
		AssessmentWarnings:           len(assessment.Warnings()),
		CandidateFreshnessScore:      recovery.CandidateFreshnessScore,
		CandidateMostAdvanced:        recovery.CandidateMostAdvanced,
		SalvagePolicy:                spec.Replication.Salvage.Policy,
		ShouldAttemptSalvage:         shouldAttemptSalvage(spec, recovery),
		MissingFromPrimaryKnown:      recovery.MissingFromPrimaryKnown,
		MissingFromPrimaryGTIDSet:    recovery.MissingFromPrimarySet,
		RecoveryGaps:                 recovery.RecoveryGaps,
		SalvageActions:               recovery.SalvageActions,
		SuggestedDonorIDs:            recovery.SuggestedDonorIDs,
		RequiresFencing:              true,
		RequiresWriterEndpointSwitch: writerEndpointEnabled(spec.WriterEndpoint.Kind),
		RepointReplicaIDs:            repointReplicaIDsForPlan(spec, view, candidate.ID),
		SkippedReplicaIDs:            skippedReplicaIDsForPlan(spec, view, candidate.ID),
	}
	plan.Steps = buildExecutionSteps(plan)

	summary := fmt.Sprintf("planned failover from %s to %s", plan.OldPrimary.ID, plan.Candidate.ID)
	_ = c.store.AppendEvent(ctx, run.ID, domain.RunEvent{
		Phase:    "plan",
		Severity: domain.EventSeverityInfo,
		Message:  summary,
		Metadata: map[string]string{
			"candidate_freshness_score": fmt.Sprintf("%d", plan.CandidateFreshnessScore),
			"candidate_most_advanced":   fmt.Sprintf("%t", plan.CandidateMostAdvanced),
			"primary_failure_confirmed": fmt.Sprintf("%t", plan.PrimaryFailureConfirmed),
			"promote_readiness":         fmt.Sprintf("%t", plan.PromoteReadinessConfirmed),
			"execution_permitted":       fmt.Sprintf("%t", plan.ExecutionPermitted),
			"missing_from_primary":      plan.MissingFromPrimaryGTIDSet,
		},
	})
	_ = c.store.UpdateRun(ctx, run.ID, domain.RunStatusSucceeded, summary)
	c.logger.Info("failover plan built", "cluster", spec.Name, "old_primary", plan.OldPrimary.ID, "candidate", plan.Candidate.ID, "salvage_policy", plan.SalvagePolicy, "candidate_most_advanced", plan.CandidateMostAdvanced, "candidate_freshness_score", plan.CandidateFreshnessScore)
	return plan, nil
}

func shouldAttemptSalvage(_ domain.ClusterSpec, recovery replication.RecoverySummary) bool {
	if recovery.MissingFromPrimaryKnown && recovery.MissingFromPrimarySet != "" {
		return true
	}
	if len(recovery.RecoveryGaps) > 0 {
		return true
	}
	return false
}

func confirmPrimaryFailure(primary domain.NodeState) (bool, string) {
	switch primary.Health {
	case domain.NodeHealthDead:
		return true, "primary health is dead in the discovered topology"
	case domain.NodeHealthSuspect:
		return false, "primary is only suspect; failure is not confirmed"
	case domain.NodeHealthAlive:
		return false, "primary is still reachable"
	default:
		return false, "primary health is unknown"
	}
}

func buildBlockingReasons(spec domain.ClusterSpec, oldPrimary, candidate domain.NodeState, assessment topology.Assessment, recovery replication.RecoverySummary, primaryFailureConfirmed bool, promoteReadinessReasons []string) []string {
	reasons := make([]string, 0, 6)
	if !primaryFailureConfirmed {
		reasons = append(reasons, "primary failure is not confirmed")
	}
	for _, reason := range promoteReadinessReasons {
		reasons = append(reasons, reason)
	}
	if candidate.Health == domain.NodeHealthDead {
		reasons = append(reasons, fmt.Sprintf("candidate %s is unreachable", candidate.ID))
	}
	if candidate.Replica == nil {
		reasons = append(reasons, fmt.Sprintf("candidate %s has no replica state", candidate.ID))
		return reasons
	}
	if !candidate.Replica.AutoPosition {
		reasons = append(reasons, fmt.Sprintf("candidate %s does not use GTID auto-position", candidate.ID))
	}
	if !candidate.Replica.SQLThreadRunning {
		reasons = append(reasons, fmt.Sprintf("candidate %s SQL thread is not running", candidate.ID))
	}
	if candidate.Replica.SourceID == "" {
		reasons = append(reasons, fmt.Sprintf("candidate %s source is not mapped to the configured topology", candidate.ID))
	}
	if !spec.Topology.AllowCascadingReplicas && candidate.Replica.SourceID != "" && candidate.Replica.SourceID != oldPrimary.ID {
		reasons = append(reasons, fmt.Sprintf("candidate %s sources from %s instead of the old primary %s", candidate.ID, candidate.Replica.SourceID, oldPrimary.ID))
	}
	// strict policy blocks at plan time: any known missing transactions cannot be
	// guaranteed to survive salvage, so the operator must resolve them manually.
	if spec.Replication.Salvage.Policy == domain.SalvageStrict || spec.Replication.Salvage.Policy == "" {
		if recovery.MissingFromPrimaryKnown && recovery.MissingFromPrimarySet != "" {
			reasons = append(reasons, "strict salvage policy: candidate is missing transactions from the old primary")
		}
		if len(recovery.RecoveryGaps) > 0 {
			reasons = append(reasons, "strict salvage policy: candidate is behind one or more surviving replicas")
		}
	}
	for _, finding := range assessment.Errors() {
		if finding.NodeID != "" && finding.NodeID != candidate.ID && finding.NodeID != oldPrimary.ID {
			continue
		}
		switch finding.Code {
		case "primary_unhealthy":
			continue
		case "replica_dead":
			if finding.NodeID != candidate.ID {
				continue
			}
		}
		reasons = append(reasons, fmt.Sprintf("assessment error[%s]: %s", finding.Code, finding.Message))
	}
	return dedupeStrings(reasons)
}

func evaluatePromoteReadiness(spec domain.ClusterSpec, oldPrimary, candidate domain.NodeState, recovery replication.RecoverySummary) []string {
	reasons := make([]string, 0, 6)
	if candidate.Health == domain.NodeHealthDead {
		reasons = append(reasons, fmt.Sprintf("candidate %s is unreachable", candidate.ID))
	}
	if candidate.Replica == nil {
		reasons = append(reasons, fmt.Sprintf("candidate %s has no replica state", candidate.ID))
		return reasons
	}
	if !candidate.Replica.AutoPosition {
		reasons = append(reasons, fmt.Sprintf("candidate %s auto-position is disabled", candidate.ID))
	}
	if !candidate.Replica.SQLThreadRunning {
		reasons = append(reasons, fmt.Sprintf("candidate %s SQL thread is not running", candidate.ID))
	}
	if candidate.Replica.SourceID == "" {
		reasons = append(reasons, fmt.Sprintf("candidate %s source is unknown", candidate.ID))
	}
	if !spec.Topology.AllowCascadingReplicas && candidate.Replica.SourceID != "" && candidate.Replica.SourceID != oldPrimary.ID {
		reasons = append(reasons, fmt.Sprintf("candidate %s is not directly replicating from the old primary %s", candidate.ID, oldPrimary.ID))
	}
	if !candidate.ReadOnly || !candidate.SuperReadOnly {
		reasons = append(reasons, fmt.Sprintf("candidate %s is writable before promotion", candidate.ID))
	}
	// Lag is NULL (parsed as -1) when IO thread is stopped. When the primary is dead the
	// IO thread stopping is expected; the SQL thread check above is the real safety gate.
	if candidate.Replica.IOThreadRunning && candidate.Replica.SecondsBehindSource < 0 {
		reasons = append(reasons, fmt.Sprintf("candidate %s lag is unknown", candidate.ID))
	}
	_ = recovery // recovery gaps are handled separately via salvage steps
	return dedupeStrings(reasons)
}

func buildExecutionSteps(plan *domain.FailoverPlan) []domain.FailoverStep {
	steps := []domain.FailoverStep{
		{Name: "acquire-lease", Status: "completed"},
		{Name: "confirm-primary-dead", Status: stepStatus(plan.PrimaryFailureConfirmed, "blocked"), Blocking: !plan.PrimaryFailureConfirmed, Reason: plan.PrimaryFailureReason},
	}

	if plan.RequiresWriterEndpointSwitch {
		steps = append(steps, domain.FailoverStep{
			Name:     "precheck-writer-endpoint",
			Status:   gateStatus(plan.ExecutionPermitted),
			Blocking: !plan.ExecutionPermitted,
			Reason:   firstBlockingReason(plan.BlockingReasons),
		})
	}

	if plan.RequiresFencing {
		steps = append(steps, domain.FailoverStep{
			Name:     "fence-old-primary",
			Status:   gateStatus(plan.ExecutionPermitted),
			Blocking: !plan.ExecutionPermitted,
			Reason:   firstBlockingReason(plan.BlockingReasons),
		})
	}

	for _, action := range plan.SalvageActions {
		steps = append(steps, domain.FailoverStep{
			Name:     action.Kind,
			Status:   gateStatus(plan.ExecutionPermitted),
			Blocking: !plan.ExecutionPermitted && action.Required,
			Required: action.Required,
			Reason:   action.Reason,
		})
	}

	steps = append(steps,
		domain.FailoverStep{
			Name:     "promote-candidate",
			Status:   gateStatus(plan.PromoteReadinessConfirmed && plan.ExecutionPermitted),
			Blocking: !plan.PromoteReadinessConfirmed || !plan.ExecutionPermitted,
			Reason:   firstBlockingReason(append(plan.PromoteReadinessReasons, plan.BlockingReasons...)),
		},
		domain.FailoverStep{
			Name:     "repoint-replicas",
			Status:   gateStatus(plan.ExecutionPermitted),
			Blocking: !plan.ExecutionPermitted,
			Reason:   firstBlockingReason(plan.BlockingReasons),
		},
	)

	if plan.RequiresWriterEndpointSwitch {
		steps = append(steps, domain.FailoverStep{
			Name:     "switch-writer-endpoint",
			Status:   gateStatus(plan.ExecutionPermitted),
			Blocking: !plan.ExecutionPermitted,
			Reason:   firstBlockingReason(plan.BlockingReasons),
		})
		steps = append(steps, domain.FailoverStep{
			Name:     "verify-writer-endpoint",
			Status:   gateStatus(plan.ExecutionPermitted),
			Blocking: !plan.ExecutionPermitted,
			Reason:   firstBlockingReason(plan.BlockingReasons),
		})
	}

	steps = append(steps, domain.FailoverStep{
		Name:     "verify-cluster",
		Status:   gateStatus(plan.ExecutionPermitted),
		Blocking: !plan.ExecutionPermitted,
		Reason:   firstBlockingReason(plan.BlockingReasons),
	})
	return steps
}

func gateStatus(permitted bool) string {
	if permitted {
		return "pending"
	}
	return "blocked"
}

func stepStatus(ok bool, blockedStatus string) string {
	if ok {
		return "completed"
	}
	return blockedStatus
}

func firstBlockingReason(reasons []string) string {
	if len(reasons) == 0 {
		return ""
	}
	return reasons[0]
}

func writerEndpointEnabled(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "none", "off":
		return false
	default:
		return true
	}
}

func repointReplicaIDsForPlan(spec domain.ClusterSpec, view *domain.ClusterView, candidateID string) []string {
	out := make([]string, 0, len(spec.Nodes))
	for _, n := range spec.Nodes {
		if n.ID == candidateID || n.NoMaster || n.ExpectedRole == domain.NodeRoleObserver {
			continue
		}
		node, ok := nodeStateByID(view, n.ID)
		if ok && node.Health == domain.NodeHealthDead {
			continue
		}
		out = append(out, n.ID)
	}
	return out
}

func skippedReplicaIDsForPlan(spec domain.ClusterSpec, view *domain.ClusterView, candidateID string) []string {
	out := make([]string, 0, len(spec.Nodes))
	for _, n := range spec.Nodes {
		if n.ID == candidateID || n.NoMaster || n.ExpectedRole == domain.NodeRoleObserver {
			continue
		}
		node, ok := nodeStateByID(view, n.ID)
		if ok && node.Health == domain.NodeHealthDead {
			out = append(out, n.ID)
		}
	}
	return out
}

func nodeStateByID(view *domain.ClusterView, id string) (domain.NodeState, bool) {
	if view == nil {
		return domain.NodeState{}, false
	}
	for _, node := range view.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return domain.NodeState{}, false
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, item := range in {
		if _, ok := seen[item]; ok {
			continue
		}
		seen[item] = struct{}{}
		out = append(out, item)
	}
	return out
}
