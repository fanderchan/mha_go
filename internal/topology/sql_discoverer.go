package topology

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"mha-go/internal/capability"
	"mha-go/internal/domain"
	sqltransport "mha-go/internal/transport/sql"
)

type SQLDiscoverer struct {
	inspector sqltransport.Inspector
}

type inspectionResult struct {
	spec       domain.NodeSpec
	inspection *sqltransport.Inspection
	err        error
}

func NewSQLDiscoverer(inspector sqltransport.Inspector) *SQLDiscoverer {
	return &SQLDiscoverer{inspector: inspector}
}

func (d *SQLDiscoverer) Discover(ctx context.Context, spec domain.ClusterSpec) (*domain.ClusterView, error) {
	results := make([]inspectionResult, len(spec.Nodes))
	var wg sync.WaitGroup
	for i, node := range spec.Nodes {
		wg.Add(1)
		go func(index int, nodeSpec domain.NodeSpec) {
			defer wg.Done()
			inspection, err := d.inspector.Inspect(ctx, nodeSpec)
			results[index] = inspectionResult{
				spec:       nodeSpec,
				inspection: inspection,
				err:        err,
			}
		}(i, node)
	}
	wg.Wait()

	view := &domain.ClusterView{
		ClusterName:  spec.Name,
		TopologyKind: spec.Topology.Kind,
		CollectedAt:  time.Now(),
	}

	idByAddress := make(map[string]string, len(spec.Nodes))
	idByHost := make(map[string]string, len(spec.Nodes))
	for _, node := range spec.Nodes {
		idByAddress[strings.ToLower(node.Address())] = node.ID
		idByHost[strings.ToLower(node.Host)] = node.ID
	}

	uuidToID := make(map[string]string, len(spec.Nodes))
	upstreamRefCount := make(map[string]int, len(spec.Nodes))
	aliveCount := 0

	for _, result := range results {
		nodeState, warnings, alive := d.toNodeState(result)
		if alive {
			aliveCount++
			if nodeState.ServerUUID != "" {
				uuidToID[strings.ToLower(nodeState.ServerUUID)] = nodeState.ID
			}
		}
		view.Nodes = append(view.Nodes, nodeState)
		view.Warnings = append(view.Warnings, warnings...)
	}

	if aliveCount == 0 {
		return nil, fmt.Errorf("cluster %q discovery failed: no nodes were reachable", spec.Name)
	}

	for i := range view.Nodes {
		replica := view.Nodes[i].Replica
		if replica == nil {
			continue
		}
		if replica.SourceID == "" {
			switch {
			case replica.SourceHost != "" && replica.SourcePort > 0:
				if nodeID, ok := idByAddress[strings.ToLower(fmt.Sprintf("%s:%d", replica.SourceHost, replica.SourcePort))]; ok {
					replica.SourceID = nodeID
				} else if nodeID, ok := idByHost[strings.ToLower(replica.SourceHost)]; ok {
					replica.SourceID = nodeID
				}
			case view.Nodes[i].LastError == "" && replica.ChannelName != "":
				view.Warnings = append(view.Warnings, fmt.Sprintf("node %s replica channel %q does not expose a source host/port", view.Nodes[i].ID, replica.ChannelName))
			}
		}
		if replica.SourceID == "" && replica.SourceUUID != "" {
			if resultID, ok := uuidToID[strings.ToLower(replica.SourceUUID)]; ok {
				replica.SourceID = resultID
			}
		}
		if replica.SourceID != "" {
			upstreamRefCount[replica.SourceID]++
		} else {
			view.Warnings = append(view.Warnings, fmt.Sprintf("node %s source %s:%d is not mapped to any configured node", view.Nodes[i].ID, replica.SourceHost, replica.SourcePort))
		}
	}

	view.PrimaryID = d.selectPrimaryID(spec, view, upstreamRefCount)
	if view.PrimaryID == "" {
		return nil, fmt.Errorf("cluster %q discovery could not identify a primary", spec.Name)
	}

	for i := range view.Nodes {
		switch {
		case view.Nodes[i].ID == view.PrimaryID:
			view.Nodes[i].Role = domain.NodeRolePrimary
			if view.Nodes[i].ReadOnly || view.Nodes[i].SuperReadOnly {
				view.Warnings = append(view.Warnings, fmt.Sprintf("node %s is the inferred primary but read_only=%t super_read_only=%t", view.Nodes[i].ID, view.Nodes[i].ReadOnly, view.Nodes[i].SuperReadOnly))
			}
		case view.Nodes[i].Replica != nil:
			view.Nodes[i].Role = domain.NodeRoleReplica
		case specNodeRole(spec, view.Nodes[i].ID) == domain.NodeRoleObserver:
			view.Nodes[i].Role = domain.NodeRoleObserver
		default:
			view.Nodes[i].Role = domain.NodeRoleUnknown
		}
	}

	if spec.Replication.SemiSync.Policy == domain.SemiSyncRequired {
		if primary, ok := view.PrimaryNode(); ok && !primary.SemiSyncSource {
			view.Warnings = append(view.Warnings, fmt.Sprintf("node %s is primary but semi-sync source is not operational while policy=require", primary.ID))
		}
	}

	slices.SortFunc(view.Nodes, func(a, b domain.NodeState) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})

	return view, nil
}

func (d *SQLDiscoverer) toNodeState(result inspectionResult) (domain.NodeState, []string, bool) {
	caps, capErr := capability.BaselineForVersionSeries(result.spec.VersionSeries)
	warnings := make([]string, 0, 4)
	if capErr != nil {
		warnings = append(warnings, fmt.Sprintf("node %s capability baseline failed: %v", result.spec.ID, capErr))
	}

	state := domain.NodeState{
		ID:                result.spec.ID,
		Address:           result.spec.Address(),
		VersionSeries:     result.spec.VersionSeries,
		Health:            domain.NodeHealthUnknown,
		CandidatePriority: result.spec.CandidatePriority,
		NoMaster:          result.spec.NoMaster,
		IgnoreFail:        result.spec.IgnoreFail,
		Capabilities:      caps,
		Labels:            mapsClone(result.spec.Labels),
	}

	if result.err != nil {
		state.Health = domain.NodeHealthDead
		state.LastError = result.err.Error()
		if result.spec.ExpectedRole == domain.NodeRoleObserver {
			state.Role = domain.NodeRoleObserver
		}
		warnings = append(warnings, fmt.Sprintf("node %s is unreachable: %v", result.spec.ID, result.err))
		return state, warnings, false
	}

	inspection := result.inspection
	state.ServerUUID = inspection.ServerUUID
	state.Version = inspection.Version
	state.VersionSeries = inspection.VersionSeries
	state.GTIDExecuted = inspection.GTIDExecuted
	state.ReadOnly = inspection.ReadOnly
	state.SuperReadOnly = inspection.SuperReadOnly
	state.SemiSyncSource = inspection.SemiSyncSourceOperational
	state.Health = domain.NodeHealthAlive

	if strings.ToUpper(strings.TrimSpace(inspection.GTIDMode)) != "ON" {
		state.Health = domain.NodeHealthSuspect
		warnings = append(warnings, fmt.Sprintf("node %s gtid_mode=%q, expected ON", state.ID, inspection.GTIDMode))
	}

	if len(inspection.ReplicaChannels) > 1 {
		state.Health = domain.NodeHealthSuspect
		warnings = append(warnings, fmt.Sprintf("node %s has %d replica channels; multi-source is reserved and not implemented in v1", state.ID, len(inspection.ReplicaChannels)))
	}

	if len(inspection.ReplicaChannels) > 0 {
		channel := inspection.ReplicaChannels[0]
		state.Replica = &domain.ReplicaState{
			SourceUUID:          channel.SourceUUID,
			SourceHost:          channel.SourceHost,
			SourcePort:          channel.SourcePort,
			ChannelName:         channel.ChannelName,
			AutoPosition:        channel.AutoPosition,
			GTIDExecuted:        channel.ExecutedGTIDSet,
			GTIDRetrieved:       channel.RetrievedGTIDSet,
			SecondsBehindSource: channel.SecondsBehindSource,
			IOThreadRunning:     channel.IOThreadRunning,
			SQLThreadRunning:    channel.SQLThreadRunning,
			SemiSyncReplica:     inspection.SemiSyncReplicaOperational,
			LastIOError:         channel.LastIOError,
			LastSQLError:        channel.LastSQLError,
		}

		if !channel.IOThreadRunning || !channel.SQLThreadRunning {
			state.Health = domain.NodeHealthSuspect
			state.LastError = strings.TrimSpace(strings.TrimSpace(channel.LastIOError + " " + channel.LastSQLError))
			if state.LastError == "" {
				state.LastError = "replication threads are not fully running"
			}
			warnings = append(warnings, fmt.Sprintf("node %s replication unhealthy: io_running=%t sql_running=%t", state.ID, channel.IOThreadRunning, channel.SQLThreadRunning))
		}
	} else if result.spec.ExpectedRole == domain.NodeRoleObserver {
		state.Role = domain.NodeRoleObserver
	}

	if state.SemiSyncSource && !inspection.SemiSyncSourceEnabled {
		warnings = append(warnings, fmt.Sprintf("node %s reports semi-sync source status without the enabled variable set", state.ID))
	}

	return state, warnings, true
}

func (d *SQLDiscoverer) selectPrimaryID(spec domain.ClusterSpec, view *domain.ClusterView, upstreamRefCount map[string]int) string {
	type primaryCandidate struct {
		id    string
		score int
	}

	candidates := make([]primaryCandidate, 0, len(view.Nodes))
	for _, node := range view.Nodes {
		score := upstreamRefCount[node.ID] * 100
		if node.Health == domain.NodeHealthAlive {
			score += 20
		}
		if node.Replica == nil {
			score += 10
		}
		if !node.ReadOnly && !node.SuperReadOnly {
			score += 5
		}
		if specNodeRole(spec, node.ID) == domain.NodeRolePrimary {
			score += 3
		}
		if score > 0 {
			candidates = append(candidates, primaryCandidate{id: node.ID, score: score})
		}
	}

	slices.SortFunc(candidates, func(a, b primaryCandidate) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		if a.id < b.id {
			return -1
		}
		if a.id > b.id {
			return 1
		}
		return 0
	})

	if len(candidates) > 0 {
		return candidates[0].id
	}

	for _, node := range view.Nodes {
		if specNodeRole(spec, node.ID) == domain.NodeRolePrimary {
			return node.ID
		}
	}

	if len(view.Nodes) > 0 {
		return view.Nodes[0].ID
	}
	return ""
}

func specNodeRole(spec domain.ClusterSpec, nodeID string) domain.NodeRole {
	for _, node := range spec.Nodes {
		if node.ID == nodeID {
			return node.ExpectedRole
		}
	}
	return domain.NodeRoleUnknown
}
