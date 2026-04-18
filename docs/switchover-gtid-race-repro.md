# Switchover GTID race reproduction

This note explains why the switchover flow must not be reduced to just:

1. Set `super_read_only=ON` on the old primary.
2. Immediately read `@@GLOBAL.gtid_executed` on the old primary.
3. Have the candidate wait for that GTID set.
4. Promote the candidate.

The core problem: `read_only` / `super_read_only` blocks *new* writes but does not wait for in-flight transactions to finish. A transaction that has already executed DML before the read-only fence can still commit *after* mha-go reads `gtid_executed`. That transaction's GTID would then never be waited for on the candidate.

## Prerequisites

- Two- or three-node MySQL 8.4 GTID replication topology.
- `db1` is the old primary, `db2` is the candidate.
- `db2` uses GTID auto-position replication and is caught up.

Set up a table:

```sql
CREATE DATABASE IF NOT EXISTS mha_race;
CREATE TABLE IF NOT EXISTS mha_race.t (
  id INT PRIMARY KEY,
  note VARCHAR(64) NOT NULL
);
```

## Reproduction steps

On the old primary `db1`, open session A and start a transaction with a write, but do not commit:

```sql
USE mha_race;
START TRANSACTION;
INSERT INTO t VALUES (1001, 'committed after gtid snapshot');
-- Keep the transaction open; do NOT COMMIT yet.
```

On the manager, run an online switchover. A vulnerable implementation first flips the old primary to read-only, then reads `@@GLOBAL.gtid_executed` once inside `WaitCandidateCatchUp`:

```bash
mha switch --config /etc/mha/cluster.yaml --new-primary db2
```

Wait until the log shows something like `waiting for candidate to catch up`, then go back to session A on `db1` and commit:

```sql
COMMIT;
SELECT @@GLOBAL.gtid_executed;
```

This commit generates a new GTID on the old primary, e.g.:

```text
aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-101
```

But the GTID set mha-go captured *before* the commit may have been only:

```text
aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-100
```

The candidate `db2` only waits for `1-100` and is then promoted. After the switchover completes, inspect the new primary:

```sql
SELECT @@GLOBAL.gtid_executed;
SELECT * FROM mha_race.t WHERE id = 1001;
```

Expected observation on the vulnerable flow:

- Old primary `db1` has `id=1001`, and `gtid_executed` contains `:101`.
- New primary `db2` does *not* have `id=1001`, and `gtid_executed` does not contain `:101`.

This is the lost-transaction window. It does not require semi-sync failure or network anomalies; any switchover that races with an in-flight (started but not yet committed) transaction can hit it.

## Remediation implemented

The implemented fix is to avoid taking a single GTID snapshot immediately after read-only. `WaitCandidateCatchUp` now waits for active InnoDB write transactions on the old primary to drain, requires `@@GLOBAL.gtid_executed` to remain stable across consecutive samples, and only then asks the candidate to wait for the final GTID set.

The safety check needs the management account to inspect active transactions, so the documented SQL privileges include `PROCESS`.
