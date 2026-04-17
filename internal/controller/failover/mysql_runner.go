package failover

import (
	"context"
	"fmt"
	"sort"
	"time"

	"mha-go/internal/domain"
	"mha-go/internal/fencing"
	"mha-go/internal/obs"
	sqltransport "mha-go/internal/transport/sql"
	"mha-go/internal/writerendpoint"
)

// MySQLActionRunner performs failover steps against MySQL using the SQL transport.
type MySQLActionRunner struct {
	sql    *sqltransport.MySQLInspector
	logger *obs.Logger
}

func NewMySQLActionRunner(inspector *sqltransport.MySQLInspector, logger *obs.Logger) *MySQLActionRunner {
	return &MySQLActionRunner{
		sql:    inspector,
		logger: logger,
	}
}

func (r *MySQLActionRunner) PrecheckWriterEndpoint(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error {
	return writerendpoint.Precheck(ctx, spec, plan)
}

func (r *MySQLActionRunner) FenceOldPrimary(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error {
	coordinator := fencing.NewCoordinator(r.logger)
	readOnly := func(ctx context.Context) error {
		ns, ok := nodeSpecByID(spec, plan.OldPrimary.ID)
		if !ok {
			return fmt.Errorf("cluster spec has no node %q for old primary", plan.OldPrimary.ID)
		}
		db, err := r.sql.OpenDB(ctx, ns)
		if err != nil {
			if plan.OldPrimary.Health == domain.NodeHealthDead {
				r.logger.Warn("read-only fence skipped: old primary unreachable (treated as already isolated)", "node", plan.OldPrimary.ID)
				return nil
			}
			return fmt.Errorf("connect to old primary %q for fencing: %w", plan.OldPrimary.ID, err)
		}
		defer db.Close()
		return sqltransport.FenceReadOnly(ctx, db, plan.OldPrimary.Capabilities)
	}
	return coordinator.FenceOldPrimary(ctx, spec, plan.OldPrimary, plan.Candidate, readOnly)
}

func (r *MySQLActionRunner) ApplySalvageAction(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan, action domain.SalvageAction) error {
	switch action.Kind {
	case "recover-from-old-primary", "recover-from-replica":
		donorSpec, ok := nodeSpecByID(spec, action.DonorNodeID)
		if !ok {
			return fmt.Errorf("missing node spec for donor %q", action.DonorNodeID)
		}
		targetSpec, ok := nodeSpecByID(spec, action.TargetNodeID)
		if !ok {
			return fmt.Errorf("missing node spec for salvage target %q", action.TargetNodeID)
		}
		donorPassword, err := r.sql.ResolvePassword(ctx, donorSpec.SQL.PasswordRef)
		if err != nil {
			return fmt.Errorf("resolve donor password: %w", err)
		}
		wait := spec.Replication.Salvage.Timeout
		if wait <= 0 {
			wait = 30 * time.Minute
		}
		db, err := r.sql.OpenDB(ctx, targetSpec)
		if err != nil {
			return fmt.Errorf("connect to salvage target %q: %w", action.TargetNodeID, err)
		}
		defer db.Close()
		return sqltransport.SalvageCatchUpFromDonor(ctx, db, donorSpec, donorPassword, action.MissingGTIDSet, wait)
	default:
		return fmt.Errorf("unsupported salvage action kind %q", action.Kind)
	}
}

func (r *MySQLActionRunner) PromoteCandidate(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error {
	ns, ok := nodeSpecByID(spec, plan.Candidate.ID)
	if !ok {
		return fmt.Errorf("cluster spec has no node %q for candidate", plan.Candidate.ID)
	}
	db, err := r.sql.OpenDB(ctx, ns)
	if err != nil {
		return fmt.Errorf("connect to candidate %q: %w", plan.Candidate.ID, err)
	}
	defer db.Close()
	return sqltransport.PromoteReplicaToPrimary(ctx, db)
}

func (r *MySQLActionRunner) RepointReplicas(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error {
	candSpec, ok := nodeSpecByID(spec, plan.Candidate.ID)
	if !ok {
		return fmt.Errorf("cluster spec has no node %q for candidate", plan.Candidate.ID)
	}
	sourcePassword, err := r.sql.ResolvePassword(ctx, candSpec.SQL.PasswordRef)
	if err != nil {
		return fmt.Errorf("resolve replication password for candidate: %w", err)
	}
	ids := plan.RepointReplicaIDs
	if ids == nil {
		ids = repointReplicaNodeIDs(spec, plan.Candidate.ID)
	}
	for _, id := range ids {
		ns, found := nodeSpecByID(spec, id)
		if !found {
			continue
		}
		if id == plan.OldPrimary.ID && plan.OldPrimary.Health == domain.NodeHealthDead {
			r.logger.Warn("repoint skipped: old primary unreachable", "node", id)
			continue
		}
		db, err := r.sql.OpenDB(ctx, ns)
		if err != nil {
			if id == plan.OldPrimary.ID && plan.OldPrimary.Health == domain.NodeHealthDead {
				r.logger.Warn("repoint skipped: old primary unreachable", "node", id)
				continue
			}
			r.logger.Warn("repoint skipped: replica unreachable", "replica", id, "error", err)
			plan.SkippedReplicaIDs = appendIfMissing(plan.SkippedReplicaIDs, id)
			continue
		}
		repErr := sqltransport.RepointReplicaToSource(ctx, db, candSpec, sourcePassword)
		_ = db.Close()
		if repErr != nil {
			r.logger.Warn("repoint skipped: replica repoint failed", "replica", id, "error", repErr)
			plan.SkippedReplicaIDs = appendIfMissing(plan.SkippedReplicaIDs, id)
			continue
		}
		r.logger.Info("repointed replica to new primary", "replica", id, "source", plan.Candidate.ID)
	}
	return nil
}

func (r *MySQLActionRunner) SwitchWriterEndpoint(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error {
	return writerendpoint.Switch(ctx, spec, plan)
}

func (r *MySQLActionRunner) VerifyWriterEndpoint(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error {
	return writerendpoint.Verify(ctx, spec, plan)
}

func (r *MySQLActionRunner) VerifyCluster(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan) error {
	return VerifyPostFailover(ctx, r.sql, spec, plan, r.logger)
}

func nodeSpecByID(spec domain.ClusterSpec, id string) (domain.NodeSpec, bool) {
	for _, n := range spec.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return domain.NodeSpec{}, false
}

func repointReplicaNodeIDs(spec domain.ClusterSpec, candidateID string) []string {
	out := make([]string, 0, len(spec.Nodes))
	for _, n := range spec.Nodes {
		if n.ID == candidateID {
			continue
		}
		if n.NoMaster || n.ExpectedRole == domain.NodeRoleObserver {
			continue
		}
		out = append(out, n.ID)
	}
	sort.Strings(out)
	return out
}

func appendIfMissing(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}
