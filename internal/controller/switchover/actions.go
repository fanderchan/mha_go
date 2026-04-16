package switchover

import (
	"context"
	"fmt"
	"sort"
	"time"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
	sqltransport "mha-go/internal/transport/sql"
	"mha-go/internal/writerendpoint"
)

// ActionRunner executes the individual steps of an online switchover.
type ActionRunner interface {
	// LockOldPrimary sets the original primary read-only to stop new writes.
	LockOldPrimary(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error
	// WaitCandidateCatchUp fetches the original primary's current GTID and blocks until
	// the candidate has applied all of those transactions.
	WaitCandidateCatchUp(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error
	// PromoteCandidate stops replication on the candidate and makes it writable.
	PromoteCandidate(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error
	// RepointReplicas redirects all replicas (except old primary and candidate) to the new primary.
	RepointReplicas(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error
	// RepointOldPrimary makes the original primary a replica of the new primary.
	RepointOldPrimary(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error
	// SwitchWriterEndpoint moves the VIP / proxy entry to the new primary.
	SwitchWriterEndpoint(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error
	// VerifyCluster confirms the new primary is writable and replicas (including old primary) point to it.
	VerifyCluster(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error
}

// ---- DryRunActionRunner ----

type DryRunActionRunner struct {
	logger *obs.Logger
}

func NewDryRunActionRunner(logger *obs.Logger) *DryRunActionRunner {
	return &DryRunActionRunner{logger: logger}
}

func (r *DryRunActionRunner) LockOldPrimary(_ context.Context, _ domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	r.logger.Info("dry-run lock old primary", "node", plan.OriginalPrimary.ID)
	return nil
}

func (r *DryRunActionRunner) WaitCandidateCatchUp(_ context.Context, _ domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	r.logger.Info("dry-run wait candidate catch-up", "candidate", plan.Candidate.ID)
	return nil
}

func (r *DryRunActionRunner) PromoteCandidate(_ context.Context, _ domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	r.logger.Info("dry-run promote candidate", "candidate", plan.Candidate.ID)
	return nil
}

func (r *DryRunActionRunner) RepointReplicas(_ context.Context, _ domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	r.logger.Info("dry-run repoint replicas to new primary", "new_primary", plan.Candidate.ID)
	return nil
}

func (r *DryRunActionRunner) RepointOldPrimary(_ context.Context, _ domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	r.logger.Info("dry-run repoint old primary to new primary", "old_primary", plan.OriginalPrimary.ID, "new_primary", plan.Candidate.ID)
	return nil
}

func (r *DryRunActionRunner) SwitchWriterEndpoint(_ context.Context, _ domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	r.logger.Info("dry-run switch writer endpoint", "new_primary", plan.Candidate.ID)
	return nil
}

func (r *DryRunActionRunner) VerifyCluster(_ context.Context, _ domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	r.logger.Info("dry-run verify cluster", "new_primary", plan.Candidate.ID)
	return nil
}

// ---- MySQLActionRunner ----

type MySQLActionRunner struct {
	sql    *sqltransport.MySQLInspector
	logger *obs.Logger
}

func NewMySQLActionRunner(inspector *sqltransport.MySQLInspector, logger *obs.Logger) *MySQLActionRunner {
	return &MySQLActionRunner{sql: inspector, logger: logger}
}

func (r *MySQLActionRunner) LockOldPrimary(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	ns, ok := nodeSpecByID(spec, plan.OriginalPrimary.ID)
	if !ok {
		return fmt.Errorf("cluster spec has no node %q for original primary", plan.OriginalPrimary.ID)
	}
	db, err := r.sql.OpenDB(ctx, ns)
	if err != nil {
		return fmt.Errorf("connect to original primary %q: %w", plan.OriginalPrimary.ID, err)
	}
	defer db.Close()
	return sqltransport.FenceReadOnly(ctx, db, plan.OriginalPrimary.Capabilities)
}

func (r *MySQLActionRunner) WaitCandidateCatchUp(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	// 1. Get the current GTID from the (now read-only) original primary.
	origSpec, ok := nodeSpecByID(spec, plan.OriginalPrimary.ID)
	if !ok {
		return fmt.Errorf("cluster spec has no node %q for original primary", plan.OriginalPrimary.ID)
	}
	origDB, err := r.sql.OpenDB(ctx, origSpec)
	if err != nil {
		return fmt.Errorf("connect to original primary %q for GTID fetch: %w", plan.OriginalPrimary.ID, err)
	}
	gtid, err := sqltransport.GetGTIDExecuted(ctx, origDB)
	_ = origDB.Close()
	if err != nil {
		return fmt.Errorf("fetch gtid_executed from original primary: %w", err)
	}

	// 2. Wait on the candidate until it has applied all those GTIDs.
	candSpec, ok := nodeSpecByID(spec, plan.Candidate.ID)
	if !ok {
		return fmt.Errorf("cluster spec has no node %q for candidate", plan.Candidate.ID)
	}
	candDB, err := r.sql.OpenDB(ctx, candSpec)
	if err != nil {
		return fmt.Errorf("connect to candidate %q for catch-up wait: %w", plan.Candidate.ID, err)
	}
	defer candDB.Close()

	timeout := spec.Replication.Salvage.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	r.logger.Info("waiting for candidate to catch up", "candidate", plan.Candidate.ID, "gtid", gtid, "timeout", timeout)
	return sqltransport.WaitForExecutedGTIDSet(ctx, candDB, gtid, timeout)
}

func (r *MySQLActionRunner) PromoteCandidate(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
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

func (r *MySQLActionRunner) RepointReplicas(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	candSpec, ok := nodeSpecByID(spec, plan.Candidate.ID)
	if !ok {
		return fmt.Errorf("cluster spec has no node %q for candidate", plan.Candidate.ID)
	}
	sourcePassword, err := r.sql.ResolvePassword(ctx, candSpec.SQL.PasswordRef)
	if err != nil {
		return fmt.Errorf("resolve replication password for new primary: %w", err)
	}
	for _, id := range repointReplicaNodeIDs(spec, plan.Candidate.ID, plan.OriginalPrimary.ID) {
		ns, ok := nodeSpecByID(spec, id)
		if !ok {
			continue
		}
		db, err := r.sql.OpenDB(ctx, ns)
		if err != nil {
			return fmt.Errorf("connect to replica %q for repoint: %w", id, err)
		}
		repErr := sqltransport.RepointReplicaToSource(ctx, db, candSpec, sourcePassword)
		_ = db.Close()
		if repErr != nil {
			return fmt.Errorf("repoint replica %q: %w", id, repErr)
		}
		r.logger.Info("repointed replica to new primary", "replica", id, "new_primary", plan.Candidate.ID)
	}
	return nil
}

func (r *MySQLActionRunner) RepointOldPrimary(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	candSpec, ok := nodeSpecByID(spec, plan.Candidate.ID)
	if !ok {
		return fmt.Errorf("cluster spec has no node %q for candidate", plan.Candidate.ID)
	}
	sourcePassword, err := r.sql.ResolvePassword(ctx, candSpec.SQL.PasswordRef)
	if err != nil {
		return fmt.Errorf("resolve replication password for new primary: %w", err)
	}
	origSpec, ok := nodeSpecByID(spec, plan.OriginalPrimary.ID)
	if !ok {
		return fmt.Errorf("cluster spec has no node %q for original primary", plan.OriginalPrimary.ID)
	}
	db, err := r.sql.OpenDB(ctx, origSpec)
	if err != nil {
		return fmt.Errorf("connect to original primary %q for repoint: %w", plan.OriginalPrimary.ID, err)
	}
	defer db.Close()
	if err := sqltransport.RepointReplicaToSource(ctx, db, candSpec, sourcePassword); err != nil {
		return fmt.Errorf("repoint old primary %q to new primary: %w", plan.OriginalPrimary.ID, err)
	}
	r.logger.Info("repointed old primary to new primary as replica",
		"old_primary", plan.OriginalPrimary.ID, "new_primary", plan.Candidate.ID)
	return nil
}

func (r *MySQLActionRunner) SwitchWriterEndpoint(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	return writerendpoint.SwitchForSwitchover(ctx, spec, plan)
}

func (r *MySQLActionRunner) VerifyCluster(ctx context.Context, spec domain.ClusterSpec, plan *domain.SwitchoverPlan) error {
	return VerifyPostSwitchover(ctx, r.sql, spec, plan, r.logger)
}

// ---- helpers ----

func nodeSpecByID(spec domain.ClusterSpec, id string) (domain.NodeSpec, bool) {
	for _, n := range spec.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return domain.NodeSpec{}, false
}

// repointReplicaNodeIDs returns the IDs of nodes that should be repointed to the new primary,
// excluding the new primary (candidateID) and the old primary (origPrimaryID, which has its own step).
func repointReplicaNodeIDs(spec domain.ClusterSpec, candidateID, origPrimaryID string) []string {
	out := make([]string, 0, len(spec.Nodes))
	for _, n := range spec.Nodes {
		if n.ID == candidateID || n.ID == origPrimaryID {
			continue
		}
		if n.NoMaster {
			continue
		}
		out = append(out, n.ID)
	}
	sort.Strings(out)
	return out
}
