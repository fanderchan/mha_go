#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/../../.." && pwd)"
COMPOSE_FILE="$SCRIPT_DIR/compose.yaml"

PROJECT="${MHA_IT_PROJECT:-mha-go-it-$(date +%s)-$$}"
MYSQL_IMAGE="${MYSQL_IMAGE:-mysql:8.4}"
MYSQL_ROOT_PASSWORD="${MYSQL_ROOT_PASSWORD:-rootpass}"
MHA_IT_PASSWORD="${MHA_IT_PASSWORD:-mha_it_pass_123}"
MHA_IT_KEEP="${MHA_IT_KEEP:-0}"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/mha-go-it.XXXXXX")"
CONFIG_FILE="$WORK_DIR/cluster.yaml"
MHA_BIN="${MHA_IT_BIN:-$WORK_DIR/mha}"

export MYSQL_IMAGE MYSQL_ROOT_PASSWORD

COMPOSE=(docker compose -p "$PROJECT" -f "$COMPOSE_FILE")

cleanup() {
  local status=$?
  if [[ "$MHA_IT_KEEP" == "1" ]]; then
    echo "keeping integration environment: project=$PROJECT work_dir=$WORK_DIR" >&2
  else
    "${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
    rm -rf "$WORK_DIR"
  fi
  exit "$status"
}
trap cleanup EXIT

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "missing required command: $1" >&2
    exit 127
  fi
}

mysql_exec() {
  local service="$1"
  local sql="$2"
  "${COMPOSE[@]}" exec -T -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" "$service" \
    mysql -uroot --batch --raw --execute "$sql"
}

mysql_scalar() {
  local service="$1"
  local sql="$2"
  "${COMPOSE[@]}" exec -T -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" "$service" \
    mysql -uroot --batch --raw --skip-column-names --execute "$sql" | tail -n 1
}

wait_for_mysql() {
  local service="$1"
  for _ in {1..90}; do
    if "${COMPOSE[@]}" exec -T -e MYSQL_PWD="$MYSQL_ROOT_PASSWORD" "$service" \
      mysql -uroot --batch --raw --skip-column-names --execute "SELECT 1" >/dev/null 2>&1; then
      return 0
    fi
    sleep 2
  done
  echo "timed out waiting for MySQL service: $service" >&2
  return 1
}

wait_for_replica() {
  local service="$1"
  local status=""
  local row_count=""
  for _ in {1..90}; do
    status="$(mysql_exec "$service" "SHOW REPLICA STATUS\\G" 2>/dev/null || true)"
    row_count="$(mysql_scalar "$service" "SELECT COUNT(*) FROM mha_it.t" 2>/dev/null || true)"
    if grep -q "Replica_IO_Running: Yes" <<<"$status" &&
      grep -q "Replica_SQL_Running: Yes" <<<"$status" &&
      [[ "$row_count" == "2" ]]; then
      return 0
    fi
    sleep 2
  done

  echo "timed out waiting for replica service: $service" >&2
  echo "$status" >&2
  return 1
}

host_port() {
  "${COMPOSE[@]}" port "$1" 3306 | sed -E 's/.*:([0-9]+)$/\1/'
}

sql_escape() {
  sed "s/'/''/g" <<<"$1"
}

run_mha() {
  MHA_IT_PASSWORD="$MHA_IT_PASSWORD" "$MHA_BIN" "$@"
}

require_cmd docker
require_cmd go

if ! docker info >/dev/null 2>&1; then
  echo "docker daemon is not reachable" >&2
  exit 1
fi

cd "$REPO_ROOT"

if [[ -z "${MHA_IT_BIN:-}" ]]; then
  go build -o "$MHA_BIN" ./cmd/mha
fi

echo "starting MySQL 8.4 integration topology: project=$PROJECT image=$MYSQL_IMAGE"
"${COMPOSE[@]}" up -d

wait_for_mysql db1
wait_for_mysql db2
wait_for_mysql db3

MHA_PASS_SQL="$(sql_escape "$MHA_IT_PASSWORD")"
mysql_exec db1 "
CREATE USER IF NOT EXISTS 'mha'@'%' IDENTIFIED BY '$MHA_PASS_SQL';
GRANT REPLICATION CLIENT,
      REPLICATION SLAVE,
      REPLICATION_SLAVE_ADMIN,
      SYSTEM_VARIABLES_ADMIN,
      SESSION_VARIABLES_ADMIN
  ON *.* TO 'mha'@'%';
CREATE DATABASE IF NOT EXISTS mha_it;
CREATE TABLE IF NOT EXISTS mha_it.t (
  id INT PRIMARY KEY,
  value VARCHAR(32) NOT NULL
);
INSERT INTO mha_it.t VALUES (1, 'primary'), (2, 'replicated');
FLUSH PRIVILEGES;
"

for replica in db2 db3; do
  mysql_exec "$replica" "
CHANGE REPLICATION SOURCE TO
  SOURCE_HOST='db1',
  SOURCE_PORT=3306,
  SOURCE_USER='mha',
  SOURCE_PASSWORD='$MHA_PASS_SQL',
  SOURCE_AUTO_POSITION=1,
  GET_SOURCE_PUBLIC_KEY=1;
START REPLICA;
SET GLOBAL read_only = ON;
SET GLOBAL super_read_only = ON;
"
done

wait_for_replica db2
wait_for_replica db3

DB1_PORT="$(host_port db1)"
DB2_PORT="$(host_port db2)"
DB3_PORT="$(host_port db3)"

cat >"$CONFIG_FILE" <<YAML
name: mysql84-integration

topology:
  kind: async-single-primary
  single_writer: true
  allow_cascading_replicas: false

controller:
  id: integration-manager
  lease:
    backend: local-memory
    ttl: 15s
  monitor:
    interval: 1s
    failure_threshold: 3
    reconfirm_timeout: 3s

replication:
  mode: gtid
  semi_sync:
    policy: preferred
    wait_for_replica_count: 0
    timeout: 5s
  salvage:
    policy: salvage-if-possible
    timeout: 30s

writer_endpoint:
  kind: none

nodes:
  - id: db1
    host: 127.0.0.1
    port: $DB1_PORT
    version_series: "8.4"
    expected_role: primary
    candidate_priority: 0
    sql:
      user: mha
      password_ref: env:MHA_IT_PASSWORD

  - id: db2
    host: 127.0.0.1
    port: $DB2_PORT
    version_series: "8.4"
    expected_role: replica
    candidate_priority: 100
    sql:
      user: mha
      password_ref: env:MHA_IT_PASSWORD

  - id: db3
    host: 127.0.0.1
    port: $DB3_PORT
    version_series: "8.4"
    expected_role: replica
    candidate_priority: 90
    sql:
      user: mha
      password_ref: env:MHA_IT_PASSWORD
YAML

echo "running check-repl"
run_mha check-repl --config "$CONFIG_FILE"

echo "running switchover dry-run"
run_mha switch --config "$CONFIG_FILE" --new-primary db2

echo "running failover-plan with live primary"
run_mha failover-plan --config "$CONFIG_FILE" --candidate db2

echo "asserting failover-execute dry-run is blocked while primary is alive"
set +e
run_mha failover-execute --config "$CONFIG_FILE" --candidate db2
failover_rc=$?
set -e
if [[ "$failover_rc" -eq 0 ]]; then
  echo "expected failover-execute to be blocked while primary is alive" >&2
  exit 1
fi
if [[ "$failover_rc" -ne 1 ]]; then
  echo "unexpected failover-execute exit code: $failover_rc" >&2
  exit "$failover_rc"
fi

echo "MySQL 8.4 integration test passed"
