# Testing Guide

[中文](testing_zh.md)

This project has three test layers:

- Unit and package tests for controller, topology, replication, state, hooks, and config behavior.
- GitHub Actions CI for formatting, module consistency, `go vet`, unit tests, and static Linux builds.
- A local MySQL 8.4 integration smoke test that starts a GTID single-primary topology in Docker.
- A future MySQL 9.7 ER/EA validation track, kept in the test blueprint until a reliable environment is available.

## Local Unit Checks

Run the same checks used by CI:

```bash
gofmt -l .
go mod tidy
git diff --exit-code -- go.mod go.sum
go vet ./...
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags="-extldflags=-static" -o /tmp/mha ./cmd/mha
```

## MySQL 8.4 Integration Test

The integration test uses Docker Compose and the official `mysql:8.4` image. It creates:

- `db1`: primary
- `db2`: replica, highest promotion priority
- `db3`: replica

The script enables GTID, configures replication with `SOURCE_AUTO_POSITION=1`, creates the `mha` SQL account on the primary, waits for both replicas to apply seed data, then runs:

- `mha check-repl`
- `mha switch --new-primary db2 --dry-run` in dry-run mode
- `mha switch --new-primary db2` against the live Docker topology
- a post-switchover `mha check-repl`
- `mha failover-plan --candidate db3`
- `mha failover-execute --candidate db3 --dry-run` and asserts it is blocked while the new primary is still alive
- stops `db2`, the current primary after switchover
- `mha failover-plan --candidate db3`
- `mha failover-execute --candidate db3`
- a post-failover write on `db3` and replication check on `db1`
- restarts recovered old primary `db2`, rejoins it to `db3` with GTID auto-position, and verifies the full three-node topology again

The `mha` binary is executed inside the Docker network, so the same node addresses are valid for both SQL inspection and `CHANGE REPLICATION SOURCE TO`.

The generated Docker config uses `salvage.policy: availability-first` because the test intentionally stops the current primary and the disposable containers do not provide an SSH binlog salvage path into a stopped old-primary host. Production examples keep the safer `salvage-if-possible` default unless SSH/agent salvage is configured.

Run it from the repository root:

```bash
./test/integration/mysql84/run.sh
```

Useful environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `MYSQL_IMAGE` | `mysql:8.4` | MySQL image to test. Keep this on 8.4.x for the release-blocking matrix. |
| `MHA_IT_RUNNER_IMAGE` | value of `MYSQL_IMAGE` | Image used to run the static `mha` binary inside the Docker network. |
| `MYSQL_ROOT_PASSWORD` | `rootpass` | Root password inside the disposable containers. |
| `MHA_IT_PASSWORD` | `mha_it_pass_123` | Password for the replicated `mha` SQL account. |
| `MHA_IT_BIN` | built into a temp directory | Existing Linux amd64 `mha` binary to test instead of building one. |
| `MHA_IT_KEEP` | `0` | Set to `1` to keep containers and generated config for debugging. |
| `MHA_IT_PROJECT` | generated | Docker Compose project name. |

When `MHA_IT_KEEP=1`, the script prints the Docker Compose project name and temp work directory before exiting.

## Coverage Matrix

Use this matrix to decide what the Docker test already proves and what still needs manual validation before a production-style release.

Legend:

- `Covered`: exercised by `test/integration/mysql84/run.sh` against live MySQL containers.
- `Unit`: covered by Go unit/package tests only.
- `Manual`: not covered by the current automated Docker flow.

| Scenario | Docker 8.4 coverage | Other automated coverage | Manual follow-up |
|----------|---------------------|--------------------------|------------------|
| Build static `mha` binary | Covered | CI build | None for basic Linux amd64 build. |
| Start 3-node MySQL 8.4 GTID topology | Covered | - | Repeat on real hosts if validating packaging/network/firewall. |
| Configure GTID auto-position replication | Covered | - | Verify production account/privilege model separately. |
| `mha check-repl` SQL discovery and assessment | Covered | Unit tests for topology assessment/discovery mapping | Run against every real target topology before maintenance. |
| Online switchover dry-run | Covered | Executor unit tests | None for basic no-endpoint switchover. |
| Online switchover execution, no writer endpoint | Covered | Switchover controller/executor/verify unit tests | Repeat with production workload characteristics. |
| Candidate catch-up and post-switchover replication | Covered | GTID set unit tests | Add lag/long transaction cases manually. |
| Failover plan while primary is still alive | Covered | Failover controller unit tests | None for the basic blocking gate. |
| Failover execution while primary is still alive is blocked | Covered | Failover executor unit tests | None for the basic blocking gate. |
| Primary stopped, real failover to surviving replica | Covered | Failover controller/executor/verify unit tests | Repeat with production fencing and endpoint configuration. |
| Old primary rejoin after recovery | Covered by direct SQL in the script | - | Manual runbook remains required; there is no `mha rejoin` command. |
| `availability-first` failover mechanics | Covered | Executor unit test for best-effort salvage failure | Test consistency policy choice with real business data expectations. |
| `salvage-if-possible` with SQL-accessible donor | Manual | SQL salvager and GTID unit tests | Create a missing-GTID gap and confirm donor catch-up succeeds/fails as expected. |
| `strict` salvage policy | Manual | Failover planning unit tests | Confirm strict mode blocks the exact operational cases you expect. |
| SSH binlog salvage from SQL-dead old primary | Manual | SSH command-building unit tests | Configure SSH, local binlog paths, `mysqlbinlog`, manager-side `mysql`, and known_hosts. |
| Semi-sync enabled and healthy | Manual | Assessment logic unit coverage | Load semi-sync plugins and validate preferred/required policy behavior. |
| Semi-sync degraded to async | Partially covered as a warning only | Assessment logic unit coverage | Manually create a degraded async window and validate chosen salvage policy. |
| Writer endpoint `vip`/`proxy` precheck/switch/verify | Manual | Writer endpoint command unit tests | Validate real VIP/proxy scripts, idempotency, and rollback behavior. |
| Required/optional external fencing steps | Manual | Fencing coordinator unit tests | Validate STONITH/cloud/proxy commands and required failure blocking. |
| SQL read-only fencing during failover when old primary is reachable | Manual | SQL admin and fencing unit tests | Exercise split-brain-adjacent cases where SQL is reachable but failover is still required. |
| Manager monitor loop automatic failover | Manual | Monitor state-machine unit tests | Run `mha manager`, kill/isolate primary, and confirm automatic handoff. |
| Manager isolated from primary by network | Manual | Monitor state-machine unit tests | Use firewall/network namespace rules; Docker stop is not equivalent. |
| Replica lag / uneven lag / lagging candidate selection | Manual | Candidate scoring unit tests | Inject lag and verify candidate ranking plus blocking behavior. |
| Candidate cannot be promoted / mid-step failure | Manual | Executor failure unit tests | Force SQL privilege or command failures and validate abort state/logs. |
| Hook scripts with real notification systems | Manual | Shell dispatcher unit tests | Confirm side effects, failure handling, and dry-run expectations. |
| MySQL 9.7 ER/EA | Manual | Version normalization unit tests | Run the same scenario list once a usable 9.7 environment exists. |

## Manual Test Case Template

For each manual case, record:

1. Scope: MySQL version, topology, semi-sync setting, salvage policy, writer endpoint, and fencing configuration.
2. Baseline: `mha check-repl --config <file>`, `SHOW REPLICA STATUS\G`, row counts or application-level consistency markers, and current writer endpoint target.
3. Dry-run: the exact `mha switch` or `mha failover-execute` command and the plan output.
4. Fault or action: the exact failure injection or maintenance action, including timestamps.
5. Execution: command output, exit code, structured logs, and hook/script outputs.
6. Verification: new primary writability, all expected replicas pointing at the new primary with GTID auto-position, old-primary state, writer endpoint target, and application write/read checks.
7. Cleanup: rejoin or rebuild steps, final `check-repl`, and any manual data reconciliation.

## MySQL 9.7 ER/EA Validation Plan

MySQL 9.7 ER/EA is a forward-compatibility target, but it is not a current release blocker without a stable test environment.

When a 9.7 environment is available, validate the same scenarios as the 8.4 integration test first:

- `check-repl`
- dry-run and real switchover
- blocked failover while the primary is alive
- real failover after primary stop
- old-primary rejoin with GTID auto-position

Add 9.7-specific checks only through capability detection. Do not add version branches that weaken the 8.4 release baseline.

## CI

CI is defined in `.github/workflows/ci.yml` and runs on every push to `main` and every pull request. Release builds are defined in `.github/workflows/release.yml` and run for tags matching `v*`.

The Docker integration test is intentionally manual and is defined in `.github/workflows/integration.yml`. Run it from the GitHub Actions UI when validating MySQL behavior before a release.

## GitHub Repository Settings

This environment can push over SSH, but it does not currently have GitHub API credentials for repository settings. Configure these once in the GitHub UI:

- About description: `GTID-only Go rewrite of MySQL MHA for MySQL 8.4 and 9.7 single-primary replication failover`
- Topics: `mysql`, `mha`, `gtid`, `failover`, `replication`, `high-availability`, `golang`
- Branch protection for `main`: require pull request before merging, require the `Go test` status check, require branches to be up to date, block force pushes, and block deletions.
