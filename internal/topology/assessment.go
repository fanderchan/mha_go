package topology

import (
	"fmt"

	"mha-go/internal/domain"
)

type Finding struct {
	Severity domain.EventSeverity
	Code     string
	NodeID   string
	Message  string
}

type Assessment struct {
	Findings []Finding
}

func (a *Assessment) Add(severity domain.EventSeverity, code, nodeID, message string) {
	a.Findings = append(a.Findings, Finding{
		Severity: severity,
		Code:     code,
		NodeID:   nodeID,
		Message:  message,
	})
}

func (a Assessment) Errors() []Finding {
	out := make([]Finding, 0, len(a.Findings))
	for _, finding := range a.Findings {
		if finding.Severity == domain.EventSeverityError {
			out = append(out, finding)
		}
	}
	return out
}

func (a Assessment) Warnings() []Finding {
	out := make([]Finding, 0, len(a.Findings))
	for _, finding := range a.Findings {
		if finding.Severity == domain.EventSeverityWarn {
			out = append(out, finding)
		}
	}
	return out
}

func (a Assessment) HasErrors() bool {
	return len(a.Errors()) > 0
}

func AssessReplication(spec domain.ClusterSpec, view *domain.ClusterView) Assessment {
	var assessment Assessment

	primary, ok := view.PrimaryNode()
	if !ok {
		assessment.Add(domain.EventSeverityError, "primary_missing", "", "no primary was identified in the discovered topology")
		return assessment
	}

	if primary.Health != domain.NodeHealthAlive {
		assessment.Add(domain.EventSeverityError, "primary_unhealthy", primary.ID, fmt.Sprintf("primary health is %s", primary.Health))
	}
	if primary.ReadOnly || primary.SuperReadOnly {
		assessment.Add(domain.EventSeverityWarn, "primary_read_only", primary.ID, fmt.Sprintf("primary is read_only=%t super_read_only=%t", primary.ReadOnly, primary.SuperReadOnly))
	}

	semiSyncReplicaCount := 0

	for _, node := range view.Nodes {
		expectedRole := specNodeRole(spec, node.ID)

		if node.Role == domain.NodeRoleObserver || expectedRole == domain.NodeRoleObserver {
			if node.Health == domain.NodeHealthDead {
				assessment.Add(domain.EventSeverityWarn, "observer_unreachable", node.ID, "observer node is unreachable")
			}
			continue
		}

		if node.Role == domain.NodeRolePrimary {
			continue
		}

		if node.Replica == nil {
			severity := domain.EventSeverityWarn
			if !node.IgnoreFail {
				severity = domain.EventSeverityError
			}
			assessment.Add(severity, "replica_missing_state", node.ID, "replica has no replication state")
			continue
		}

		if node.Replica.SemiSyncReplica {
			semiSyncReplicaCount++
		}

		if node.Health == domain.NodeHealthDead {
			severity := domain.EventSeverityError
			if node.IgnoreFail {
				severity = domain.EventSeverityWarn
			}
			assessment.Add(severity, "replica_dead", node.ID, "replica is unreachable")
		}

		if expectedSeries := specNodeVersionSeries(spec, node.ID); node.VersionSeries != "" && expectedSeries != "" && node.VersionSeries != expectedSeries {
			assessment.Add(domain.EventSeverityWarn, "version_series_mismatch", node.ID, fmt.Sprintf("config expects %s but node reports %s", expectedSeries, node.VersionSeries))
		}

		if !node.Replica.AutoPosition {
			assessment.Add(domain.EventSeverityError, "auto_position_disabled", node.ID, "replica auto-position is disabled but GTID-only mode is required")
		}
		if !node.Replica.SQLThreadRunning {
			assessment.Add(domain.EventSeverityError, "replica_sql_thread_down", node.ID, "replica SQL thread is not running")
		}
		if !node.Replica.IOThreadRunning {
			severity := domain.EventSeverityWarn
			if primary.Health == domain.NodeHealthAlive {
				severity = domain.EventSeverityError
			}
			assessment.Add(severity, "replica_io_thread_down", node.ID, "replica IO thread is not running")
		}
		if node.Replica.SourceID == "" {
			assessment.Add(domain.EventSeverityError, "replica_source_unknown", node.ID, "replica source is not mapped to any configured node")
		} else if !spec.Topology.AllowCascadingReplicas && node.Replica.SourceID != primary.ID {
			assessment.Add(domain.EventSeverityError, "cascading_not_allowed", node.ID, fmt.Sprintf("replica source %s is not the primary %s", node.Replica.SourceID, primary.ID))
		}

		if !node.ReadOnly || !node.SuperReadOnly {
			assessment.Add(domain.EventSeverityWarn, "replica_writable", node.ID, fmt.Sprintf("replica is writable: read_only=%t super_read_only=%t", node.ReadOnly, node.SuperReadOnly))
		}
		if node.Replica.SecondsBehindSource > 30 {
			assessment.Add(domain.EventSeverityWarn, "replica_lagging", node.ID, fmt.Sprintf("replica lag is %d seconds", node.Replica.SecondsBehindSource))
		}
	}

	switch spec.Replication.SemiSync.Policy {
	case domain.SemiSyncRequired:
		if !primary.SemiSyncSource {
			assessment.Add(domain.EventSeverityError, "semisync_required_but_disabled", primary.ID, "semi-sync source is not operational on the primary")
		}
		if semiSyncReplicaCount < spec.Replication.SemiSync.WaitForReplicaCount {
			assessment.Add(domain.EventSeverityError, "semisync_replica_shortfall", primary.ID, fmt.Sprintf("semi-sync replicas=%d, required=%d", semiSyncReplicaCount, spec.Replication.SemiSync.WaitForReplicaCount))
		}
	case domain.SemiSyncPreferred:
		if !primary.SemiSyncSource {
			assessment.Add(domain.EventSeverityWarn, "semisync_degraded", primary.ID, "semi-sync is not operational on the primary; cluster is currently relying on async durability")
		}
	}

	return assessment
}
func specNodeVersionSeries(spec domain.ClusterSpec, nodeID string) string {
	for _, node := range spec.Nodes {
		if node.ID == nodeID {
			return node.VersionSeries
		}
	}
	return ""
}
