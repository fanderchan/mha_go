package switchover

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
	store      state.RunStore
	logger     *obs.Logger
}

func NewController(discoverer topology.Discoverer, selector topology.CandidateSelector, store state.RunStore, logger *obs.Logger) *Controller {
	return &Controller{
		discoverer: discoverer,
		selector:   selector,
		store:      store,
		logger:     logger,
	}
}

func (c *Controller) BuildPlan(ctx context.Context, spec domain.ClusterSpec) (*domain.SwitchoverPlan, error) {
	run, err := c.store.CreateRun(ctx, domain.RunRecord{
		Cluster: spec.Name,
		Kind:    domain.RunKindSwitch,
		Status:  domain.RunStatusRunning,
	})
	if err != nil {
		return nil, err
	}

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
	if assessment.HasErrors() {
		err = fmt.Errorf("switchover precheck failed: %d assessment errors", len(assessment.Errors()))
		_ = c.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, err.Error())
		return nil, err
	}

	primary, ok := view.PrimaryNode()
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
	if !candidate.ReadOnly || !candidate.SuperReadOnly {
		if errantGTID, err := candidateErrantGTID(*primary, *candidate); err != nil {
			err = fmt.Errorf("switchover precheck failed: validate writable candidate %s GTID: %w", candidate.ID, err)
			_ = c.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, err.Error())
			return nil, err
		} else if errantGTID != "" {
			err = fmt.Errorf("switchover precheck failed: writable candidate %s has errant GTIDs not present on original primary %s: %s",
				candidate.ID, primary.ID, errantGTID)
			_ = c.store.UpdateRun(ctx, run.ID, domain.RunStatusFailed, err.Error())
			return nil, err
		}
	}

	requiresEndpointSwitch := writerEndpointEnabled(spec.WriterEndpoint.Kind)
	plan := &domain.SwitchoverPlan{
		ClusterName:                  spec.Name,
		CreatedAt:                    time.Now(),
		OriginalPrimary:              *primary,
		Candidate:                    *candidate,
		RequiresWriterEndpointSwitch: requiresEndpointSwitch,
		Steps:                        buildSwitchoverSteps(spec, candidate.ID, primary.ID, requiresEndpointSwitch),
	}

	summary := fmt.Sprintf("planned switchover from %s to %s", plan.OriginalPrimary.ID, plan.Candidate.ID)
	_ = c.store.AppendEvent(ctx, run.ID, domain.RunEvent{
		Phase:    "plan",
		Severity: domain.EventSeverityInfo,
		Message:  summary,
	})
	_ = c.store.UpdateRun(ctx, run.ID, domain.RunStatusSucceeded, summary)
	c.logger.Info("switchover plan built", "cluster", spec.Name, "primary", plan.OriginalPrimary.ID, "candidate", plan.Candidate.ID, "steps", len(plan.Steps))
	return plan, nil
}

func candidateErrantGTID(primary, candidate domain.NodeState) (string, error) {
	candidateGTID := strings.TrimSpace(candidate.GTIDExecuted)
	if candidateGTID == "" {
		return "", nil
	}
	primaryGTID := strings.TrimSpace(primary.GTIDExecuted)
	if primaryGTID == "" {
		return "", fmt.Errorf("original primary GTID is unavailable")
	}
	errantGTID, _, err := replication.SubtractGTIDSets(candidateGTID, primaryGTID)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(errantGTID), nil
}

func writerEndpointEnabled(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", "none", "off":
		return false
	default:
		return true
	}
}

// buildSwitchoverSteps assembles the ordered execution steps for the switchover.
func buildSwitchoverSteps(spec domain.ClusterSpec, candidateID, origPrimaryID string, requiresEndpointSwitch bool) []domain.SwitchoverStep {
	pending := func(name string) domain.SwitchoverStep {
		return domain.SwitchoverStep{Name: name, Status: "pending"}
	}
	skipped := func(name string) domain.SwitchoverStep {
		return domain.SwitchoverStep{Name: name, Status: "skipped"}
	}

	steps := []domain.SwitchoverStep{
		pending("lock-candidate"),
		pending("lock-old-primary"),
		pending("wait-candidate-catchup"),
		pending("promote-candidate"),
	}

	if requiresEndpointSwitch {
		steps = append([]domain.SwitchoverStep{pending("precheck-writer-endpoint")}, steps...)
	}

	// repoint-replicas only if there are other replicas besides candidate and old primary
	hasOtherReplicas := false
	for _, n := range spec.Nodes {
		if n.ID == candidateID || n.ID == origPrimaryID || n.NoMaster {
			continue
		}
		if n.ExpectedRole == domain.NodeRoleObserver {
			continue
		}
		hasOtherReplicas = true
		break
	}
	if hasOtherReplicas {
		steps = append(steps, pending("repoint-replicas"))
	} else {
		steps = append(steps, skipped("repoint-replicas"))
	}

	steps = append(steps, pending("repoint-old-primary"))

	if requiresEndpointSwitch {
		steps = append(steps, pending("switch-writer-endpoint"))
		steps = append(steps, pending("verify-writer-endpoint"))
	} else {
		steps = append(steps, skipped("switch-writer-endpoint"))
	}

	steps = append(steps, pending("verify"))
	return steps
}
