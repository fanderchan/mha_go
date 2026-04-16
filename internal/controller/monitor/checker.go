package monitor

import (
	"context"
	"strings"

	"mha-go/internal/domain"
	sqltransport "mha-go/internal/transport/sql"
)

// pingPrimary opens and immediately closes a SQL connection to the given node.
// Returns nil if the node is reachable, non-nil otherwise.
func pingPrimary(ctx context.Context, inspector *sqltransport.MySQLInspector, ns domain.NodeSpec) error {
	db, err := inspector.OpenDB(ctx, ns)
	if err != nil {
		return err
	}
	_ = db.Close()
	return nil
}

// replicaSeesSource returns true if the given replica's IO thread is running and its configured
// source host:port matches primaryHost:primaryPort.
func replicaSeesSource(ctx context.Context, inspector *sqltransport.MySQLInspector, replicaSpec domain.NodeSpec, primaryHost string, primaryPort int) (bool, error) {
	in, err := inspector.Inspect(ctx, replicaSpec)
	if err != nil {
		return false, err
	}
	for _, ch := range in.ReplicaChannels {
		if !ch.IOThreadRunning {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(ch.SourceHost), strings.TrimSpace(primaryHost)) {
			continue
		}
		if ch.SourcePort != 0 && ch.SourcePort != primaryPort {
			continue
		}
		return true, nil
	}
	return false, nil
}

// nodeSpecByID looks up a NodeSpec from the cluster config by node ID.
func nodeSpecByID(spec domain.ClusterSpec, id string) (domain.NodeSpec, bool) {
	for _, n := range spec.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return domain.NodeSpec{}, false
}
