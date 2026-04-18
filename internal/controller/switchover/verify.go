package switchover

import (
	"context"
	"fmt"
	"strings"

	"mha-go/internal/domain"
	"mha-go/internal/obs"
	sqltransport "mha-go/internal/transport/sql"
)

type nodeInspector interface {
	Inspect(ctx context.Context, node domain.NodeSpec) (*sqltransport.Inspection, error)
}

// VerifyPostSwitchover confirms that:
//  1. The new primary (candidate) is read-write and has no replication channels.
//  2. The original primary is now read-only and replicates from the new primary.
//  3. All other replicas replicate from the new primary with auto-position.
func VerifyPostSwitchover(ctx context.Context, inspector nodeInspector, spec domain.ClusterSpec, plan *domain.SwitchoverPlan, logger *obs.Logger) error {
	candSpec, ok := nodeSpecByID(spec, plan.Candidate.ID)
	if !ok {
		return fmt.Errorf("cluster spec has no node %q for candidate", plan.Candidate.ID)
	}

	// 1. New primary must be writable with no replica channels.
	candIn, err := inspector.Inspect(ctx, candSpec)
	if err != nil {
		return fmt.Errorf("inspect new primary %q: %w", plan.Candidate.ID, err)
	}
	if candIn.ReadOnly || candIn.SuperReadOnly {
		return fmt.Errorf("new primary %s is still read-only after promotion", plan.Candidate.ID)
	}
	if len(candIn.ReplicaChannels) > 0 {
		return fmt.Errorf("new primary %s still has replica channels after promotion", plan.Candidate.ID)
	}

	// 2. Old primary must now be a replica of the new primary.
	origSpec, ok := nodeSpecByID(spec, plan.OriginalPrimary.ID)
	if !ok {
		return fmt.Errorf("cluster spec has no node %q for original primary", plan.OriginalPrimary.ID)
	}
	origIn, err := inspector.Inspect(ctx, origSpec)
	if err != nil {
		return fmt.Errorf("inspect old primary %q: %w", plan.OriginalPrimary.ID, err)
	}
	if !origIn.ReadOnly {
		return fmt.Errorf("old primary %s is still writable after switchover", plan.OriginalPrimary.ID)
	}
	if len(origIn.ReplicaChannels) == 0 {
		return fmt.Errorf("old primary %s has no replica channel (expected to replicate from %s)", plan.OriginalPrimary.ID, plan.Candidate.ID)
	}
	if !replicaPointsToNode(origIn.ReplicaChannels[0], candSpec) {
		return fmt.Errorf("old primary %s replicates from %s:%d, expected new primary %s:%d",
			plan.OriginalPrimary.ID,
			origIn.ReplicaChannels[0].SourceHost, origIn.ReplicaChannels[0].SourcePort,
			candSpec.Host, candSpec.Port)
	}

	// 3. Other replicas must point to the new primary.
	for _, n := range spec.Nodes {
		if n.ID == plan.Candidate.ID || n.ID == plan.OriginalPrimary.ID {
			continue
		}
		if n.ExpectedRole == domain.NodeRoleObserver || n.NoMaster {
			continue
		}
		in, err := inspector.Inspect(ctx, n)
		if err != nil {
			return fmt.Errorf("inspect replica %q: %w", n.ID, err)
		}
		if len(in.ReplicaChannels) == 0 {
			return fmt.Errorf("replica %s has no channel after switchover (expected to replicate from %s)", n.ID, plan.Candidate.ID)
		}
		ch := in.ReplicaChannels[0]
		if !replicaPointsToNode(ch, candSpec) {
			return fmt.Errorf("replica %s source %s:%d does not match new primary %s:%d",
				n.ID, ch.SourceHost, ch.SourcePort, candSpec.Host, candSpec.Port)
		}
		if !ch.AutoPosition {
			return fmt.Errorf("replica %s replication is not using auto-position", n.ID)
		}
		if !ch.IOThreadRunning || !ch.SQLThreadRunning {
			return fmt.Errorf("replica %s replication threads not running (io=%v sql=%v)", n.ID, ch.IOThreadRunning, ch.SQLThreadRunning)
		}
	}
	return nil
}

func replicaPointsToNode(ch sqltransport.ReplicaChannelStatus, node domain.NodeSpec) bool {
	hostMatch := strings.EqualFold(strings.TrimSpace(ch.SourceHost), strings.TrimSpace(node.Host))
	if ch.SourcePort == 0 {
		return hostMatch
	}
	return hostMatch && ch.SourcePort == node.Port
}
