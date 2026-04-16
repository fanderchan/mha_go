package failover

import (
	"context"
	"fmt"
	"strings"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
	sqltransport "mha-go/internal/transport/sql"
)

// VerifyPostFailover checks that the new primary is writable and other nodes replicate from it.
func VerifyPostFailover(ctx context.Context, inspector *sqltransport.MySQLInspector, spec domain.ClusterSpec, plan *domain.FailoverPlan, logger *obs.Logger) error {
	candSpec, ok := nodeSpecByID(spec, plan.Candidate.ID)
	if !ok {
		return fmt.Errorf("cluster spec has no node %q for candidate", plan.Candidate.ID)
	}
	candIn, err := inspector.Inspect(ctx, candSpec)
	if err != nil {
		return fmt.Errorf("inspect candidate %q: %w", plan.Candidate.ID, err)
	}
	if candIn.ReadOnly || candIn.SuperReadOnly {
		return fmt.Errorf("candidate %s is still read-only after promotion", plan.Candidate.ID)
	}
	if len(candIn.ReplicaChannels) > 0 {
		return fmt.Errorf("candidate %s still has replica channels after promotion (expected none)", plan.Candidate.ID)
	}

	for _, n := range spec.Nodes {
		if n.ID == plan.Candidate.ID {
			continue
		}
		if n.ExpectedRole == domain.NodeRoleObserver {
			continue
		}
		if n.NoMaster {
			continue
		}
		if n.ID == plan.OldPrimary.ID && plan.OldPrimary.Health == domain.NodeHealthDead {
			continue
		}
		in, err := inspector.Inspect(ctx, n)
		if err != nil {
			return fmt.Errorf("inspect node %q: %w", n.ID, err)
		}
		if len(in.ReplicaChannels) == 0 {
			logger.Warn("verify: node has no replica channel (skipping replication check)", "node", n.ID)
			continue
		}
		ch := in.ReplicaChannels[0]
		if !replicaPointsToCandidate(ch, candSpec) {
			return fmt.Errorf("node %s replication source %s:%d does not match candidate %s:%d",
				n.ID, ch.SourceHost, ch.SourcePort, candSpec.Host, candSpec.Port)
		}
		if !ch.AutoPosition {
			return fmt.Errorf("node %s replication is not using auto-position", n.ID)
		}
		if !ch.IOThreadRunning || !ch.SQLThreadRunning {
			return fmt.Errorf("node %s replication threads not running (io=%v sql=%v)", n.ID, ch.IOThreadRunning, ch.SQLThreadRunning)
		}
	}
	return nil
}

func replicaPointsToCandidate(ch sqltransport.ReplicaChannelStatus, cand domain.NodeSpec) bool {
	hostMatch := strings.EqualFold(strings.TrimSpace(ch.SourceHost), strings.TrimSpace(cand.Host))
	if ch.SourcePort == 0 {
		return hostMatch
	}
	return hostMatch && ch.SourcePort == cand.Port
}
