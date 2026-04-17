# MHA Go Rewrite Blueprint

[中文](mha-go-blueprint_zh.md)

Last updated: 2026-04-17
Status: baseline design document

## 1. Purpose

This document defines the direction of the Go rewrite of MHA and serves as the baseline for ongoing development.

The goal is not to replicate `mha4mysql-manager` / `mha4mysql-node` 0.58 line-by-line, but to:

- Inherit MHA's core capabilities for async single-writer replication topologies
- Resolve the main pain points of 0.58
- Commit explicitly to modern versions and modern operating practices
- Leave room for Group Replication / InnoDB Cluster without implementing them yet

## 2. Product Scope

### 2.1 Current version scope

This version supports only:

- MySQL `8.4.x`: primary support target, both test and production baseline
- MySQL `9.7 ER/EA`: pre-adaptation target for forward-looking validation

Not supported:

- MySQL `5.7`
- MySQL `8.0`
- MySQL `9.6`
- Non-GTID replication

Notes:

- As of 2026-04-14, `8.4` is the stable long-term support line.
- `9.7` is still treated as `ER/EA`. Code must prioritize capability detection and must not hard-code version assumptions.
- Without a stable `9.7` validation environment, `9.7` stays in the test blueprint and the forward-compatibility design; it is not a release blocker right now.

### 2.2 Current topology scope

This version covers only:

- Async single-writer replication
- GTID replication
- Optional semi-sync replication
- Single primary with multiple replicas
- Multi-level replication is recognized, but v1's main test surface is single-level primary-to-replica

Reserved but not implemented for now:

- Group Replication
- InnoDB Cluster
- Multi-writer topologies

## 3. Pain Points of 0.58 That This Rewrite Addresses

### 3.1 Main problems in 0.58

- Depends on Perl, SSH, public keys, and the node toolkit — deployment and maintenance are costly
- Monitoring, failover, and online switchover logic are scattered; execution is hard to audit
- Weak recovery on mid-execution failures; mostly relies on human intervention
- Poor observability; no structured event stream or unified history record
- External hooks rely on shell argument concatenation and are brittle
- Non-GTID / relay-log recovery logic is complex, and the core model is dragged down by historical compatibility
- Adapting to modern version evolution leans empirical rather than capability-based

### 3.2 Solutions in the new version

- Single Go binary with minimum dependencies
- State machines drive monitoring, failover, and online switchover
- `GTID-first` as a core principle — non-GTID paths no longer shape the architecture
- Capability detection replaces large amounts of hard-coded version branching
- Structured event logs, run logs, audit logs, and metrics
- Typed hooks; shell compatibility as an adapter layer
- Optional agent mode that gradually reduces reliance on raw SSH

## 4. Design Principles

- `GTID-only`
- `State-machine first`
- `Capability-driven`
- `Journaled by structured logs`
- `Agent-optional`
- `Production on 8.4 first`
- `9.7 ER compatibility by detection, not assumption`

## 5. Overall Architecture

```text
cmd/mha
├─ manager        long-running monitor + automatic failover
├─ switch         online switchover
├─ check-repl     topology and replication health check
├─ failover-plan  failover planning
├─ failover-execute failover execution
└─ version        version info

internal/
├─ config         config model; compat with legacy MHA config
├─ capability     version capability detection
├─ domain         domain objects
├─ topology       topology discovery and candidate selection
├─ monitor        health checks and false-positive suppression
├─ failover       automatic failover state machine
├─ switchover     online switchover state machine
├─ replication    GTID replication control and salvage logic
├─ fencing        VIP / STONITH / endpoint switching
├─ hooks          typed hooks + shell compat layer
├─ state          in-process state, events, run records
├─ transport      SQL / SSH / Agent RPC
└─ obs            logging, metrics, audit, event queries
```

## 6. Core Modules

### 6.1 `config`

Responsibilities:

- Read YAML/TOML/JSON primary config
- Import legacy MHA `cnf` for compatibility
- Validate required fields
- Validate mutually exclusive fields
- Normalize defaults

Core requirements:

- Stop using "block name + Perl-style parameters" as the internal model
- Legacy formats are input adapters only; they do not enter the core domain
- `password_ref` uses a unified reference form. In v1 we support `env:NAME`, `file:/path`, and `plain:value`
- Offline demos and unit tests may use `static discoverer`; real topology checks go through `sql discoverer`

### 6.2 `capability`

Detect capabilities per node:

- `HasGTID`
- `HasAutoPosition`
- `HasSuperReadOnly`
- `HasSemiSync`
- `HasPerfSchemaReplicationTables`
- `HasClonePlugin`
- `SupportsReplicationChannels`
- `SupportsDynamicPrivileges`
- `SupportsReadOnlyFence`

Rules:

- Controllers must not write `if version >= x`
- Check capabilities first, then decide behavior

### 6.3 `domain`

Suggested objects:

- `ClusterSpec`
- `NodeSpec`
- `ClusterView`
- `NodeState`
- `ReplicaState`
- `CandidateScore`
- `FailoverPlan`
- `SwitchoverPlan`
- `RunRecord`
- `RunEvent`

### 6.4 `topology`

Responsibilities:

- Discover the current writer
- Distinguish alive / dead / replica / non-replica
- Judge candidate availability
- Check replication filters and config consistency
- Recognize multi-level replication
- Produce a `ClusterView` for the state machine to consume

Current implementation constraints:

- `sql discoverer` probes node basics, GTID, `SHOW REPLICA STATUS`, and semi-sync state via read-only SQL
- `static discoverer` is used only for offline dry-run, examples, and tests — never as the production discovery path
- `check-repl` runs a replication health assessment after discovery and separates error vs. warn findings
- Candidate ranking currently combines `auto-position`, replication thread state, source mapping, read-only state, semi-sync, and lag — not just a static priority
- `failover-plan` first acquires the lease, then computes candidate freshness, primary gap, and donor recommendation from the GTID sets
- `failover-plan` also emits an execution gate: whether the primary is confirmed dead, the blocking reasons, and the recommended salvage actions
- `failover-plan` produces a typed step outline covering `confirm`, `fence`, `salvage`, `promote`, `repoint`, `switch-writer-endpoint`, and `verify`
- `failover-execute --dry-run` consumes the typed step outline and halts at the first blocking step
- `failover-execute --dry-run=false` uses `MySQLActionRunner`: when `writer_endpoint.kind` is `vip`/`proxy` it runs the endpoint precheck first (confirming the switch command exists and running the optional `precheck_command`); it fences the old primary per `fencing.steps` with a default required `read_only` (SQL `super_read_only`/`read_only` when reachable, skipped when the old primary is already marked dead and unreachable); salvage steps point the candidate at a donor followed by `WAIT_FOR_EXECUTED_GTID_SET`; the candidate is promoted via `STOP REPLICA` / `RESET REPLICA ALL` / turning off read-only; only replicas reachable at planning time are repointed, dead replicas are skipped and left for later rejoin; the writer endpoint switch runs the external script via `writer_endpoint.command` or the `MHA_WRITER_ENDPOINT_COMMAND` env var; the optional `verify_command` validates the endpoint; `verify-cluster` uses SQL to confirm the new primary is writable and reachable replicas point at it

### 6.5 `monitor`

Responsibilities:

- Primary health probing
- Multi-observer secondary confirmation
- Network-partition false-positive suppression
- Manager's own lease protection

Basic state machine:

```text
Init
-> DiscoverTopology
-> Healthy
-> Suspect
-> SecondaryCheck
-> ReconfirmTopology
-> DeadConfirmed
-> HandoverToFailover
```

Implementation details (`internal/controller/monitor`):

```
Healthy ──probe fails──► Suspect ──threshold reached──► SecondaryCheck
  ▲                        │                                │
  │                      recovers                  replica IO thread confirms primary alive
  └────────────────────────┘                                │
                                                   all fail │
                                                            ▼
                                        ReconfirmTopology ──rediscovery shows primary alive──► Healthy
                                                            │
                                                    primary still dead
                                                            ▼
                                                    DeadConfirmed ──► HandleFailover()
```

- **Healthy**: probe (SQL ping) the primary every interval. Failure → Suspect; success resets failureCount.
- **Suspect**: keep probing; accumulate failures. Reaching `failure_threshold` → SecondaryCheck; any success → Healthy.
- **SecondaryCheck**: check each replica's IO thread to see whether it is still connected to the primary. If `secondary_checks` is configured, ask the specified observer nodes as well. Any confirmation that the primary is alive → Healthy; all fail → ReconfirmTopology.
- **ReconfirmTopology**: within `reconfirm_timeout`, re-run full topology discovery. Primary alive → Healthy; primary still dead or discovery fails → DeadConfirmed.
- **DeadConfirmed**: call `FailoverHandler.HandleFailover()`. The manager loop exits. A human or ops automation must restart the manager to monitor the new primary.

### 6.6 `failover`

Responsibilities:

- Confirm old primary is dead
- Fence old primary
- Select candidate
- Salvage
- Promote new primary
- Repoint other replicas
- Switch writer endpoint
- Verify result

State machine:

```text
LoadSpec
-> SnapshotTopology
-> AcquireLease
-> ConfirmPrimaryDead
-> FenceOldPrimary
-> SelectCandidate
-> RecoverMissingTransactions
-> PromoteCandidate
-> RepointReplicas
-> SwitchWriterEndpoint
-> Verify
-> Complete
```

### 6.7 `switchover`

Responsibilities:

- Pre-switch checks
- Reject new writes
- Lock the old primary
- Wait for the candidate to catch up
- Switch to the new primary
- Repoint old primary and other replicas

State machine:

```text
Precheck
-> PrecheckWriterEndpoint
-> LockOldPrimary
-> WaitCandidateCatchUp
-> PromoteCandidate
-> RepointReplicas
-> RepointOldPrimary
-> SwitchWriterEndpoint
-> VerifyWriterEndpoint
-> Verify
-> Complete
```

Note: there is no separate `FreezeWrites` step. `LockOldPrimary` (setting `super_read_only`) already blocks new writes at the MySQL layer and is equivalent in effect. Proxy-layer traffic cutover is handled at the end by `SwitchWriterEndpoint` via an external script; the two responsibilities don't overlap, so no intermediate step is needed.

### 6.8 `replication`

Currently GTID-only.

Two classes of logic:

- `gtid`: normal GTID auto-switching and catch-up
- `salvage`: handling the semi-sync downgrade / async gap window

### 6.9 `fencing`

Unified fencing interface:

- `ReadOnlyFence`
- `VIPFence`
- `STONITHFence`
- `CloudRouteFence`
- `ProxyWriterFence`

Requirements:

- Fencing is a first-class citizen, not an attached script
- Failover must not reach writer endpoint switching until fencing is complete
- The v1 SQL-level read-only fence counts only as `ReadOnlyFence`, not full fault isolation
- Production fencing should prefer a typed coordinator; a shell-compat adapter is layered on top

Recommended implementation order:

1. `ReadOnlyFence`: when the old primary is reachable, set `super_read_only=ON` / `read_only=ON` as the basic MySQL-layer guard.
2. `ProxyWriterFence` / `VIPFence`: remove the writer entry from the old primary via a typed interface or a compat script, with verifiable evidence that writes now go only to the new primary.
3. `STONITHFence` / `CloudRouteFence`: with explicit configuration, execute power, cloud route, security group, or instance-level isolation.
4. `FenceCoordinator`: run configured fencing steps in order and emit structured logs for each action; any failure of a required fence must block writer endpoint switching.

Writer endpoint switching answers "where should new writes go?"; fencing answers "can the old primary still accept writes?" These two must be modeled separately — one VIP script cannot carry both semantics.

### 6.10 `state`

The `RunStore` interface tracks **in-process** state for a single operation (a failover / switchover / monitor session): each step's result is written to `RunRecord`/`RunEvent`, and the caller summarizes the results when the operation ends. It is an internal coordination mechanism, not a persistence database.

Ops auditing (history, post-incident review) relies on **structured log files** (stderr JSON/logfmt redirected to files), queried with `grep` / `jq`. We do not introduce SQLite, an embedded database, or any additional persistent store.

Constraints:

- Do not implement the persistent database required by `admin history`.
- Do not add `--state-db` / a SQLite runtime.
- Any history that must be kept goes to a log file or is collected by an external log system.
- `RunStore` only coordinates a single operation within the current process; it cannot be used as a cross-process recovery source.

Current implementation: `MemoryStore` (in-process, reset on restart) + `LocalLeaseManager` (single-process).

## 7. Semi-sync and Async-gap Salvage Strategy

This is a problem the new version must address head-on.

### 7.1 The problem

Even with semi-sync enabled, the following can happen:

- Semi-sync times out and downgrades to async
- Transactions are committed locally on the primary but replicas never receive them
- After the primary crashes, the newest transactions exist only in the old primary's local binlog

Promoting the most advanced replica directly in this case risks losing transactions.

### 7.2 Design goals

Under a GTID-only baseline, the salvage logic must explicitly support three policies:

#### `strict`

- Do not promote automatically until loss is ruled out
- Salvage must succeed before failover is allowed

For high-consistency workloads.

#### `salvage-if-possible`

- First try to extract missing GTID transactions from the old primary
- Apply them to the candidate on success
- Abort the auto switch if extraction fails

This is the recommended default.

#### `availability-first`

- If the old primary is unreachable, allow promotion of the most advanced replica
- Explicitly record the "suspected lost transaction window"
- Emit a high-priority audit event and alert

For workloads that prioritize availability.

### 7.3 Salvage implementation approach

Priority order:

1. Old primary is SQL-reachable: query GTID, binlog position, and read-only state directly
2. Old primary not SQL-reachable but reachable via agent/SSH: read local binlog and extract the missing GTID transactions
3. Old primary fully unreachable: follow the configured policy to abort or continue

Abstract interface:

```go
type TransactionSalvager interface {
    CollectMissingTransactions(ctx context.Context, oldPrimary NodeRef, candidate NodeRef, gap GTIDSet) (ArtifactRef, error)
    ApplyTransactions(ctx context.Context, candidate NodeRef, artifact ArtifactRef) error
}
```

### 7.4 Why salvage is still needed

Because:

- GTID only solves "locate and reconnect"; it does not automatically solve "transaction exists only on the old primary"
- Semi-sync is not absolutely safe — as long as it can downgrade, salvage and conservative policies must be designed in

## 8. Candidate Selection Rules

Candidate selection must be two-phase:

### 8.1 Eligibility filter

Must satisfy:

- Reachable
- Not `no_master`
- Replication threads healthy
- Lag within tolerance
- `log_bin` enabled
- `read_only` / `super_read_only` controllable
- Replication filters compatible with the business policy

### 8.2 Scoring rank

Suggested dimensions:

- Most advanced GTID
- `candidate_master` preference
- Better semi-sync state
- Same city / same AZ preferred
- Fewer historical failures
- Lower read-only switch latency

## 9. Management vs. Runtime Surface

### 9.1 CLI

v1 CLI:

- `mha manager`
- `mha switch`
- `mha check-repl`
- `mha failover-plan`
- `mha failover-execute`
- `mha version`

Considered later but not current targets:

- `mha manager run-once`
- `mha compat import-mha-cnf`
- `mha admin status`

Explicitly not doing:

- `mha admin history`: history auditing is served uniformly by structured log files.
- `mha admin resume`: v1 does not introduce a persistent state database; mid-operation recovery relies on human log review plus re-running idempotent steps.

### 9.2 Management API

Reserved for later:

- `GET /status`
- `POST /switch`
- `POST /stop`

v1 implements only the local CLI. REST/gRPC is a later extension. A management API must not retroactively force a SQLite or embedded state DB.

## 10. Hook Specification

Do not let the core state machine concatenate shell arguments directly anymore.

Hooks are for alerting, audit, legacy callback compat, and external notification — they must not carry VIP / proxy writer switch semantics. VIP drift and proxy writer updates must go through the `writer_endpoint` step; `failover.writer_switched` is a post-success notification, not the trigger for VIP drift.

Internal unified events:

- `monitor.suspect`
- `failover.start`
- `failover.fence`
- `failover.promote`
- `failover.writer_switched`
- `failover.complete`
- `failover.abort`
- `switchover.start`
- `switchover.complete`

Two external implementations are supported:

- typed Go plugin / RPC handler
- shell compatibility adapter

## 11. Group Replication / InnoDB Cluster Reserved Specification

Not implemented now, but the interface must be reserved.

### 11.1 Principles

- Do not cram GR/Cluster into the async replication controller
- Abstract "topology mode" and "writer management scheme"

### 11.2 Reserved interface

```go
type TopologyMode interface {
    Name() string
    Discover(ctx context.Context, cluster ClusterSpec) (*ClusterView, error)
    Validate(ctx context.Context, view *ClusterView) error
    SupportsManualPromotion() bool
    SupportsExternalWriterEndpoint() bool
}
```

First set of modes:

- `AsyncSinglePrimaryMode`
- `GroupReplicationSinglePrimaryMode`
- `GroupReplicationMultiPrimaryMode`
- `InnoDBClusterMode`

### 11.3 Points to consider in advance

- GR has its own primary election, so async replication promotion logic cannot be reused directly
- InnoDB Cluster writer switching depends more on metadata and Router
- Endpoint switching and fencing have different responsibility boundaries
- Monitoring is no longer just `SHOW REPLICA STATUS`

## 12. Testing Strategy

### 12.1 Support matrix

Must be continuously tested:

- MySQL `8.4.x`
- MySQL `9.7 ER/EA` (run when a usable environment exists)

Where:

- `8.4` is the release-blocking matrix
- `9.7 ER/EA` is the forward-compatibility matrix; when no environment is available it stays in the test blueprint without blocking releases

### 12.2 Required scenarios

- Primary crash
- Manager isolated from primary by network
- Single replica lag
- Uneven lag across replicas
- Candidate cannot be promoted
- Old primary fencing fails
- Normal semi-sync switch
- Salvage succeeds after semi-sync downgrade to async
- Salvage fails after semi-sync downgrade to async
- Strict mode when old primary is fully unreachable
- Online switchover interrupted mid-way — the manual recovery runbook
- Hook failures
- Idempotency of repeated failover/switchover key steps
- Writer endpoint switch must not proceed if fencing failed

## 13. Phased Development Plan

### Phase 1

- Config model
- Capability detection
- Topology discovery
- `check-repl`
- Basic journal

### Phase 2

- Manager monitor loop
- Suspect / secondary check / reconfirm
- Basic failover state machine

### Phase 3

- GTID failover
- Basic `ReadOnlyFence`
- Writer endpoint switch
- Structured log audit

### Phase 4

- Online switchover
- Manual recovery runbook
- Shell hook compatibility layer

### Phase 5

- Salvage logic after semi-sync downgrade
- Typed fencing coordinator
- Agent/SSH binlog salvage
- Dual manager / distributed lease evaluation (roadmap item, not a v1 target)

### Phase 6

- GR/Cluster mode implementation

## 14. Explicit Non-goals

This version will not:

- Support non-GTID
- Compat with 5.7 / 8.0 / 9.6
- Keep extending features for the legacy MHA node toolkit
- Make shell scripts the core interface
- Strongly depend on SSH by default
- Introduce SQLite, an embedded database, or `admin history`
- Run dual manager / distributed lease

## 15. Conclusions

The project's architectural trajectory is fixed as:

- `8.4 first`
- `9.7 ER pre-adaptation in test blueprint`
- `GTID-only`
- `semi-sync aware`
- `async gap salvage capable`
- `state-machine driven`
- `journaled by structured log files`
- `single-manager by default, close to Perl MHA operating model`
- `GR/Cluster extension ready`

Any future change that conflicts with this document must update this document first, then change the implementation.
