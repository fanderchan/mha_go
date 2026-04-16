package agent

import "context"

type Client interface {
	Health(ctx context.Context) error
	Fence(ctx context.Context, target string) error
}
