# mha-go Deployment Guide

[中文](deploy-mha-go_zh.md)

For a 1-primary 2-replica MySQL 8.4 GTID topology deployed with dbbot's `master_slave.yml` playbook.

## Prerequisites

| Item | Requirement |
|---|---|
| MySQL version | 8.4.x (GTID ON, `gtid_mode=ON`, `enforce_gtid_consistency=ON`) |
| Topology | 1 primary + 2 replicas deployed by `master_slave.yml` (`master_slave_finish.flag` present) |
| Account | `admin` account with `SUPER`/`SYSTEM_VARIABLES_ADMIN` privileges for granting |
| Network | Control host (manager_ip) can reach all three nodes on TCP 3306 |
| OS | x86_64 Linux; glibc ≥ 2.17 (statically compiled binary, no extra dependencies) |

## Architecture

```
   ┌─────────────────────────────────┐
   │  192.168.161.11  (db1 / manager)│  ← primary + control host
   │  mysql3306  running             │
   │  /usr/local/bin/mha             │
   │  /etc/mha/cluster.yaml          │
   └────────────┬────────────────────┘
                │  replication (GTID)
       ┌────────┴────────┐
       ▼                 ▼
192.168.161.12         192.168.161.13
  (db2 / replica)      (db3 / replica)
```

> The manager process only runs on the `manager_ip` node, so the `mha` binary needs to be installed there.

## Step 1: Build the mha binary

On a build host with Go 1.25+:

```bash
git clone git@github.com:fanderchan/mha_go.git
cd mha_go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags="-extldflags=-static" -o mha ./cmd/mha
```

The resulting `mha` is statically linked and can be dropped onto any x86_64 Linux host.

## Step 2: Deploy the binary

```bash
scp mha root@<manager_ip>:/usr/local/bin/mha
ssh root@<manager_ip> "chmod +x /usr/local/bin/mha && mha --help"
```

## Step 3: Create the mha MySQL account

Run on the **primary** (will be replicated to all replicas via GTID):

```sql
CREATE USER IF NOT EXISTS 'mha'@'<subnet>%'
  IDENTIFIED BY '<password>';

GRANT SELECT, RELOAD, PROCESS, SUPER,
      REPLICATION SLAVE, REPLICATION CLIENT
  ON *.* TO 'mha'@'<subnet>%';

FLUSH PRIVILEGES;
```

dbbot defaults:
- Username: `mha`
- Password: `Dbbot_mha@8888`
- Host range: `192.168.161.%` (adjust to your subnet)
- Replication source account created by `master_slave.yml`: `repl` / `Dbbot_repl@8888` on `192.168.161.%`

Keep the management account and replication account separate in `cluster.yaml`.
`mha` is used to inspect and orchestrate nodes; `repl` is written into
`SOURCE_USER` / `SOURCE_PASSWORD` when mha-go repoints replicas.

Verify it replicated to the replicas:

```bash
mysql -h 192.168.161.12 -u admin -p -e "SHOW GRANTS FOR 'mha'@'192.168.161.%';"
```

## Step 4: Write the cluster config

Create `/etc/mha/cluster.yaml` (example mirrors the test environment):

```yaml
name: <cluster-name>

topology:
  kind: async-single-primary
  single_writer: true
  allow_cascading_replicas: false

controller:
  id: manager-1
  lease:
    backend: local-memory
    ttl: 15s
  monitor:
    interval: 2s
    failure_threshold: 3
    reconfirm_timeout: 5s

replication:
  mode: gtid
  semi_sync:
    policy: preferred          # takes effect when the plugin is loaded; otherwise falls back to async
    wait_for_replica_count: 1
    timeout: 5s
  salvage:
    policy: salvage-if-possible
    timeout: 30s

writer_endpoint:
  kind: none                   # switch to vip or proxy in production

nodes:
  - id: db1
    host: 192.168.161.11
    port: 3306
    version_series: "8.4"
    expected_role: primary
    candidate_priority: 0
    sql:
      user: mha
      password_ref: plain:Dbbot_mha@8888
      replication_user: repl
      replication_password_ref: plain:Dbbot_repl@8888

  - id: db2
    host: 192.168.161.12
    port: 3306
    version_series: "8.4"
    expected_role: replica
    candidate_priority: 100    # highest priority; promoted first
    sql:
      user: mha
      password_ref: plain:Dbbot_mha@8888
      replication_user: repl
      replication_password_ref: plain:Dbbot_repl@8888

  - id: db3
    host: 192.168.161.13
    port: 3306
    version_series: "8.4"
    expected_role: replica
    candidate_priority: 90
    sql:
      user: mha
      password_ref: plain:Dbbot_mha@8888
      replication_user: repl
      replication_password_ref: plain:Dbbot_repl@8888
```

`password_ref` supports three forms:
- `plain:<value>` — plaintext (testing only)
- `env:<VAR>` — read from environment variable
- `file:</path/to/file>` — read from file (recommended for production)

## Step 5: Verify the topology

```bash
mha check-repl --config /etc/mha/cluster.yaml
```

Expected output:

```
Cluster: <name>  mode=async-single-primary  primary=db1  nodes=3
  - db1    role=primary health=alive   addr=192.168.161.11:3306   ro=false sro=false
  - db2    role=replica health=alive   addr=192.168.161.12:3306   ro=true sro=true
         replica: source=db1 io=true sql=true lag=0s autopos=true
  - db3    role=replica health=alive   addr=192.168.161.13:3306   ro=true sro=true
         replica: source=db1 io=true sql=true lag=0s autopos=true
Assessment: OK
```

## Step 6: Online switchover

Online switchover doesn't interrupt the workload and is appropriate for planned maintenance (host drain, upgrade, etc.):

```bash
# Dry-run (no MySQL writes)
mha switch --config /etc/mha/cluster.yaml --new-primary db2 --dry-run

# Execute for real
mha switch --config /etc/mha/cluster.yaml --new-primary db2
```

## Step 7: Failover plan (emergency rehearsal)

```bash
# Inspect the plan and any blocking reasons
mha failover-plan --config /etc/mha/cluster.yaml

# Execute (only after the primary is confirmed dead)
mha failover-execute --config /etc/mha/cluster.yaml
```

## Long-running monitor mode

```bash
# Foreground (for testing)
mha manager --config /etc/mha/cluster.yaml

# systemd-managed (production)
systemctl start mha-manager
systemctl enable mha-manager
```

See the systemd section below.

## systemd unit file

`/etc/systemd/system/mha-manager.service`:

```ini
[Unit]
Description=dbbot MHA Go Manager
After=network.target mysqld.service

[Service]
Type=simple
User=mysql
ExecStart=/usr/local/bin/mha manager --config /etc/mha/cluster.yaml
Restart=on-failure
RestartSec=5s
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

The manager exits normally after a successful failover. Update `/etc/mha/cluster.yaml` with the new primary/replica roles, verify the new topology with `mha check-repl --config /etc/mha/cluster.yaml`, and then explicitly run `systemctl restart mha-manager`. `Restart=on-failure` only covers crashes or non-zero exits — it should not be used to automatically restart the monitor while the config still points at the old primary.

## Troubleshooting

### mha user is missing RELOAD/SUPER

Missing privileges on a replica usually means the GRANT never replicated because the replication thread was stopped. Steps:

```bash
# 1. Check replica threads
mysql -h <replica> -u admin -p -e "SHOW REPLICA STATUS\G" | grep Running

# 2. If IO/SQL thread is stopped, start it
mysql -h <replica> -u admin -p -e "START REPLICA;"

# 3. Wait for GTID to catch up, then re-check grants
mysql -h <replica> -u admin -p -e "SHOW GRANTS FOR 'mha'@'...%';"
```

### glibc version mismatch

Symptom: `/lib64/libc.so.6: version 'GLIBC_2.32' not found`

Cause: the binary was dynamically linked on a host with a newer glibc.

Fix: rebuild with `CGO_ENABLED=0` (see Step 1).

### Replication threads stop after switchover

During an online switchover, `RESET REPLICA ALL` + `CHANGE REPLICATION SOURCE TO` can stall if the old primary still holds locks. Recover with:

```bash
mysql -h <node> -u admin -p -e "SET GLOBAL super_read_only=0; START REPLICA;"
```

## References

- Architecture blueprint: [mha-go-blueprint.md](mha-go-blueprint.md)
- Operations guide: [operations.md](operations.md)
- Example config: [../examples/cluster-test.yaml](../examples/cluster-test.yaml)
