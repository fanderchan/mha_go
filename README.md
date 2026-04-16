# mha-go

[![CI](https://github.com/fanderchan/mha_go/actions/workflows/ci.yml/badge.svg)](https://github.com/fanderchan/mha_go/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/fanderchan/mha_go?display_name=tag)](https://github.com/fanderchan/mha_go/releases)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A Go rewrite of [MySQL MHA](https://github.com/yoshinorim/mha4mysql-manager) (Master High Availability). Automates **failover** and **online switchover** for GTID-based, single-primary MySQL replication topologies.

[中文文档](docs/README_zh.md)

## Features

- **Single binary** — no Perl, no node agent, no external dependencies
- **GTID-native** — built exclusively for GTID replication (no relay-log positioning)
- **Safe by default** — all write operations are dry-run unless explicitly confirmed
- **Online switchover** — graceful primary migration with zero data loss
- **Automatic failover** — monitor daemon detects primary failure and promotes the best candidate
- **GTID salvage** — recovers missing transactions from donors before promotion
- **Pluggable hooks** — shell callbacks on every lifecycle event (alert, VIP move, etc.)
- **Credential safety** — passwords via env vars or files; never hardcoded in config

## Supported MySQL Versions

| Version | Status |
|---------|--------|
| MySQL 8.4.x | Primary target (release-blocking) |
| MySQL 9.7 ER/EA | Forward-compatibility target |

MySQL 5.7, 8.0, and 9.6 are **not** supported. GTID must be enabled on all nodes.

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

-- Minimum privileges for health checks + failover
GRANT RELOAD,
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
MHA_VERSION=v0.1.2
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

Minimal configuration (`cluster.yaml`):

```yaml
name: my-cluster

topology:
  kind: async-single-primary

replication:
  mode: gtid

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
    candidate_priority: 100
    sql:
      user: mha
      password_ref: env:MHA_DB_PASSWORD

  - id: db3
    host: 10.0.0.13
    port: 3306
    version_series: "8.4"
    expected_role: replica
    candidate_priority: 90
    sql:
      user: mha
      password_ref: env:MHA_DB_PASSWORD
```

Set the password via environment variable:

```bash
export MHA_DB_PASSWORD='your-strong-password'
```

### 5. Verify Replication Health

```bash
./mha check-repl --config /etc/mha/cluster.yaml
```

Expected output:

```
Cluster: my-cluster  mode=async-single-primary  primary=db1  nodes=3
  - db1    role=primary health=alive   addr=10.0.0.11:3306   ro=false
  - db2    role=replica health=alive   addr=10.0.0.12:3306   ro=true
         replica: source=db1 io=true sql=true lag=0s autopos=true
  - db3    role=replica health=alive   addr=10.0.0.13:3306   ro=true
         replica: source=db1 io=true sql=true lag=0s autopos=true
Assessment: OK
```

### 6. Use It

```bash
# Online switchover (dry-run first, then for real)
./mha switch --config /etc/mha/cluster.yaml --new-primary db2
./mha switch --config /etc/mha/cluster.yaml --new-primary db2 --dry-run=false

# Failover planning and execution
./mha failover-plan --config /etc/mha/cluster.yaml
./mha failover-execute --config /etc/mha/cluster.yaml --dry-run=false

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

All subcommands accept:

- `--config <file>` — cluster config file (required)
- `--log-level debug|info|warn|error` (default `info`)
- `--log-format text|json` (default `text`)
- `--dry-run` — `switch` and `failover-execute` default to `true`

## Credential Reference

The `sql.password_ref` field supports three schemes:

| Scheme | Example | Notes |
|--------|---------|-------|
| `env:VAR` | `env:MHA_DB_PASSWORD` | Read from environment variable (recommended) |
| `file:/path` | `file:/etc/mha/db.secret` | Read from file; trailing newline stripped |
| `plain:value` | `plain:s3cr3t` | Literal value — **not recommended for production** |

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
Environment=MHA_DB_PASSWORD=your-password-here
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now mha-manager
journalctl -u mha-manager -f
```

> **Note**: The manager exits after a successful failover. `Restart=on-failure` ensures it restarts for the next monitoring cycle after you update the config to reflect the new topology.

## Documentation

| Document | Description |
|----------|-------------|
| [Operations Guide](docs/operations.md) | Full config reference, MySQL prerequisites, all workflows |
| [Architecture Blueprint](docs/mha-go-blueprint.md) | Design decisions and module responsibilities |
| [Deployment Guide](docs/deploy-mha-go.md) | Step-by-step deployment with [dbbot](https://github.com/fanderchan/dbbot) |
| [Testing Guide](docs/testing.md) | Unit, CI, and local MySQL 8.4 integration tests |
| [Example: MySQL 8.4](examples/cluster-8.4.yaml) | Annotated config for a 3-node cluster |

## License

Apache License 2.0 — see [LICENSE](LICENSE) for details.
