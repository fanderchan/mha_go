package hooks

import (
	"context"

	"mha-go/internal/domain"
)

type Event struct {
	Name    string
	Cluster string
	RunKind domain.RunKind
	NodeID  string
	Data    map[string]string
}

type Dispatcher interface {
	Dispatch(ctx context.Context, event Event) error
}
