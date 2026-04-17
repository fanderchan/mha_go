package domain

import (
	"fmt"
	"slices"
	"time"
)

type TopologyKind string

const (
	TopologyAsyncSinglePrimary            TopologyKind = "async-single-primary"
	TopologyGroupReplicationSinglePrimary TopologyKind = "group-replication-single-primary"
	TopologyGroupReplicationMultiPrimary  TopologyKind = "group-replication-multi-primary"
	TopologyInnoDBCluster                 TopologyKind = "innodb-cluster"
)

type ReplicationMode string

const (
	ReplicationModeGTID ReplicationMode = "gtid"
)

type SemiSyncPolicy string

const (
	SemiSyncDisabled  SemiSyncPolicy = "disabled"
	SemiSyncPreferred SemiSyncPolicy = "preferred"
	SemiSyncRequired  SemiSyncPolicy = "required"
)

type SalvagePolicy string

const (
	SalvageStrict            SalvagePolicy = "strict"
	SalvageIfPossible        SalvagePolicy = "salvage-if-possible"
	SalvageAvailabilityFirst SalvagePolicy = "availability-first"
)

type NodeRole string

const (
	NodeRoleUnknown  NodeRole = "unknown"
	NodeRolePrimary  NodeRole = "primary"
	NodeRoleReplica  NodeRole = "replica"
	NodeRoleObserver NodeRole = "observer"
)

type NodeHealth string

const (
	NodeHealthUnknown NodeHealth = "unknown"
	NodeHealthAlive   NodeHealth = "alive"
	NodeHealthSuspect NodeHealth = "suspect"
	NodeHealthDead    NodeHealth = "dead"
)

type RunKind string

const (
	RunKindMonitor   RunKind = "monitor"
	RunKindCheckRepl RunKind = "check-repl"
	RunKindFailover  RunKind = "failover"
	RunKindSwitch    RunKind = "switch"
)

type RunStatus string

const (
	RunStatusPending   RunStatus = "pending"
	RunStatusRunning   RunStatus = "running"
	RunStatusSucceeded RunStatus = "succeeded"
	RunStatusFailed    RunStatus = "failed"
	RunStatusAborted   RunStatus = "aborted"
)

type EventSeverity string

const (
	EventSeverityInfo  EventSeverity = "info"
	EventSeverityWarn  EventSeverity = "warn"
	EventSeverityError EventSeverity = "error"
)

type ClusterSpec struct {
	Name           string
	Topology       TopologySpec
	Controller     ControllerSpec
	Replication    ReplicationSpec
	Fencing        FencingSpec
	WriterEndpoint WriterEndpointSpec
	Nodes          []NodeSpec
	Hooks          HookSpec
}

type TopologySpec struct {
	Kind                   TopologyKind
	SingleWriter           bool
	AllowCascadingReplicas bool
}

type ControllerSpec struct {
	ID              string
	Lease           LeaseSpec
	Monitor         MonitorSpec
	SecondaryChecks []SecondaryCheckSpec
}

type LeaseSpec struct {
	Backend string
	TTL     time.Duration
}

type MonitorSpec struct {
	Interval         time.Duration
	FailureThreshold int
	ReconfirmTimeout time.Duration
}

type SecondaryCheckSpec struct {
	Name         string
	ObserverNode string
	Timeout      time.Duration
}

type ReplicationSpec struct {
	Mode     ReplicationMode
	SemiSync SemiSyncSpec
	Salvage  SalvageSpec
}

type SemiSyncSpec struct {
	Policy              SemiSyncPolicy
	WaitForReplicaCount int
	Timeout             time.Duration
}

type SalvageSpec struct {
	Policy  SalvagePolicy
	Timeout time.Duration
}

type WriterEndpointSpec struct {
	Kind            string
	Target          string
	Command         string // optional shell for VIP/proxy switch (kind vip/proxy)
	PrecheckCommand string // optional shell precheck before promotion
	VerifyCommand   string // optional shell verification after switch
}

type FencingSpec struct {
	Steps []FencingStepSpec
}

type FencingStepSpec struct {
	Kind     string
	Required bool
	Command  string
	Timeout  time.Duration
}

type HookSpec struct {
	ShellCompat bool
	Command     string // shell command executed for every hook event when ShellCompat is true
}

type NodeSpec struct {
	ID                string
	Host              string
	Port              int
	VersionSeries     string
	ExpectedRole      NodeRole
	CandidatePriority int
	NoMaster          bool
	IgnoreFail        bool
	Zone              string
	Labels            map[string]string
	SQL               SQLTargetSpec
	SSH               *SSHTargetSpec
	Agent             *AgentTargetSpec
}

func (n NodeSpec) Address() string {
	return fmt.Sprintf("%s:%d", n.Host, n.Port)
}

type SQLTargetSpec struct {
	User        string
	PasswordRef string
	TLSProfile  string
}

type SSHTargetSpec struct {
	User        string
	Port        int
	PasswordRef string
}

type AgentTargetSpec struct {
	Address   string
	AuthToken string
}

type CapabilitySet struct {
	HasGTID                     bool
	HasAutoPosition             bool
	HasSuperReadOnly            bool
	HasSemiSync                 bool
	HasPerfSchemaReplication    bool
	HasClonePlugin              bool
	SupportsReplicationChannels bool
	SupportsDynamicPrivileges   bool
	SupportsReadOnlyFence       bool
}

type ReplicaState struct {
	SourceID            string
	SourceUUID          string
	SourceHost          string
	SourcePort          int
	ChannelName         string
	AutoPosition        bool
	GTIDExecuted        string
	GTIDRetrieved       string
	SecondsBehindSource int64
	IOThreadRunning     bool
	SQLThreadRunning    bool
	SemiSyncReplica     bool
	LastIOError         string
	LastSQLError        string
}

type NodeState struct {
	ID                string
	Address           string
	ServerUUID        string
	Version           string
	VersionSeries     string
	Role              NodeRole
	Health            NodeHealth
	CandidatePriority int
	NoMaster          bool
	IgnoreFail        bool
	Capabilities      CapabilitySet
	GTIDExecuted      string
	ReadOnly          bool
	SuperReadOnly     bool
	SemiSyncSource    bool
	Replica           *ReplicaState
	Labels            map[string]string
	LastError         string
}

type ClusterView struct {
	ClusterName  string
	TopologyKind TopologyKind
	CollectedAt  time.Time
	PrimaryID    string
	Nodes        []NodeState
	Warnings     []string
}

func (v *ClusterView) PrimaryNode() (*NodeState, bool) {
	for i := range v.Nodes {
		if v.Nodes[i].ID == v.PrimaryID {
			return &v.Nodes[i], true
		}
	}
	return nil, false
}

func (v *ClusterView) ReplicaNodes() []NodeState {
	out := make([]NodeState, 0, len(v.Nodes))
	for _, node := range v.Nodes {
		if node.Role == NodeRoleReplica {
			out = append(out, node)
		}
	}
	return out
}

func (v *ClusterView) ReplicaIDs() []string {
	out := make([]string, 0, len(v.Nodes))
	for _, node := range v.ReplicaNodes() {
		out = append(out, node.ID)
	}
	slices.Sort(out)
	return out
}

type FailoverPlan struct {
	ClusterName                  string
	CreatedAt                    time.Time
	OldPrimary                   NodeState
	Candidate                    NodeState
	LeaseKey                     string
	LeaseOwner                   string
	PrimaryFailureConfirmed      bool
	PrimaryFailureReason         string
	PromoteReadinessConfirmed    bool
	PromoteReadinessReasons      []string
	ExecutionPermitted           bool
	BlockingReasons              []string
	AssessmentErrors             int
	AssessmentWarnings           int
	CandidateFreshnessScore      int
	CandidateMostAdvanced        bool
	SalvagePolicy                SalvagePolicy
	ShouldAttemptSalvage         bool
	MissingFromPrimaryKnown      bool
	MissingFromPrimaryGTIDSet    string
	RecoveryGaps                 []RecoveryGap
	SalvageActions               []SalvageAction
	SuggestedDonorIDs            []string
	Steps                        []FailoverStep
	RequiresFencing              bool
	RequiresWriterEndpointSwitch bool
	RepointReplicaIDs            []string
	SkippedReplicaIDs            []string
}

type RecoveryGap struct {
	SourceNodeID     string
	MissingGTIDSet   string
	MissingGTIDKnown bool
}

type SalvageAction struct {
	Kind           string
	DonorNodeID    string
	TargetNodeID   string
	MissingGTIDSet string
	Required       bool
	Reason         string
}

type FailoverStep struct {
	Name     string
	Status   string
	Blocking bool
	Required bool // if false, a salvage step failure warns but does not abort the failover
	Reason   string
}

type FailoverExecution struct {
	ClusterName string
	DryRun      bool
	StartedAt   time.Time
	FinishedAt  time.Time
	Succeeded   bool
	Blocked     bool
	FailedStep  string
	Plan        FailoverPlan
	StepResults []FailoverStepResult
}

type FailoverStepResult struct {
	Name    string
	Status  string
	Message string
}

type SwitchoverPlan struct {
	ClusterName                  string
	CreatedAt                    time.Time
	OriginalPrimary              NodeState
	Candidate                    NodeState
	RequiresWriterEndpointSwitch bool
	Steps                        []SwitchoverStep
}

type SwitchoverStep struct {
	Name   string
	Status string // pending | skipped
}

type SwitchoverExecution struct {
	ClusterName string
	DryRun      bool
	StartedAt   time.Time
	FinishedAt  time.Time
	Succeeded   bool
	FailedStep  string
	Plan        SwitchoverPlan
	StepResults []SwitchoverStepResult
}

type SwitchoverStepResult struct {
	Name    string
	Status  string
	Message string
}

type RunRecord struct {
	ID        string
	Cluster   string
	Kind      RunKind
	Status    RunStatus
	StartedAt time.Time
	UpdatedAt time.Time
	Summary   string
	Events    []RunEvent
}

type RunEvent struct {
	Sequence int64
	At       time.Time
	Phase    string
	Severity EventSeverity
	Message  string
	Metadata map[string]string
}
