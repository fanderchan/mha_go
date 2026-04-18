package config

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"mha-go/internal/capability"
	"mha-go/internal/domain"
)

type fileSpec struct {
	Name           string                 `json:"name" yaml:"name" toml:"name"`
	Topology       fileTopologySpec       `json:"topology" yaml:"topology" toml:"topology"`
	Controller     fileControllerSpec     `json:"controller" yaml:"controller" toml:"controller"`
	Replication    fileReplicationSpec    `json:"replication" yaml:"replication" toml:"replication"`
	Fencing        fileFencingSpec        `json:"fencing" yaml:"fencing" toml:"fencing"`
	WriterEndpoint fileWriterEndpointSpec `json:"writer_endpoint" yaml:"writer_endpoint" toml:"writer_endpoint"`
	Nodes          []fileNodeSpec         `json:"nodes" yaml:"nodes" toml:"nodes"`
	Hooks          fileHookSpec           `json:"hooks" yaml:"hooks" toml:"hooks"`
}

type fileTopologySpec struct {
	Kind                   string `json:"kind" yaml:"kind" toml:"kind"`
	SingleWriter           *bool  `json:"single_writer" yaml:"single_writer" toml:"single_writer"`
	AllowCascadingReplicas bool   `json:"allow_cascading_replicas" yaml:"allow_cascading_replicas" toml:"allow_cascading_replicas"`
}

type fileControllerSpec struct {
	ID              string                   `json:"id" yaml:"id" toml:"id"`
	Lease           fileLeaseSpec            `json:"lease" yaml:"lease" toml:"lease"`
	Monitor         fileMonitorSpec          `json:"monitor" yaml:"monitor" toml:"monitor"`
	SecondaryChecks []fileSecondaryCheckSpec `json:"secondary_checks" yaml:"secondary_checks" toml:"secondary_checks"`
}

type fileLeaseSpec struct {
	Backend string `json:"backend" yaml:"backend" toml:"backend"`
	TTL     string `json:"ttl" yaml:"ttl" toml:"ttl"`
}

type fileMonitorSpec struct {
	Interval         string `json:"interval" yaml:"interval" toml:"interval"`
	FailureThreshold int    `json:"failure_threshold" yaml:"failure_threshold" toml:"failure_threshold"`
	ReconfirmTimeout string `json:"reconfirm_timeout" yaml:"reconfirm_timeout" toml:"reconfirm_timeout"`
}

type fileSecondaryCheckSpec struct {
	Name         string `json:"name" yaml:"name" toml:"name"`
	ObserverNode string `json:"observer_node" yaml:"observer_node" toml:"observer_node"`
	Timeout      string `json:"timeout" yaml:"timeout" toml:"timeout"`
}

type fileReplicationSpec struct {
	Mode     string           `json:"mode" yaml:"mode" toml:"mode"`
	SemiSync fileSemiSyncSpec `json:"semi_sync" yaml:"semi_sync" toml:"semi_sync"`
	Salvage  fileSalvageSpec  `json:"salvage" yaml:"salvage" toml:"salvage"`
}

type fileSemiSyncSpec struct {
	Policy              string `json:"policy" yaml:"policy" toml:"policy"`
	WaitForReplicaCount int    `json:"wait_for_replica_count" yaml:"wait_for_replica_count" toml:"wait_for_replica_count"`
	Timeout             string `json:"timeout" yaml:"timeout" toml:"timeout"`
}

type fileSalvageSpec struct {
	Policy  string `json:"policy" yaml:"policy" toml:"policy"`
	Timeout string `json:"timeout" yaml:"timeout" toml:"timeout"`
}

type fileWriterEndpointSpec struct {
	Kind            string `json:"kind" yaml:"kind" toml:"kind"`
	Target          string `json:"target" yaml:"target" toml:"target"`
	Command         string `json:"command" yaml:"command" toml:"command"`
	PrecheckCommand string `json:"precheck_command" yaml:"precheck_command" toml:"precheck_command"`
	VerifyCommand   string `json:"verify_command" yaml:"verify_command" toml:"verify_command"`
}

type fileFencingSpec struct {
	Steps []fileFencingStepSpec `json:"steps" yaml:"steps" toml:"steps"`
}

type fileFencingStepSpec struct {
	Kind     string `json:"kind" yaml:"kind" toml:"kind"`
	Required *bool  `json:"required" yaml:"required" toml:"required"`
	Command  string `json:"command" yaml:"command" toml:"command"`
	Timeout  string `json:"timeout" yaml:"timeout" toml:"timeout"`
}

type fileHookSpec struct {
	ShellCompat bool   `json:"shell_compat" yaml:"shell_compat" toml:"shell_compat"`
	Command     string `json:"command" yaml:"command" toml:"command"`
}

type fileNodeSpec struct {
	ID                string            `json:"id" yaml:"id" toml:"id"`
	Host              string            `json:"host" yaml:"host" toml:"host"`
	Port              int               `json:"port" yaml:"port" toml:"port"`
	VersionSeries     string            `json:"version_series" yaml:"version_series" toml:"version_series"`
	ExpectedRole      string            `json:"expected_role" yaml:"expected_role" toml:"expected_role"`
	CandidatePriority int               `json:"candidate_priority" yaml:"candidate_priority" toml:"candidate_priority"`
	NoMaster          bool              `json:"no_master" yaml:"no_master" toml:"no_master"`
	IgnoreFail        bool              `json:"ignore_fail" yaml:"ignore_fail" toml:"ignore_fail"`
	Zone              string            `json:"zone" yaml:"zone" toml:"zone"`
	Labels            map[string]string `json:"labels" yaml:"labels" toml:"labels"`
	SQL               fileSQLSpec       `json:"sql" yaml:"sql" toml:"sql"`
	SSH               *fileSSHSpec      `json:"ssh" yaml:"ssh" toml:"ssh"`
	Agent             *fileAgentSpec    `json:"agent" yaml:"agent" toml:"agent"`
}

type fileSQLSpec struct {
	User                   string `json:"user" yaml:"user" toml:"user"`
	PasswordRef            string `json:"password_ref" yaml:"password_ref" toml:"password_ref"`
	ReplicationUser        string `json:"replication_user" yaml:"replication_user" toml:"replication_user"`
	ReplicationPasswordRef string `json:"replication_password_ref" yaml:"replication_password_ref" toml:"replication_password_ref"`
	TLSProfile             string `json:"tls_profile" yaml:"tls_profile" toml:"tls_profile"`
}

type fileSSHSpec struct {
	User                    string `json:"user" yaml:"user" toml:"user"`
	Port                    int    `json:"port" yaml:"port" toml:"port"`
	PasswordRef             string `json:"password_ref" yaml:"password_ref" toml:"password_ref"`
	PrivateKeyRef           string `json:"private_key_ref" yaml:"private_key_ref" toml:"private_key_ref"`
	PrivateKeyPassphraseRef string `json:"private_key_passphrase_ref" yaml:"private_key_passphrase_ref" toml:"private_key_passphrase_ref"`
	BinlogDir               string `json:"binlog_dir" yaml:"binlog_dir" toml:"binlog_dir"`
	BinlogIndex             string `json:"binlog_index" yaml:"binlog_index" toml:"binlog_index"`
	BinlogPrefix            string `json:"binlog_prefix" yaml:"binlog_prefix" toml:"binlog_prefix"`
	MySQLBinlogPath         string `json:"mysqlbinlog_path" yaml:"mysqlbinlog_path" toml:"mysqlbinlog_path"`
}

type fileAgentSpec struct {
	Address   string `json:"address" yaml:"address" toml:"address"`
	AuthToken string `json:"auth_token" yaml:"auth_token" toml:"auth_token"`
}

func (f fileSpec) toDomain() (domain.ClusterSpec, error) {
	spec := domain.ClusterSpec{}
	if strings.TrimSpace(f.Name) == "" {
		return spec, errors.New("config.name must be set")
	}
	spec.Name = f.Name

	topologyKind := domain.TopologyKind(strings.TrimSpace(f.Topology.Kind))
	if topologyKind == "" {
		topologyKind = domain.TopologyAsyncSinglePrimary
	}
	switch topologyKind {
	case domain.TopologyAsyncSinglePrimary:
	case domain.TopologyGroupReplicationSinglePrimary, domain.TopologyGroupReplicationMultiPrimary, domain.TopologyInnoDBCluster:
		return spec, fmt.Errorf("topology kind %q is reserved but not implemented in v1", topologyKind)
	default:
		return spec, fmt.Errorf("unsupported topology kind %q", topologyKind)
	}
	singleWriter := true
	if f.Topology.SingleWriter != nil {
		singleWriter = *f.Topology.SingleWriter
	}
	spec.Topology = domain.TopologySpec{
		Kind:                   topologyKind,
		SingleWriter:           singleWriter,
		AllowCascadingReplicas: f.Topology.AllowCascadingReplicas,
	}

	leaseTTL, err := parseDurationDefault(f.Controller.Lease.TTL, 15*time.Second)
	if err != nil {
		return spec, fmt.Errorf("controller.lease.ttl: %w", err)
	}
	monitorInterval, err := parseDurationDefault(f.Controller.Monitor.Interval, time.Second)
	if err != nil {
		return spec, fmt.Errorf("controller.monitor.interval: %w", err)
	}
	reconfirmTimeout, err := parseDurationDefault(f.Controller.Monitor.ReconfirmTimeout, 3*time.Second)
	if err != nil {
		return spec, fmt.Errorf("controller.monitor.reconfirm_timeout: %w", err)
	}
	failureThreshold := f.Controller.Monitor.FailureThreshold
	if failureThreshold <= 0 {
		failureThreshold = 3
	}
	spec.Controller = domain.ControllerSpec{
		ID: fallback(f.Controller.ID, "controller-1"),
		Lease: domain.LeaseSpec{
			Backend: fallback(f.Controller.Lease.Backend, "local-memory"),
			TTL:     leaseTTL,
		},
		Monitor: domain.MonitorSpec{
			Interval:         monitorInterval,
			FailureThreshold: failureThreshold,
			ReconfirmTimeout: reconfirmTimeout,
		},
	}
	for _, sc := range f.Controller.SecondaryChecks {
		timeout, err := parseDurationDefault(sc.Timeout, 2*time.Second)
		if err != nil {
			return spec, fmt.Errorf("secondary check %q timeout: %w", sc.Name, err)
		}
		spec.Controller.SecondaryChecks = append(spec.Controller.SecondaryChecks, domain.SecondaryCheckSpec{
			Name:         sc.Name,
			ObserverNode: sc.ObserverNode,
			Timeout:      timeout,
		})
	}

	mode := domain.ReplicationMode(strings.TrimSpace(f.Replication.Mode))
	if mode == "" {
		mode = domain.ReplicationModeGTID
	}
	if mode != domain.ReplicationModeGTID {
		return spec, fmt.Errorf("replication.mode=%q is not supported; only gtid is allowed", mode)
	}
	semiSyncPolicy := domain.SemiSyncPolicy(strings.TrimSpace(f.Replication.SemiSync.Policy))
	if semiSyncPolicy == "" {
		semiSyncPolicy = domain.SemiSyncPreferred
	}
	switch semiSyncPolicy {
	case domain.SemiSyncDisabled, domain.SemiSyncPreferred, domain.SemiSyncRequired:
	default:
		return spec, fmt.Errorf("invalid semi_sync.policy %q", semiSyncPolicy)
	}
	semiSyncTimeout, err := parseDurationDefault(f.Replication.SemiSync.Timeout, 5*time.Second)
	if err != nil {
		return spec, fmt.Errorf("replication.semi_sync.timeout: %w", err)
	}
	salvagePolicy := domain.SalvagePolicy(strings.TrimSpace(f.Replication.Salvage.Policy))
	if salvagePolicy == "" {
		salvagePolicy = domain.SalvageIfPossible
	}
	switch salvagePolicy {
	case domain.SalvageStrict, domain.SalvageIfPossible, domain.SalvageAvailabilityFirst:
	default:
		return spec, fmt.Errorf("invalid salvage.policy %q", salvagePolicy)
	}
	salvageTimeout, err := parseDurationDefault(f.Replication.Salvage.Timeout, 30*time.Second)
	if err != nil {
		return spec, fmt.Errorf("replication.salvage.timeout: %w", err)
	}
	waitForReplicaCount := f.Replication.SemiSync.WaitForReplicaCount
	if waitForReplicaCount < 0 {
		waitForReplicaCount = 0
	}
	spec.Replication = domain.ReplicationSpec{
		Mode: mode,
		SemiSync: domain.SemiSyncSpec{
			Policy:              semiSyncPolicy,
			WaitForReplicaCount: waitForReplicaCount,
			Timeout:             semiSyncTimeout,
		},
		Salvage: domain.SalvageSpec{
			Policy:  salvagePolicy,
			Timeout: salvageTimeout,
		},
	}

	spec.WriterEndpoint = domain.WriterEndpointSpec{
		Kind:            fallback(f.WriterEndpoint.Kind, "none"),
		Target:          f.WriterEndpoint.Target,
		Command:         strings.TrimSpace(f.WriterEndpoint.Command),
		PrecheckCommand: strings.TrimSpace(f.WriterEndpoint.PrecheckCommand),
		VerifyCommand:   strings.TrimSpace(f.WriterEndpoint.VerifyCommand),
	}
	for i, step := range f.Fencing.Steps {
		kind := strings.TrimSpace(step.Kind)
		if kind == "" {
			return spec, fmt.Errorf("fencing.steps[%d].kind must be set", i)
		}
		required := true
		if step.Required != nil {
			required = *step.Required
		}
		timeout, err := parseOptionalDuration(step.Timeout)
		if err != nil {
			return spec, fmt.Errorf("fencing.steps[%d].timeout: %w", i, err)
		}
		spec.Fencing.Steps = append(spec.Fencing.Steps, domain.FencingStepSpec{
			Kind:     kind,
			Required: required,
			Command:  strings.TrimSpace(step.Command),
			Timeout:  timeout,
		})
	}
	spec.Hooks = domain.HookSpec{
		ShellCompat: f.Hooks.ShellCompat,
		Command:     strings.TrimSpace(f.Hooks.Command),
	}

	if len(f.Nodes) < 2 {
		return spec, errors.New("at least 2 nodes are required")
	}
	primaryCount := 0
	for _, n := range f.Nodes {
		versionSeries, err := capability.NormalizeVersionSeries(n.VersionSeries)
		if err != nil {
			return spec, fmt.Errorf("node %q version_series: %w", n.ID, err)
		}
		role := domain.NodeRole(strings.TrimSpace(n.ExpectedRole))
		if role == "" {
			role = domain.NodeRoleReplica
		}
		switch role {
		case domain.NodeRolePrimary, domain.NodeRoleReplica, domain.NodeRoleObserver:
		default:
			return spec, fmt.Errorf("node %q expected_role %q is invalid", n.ID, role)
		}
		if role == domain.NodeRolePrimary {
			primaryCount++
		}
		if strings.TrimSpace(n.ID) == "" {
			return spec, errors.New("node.id must be set")
		}
		if strings.TrimSpace(n.Host) == "" {
			return spec, fmt.Errorf("node %q host must be set", n.ID)
		}
		if n.Port == 0 {
			n.Port = 3306
		}
		if (strings.TrimSpace(n.SQL.ReplicationUser) == "") != (strings.TrimSpace(n.SQL.ReplicationPasswordRef) == "") {
			return spec, fmt.Errorf("node %q sql.replication_user and sql.replication_password_ref must be set together", n.ID)
		}
		spec.Nodes = append(spec.Nodes, domain.NodeSpec{
			ID:                n.ID,
			Host:              n.Host,
			Port:              n.Port,
			VersionSeries:     versionSeries,
			ExpectedRole:      role,
			CandidatePriority: n.CandidatePriority,
			NoMaster:          n.NoMaster,
			IgnoreFail:        n.IgnoreFail,
			Zone:              n.Zone,
			Labels:            n.Labels,
			SQL: domain.SQLTargetSpec{
				User:                   n.SQL.User,
				PasswordRef:            n.SQL.PasswordRef,
				ReplicationUser:        n.SQL.ReplicationUser,
				ReplicationPasswordRef: n.SQL.ReplicationPasswordRef,
				TLSProfile:             n.SQL.TLSProfile,
			},
		})
		last := &spec.Nodes[len(spec.Nodes)-1]
		if n.SSH != nil {
			port := n.SSH.Port
			if port == 0 {
				port = 22
			}
			last.SSH = &domain.SSHTargetSpec{
				User:                    n.SSH.User,
				Port:                    port,
				PasswordRef:             n.SSH.PasswordRef,
				PrivateKeyRef:           n.SSH.PrivateKeyRef,
				PrivateKeyPassphraseRef: n.SSH.PrivateKeyPassphraseRef,
				BinlogDir:               n.SSH.BinlogDir,
				BinlogIndex:             n.SSH.BinlogIndex,
				BinlogPrefix:            n.SSH.BinlogPrefix,
				MySQLBinlogPath:         n.SSH.MySQLBinlogPath,
			}
		}
		if n.Agent != nil {
			last.Agent = &domain.AgentTargetSpec{
				Address:   n.Agent.Address,
				AuthToken: n.Agent.AuthToken,
			}
		}
	}
	if primaryCount > 1 {
		return spec, errors.New("only one node can have expected_role=primary")
	}
	if primaryCount == 0 {
		spec.Nodes[0].ExpectedRole = domain.NodeRolePrimary
	}
	return spec, nil
}

func parseDurationDefault(raw string, fallback time.Duration) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return fallback, nil
	}
	return time.ParseDuration(raw)
}

func parseOptionalDuration(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	return time.ParseDuration(raw)
}

func fallback(value, defaultValue string) string {
	if strings.TrimSpace(value) == "" {
		return defaultValue
	}
	return value
}
