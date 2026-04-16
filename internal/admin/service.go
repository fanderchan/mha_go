package admin

import (
	"context"

	"mha-go/internal/domain"
	"mha-go/internal/state"
)

type Service struct {
	store state.RunStore
}

func NewService(store state.RunStore) *Service {
	return &Service{store: store}
}

func (s *Service) History(ctx context.Context, cluster string, limit int) ([]domain.RunRecord, error) {
	return s.store.ListRuns(ctx, cluster, limit)
}
