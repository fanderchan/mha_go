# Switchover GTID 竞态复现

这个复现说明 switchover 流程为什么不能只做：

1. 旧主 `SET GLOBAL super_read_only=ON`
2. 立刻读取旧主 `@@GLOBAL.gtid_executed`
3. 让候选主等待这个 GTID set
4. 提升候选主

核心问题：`read_only` / `super_read_only` 会阻止新写入，但不会等待已经开始的事务结束。一个在只读围栏前已经执行了 DML 的事务，可以在 mha-go 读取 `gtid_executed` 之后才提交。这个事务的 GTID 就不会被候选主等待。

## 前提

- 两节点或三节点 MySQL 8.4 GTID 复制拓扑。
- `db1` 是旧主，`db2` 是候选主。
- `db2` 使用 GTID auto-position 复制，并且已经追平。

准备表：

```sql
CREATE DATABASE IF NOT EXISTS mha_race;
CREATE TABLE IF NOT EXISTS mha_race.t (
  id INT PRIMARY KEY,
  note VARCHAR(64) NOT NULL
);
```

## 复现步骤

在旧主 `db1` 打开会话 A，先开始一个事务并执行写入，但不要提交：

```sql
USE mha_race;
START TRANSACTION;
INSERT INTO t VALUES (1001, 'committed after gtid snapshot');
-- 这里保持事务打开，不要 COMMIT
```

在 manager 上执行在线切换。一个有漏洞的实现会先把旧主设为只读，然后在 `WaitCandidateCatchUp` 中只读取一次旧主 `@@GLOBAL.gtid_executed`：

```bash
mha switch --config /etc/mha/cluster.yaml --new-primary db2
```

等到日志出现类似 `waiting for candidate to catch up` 后，回到旧主 `db1` 的会话 A 提交事务：

```sql
COMMIT;
SELECT @@GLOBAL.gtid_executed;
```

这次提交会在旧主上生成一个新的 GTID，例如：

```text
aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-101
```

但 mha-go 在提交前读取到的可能只是：

```text
aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa:1-100
```

候选主 `db2` 只等待 `1-100` 后就会被提升。切换完成后检查新主：

```sql
SELECT @@GLOBAL.gtid_executed;
SELECT * FROM mha_race.t WHERE id = 1001;
```

有漏洞流程下的预期现象：

- 旧主 `db1` 有 `id=1001`，并且 `gtid_executed` 包含 `:101`。
- 新主 `db2` 没有 `id=1001`，并且 `gtid_executed` 不包含 `:101`。

这就是丢事务窗口。它不要求半同步故障，也不要求网络异常；只要切换时有已经开始但尚未提交的事务，就可能发生。

## 已实现的修复

当前实现不再在只读后立刻取一次 GTID 就结束。`WaitCandidateCatchUp` 会先等待旧主上的活动 InnoDB 写事务排空，再要求 `@@GLOBAL.gtid_executed` 连续多次采样保持稳定，最后才让候选主等待这个最终 GTID set。

这个安全检查需要管理账号能查看活动事务，所以 SQL 权限文档已包含 `PROCESS`。
