# mha-go Operations Guide

## Table of contents

1. [MySQL prerequisites](#1-mysql-prerequisites)
2. [Config file reference](#2-config-file-reference)
3. [Credential reference formats](#3-credential-reference-formats)
4. [Subcommand reference](#4-subcommand-reference)
5. [Typical workflows](#5-typical-workflows)
6. [Hook events](#6-hook-events)
7. [Writer endpoint integration](#7-writer-endpoint-integration)
8. [Fencing model](#8-fencing-model)
9. [Operational history](#9-operational-history)
10. [Salvage policy](#10-salvage-policy)

---

## 1. MySQL prerequisites

### 1.1 GTID configuration

All nodes must have GTID enabled and enforced. Add to `my.cnf`:

```ini
[mysqld]
gtid_mode                  = ON
enforce_gtid_consistency   = ON
log_bin                    = ON
log_replica_updates        = ON   # required on replicas
```

Verify:
```sql
SHOW VARIABLES WHERE Variable_name IN ('gtid_mode','enforce_gtid_consistency');
```

### 1.2 Replication account

mha-go uses a single SQL account to connect to every node. The same account is used for health checks, topology discovery, and for wiring replication after a failover.

```sql
-- Create the account (run on every node, or replicate from primary)
CREATE USER 'mha'@'%' IDENTIFIED BY 'strong-password';

-- Privileges needed for health checks and topology discovery
GRANT REPLICATION CLIENT ON *.* TO 'mha'@'%';

-- Privilege needed for RESET REPLICA ALL
GRANT RELOAD ON *.* TO 'mha'@'%';

-- Privileges needed for fencing and promotion
GRANT SYSTEM_VARIABLES_ADMIN, SESSION_VARIABLES_ADMIN ON *.* TO 'mha'@'%';

-- Privileges needed for STOP/START/RESET/CHANGE REPLICA
GRANT REPLICATION_SLAVE_ADMIN ON *.* TO 'mha'@'%';

-- Privilege needed to replicate from this account
GRANT REPLICATION SLAVE ON *.* TO 'mha'@'%';

FLUSH PRIVILEGES;
```

> **Minimum viable alternative (simpler but broader):**
> `GRANT SUPER, REPLICATION CLIENT, REPLICATION SLAVE ON *.* TO 'mha'@'%';`

### 1.3 Semi-sync (optional)

If `replication.semi_sync.policy` is `preferred` or `required`, the semi-sync plugins must be loaded:

```sql
INSTALL PLUGIN rpl_semi_sync_source SONAME 'semisync_source.so';
INSTALL PLUGIN rpl_semi_sync_replica SONAME 'semisync_replica.so';
SET GLOBAL rpl_semi_sync_source_enabled = ON;  -- on primary
SET GLOBAL rpl_semi_sync_replica_enabled = ON; -- on replicas
```

---

## 2. Config file reference

A minimal two-node cluster (`cluster.yaml`):

```yaml
name: app1

nodes:
  - id: db1
    host: 10.0.0.11
    port: 3306
    version_series: "8.4"
    expected_role: primary
    sql:
      user: mha
      password_ref: env:MHA_DB_PASSWORD

  - id: db2
    host: 10.0.0.12
    port: 3306
    version_series: "8.4"
    expected_role: replica
    sql:
      user: mha
      password_ref: env:MHA_DB_PASSWORD
```

### Full field reference

#### `name` (required)
Unique cluster name. Used in log messages, lease keys, and hook env vars.

#### `topology`

| Field | Default | Description |
|-------|---------|-------------|
| `kind` | `async-single-primary` | Topology kind. Only `async-single-primary` is supported in v1. |
| `single_writer` | `true` | Enforce single-writer invariant. |
| `allow_cascading_replicas` | `false` | Allow replicas that replicate from another replica, not directly from primary. |

#### `controller`

| Field | Default | Description |
|-------|---------|-------------|
| `id` | `controller-1` | Unique ID for this mha-go instance. Used as lease owner. |
| `lease.ttl` | `15s` | Duration strings (`15s`, `1m`, etc.) |
| `monitor.interval` | `1s` | How often to probe the primary. |
| `monitor.failure_threshold` | `3` | Consecutive probe failures before entering secondary-check phase. |
| `monitor.reconfirm_timeout` | `3s` | Timeout for the topology re-discovery during reconfirmation. |

`secondary_checks` (optional array) — additional observer nodes that mha-go will query to confirm primary reachability before declaring it dead:

```yaml
controller:
  secondary_checks:
    - name: replica2-check
      observer_node: db2   # must match a node ID in the nodes list
      timeout: 2s
```

#### `replication`

| Field | Default | Description |
|-------|---------|-------------|
| `mode` | `gtid` | Only `gtid` is supported. |
| `semi_sync.policy` | `preferred` | `disabled`, `preferred`, or `required`. |
| `semi_sync.wait_for_replica_count` | `0` | Minimum semi-sync replicas required (only enforced at check time). |
| `semi_sync.timeout` | `5s` | Semi-sync ACK timeout (informational; actual timeout set on MySQL). |
| `salvage.policy` | `salvage-if-possible` | See [Salvage policy](#10-salvage-policy). |
| `salvage.timeout` | `30s` | Maximum time to wait for GTID catch-up during salvage. |

#### `writer_endpoint`

| Field | Default | Description |
|-------|---------|-------------|
| `kind` | `none` | `none` / `off` (skip), `vip`, or `proxy`. |
| `target` | | VIP address or proxy identifier (passed as `MHA_WRITER_ENDPOINT_TARGET` to the script). |
| `command` | | Path to the script that moves the endpoint. Falls back to env `MHA_WRITER_ENDPOINT_COMMAND`. |
| `precheck_command` | | Optional command run before promotion. Falls back to env `MHA_WRITER_ENDPOINT_PRECHECK_COMMAND`. |
| `verify_command` | | Optional command run after endpoint switch. Falls back to env `MHA_WRITER_ENDPOINT_VERIFY_COMMAND`. |

#### `fencing`

If omitted, failover uses the default required SQL read-only fence (`super_read_only` / `read_only`) when the old primary is reachable.

```yaml
fencing:
  steps:
    - kind: read_only
      required: true
    - kind: stonith
      required: false
      command: /usr/local/bin/fence-old-primary.sh
      timeout: 10s
```

| Field | Default | Description |
|-------|---------|-------------|
| `steps[].kind` | required | `read_only`, `command`, `vip`, `proxy`, `stonith`, or `cloud_route`. |
| `steps[].required` | `true` | Required fence failures abort failover. Optional failures are logged and failover continues. |
| `steps[].command` | | Shell command for non-`read_only` fence steps. |
| `steps[].timeout` | | Optional duration limit for the individual fence step. |

#### `hooks`

| Field | Default | Description |
|-------|---------|-------------|
| `shell_compat` | `false` | Enable the shell hook dispatcher. |
| `command` | | Command passed to `sh -c` on each hook event. |

#### `nodes` (required, minimum 2)

| Field | Default | Description |
|-------|---------|-------------|
| `id` | required | Unique node ID. Referenced by `--new-primary`, `--candidate`, secondary check `observer_node`. |
| `host` | required | Hostname or IP. |
| `port` | `3306` | MySQL port. |
| `version_series` | required | `8.4` or `9.7`. |
| `expected_role` | `replica` | `primary`, `replica`, or `observer`. |
| `candidate_priority` | `0` | Higher values preferred during automatic candidate selection. |
| `no_master` | `false` | Exclude node from ever being promoted. |
| `ignore_fail` | `false` | Downgrade assessment errors for this node to warnings. |
| `zone` | | Availability zone label (informational). |
| `labels` | | Key/value map (informational). |
| `sql.user` | | MySQL username. |
| `sql.password_ref` | | Credential reference (see [§3](#3-credential-reference-formats)). |
| `sql.tls_profile` | `disabled` | `disabled`, `default`, `required`, `preferred`, `skip-verify`. |

---

## 3. Credential reference formats

The `sql.password_ref` field supports three schemes:

| Scheme | Example | Notes |
|--------|---------|-------|
| `env:VARNAME` | `env:MHA_DB_PASSWORD` | Read from environment variable at runtime. |
| `file:/absolute/path` | `file:/etc/mha/db.secret` | Read from file; trailing `\r\n` stripped. |
| `plain:value` | `plain:s3cr3t` | Literal — not recommended for production. |

---

## 4. Subcommand reference

### `mha check-repl`

Performs a single topology discovery and replication health check. Exits 0 if healthy, 1 if assessment errors are found.

```bash
mha check-repl --config cluster.yaml
```

Use this as a monitoring script or before any planned maintenance.

### `mha manager`

Long-running HA monitor. Probes the primary on every `monitor.interval`. Triggers automatic failover when the primary is confirmed dead.

```bash
mha manager --config cluster.yaml
```

- Acquires a local lease on startup; will refuse to run if another manager instance holds it.
- After a successful failover the process exits (exit 0). The cluster must be re-configured and the manager restarted before a new monitor session begins.
- `--dry-run=true` runs the full monitor loop but executes failover steps as dry-run only (no MySQL writes).

### `mha switch`

Online (graceful) switchover. Defaults to `--dry-run=true`; pass `--dry-run=false` to execute for real.

```bash
# Dry-run first — verify the plan
mha switch --config cluster.yaml --new-primary db2

# Execute for real
mha switch --config cluster.yaml --new-primary db2 --dry-run=false
```

- `--new-primary <id>`: target node for promotion. If omitted, the best available replica is chosen automatically.
- Requires the cluster to be healthy (assessment must pass). If the primary is already dead, use `failover-execute` instead.

Steps executed:
1. `precheck-writer-endpoint` — verify the endpoint switch can run (only when `writer_endpoint.kind` is `vip` or `proxy`).
2. `lock-old-primary` — set `super_read_only=ON` on the current primary.
3. `wait-candidate-catchup` — wait until the candidate has applied all GTIDs from the current primary.
4. `promote-candidate` — stop replication on the candidate, set it writable.
5. `repoint-replicas` — redirect other replicas to the new primary.
6. `repoint-old-primary` — make the old primary a replica of the new primary.
7. `switch-writer-endpoint` — execute the endpoint script (if configured).
8. `verify-writer-endpoint` — run the endpoint verify script (if configured).
9. `verify` — confirm the new topology is correct.

### `mha failover-plan`

Builds and prints a failover plan without executing it. Useful for auditing what would happen during an emergency.

```bash
mha failover-plan --config cluster.yaml
mha failover-plan --config cluster.yaml --candidate db2
```

The plan output shows:
- Whether primary failure is confirmed
- Whether execution is permitted (no blocking reasons)
- Salvage actions required
- All steps with their status

### `mha failover-execute`

Builds and executes a failover plan. Defaults to `--dry-run=true`.

```bash
# Safe audit — no MySQL writes
mha failover-execute --config cluster.yaml

# Execute for real (only after the primary is confirmed dead)
mha failover-execute --config cluster.yaml --dry-run=false
```

- If the plan's execution is blocked (e.g. primary is still alive), steps are recorded as `blocked`/`skipped` and the exit code is 1.
- `--candidate <id>` overrides automatic selection.

---

## 5. Typical workflows

### Planned maintenance (switchover)

```bash
# 1. Verify cluster is healthy
./mha check-repl --config cluster.yaml

# 2. Dry-run the switchover to check the plan
./mha switch --config cluster.yaml --new-primary db2

# 3. Confirm the plan looks correct, then execute
./mha switch --config cluster.yaml --new-primary db2 --dry-run=false

# 4. Verify again
./mha check-repl --config cluster.yaml
```

### Unplanned failover (primary is dead)

```bash
# 1. Confirm the situation
./mha failover-plan --config cluster.yaml

# 2. Audit dry-run execution
./mha failover-execute --config cluster.yaml

# 3. Execute for real
./mha failover-execute --config cluster.yaml --dry-run=false

# 4. Update cluster.yaml to reflect the new primary, then restart manager
```

### Starting the HA monitor daemon

```bash
# As a systemd service:
# [Service]
# ExecStart=/usr/local/bin/mha manager --config /etc/mha/cluster.yaml --log-format json
# Restart=on-failure

./mha manager --config cluster.yaml --log-format json 2>> /var/log/mha.log
```

The manager exits after a successful failover. Update `cluster.yaml` so the new primary is marked as primary, verify the topology, then restart the manager explicitly. `Restart=on-failure` is still useful for crashes, but a successful failover exits cleanly and should not be restarted blindly with stale topology configuration.

---

## 6. Hook events

If `hooks.shell_compat: true` and `hooks.command` is set, mha-go runs `sh -c <command>` on each event, passing context via environment variables.

Hooks are for notification, audit, and compatibility callbacks. VIP/proxy movement is not driven by hooks; configure the primary writer move under [`writer_endpoint`](#7-writer-endpoint-integration). The `failover.writer_switched` event is emitted after the writer endpoint switch step succeeds.

### Common env vars (all events)

| Variable | Description |
|----------|-------------|
| `MHA_EVENT` | Event name (e.g. `failover.start`) |
| `MHA_CLUSTER` | Cluster name |
| `MHA_NODE_ID` | Primary node involved |

### Event catalogue

| Event | When | Additional env vars |
|-------|------|---------------------|
| `monitor.suspect` | Primary probe failures reach threshold | `MHA_PRIMARY`, `MHA_FAILURE_COUNT` |
| `monitor.dead_confirmed` | Primary confirmed dead, failover triggered | `MHA_PRIMARY` |
| `failover.start` | Failover execution begins | `MHA_OLD_PRIMARY`, `MHA_CANDIDATE` |
| `failover.fence` | Old primary fenced (read-only set) | `MHA_FENCED_NODE` |
| `failover.promote` | Candidate promoted to primary | `MHA_NEW_PRIMARY` |
| `failover.writer_switched` | Writer endpoint moved | `MHA_NEW_PRIMARY`, `MHA_OLD_PRIMARY` |
| `failover.complete` | Failover succeeded | `MHA_NEW_PRIMARY`, `MHA_OLD_PRIMARY` |
| `failover.abort` | Failover blocked or failed | `MHA_FAILED_STEP`, `MHA_ERROR` |
| `switchover.start` | Switchover execution begins | `MHA_ORIGINAL_PRIMARY`, `MHA_CANDIDATE` |
| `switchover.complete` | Switchover succeeded | `MHA_NEW_PRIMARY`, `MHA_ORIGINAL_PRIMARY` |
| `switchover.abort` | Switchover failed | `MHA_FAILED_STEP`, `MHA_ERROR` |

### Example hook script

```bash
# hooks.command: /usr/local/bin/mha-notify.sh
#!/bin/bash
case "$MHA_EVENT" in
  failover.complete)
    curl -s -X POST https://alertmanager.internal/api/v1/alerts \
      -d "[{\"labels\":{\"alertname\":\"MHAFailover\",\"cluster\":\"$MHA_CLUSTER\",\"new_primary\":\"$MHA_NEW_PRIMARY\"}}]"
    ;;
  monitor.dead_confirmed)
    echo "$(date) PRIMARY DEAD: cluster=$MHA_CLUSTER node=$MHA_PRIMARY" >> /var/log/mha-events.log
    ;;
esac
```

---

## 7. Writer endpoint integration

When `writer_endpoint.kind` is `vip` or `proxy`, mha-go calls the configured script after promoting the new primary. This step moves the write entry point to the new primary; it is not a complete fencing mechanism by itself. The script receives the context via environment variables:

This is the supported path for VIP movement and proxy writer updates. Do not put the primary VIP move in `hooks.command`; hooks run as lifecycle callbacks and are not the authoritative writer endpoint switch.

| Variable | Description |
|----------|-------------|
| `MHA_CLUSTER` | Cluster name |
| `MHA_WRITER_ENDPOINT_ACTION` | `precheck`, `switch`, or `verify` |
| `MHA_WRITER_ENDPOINT_KIND` | `vip` or `proxy` |
| `MHA_WRITER_ENDPOINT_TARGET` | Value of `writer_endpoint.target` |
| `MHA_NEW_PRIMARY_ID` | New primary node ID |
| `MHA_NEW_PRIMARY_ADDRESS` | New primary `host:port` |
| `MHA_NEW_PRIMARY_HOST` | New primary host only |
| `MHA_NEW_PRIMARY_PORT` | New primary port |
| `MHA_OLD_PRIMARY_ID` | Old primary node ID |
| `MHA_OLD_PRIMARY_ADDRESS` | Old primary `host:port` |
| `MHA_OLD_PRIMARY_HOST` | Old primary host only |
| `MHA_OLD_PRIMARY_PORT` | Old primary port |

The script is called as `sh -c <command>`. A non-zero exit code aborts the operation with an error.

```yaml
writer_endpoint:
  kind: vip
  target: 192.0.2.10
  command: /usr/local/bin/move-vip.sh
  precheck_command: /usr/local/bin/check-vip-move.sh
  verify_command: /usr/local/bin/verify-vip.sh
```

```bash
# move-vip.sh
#!/bin/bash
set -euo pipefail
ip addr del "$MHA_WRITER_ENDPOINT_TARGET/24" dev eth0 2>/dev/null || true
ssh "$MHA_NEW_PRIMARY_HOST" "ip addr add $MHA_WRITER_ENDPOINT_TARGET/24 dev eth0"
arping -U -c 3 -I eth0 "$MHA_WRITER_ENDPOINT_TARGET" || true
```

---

## 8. Fencing model

Fencing prevents the old primary from accepting writes after a failover decision. mha-go currently performs SQL-side read-only fencing when the old primary is reachable. Production deployments should treat this as the first layer, not the entire isolation story.

Recommended order:

1. SQL read-only fence: set `super_read_only=ON` / `read_only=ON` on the old primary when it is reachable.
2. Writer-entry fence: remove the old primary from proxy writer pools or move the VIP away from it.
3. Infrastructure fence: use STONITH, cloud route changes, security groups, or instance shutdown when configured and operationally acceptable.

The writer endpoint switch and fencing have different meanings:

- fencing answers: “can the old primary still accept writes?”
- writer endpoint switch answers: “where should new writes go now?”

For failover, writer endpoint precheck runs before SQL changes, and required fencing must complete before the writer endpoint is switched. If the old primary is completely unreachable, the configured salvage policy and the operator's availability/consistency choice determine whether to continue.

Unreachable ordinary replicas do not block failover. A replica that is already dead during planning is skipped for repoint and logged for later rejoin. The candidate new primary is different: it must be SQL reachable, and if the configured VIP/proxy precheck requires SSH or another host-level access path to the candidate, that precheck must pass before promotion.

---

## 9. Operational history

mha-go does not use SQLite or an embedded state database for history. Runtime `RunStore` data is in-process only and is lost on restart.

For persistent audit history, run with structured logs and redirect stderr to a log file or journald:

```bash
./mha manager --config /etc/mha/cluster.yaml --log-format json 2>> /var/log/mha-go/manager.jsonl
```

Examples:

```bash
journalctl -u mha-manager --output cat | jq 'select(.cluster=="my-cluster")'
jq 'select(.level=="ERROR" or .level=="WARN")' /var/log/mha-go/manager.jsonl
```

There is intentionally no `admin history` command in v1. Keep log retention, rotation, and central collection in the host logging stack.

---

## 10. Salvage policy

When a failover happens, the candidate may be missing transactions that were committed on the old primary but not yet replicated. The salvage policy controls how mha-go handles this situation.

| Policy | Behaviour |
|--------|-----------|
| `strict` | Block at plan time if any missing transactions are detected. The operator must resolve them manually before executing. |
| `salvage-if-possible` (default) | Attempt to catch up the candidate by pointing it at the old primary (or another donor) via GTID. If this fails, abort the failover. |
| `availability-first` | Same as `salvage-if-possible`, but a catch-up failure is recorded as a warning and the failover continues. Prioritises availability over zero data loss. |

The catch-up uses `WAIT_FOR_EXECUTED_GTID_SET()` with the timeout configured in `replication.salvage.timeout` (default `30s`).
