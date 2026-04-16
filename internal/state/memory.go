package state

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"mha-go/internal/domain"
)

type MemoryStore struct {
	mu      sync.RWMutex
	counter atomic.Int64
	runs    map[string]domain.RunRecord
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{runs: make(map[string]domain.RunRecord)}
}

func (s *MemoryStore) CreateRun(_ context.Context, record domain.RunRecord) (domain.RunRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if record.ID == "" {
		record.ID = fmt.Sprintf("run-%06d", s.counter.Add(1))
	}
	now := time.Now()
	if record.StartedAt.IsZero() {
		record.StartedAt = now
	}
	record.UpdatedAt = record.StartedAt
	if record.Status == "" {
		record.Status = domain.RunStatusRunning
	}
	s.runs[record.ID] = record
	return record, nil
}

func (s *MemoryStore) AppendEvent(_ context.Context, runID string, event domain.RunEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	if event.Sequence == 0 {
		event.Sequence = int64(len(record.Events) + 1)
	}
	if event.At.IsZero() {
		event.At = time.Now()
	}
	record.Events = append(record.Events, event)
	record.UpdatedAt = event.At
	s.runs[runID] = record
	return nil
}

func (s *MemoryStore) UpdateRun(_ context.Context, runID string, status domain.RunStatus, summary string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	record, ok := s.runs[runID]
	if !ok {
		return ErrRunNotFound
	}
	record.Status = status
	record.Summary = summary
	record.UpdatedAt = time.Now()
	s.runs[runID] = record
	return nil
}

func (s *MemoryStore) GetRun(_ context.Context, runID string) (domain.RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	record, ok := s.runs[runID]
	if !ok {
		return domain.RunRecord{}, ErrRunNotFound
	}
	return record, nil
}

func (s *MemoryStore) ListRuns(_ context.Context, cluster string, limit int) ([]domain.RunRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	out := make([]domain.RunRecord, 0, len(s.runs))
	for _, record := range s.runs {
		if cluster == "" || record.Cluster == cluster {
			out = append(out, record)
		}
	}
	slices.SortFunc(out, func(a, b domain.RunRecord) int {
		if a.StartedAt.After(b.StartedAt) {
			return -1
		}
		if a.StartedAt.Before(b.StartedAt) {
			return 1
		}
		return 0
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

type LocalLeaseManager struct {
	mu     sync.Mutex
	leases map[string]string
}

func NewLocalLeaseManager() *LocalLeaseManager {
	return &LocalLeaseManager{leases: make(map[string]string)}
}

func (m *LocalLeaseManager) Acquire(_ context.Context, key, owner string, _ time.Duration) (LeaseHandle, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if currentOwner, exists := m.leases[key]; exists && currentOwner != owner {
		return nil, fmt.Errorf("lease %q is already held by %q", key, currentOwner)
	}
	m.leases[key] = owner
	return localLeaseHandle{manager: m, key: key, owner: owner}, nil
}

type localLeaseHandle struct {
	manager *LocalLeaseManager
	key     string
	owner   string
}

func (h localLeaseHandle) Key() string   { return h.key }
func (h localLeaseHandle) Owner() string { return h.owner }

func (h localLeaseHandle) Release(_ context.Context) error {
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()
	delete(h.manager.leases, h.key)
	return nil
}
