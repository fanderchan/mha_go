package replication

import (
	"context"

	"mha-go/internal/domain"
)

type SalvageArtifact struct {
	Reference string
}

type Salvager interface {
	CollectMissingTransactions(ctx context.Context, spec domain.ClusterSpec, view *domain.ClusterView, oldPrimary, candidate domain.NodeState, missingGTIDSet string) (SalvageArtifact, error)
	ApplyTransactions(ctx context.Context, spec domain.ClusterSpec, candidate domain.NodeState, artifact SalvageArtifact) error
}
