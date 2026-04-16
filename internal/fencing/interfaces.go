package fencing

import (
	"context"

	"mha-go/internal/domain"
)

type Coordinator interface {
	FenceOldPrimary(ctx context.Context, spec domain.ClusterSpec, oldPrimary domain.NodeState) error
	SwitchWriterEndpoint(ctx context.Context, spec domain.ClusterSpec, newPrimary domain.NodeState) error
}
