# Testing Guide

This project has three test layers:

- Unit and package tests for controller, topology, replication, state, hooks, and config behavior.
- GitHub Actions CI for formatting, module consistency, `go vet`, unit tests, and static Linux builds.
- A local MySQL 8.4 integration smoke test that starts a GTID single-primary topology in Docker.

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
- `mha switch --new-primary db2` in dry-run mode
- `mha failover-plan --candidate db2`
- `mha failover-execute --candidate db2` and asserts it is blocked while the primary is still alive

Run it from the repository root:

```bash
./test/integration/mysql84/run.sh
```

Useful environment variables:

| Variable | Default | Description |
|----------|---------|-------------|
| `MYSQL_IMAGE` | `mysql:8.4` | MySQL image to test. Keep this on 8.4.x for the release-blocking matrix. |
| `MYSQL_ROOT_PASSWORD` | `rootpass` | Root password inside the disposable containers. |
| `MHA_IT_PASSWORD` | `mha_it_pass_123` | Password for the replicated `mha` SQL account. |
| `MHA_IT_BIN` | built into a temp directory | Existing `mha` binary to test instead of building one. |
| `MHA_IT_KEEP` | `0` | Set to `1` to keep containers and generated config for debugging. |
| `MHA_IT_PROJECT` | generated | Docker Compose project name. |

When `MHA_IT_KEEP=1`, the script prints the Docker Compose project name and temp work directory before exiting.

## CI

CI is defined in `.github/workflows/ci.yml` and runs on every push to `main` and every pull request. Release builds are defined in `.github/workflows/release.yml` and run for tags matching `v*`.

## GitHub Repository Settings

This environment can push over SSH, but it does not currently have GitHub API credentials for repository settings. Configure these once in the GitHub UI:

- About description: `GTID-only Go rewrite of MySQL MHA for MySQL 8.4 and 9.7 single-primary replication failover`
- Topics: `mysql`, `mha`, `gtid`, `failover`, `replication`, `high-availability`, `golang`
- Branch protection for `main`: require pull request before merging, require the `Go test` status check, require branches to be up to date, block force pushes, and block deletions.
