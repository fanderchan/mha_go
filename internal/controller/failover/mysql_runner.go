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
	sshtransport "mha-go/internal/transport/ssh"
	"mha-go/internal/writerendpoint"
)

// MySQLActionRunner performs failover steps against MySQL using the SQL transport.
type MySQLActionRunner struct {
	sql    *sqltransport.MySQLInspector
	ssh    *sshtransport.Client
	logger *obs.Logger
}

func NewMySQLActionRunner(inspector *sqltransport.MySQLInspector, logger *obs.Logger) *MySQLActionRunner {
	return &MySQLActionRunner{
		sql:    inspector,
		ssh:    sshtransport.NewClient(sqltransport.NewRefResolver()),
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
		err = sqltransport.FenceReadOnly(ctx, db, plan.OldPrimary.Capabilities)
		if sqltransport.IsReadOnlyFenceDegraded(err) {
			// Writes are still blocked to non-SUPER users; treat degraded fence as a warning,
			// not a step failure, so a Required read-only fence step does not abort failover.
			r.logger.Warn("read-only fence degraded: super_read_only failed and read_only fallback was applied",
				"node", plan.OldPrimary.ID, "error", err)
			return nil
		}
		return err
	}
	return coordinator.FenceOldPrimary(ctx, spec, plan.OldPrimary, plan.Candidate, readOnly)
}

func (r *MySQLActionRunner) ApplySalvageAction(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan, action domain.SalvageAction) error {
	switch action.Kind {
	case "recover-from-old-primary", "recover-from-replica", "recover-from-old-primary-binlog":
		donorSpec, ok := nodeSpecByID(spec, action.DonorNodeID)
		if !ok {
			return fmt.Errorf("missing node spec for donor %q", action.DonorNodeID)
		}
		targetSpec, ok := nodeSpecByID(spec, action.TargetNodeID)
		if !ok {
			return fmt.Errorf("missing node spec for salvage target %q", action.TargetNodeID)
		}
		if action.Kind == "recover-from-old-primary-binlog" || (action.Kind == "recover-from-old-primary" && plan.OldPrimary.Health == domain.NodeHealthDead && donorSpec.SSH != nil) {
			return r.applySSHBinlogSalvage(ctx, spec, plan, action, donorSpec, targetSpec)
		}
		err := r.applySQLDonorSalvage(ctx, spec, action, donorSpec, targetSpec)
		if err != nil && action.Kind == "recover-from-old-primary" && donorSpec.SSH != nil {
			r.logger.Warn("SQL salvage from old primary failed; trying SSH binlog salvage",
				"donor", action.DonorNodeID, "target", action.TargetNodeID, "error", err)
			if sshErr := r.applySSHBinlogSalvage(ctx, spec, plan, action, donorSpec, targetSpec); sshErr != nil {
				return fmt.Errorf("SQL salvage failed: %v; SSH binlog salvage failed: %w", err, sshErr)
			}
			return nil
		}
		return err
	default:
		return fmt.Errorf("unsupported salvage action kind %q", action.Kind)
	}
}

func (r *MySQLActionRunner) applySQLDonorSalvage(ctx context.Context, spec domain.ClusterSpec, action domain.SalvageAction, donorSpec, targetSpec domain.NodeSpec) error {
	donorPassword, err := r.sql.ResolvePassword(ctx, donorSpec.SQL.ReplicationPasswordRefOrDefault())
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
	err = sqltransport.SalvageCatchUpFromDonor(ctx, db, donorSpec, donorPassword, action.MissingGTIDSet, wait)
	if err != nil {
		// Ensure the replica channel left configured by RepointReplicaToSource is stopped
		// before returning. Otherwise a later SSH binlog salvage fallback would write to the
		// candidate concurrently with the IO/SQL thread still pulling from the same donor.
		if cleanupErr := sqltransport.StopAndResetReplica(ctx, db); cleanupErr != nil {
			r.logger.Warn("failed to quiesce replica channel after SQL salvage failure",
				"target", action.TargetNodeID, "error", cleanupErr)
		}
	}
	return err
}

func (r *MySQLActionRunner) applySSHBinlogSalvage(ctx context.Context, spec domain.ClusterSpec, plan *domain.FailoverPlan, action domain.SalvageAction, donorSpec, targetSpec domain.NodeSpec) error {
	if donorSpec.SSH == nil {
		return fmt.Errorf("donor %q has no ssh config for binlog salvage", donorSpec.ID)
	}
	includeGTIDSet := action.MissingGTIDSet
	excludeGTIDSet := ""
	if action.Kind == "recover-from-old-primary-binlog" {
		includeGTIDSet = ""
		excludeGTIDSet = plan.Candidate.GTIDExecuted
	}
	if includeGTIDSet == "" && excludeGTIDSet == "" {
		return fmt.Errorf("SSH binlog salvage needs a missing GTID set or candidate GTID set filter")
	}

	targetPassword, err := r.sql.ResolvePassword(ctx, targetSpec.SQL.PasswordRef)
	if err != nil {
		return fmt.Errorf("resolve salvage target admin password: %w", err)
	}
	db, err := r.sql.OpenDB(ctx, targetSpec)
	if err != nil {
		return fmt.Errorf("connect to salvage target %q: %w", action.TargetNodeID, err)
	}
	defer db.Close()

	restoreReadOnly, err := sqltransport.TemporarilyDisableReadOnly(ctx, db)
	if err != nil {
		return fmt.Errorf("prepare candidate for client-side binlog apply: %w", err)
	}

	wait := spec.Replication.Salvage.Timeout
	if wait <= 0 {
		wait = 30 * time.Minute
	}
	r.logger.Info("applying salvaged old-primary binlog over SSH",
		"donor", action.DonorNodeID, "target", action.TargetNodeID, "include_gtids", includeGTIDSet, "exclude_gtids", excludeGTIDSet)
	applyErr := sshtransport.ApplyBinlogDump(ctx, r.ssh, sshtransport.BinlogApplyOptions{
		OldPrimary:        donorSpec,
		Candidate:         targetSpec,
		CandidatePassword: targetPassword,
		IncludeGTIDSet:    includeGTIDSet,
		ExcludeGTIDSet:    excludeGTIDSet,
		Timeout:           wait,
	})
	restoreErr := restoreReadOnly(ctx)
	if applyErr != nil {
		if restoreErr != nil {
			return fmt.Errorf("SSH binlog salvage failed: %v; restore read-only failed: %w", applyErr, restoreErr)
		}
		return applyErr
	}
	if restoreErr != nil {
		return fmt.Errorf("restore read-only after SSH binlog salvage: %w", restoreErr)
	}
	return nil
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
	sourcePassword, err := r.sql.ResolvePassword(ctx, candSpec.SQL.ReplicationPasswordRefOrDefault())
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
