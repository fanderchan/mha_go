# Changelog

[中文](CHANGELOG_zh.md)

## v0.1.4 - 2026-04-17

- Extended the MySQL 8.4 Docker integration test to rejoin the recovered old primary after failover.
- Added a final topology check after old-primary recovery.

## v0.1.3 - 2026-04-17

- Extended the MySQL 8.4 Docker integration test to stop the current primary and execute a real failover to `db3`.
- Added post-failover write and replication verification.
- Added a manually triggered GitHub Actions integration workflow.
- Linked the changelog from the English and Chinese README files.

## v0.1.2 - 2026-04-17

- Added a real MySQL 8.4 switchover path to the Docker integration test.
- Ran the integration `mha` binary inside the Docker network so SQL inspection and replication source addresses match.
- Added release-time version injection so `mha version` reports the tag.
- Documented `RELOAD` as required for `RESET REPLICA ALL`.
- Updated README download examples to `v0.1.2`.

## v0.1.1 - 2026-04-17

- Added local MySQL 8.4 Docker integration smoke tests.
- Added a testing guide covering unit checks, CI, and integration test usage.
- Added README CI and release badges.
- Fixed MySQL 8.4 dynamic privilege spelling for `REPLICATION_SLAVE_ADMIN`.

## v0.1.0 - 2026-04-17

- Initial release of the GTID-only Go rewrite of MySQL MHA.
- Added CI and release workflows.
- Published Linux amd64 and arm64 static binaries.
