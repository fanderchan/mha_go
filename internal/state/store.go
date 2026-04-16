package state

import (
	"context"
	"errors"
	"time"

	"mha-go/internal/domain"
)

var ErrRunNotFound = errors.New("run not found")

type RunStore interface {
	CreateRun(ctx context.Context, record domain.RunRecord) (domain.RunRecord, error)
	AppendEvent(ctx context.Context, runID string, event domain.RunEvent) error
	UpdateRun(ctx context.Context, runID string, status domain.RunStatus, summary string) error
	GetRun(ctx context.Context, runID string) (domain.RunRecord, error)
	ListRuns(ctx context.Context, cluster string, limit int) ([]domain.RunRecord, error)
}

type LeaseHandle interface {
	Key() string
	Owner() string
	Release(ctx context.Context) error
}

type LeaseManager interface {
	Acquire(ctx context.Context, key, owner string, ttl time.Duration) (LeaseHandle, error)
}
