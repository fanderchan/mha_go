# mha-go

[![CI](https://github.com/fanderchan/mha_go/actions/workflows/ci.yml/badge.svg)](https://github.com/fanderchan/mha_go/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/fanderchan/mha_go?display_name=tag)](https://github.com/fanderchan/mha_go/releases)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A Go rewrite of [MySQL MHA](https://github.com/yoshinorim/mha4mysql-manager) (Master High Availability). Automates **failover** and **online switchover** for GTID-based, single-primary MySQL replication topologies.

[中文文档](docs/README_zh.md)

## Feature Comparison With Perl MHA

### Operating Model

| Topic | Perl MHA 0.58 | mha-go |
|-------|---------------|--------|
| Implementation language | Perl | Go |
| Packaging model | Manager plus node tools | Single manager binary; agent/SSH paths are optional extension points |
| Primary compatibility goal | Broad historical MySQL/MariaDB-era deployments | Modern MySQL GTID single-primary replication |
| Supported MySQL baseline | Legacy versions, including non-GTID paths | MySQL 8.4.x release baseline; MySQL 9.7 ER/EA forward track |
| Replication positioning | File/position and GTID-era logic | GTID-only; no relay-log position model |
| State and history | Script output and manager logs | In-process run state plus structured log-file audit trail |
| Writer endpoint model | Typically handled by hook scripts such as VIP failover scripts | Dedicated `writer_endpoint` step with optional precheck and verify commands |
| Hook role | Operational scripts often carry critical switch behavior | Notification, audit, and compatibility callbacks; not the main VIP/proxy switch path |
| Fencing model | Mostly external scripts and operational convention | Explicit fencing steps with required/optional semantics |
| Execution model | Commands generally execute when invoked | `switch` and `failover-execute` execute by default; add `--dry-run` to preview |
| Persistence policy | No embedded state DB | No SQLite or embedded DB; persistent history belongs in logs |
| Controller HA model | Single active manager in normal operation | Single active manager by default, matching the Perl MHA operating model |

### Capability Matrix

Legend: `✓` supported, `-` not supported by design, `Partial` implemented for the common path but not feature-complete.

| Area | Capability | Perl MHA 0.58 | mha-go |
|------|------------|---------------|--------|
| Deployment | Single self-contained binary | - | ✓ |
| Deployment | No Perl runtime dependency | - | ✓ |
| Deployment | No mandatory node package on every MySQL host | - | ✓ |
| Version scope | Legacy MySQL support | ✓ | - |
| Version scope | Explicit MySQL 8.4 release baseline | - | ✓ |
| Version scope | MySQL 9.7 ER/EA forward-compatibility track | - | Partial |
| Replication model | GTID-only safety model | - | ✓ |
| Replication model | Non-GTID / file-position failover | ✓ | - |
| Topology check | One-shot replication health check | ✓ | ✓ |
| Topology check | Capability-driven SQL discovery | - | ✓ |
| Failover | Automatic primary failure detection | ✓ | ✓ |
| Failover | Candidate priority / no-master controls | ✓ | ✓ |
| Failover | Typed, ordered failover plan before execution | - | ✓ |
| Failover | Explicit dry-run for write operations | - | ✓ |
| Recovery | Relay-log / binlog recovery through SSH node tools | ✓ | - |
| Recovery | GTID catch-up from SQL-accessible donors | - | ✓ |
| Recovery | SSH/node-tool binlog salvage for unreachable SQL paths | ✓ | Partial |
| Switchover | Online primary switchover | ✓ | ✓ |
| Writer endpoint | VIP/proxy switch by external command | ✓ | ✓ |
| Writer endpoint | Precheck before promotion | - | ✓ |
| Writer endpoint | Post-switch verify command | - | ✓ |
| Fencing | SQL read-only fence | Partial | ✓ |
| Fencing | Configurable required/optional fencing steps | - | ✓ |
| Hooks | Lifecycle shell callbacks | ✓ | ✓ |
| Hooks | Hooks used as the main VIP move path | ✓ | - |
| Observability | Structured logs for audit/history | - | ✓ |
| Secrets | Env/file/plain credential references | - | ✓ |
| Testing | Go unit tests and CI static builds | - | ✓ |

## Supported MySQL Versions

| Version | Status |
|---------|--------|
| MySQL 8.4.x | Primary target (release-blocking) |
| MySQL 9.7 ER/EA | Forward-compatibility target |

MySQL 5.7, 8.0, and 9.6 are **not** supported. GTID must be enabled on all nodes.
For MySQL 9.7.0, use the same configuration shape and set each node's
`version_series` to `"9.7"`.

## Quick Start

### 1. Prerequisites

**On all MySQL nodes**, ensure GTID is enabled (`my.cnf`):

```ini
[mysqld]
gtid_mode                = ON
enforce_gtid_consistency = ON
log_bin                  = ON
log_replica_updates      = ON
```

Verify:

```sql
SHOW VARIABLES WHERE Variable_name IN ('gtid_mode', 'enforce_gtid_consistency');
```

### 2. Create the MHA MySQL Account

Run on the **primary** (replicates to all replicas via GTID):

```sql
CREATE USER IF NOT EXISTS 'mha'@'<your-subnet>%'
  IDENTIFIED BY '<strong-password>';

-- Operational privileges for health checks, switchover, and failover
GRANT SELECT,
      RELOAD,
      PROCESS,
      SUPER,
      REPLICATION CLIENT,
      REPLICATION SLAVE,
      REPLICATION_SLAVE_ADMIN,
      SYSTEM_VARIABLES_ADMIN,
      SESSION_VARIABLES_ADMIN
  ON *.* TO 'mha'@'<your-subnet>%';

FLUSH PRIVILEGES;
```

> **Tip**: Replace `<your-subnet>` with your network range (e.g. `192.168.1.%` or `10.0.%`).

### 3. Install

Prebuilt binaries do not require Go. Building from source requires Go 1.25+.

Download a prebuilt Linux binary:

```bash
MHA_VERSION=v0.1.4
case "$(uname -m)" in
  x86_64) ASSET="mha_${MHA_VERSION}_linux_amd64" ;;
  aarch64|arm64) ASSET="mha_${MHA_VERSION}_linux_arm64" ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

curl -fL -o mha \
  "https://github.com/fanderchan/mha_go/releases/download/${MHA_VERSION}/${ASSET}"
curl -fL -o SHA256SUMS \
  "https://github.com/fanderchan/mha_go/releases/download/${MHA_VERSION}/SHA256SUMS"
grep " ${ASSET}$" SHA256SUMS | sha256sum -c -
chmod +x mha
```

Or build from source:

```bash
git clone git@github.com:fanderchan/mha_go.git
cd mha_go

# Dynamic build
go build -o mha ./cmd/mha

# Static build (recommended for deployment)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags="-extldflags=-static" -o mha ./cmd/mha
```

### 4. Configure

Copy and edit the example config:

```bash
cp examples/cluster-8.4.yaml /etc/mha/cluster.yaml
```

`examples/cluster-8.4.yaml` is the starter template. It intentionally leaves out
advanced sections such as VIP/proxy switching, custom fencing, hooks, and SSH
binlog salvage. Use [examples/cluster-8.4.full.yaml](examples/cluster-8.4.full.yaml)
as the field-by-field reference when enabling those features.
For a MySQL 9.7.0 cluster, keep the same template and change every
`version_series: "8.4"` entry to `version_series: "9.7"`.

Minimal configuration (`cluster.yaml`):

```yaml
name: my-cluster

topology:
  kind: mysql-replication-single-primary

replication:
  mode: gtid
  semi_sync:
    policy: disabled

nodes:
  - id: db1
    host: 10.0.0.11
    port: 3306
    version_series: "8.4"  # use "9.7" for MySQL 9.7.0
    expected_role: primary
    sql:
      user: mha
      password_ref: file:/etc/mha/secrets/admin-password
      replication_user: repl
      replication_password_ref: file:/etc/mha/secrets/repl-password

  - id: db2
    host: 10.0.0.12
    port: 3306
    version_series: "8.4"  # use "9.7" for MySQL 9.7.0
    expected_role: replica
    candidate_priority: 100
    sql:
      user: mha
      password_ref: file:/etc/mha/secrets/admin-password
      replication_user: repl
      replication_password_ref: file:/etc/mha/secrets/repl-password

  - id: db3
    host: 10.0.0.13
    port: 3306
    version_series: "8.4"  # use "9.7" for MySQL 9.7.0
    expected_role: replica
    candidate_priority: 90
    sql:
      user: mha
      password_ref: file:/etc/mha/secrets/admin-password
      replication_user: repl
      replication_password_ref: file:/etc/mha/secrets/repl-password
```

Create the secret files referenced by the config. Keep credentials out of the
cluster YAML and out of systemd `Environment=` lines:

```bash
install -d -m 0700 /etc/mha/secrets
printf '%s\n' 'your-admin-password' > /etc/mha/secrets/admin-password
printf '%s\n' 'your-replication-password' > /etc/mha/secrets/repl-password
chmod 0600 /etc/mha/secrets/admin-password /etc/mha/secrets/repl-password
chown root:root /etc/mha/secrets/admin-password /etc/mha/secrets/repl-password
```

### 5. Verify Replication Health

```bash
./mha check-repl --config /etc/mha/cluster.yaml
```

Expected output:

```
Cluster: my-cluster  mode=mysql-replication-single-primary  primary=db1  nodes=3
  - db1    role=primary health=alive   addr=10.0.0.11:3306   ro=false sro=false
  - db2    role=replica health=alive   addr=10.0.0.12:3306   ro=true sro=true
         replica: source=db1 io=true sql=true lag=0s autopos=true
  - db3    role=replica health=alive   addr=10.0.0.13:3306   ro=true sro=true
         replica: source=db1 io=true sql=true lag=0s autopos=true
Assessment: OK
```

### 6. Use It

```bash
# Online switchover (preview first, then execute)
./mha switch --config /etc/mha/cluster.yaml --new-primary db2 --dry-run
./mha switch --config /etc/mha/cluster.yaml --new-primary db2

# Failover planning and execution
./mha failover-plan --config /etc/mha/cluster.yaml
./mha failover-execute --config /etc/mha/cluster.yaml

# Start the HA monitor daemon
./mha manager --config /etc/mha/cluster.yaml
```

## Subcommands

| Command | Description |
|---------|-------------|
| `check-repl` | One-shot topology and replication health check |
| `manager` | Long-running HA monitor; triggers automatic failover on primary death |
| `switch` | Online (graceful) switchover to a specified or best-available replica |
| `failover-plan` | Build and display a failover plan without executing |
| `failover-execute` | Build and execute a failover plan |

Operational subcommands (`check-repl`, `manager`, `switch`, `failover-plan`, and `failover-execute`) accept:

- `--config <file>` — cluster config file (required)
- `--discoverer sql|static` (default `sql`)
- `--log-level debug|info|warn|error` (default `info`)
- `--log-format text|json` (default `text`)

`manager`, `switch`, and `failover-execute` also accept `--dry-run` for preview/no-write mode. `switch` and `failover-execute` execute by default when `--dry-run` is omitted.

## Credential Reference

The `sql.password_ref` and `sql.replication_password_ref` fields support three schemes:

| Scheme | Example | Notes |
|--------|---------|-------|
| `file:/path` | `file:/etc/mha/secrets/admin-password` | Recommended for production on hosts/VMs. Protect the file with owner and mode, for example `root:root 0600`. Trailing newline is stripped. |
| `env:VAR` | `env:MHA_ADMIN_PASSWORD` | Useful for short-lived tests, containers, or external secret injection. Avoid putting secrets in shell history or systemd `Environment=` lines. |
| `plain:value` | `plain:s3cr3t` | Literal value. Use only for local demos/tests; do not use in production. |

## Production Deployment

### systemd

```ini
# /etc/systemd/system/mha-manager.service
[Unit]
Description=MHA Go Manager
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/mha manager --config /etc/mha/cluster.yaml --log-format json
Restart=on-failure
RestartSec=5s
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

The service above assumes `password_ref: file:/etc/mha/secrets/...` in the
cluster config. Avoid storing SQL passwords in `Environment=` because process
environments and unit files are easier to expose during troubleshooting.

```bash
systemctl daemon-reload
systemctl enable --now mha-manager
journalctl -u mha-manager -f
```

> **Note**: The manager exits after a successful failover. Update the config to reflect the new topology, verify it with `check-repl`, then restart the service explicitly. `Restart=on-failure` only covers crashes or non-zero exits.

## Documentation

| Document | Description |
|----------|-------------|
| [Operations Guide](docs/operations.md) | Full config reference, MySQL prerequisites, all workflows |
| [Architecture Blueprint](docs/mha-go-blueprint.md) | Design decisions and module responsibilities |
| [Deployment Guide](docs/deploy-mha-go.md) | Step-by-step deployment with [dbbot](https://github.com/fanderchan/dbbot) |
| [Testing Guide](docs/testing.md) | Unit, CI, local MySQL 8.4 integration tests, and MySQL 9.7 validation notes |
| [Changelog](CHANGELOG.md) | Release history |
| [Example: MySQL 8.4 starter](examples/cluster-8.4.yaml) | Small starter config for a 3-node cluster |
| [Example: MySQL 8.4 full reference](examples/cluster-8.4.full.yaml) | Verbose field-by-field annotated config |

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
