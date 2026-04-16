package topology

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"mha-go/internal/capability"
	"mha-go/internal/domain"
	"mha-go/internal/replication"
)

var ErrNoCandidateReplica = errors.New("no replica qualifies as a candidate")

type Discoverer interface {
	Discover(ctx context.Context, spec domain.ClusterSpec) (*domain.ClusterView, error)
}

type CandidateSelector interface {
	SelectFailoverCandidate(ctx context.Context, spec domain.ClusterSpec, view *domain.ClusterView) (*domain.NodeState, error)
}

type StaticDiscoverer struct{}

func NewStaticDiscoverer() *StaticDiscoverer {
	return &StaticDiscoverer{}
}

func (d *StaticDiscoverer) Discover(_ context.Context, spec domain.ClusterSpec) (*domain.ClusterView, error) {
	view := &domain.ClusterView{
		ClusterName:  spec.Name,
		TopologyKind: spec.Topology.Kind,
		CollectedAt:  time.Now(),
	}

	primaryAssigned := false
	for i, node := range spec.Nodes {
		role := node.ExpectedRole
		if role == "" {
			role = domain.NodeRoleReplica
		}
		if !primaryAssigned && (role == domain.NodeRolePrimary || i == 0) {
			role = domain.NodeRolePrimary
			primaryAssigned = true
			view.PrimaryID = node.ID
		} else if role == domain.NodeRolePrimary {
			role = domain.NodeRoleReplica
		}

		caps, err := capability.BaselineForVersionSeries(node.VersionSeries)
		if err != nil {
			return nil, fmt.Errorf("build capabilities for node %q: %w", node.ID, err)
		}

		state := domain.NodeState{
			ID:                node.ID,
			Address:           node.Address(),
			VersionSeries:     node.VersionSeries,
			Role:              role,
			Health:            domain.NodeHealthAlive,
			CandidatePriority: node.CandidatePriority,
			NoMaster:          node.NoMaster,
			IgnoreFail:        node.IgnoreFail,
			Capabilities:      caps,
			Labels:            mapsClone(node.Labels),
		}
		if role == domain.NodeRolePrimary {
			state.ReadOnly = false
			state.SuperReadOnly = false
			state.SemiSyncSource = spec.Replication.SemiSync.Policy != domain.SemiSyncDisabled
		} else {
			state.ReadOnly = true
			state.SuperReadOnly = true
		}
		view.Nodes = append(view.Nodes, state)
	}

	if view.PrimaryID == "" && len(view.Nodes) > 0 {
		view.PrimaryID = view.Nodes[0].ID
		view.Nodes[0].Role = domain.NodeRolePrimary
	}

	primarySpec := spec.Nodes[0]
	for _, node := range spec.Nodes {
		if node.ID == view.PrimaryID {
			primarySpec = node
			break
		}
	}

	for i := range view.Nodes {
		if view.Nodes[i].Role == domain.NodeRoleReplica {
			view.Nodes[i].Replica = &domain.ReplicaState{
				SourceID:            view.PrimaryID,
				SourceHost:          primarySpec.Host,
				SourcePort:          primarySpec.Port,
				AutoPosition:        true,
				SecondsBehindSource: 0,
				IOThreadRunning:     true,
				SQLThreadRunning:    true,
				SemiSyncReplica:     spec.Replication.SemiSync.Policy != domain.SemiSyncDisabled,
			}
		}
	}

	return view, nil
}

// PinnedCandidateSelector always returns the node with the given ID.
// It still verifies the node exists in the view and is not dead or NoMaster.
type PinnedCandidateSelector struct {
	NodeID string
}

func NewPinnedCandidateSelector(nodeID string) *PinnedCandidateSelector {
	return &PinnedCandidateSelector{NodeID: nodeID}
}

func (s *PinnedCandidateSelector) SelectFailoverCandidate(_ context.Context, _ domain.ClusterSpec, view *domain.ClusterView) (*domain.NodeState, error) {
	for _, node := range view.Nodes {
		if node.ID != s.NodeID {
			continue
		}
		if node.Role == domain.NodeRolePrimary {
			return nil, fmt.Errorf("node %q is already the primary; cannot use as candidate", s.NodeID)
		}
		if node.NoMaster {
			return nil, fmt.Errorf("node %q has no_master=true and cannot be promoted", s.NodeID)
		}
		if node.Health == domain.NodeHealthDead {
			return nil, fmt.Errorf("node %q is dead and cannot be used as a failover candidate", s.NodeID)
		}
		return &node, nil
	}
	return nil, fmt.Errorf("node %q not found in discovered topology", s.NodeID)
}

type DefaultCandidateSelector struct{}

func NewDefaultCandidateSelector() *DefaultCandidateSelector {
	return &DefaultCandidateSelector{}
}

func (s *DefaultCandidateSelector) SelectFailoverCandidate(_ context.Context, _ domain.ClusterSpec, view *domain.ClusterView) (*domain.NodeState, error) {
	replicas := make([]domain.NodeState, 0, len(view.Nodes))
	for _, node := range view.Nodes {
		if node.Role != domain.NodeRoleReplica {
			continue
		}
		if node.Health == domain.NodeHealthDead {
			continue
		}
		if node.NoMaster {
			continue
		}
		if node.Replica == nil {
			continue
		}
		if !node.Replica.AutoPosition {
			continue
		}
		replicas = append(replicas, node)
	}
	if len(replicas) == 0 {
		return nil, ErrNoCandidateReplica
	}

	slices.SortFunc(replicas, func(a, b domain.NodeState) int {
		scoreA := candidateScore(a, view)
		scoreB := candidateScore(b, view)
		if scoreA != scoreB {
			if scoreA > scoreB {
				return -1
			}
			return 1
		}
		if a.CandidatePriority != b.CandidatePriority {
			if a.CandidatePriority > b.CandidatePriority {
				return -1
			}
			return 1
		}
		if a.VersionSeries != b.VersionSeries {
			if a.VersionSeries > b.VersionSeries {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})

	candidate := replicas[0]
	return &candidate, nil
}

func candidateScore(node domain.NodeState, view *domain.ClusterView) int {
	score := 0
	if node.Health == domain.NodeHealthAlive {
		score += 200
	} else if node.Health == domain.NodeHealthSuspect {
		score += 100
	}
	if node.Replica == nil {
		return score
	}
	if node.Replica.SourceID == view.PrimaryID {
		score += 80
	}
	if node.Replica.SQLThreadRunning {
		score += 60
	}
	if node.Replica.IOThreadRunning {
		score += 40
	}
	if node.Replica.SemiSyncReplica {
		score += 30
	}
	if node.ReadOnly && node.SuperReadOnly {
		score += 20
	}
	switch {
	case node.Replica.SecondsBehindSource < 0:
		score -= 20
	case node.Replica.SecondsBehindSource == 0:
		score += 50
	case node.Replica.SecondsBehindSource <= 5:
		score += 20
	case node.Replica.SecondsBehindSource <= 30:
		score += 5
	default:
		score -= int(node.Replica.SecondsBehindSource / 5)
	}
	score += replication.CandidateFreshnessScore(node, view.Nodes)
	return score
}

func mapsClone(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
