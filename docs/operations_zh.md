# mha-go 操作手册

[English](operations.md)

## 目录

1. [MySQL 前置条件](#1-mysql-前置条件)
2. [配置文件参考](#2-配置文件参考)
3. [凭据引用格式](#3-凭据引用格式)
4. [子命令参考](#4-子命令参考)
5. [典型运维流程](#5-典型运维流程)
6. [Hook 事件](#6-hook-事件)
7. [Writer endpoint 集成](#7-writer-endpoint-集成)
8. [Fencing 模型](#8-fencing-模型)
9. [运行历史](#9-运行历史)
10. [Salvage 策略](#10-salvage-策略)

---

## 1. MySQL 前置条件

### 1.1 GTID 配置

所有节点都必须启用并强制 GTID。在 `my.cnf` 中加入：

```ini
[mysqld]
gtid_mode                  = ON
enforce_gtid_consistency   = ON
log_bin                    = ON
log_replica_updates        = ON   # 从库必需
```

验证：

```sql
SHOW VARIABLES WHERE Variable_name IN ('gtid_mode','enforce_gtid_consistency');
```

### 1.2 SQL 账号

mha-go 使用管理 SQL 账号做健康检查、拓扑发现、fencing、提升和重建复制。生产集群应该为 `CHANGE REPLICATION SOURCE TO` 单独配置复制账号。

```sql
-- 管理账号（在每个节点上执行，或从主库复制过去）
CREATE USER 'mha'@'%' IDENTIFIED BY 'strong-password';

-- 健康检查与拓扑发现所需权限
GRANT REPLICATION CLIENT ON *.* TO 'mha'@'%';

-- RESET REPLICA ALL 所需权限
GRANT RELOAD ON *.* TO 'mha'@'%';

-- 在线切换时等待活动写事务排空所需权限
GRANT PROCESS ON *.* TO 'mha'@'%';

-- Fencing 和提升所需权限
GRANT SYSTEM_VARIABLES_ADMIN, SESSION_VARIABLES_ADMIN ON *.* TO 'mha'@'%';

-- STOP/START/RESET/CHANGE REPLICA 所需权限
GRANT REPLICATION_SLAVE_ADMIN ON *.* TO 'mha'@'%';

-- 独立复制账号，写入 SOURCE_USER/SOURCE_PASSWORD
CREATE USER 'repl'@'%' IDENTIFIED BY 'strong-repl-password';
GRANT REPLICATION SLAVE ON *.* TO 'repl'@'%';

FLUSH PRIVILEGES;
```

> **最简替代方案（更粗但更省事）：**
> `GRANT SUPER, PROCESS, REPLICATION CLIENT, REPLICATION SLAVE ON *.* TO 'mha'@'%';`
> 如果省略 `sql.replication_user` / `sql.replication_password_ref`，mha-go 会为了兼容旧配置回退到 `sql.user` / `sql.password_ref`。生产环境不要依赖这个回退。

### 1.3 半同步（可选）

如果 `replication.semi_sync.policy` 设为 `preferred` 或 `required`，必须装好半同步插件：

```sql
INSTALL PLUGIN rpl_semi_sync_source SONAME 'semisync_source.so';
INSTALL PLUGIN rpl_semi_sync_replica SONAME 'semisync_replica.so';
SET GLOBAL rpl_semi_sync_source_enabled = ON;  -- 在主库执行
SET GLOBAL rpl_semi_sync_replica_enabled = ON; -- 在从库执行
```

---

## 2. 配置文件参考

新集群建议先复制 [examples/cluster-8.4.yaml](../examples/cluster-8.4.yaml)，
它是小而直接的三节点起步模板。需要 VIP/proxy、fencing、hooks、SSH binlog
salvage 或逐字段注释时，再看
[examples/cluster-8.4.full.yaml](../examples/cluster-8.4.full.yaml)。

最小的两节点集群配置（`cluster.yaml`）：

```yaml
name: app1

replication:
  mode: gtid
  semi_sync:
    policy: disabled

nodes:
  - id: db1
    host: 10.0.0.11
    port: 3306
    version_series: "8.4"
    expected_role: primary
    sql:
      user: mha
      password_ref: env:MHA_ADMIN_PASSWORD
      replication_user: repl
      replication_password_ref: env:MHA_REPL_PASSWORD

  - id: db2
    host: 10.0.0.12
    port: 3306
    version_series: "8.4"
    expected_role: replica
    sql:
      user: mha
      password_ref: env:MHA_ADMIN_PASSWORD
      replication_user: repl
      replication_password_ref: env:MHA_REPL_PASSWORD
```

### 完整字段参考

#### `name`（必填）
集群的唯一名称，用于日志、lease key、hook 环境变量。

#### `topology`

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `kind` | `mysql-replication-single-primary` | 拓扑类型。v1 仅支持 `mysql-replication-single-primary`。这里表示普通 MySQL 单主复制拓扑；纯异步和半同步都属于这个 kind。 |
| `single_writer` | `true` | 强制单写约束。 |
| `allow_cascading_replicas` | `false` | 是否允许从库的从库（级联复制）。 |

#### `controller`

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `id` | `controller-1` | 当前 mha-go 实例的唯一 ID，作为 lease 持有者。 |
| `lease.ttl` | `15s` | 时长字符串（`15s`、`1m` 等）。 |
| `monitor.interval` | `1s` | 主库探测频率。 |
| `monitor.failure_threshold` | `3` | 连续失败多少次后进入次级检查阶段。 |
| `monitor.reconfirm_timeout` | `3s` | 重确认阶段重新发现拓扑的超时。 |

`secondary_checks`（可选数组）—— 额外的观察者节点，mha-go 会在宣告主库死亡前询问它们：

```yaml
controller:
  secondary_checks:
    - name: replica2-check
      observer_node: db2   # 必须是 nodes 列表中的某个节点 ID
      timeout: 2s
```

#### `replication`

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `mode` | `gtid` | 仅支持 `gtid`。 |
| `semi_sync.policy` | `preferred` | `disabled`、`preferred`、`required` 之一。它是在 `mysql-replication-single-primary` 拓扑里的半同步期望，不是另一种 topology kind。 |
| `semi_sync.wait_for_replica_count` | `0` | 所需的最小半同步从库数量（仅在检查时强制）。 |
| `semi_sync.timeout` | `5s` | 半同步 ACK 超时（信息性字段；实际超时由 MySQL 控制）。 |
| `salvage.policy` | `salvage-if-possible` | 详见 [Salvage 策略](#10-salvage-策略)。 |
| `salvage.timeout` | `30s` | salvage 过程中等待 GTID 追平的最长时间。 |

#### `writer_endpoint`

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `kind` | `none` | `none` / `off`（跳过）、`vip`、`proxy`。 |
| `target` | | VIP 地址或代理标识（通过 `MHA_WRITER_ENDPOINT_TARGET` 传给脚本）。 |
| `command` | | 切换 endpoint 的脚本路径。缺省时回退到环境变量 `MHA_WRITER_ENDPOINT_COMMAND`。 |
| `precheck_command` | | 提升前的可选预检命令。回退到 `MHA_WRITER_ENDPOINT_PRECHECK_COMMAND`。 |
| `verify_command` | | 切换后的可选验证命令。回退到 `MHA_WRITER_ENDPOINT_VERIFY_COMMAND`。 |

#### `fencing`

未配置时，故障转移会在旧主可达的情况下执行默认的 required SQL 只读 fence（`super_read_only` / `read_only`）。

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

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `steps[].kind` | 必填 | `read_only`、`command`、`vip`、`proxy`、`stonith`、`cloud_route` 之一。 |
| `steps[].required` | `true` | required 的 fence 失败会中止故障转移；optional 的失败只记录日志并继续。 |
| `steps[].command` | | 非 `read_only` 类型的 shell 命令。 |
| `steps[].timeout` | | 单个 fence 步骤的可选超时。 |

#### `hooks`

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `shell_compat` | `false` | 是否启用 shell hook 派发器。 |
| `command` | | 每次 hook 事件都以 `sh -c` 执行的命令。 |

#### `nodes`（必填，至少 2 个）

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `id` | 必填 | 节点唯一 ID。`--new-primary`、`--candidate`、次级检查 `observer_node` 都引用此字段。 |
| `host` | 必填 | 主机名或 IP。 |
| `port` | `3306` | MySQL 端口。 |
| `version_series` | 必填 | `8.4` 或 `9.7`。 |
| `expected_role` | `replica` | `primary`、`replica` 或 `observer`。 |
| `candidate_priority` | `0` | 自动选择候选主时数值越大越优先。 |
| `no_master` | `false` | 绝不允许被提升为主库。 |
| `ignore_fail` | `false` | 将该节点的评估错误降级为警告。 |
| `zone` | | 可用区标签（信息性）。 |
| `labels` | | 键值标签（信息性）。 |
| `sql.user` | | MySQL 用户名。 |
| `sql.password_ref` | | 凭据引用（见 [§3](#3-凭据引用格式)）。 |
| `sql.replication_user` | `sql.user` | 节点成为复制源时写入 `SOURCE_USER` 的账号。必须与 `sql.replication_password_ref` 成对配置。 |
| `sql.replication_password_ref` | `sql.password_ref` | 节点成为复制源时写入 `SOURCE_PASSWORD` 的凭据引用。必须与 `sql.replication_user` 成对配置。 |
| `sql.tls_profile` | `disabled` | `disabled`、`default`、`required`、`preferred`、`skip-verify`。 |
| `ssh.user` | | 旧主 binlog salvage 使用的 SSH 用户。 |
| `ssh.port` | `22` | SSH 端口。 |
| `ssh.password_ref` | | 可选 SSH 密码凭据引用。 |
| `ssh.private_key_ref` | | 可选私钥凭据引用，生产推荐 `file:/path`。 |
| `ssh.private_key_passphrase_ref` | | 加密私钥的可选 passphrase 凭据引用。 |
| `ssh.binlog_dir` | `/var/lib/mysql` | MySQL 主机上的本地 binlog 目录。 |
| `ssh.binlog_index` | | 可选 binlog index 路径；省略时先尝试 `<binlog_dir>/<binlog_prefix>.index`，再做文件发现。 |
| `ssh.binlog_prefix` | `binlog` | 文件发现时使用的 binlog 前缀，例如 `mysql-bin`。 |
| `ssh.mysqlbinlog_path` | `mysqlbinlog` | 远端 `mysqlbinlog` 命令路径。 |

---

## 3. 凭据引用格式

`sql.password_ref`、`sql.replication_password_ref`、`ssh.password_ref`、`ssh.private_key_ref`、`ssh.private_key_passphrase_ref` 字段支持三种格式：

| 格式 | 示例 | 说明 |
|------|------|------|
| `env:变量名` | `env:MHA_ADMIN_PASSWORD` | 运行时从环境变量读取。 |
| `file:/绝对路径` | `file:/etc/mha/db.secret` | 从文件读取；自动去除尾部 `\r\n`。 |
| `plain:值` | `plain:s3cr3t` | 明文 —— 不建议用于生产环境。 |

---

## 4. 子命令参考

### `mha check-repl`

执行一次拓扑发现和复制健康检查。健康则退出码 0，发现评估错误则退出码 1。

```bash
mha check-repl --config cluster.yaml
```

可作为监控脚本或计划运维前的检查工具。

### `mha manager`

长驻 HA 监控。每个 `monitor.interval` 探测一次主库，确认主库死亡时触发自动故障转移。

```bash
mha manager --config cluster.yaml
```

- 使用配置的 lease manager。默认的 `local-memory` lease 只保护当前进程内的并发操作；它不是跨进程或跨主机的 manager 锁。
- 成功故障转移后进程退出（exit 0）。必须先更新集群配置，再重启 manager 才能开启新的监控会话。
- `--dry-run=true` 会走完完整监控循环，但故障转移步骤仅以 dry-run 方式执行（不对 MySQL 写入）。

### `mha switch`

在线（平滑）切换默认真实执行；需要只预演计划和步骤、不写 MySQL 时加 `--dry-run`。

```bash
# 先 dry-run —— 核对计划
mha switch --config cluster.yaml --new-primary db2 --dry-run

# 真实执行
mha switch --config cluster.yaml --new-primary db2
```

- `--new-primary <id>`：指定提升目标。若不指定，则自动选择最佳从库。
- 要求集群处于健康状态（评估通过）。若主库已死，请改用 `failover-execute`。

执行的步骤：

1. `precheck-writer-endpoint` —— 验证 endpoint 切换可运行（仅在 `writer_endpoint.kind` 为 `vip` 或 `proxy` 时）。
2. `lock-candidate` —— 在锁旧主前把候选主设为只读；如果计划阶段发现候选主当前可写，会先确认它没有旧主缺失的 errant GTID。
3. `lock-old-primary` —— 对当前主库设置 `super_read_only=ON`。
4. `wait-candidate-catchup` —— 等待候选主把当前主库的 GTID 全部应用完。
5. `promote-candidate` —— 停掉候选主的复制，置为可写。
6. `repoint-replicas` —— 把其他从库重指向新主。
7. `repoint-old-primary` —— 让旧主成为新主的从库。
8. `switch-writer-endpoint` —— 执行 endpoint 脚本（如果配置了）。
9. `verify-writer-endpoint` —— 执行 endpoint verify 脚本（如果配置了）。
10. `verify` —— 确认新拓扑正确。

### `mha failover-plan`

构建并打印故障转移计划，但不执行。用于在应急演练时审计"如果真故障了会发生什么"。

```bash
mha failover-plan --config cluster.yaml
mha failover-plan --config cluster.yaml --candidate db2
```

计划输出包含：

- 主库故障是否已确认
- 是否允许执行（是否存在阻断原因）
- 需要做的 salvage 动作
- 所有步骤及其状态

### `mha failover-execute`

构建并执行故障转移计划。默认真实执行；需要只审计步骤、不写 MySQL 时加 `--dry-run`。

```bash
# 安全审计 —— 不对 MySQL 写入
mha failover-execute --config cluster.yaml --dry-run

# 真实执行（仅在主库确认死亡后使用）
mha failover-execute --config cluster.yaml
```

- 计划执行被阻断时（比如主库还活着），步骤会被标记为 `blocked`/`skipped`，退出码 1。
- `--candidate <id>` 会覆盖自动选择结果。

---

## 5. 典型运维流程

### 计划内运维（switchover）

```bash
# 1. 验证集群健康
./mha check-repl --config cluster.yaml

# 2. Dry-run 切换，核对计划
./mha switch --config cluster.yaml --new-primary db2 --dry-run

# 3. 确认计划无误后真实执行
./mha switch --config cluster.yaml --new-primary db2

# 4. 再次验证
./mha check-repl --config cluster.yaml
```

### 计划外故障转移（主库已死）

```bash
# 1. 确认情况
./mha failover-plan --config cluster.yaml

# 2. dry-run 审计
./mha failover-execute --config cluster.yaml --dry-run

# 3. 真实执行
./mha failover-execute --config cluster.yaml

# 4. 更新 cluster.yaml 反映新主，然后重启 manager
```

### 启动 HA 监控守护进程

```bash
# 作为 systemd 服务：
# [Service]
# ExecStart=/usr/local/bin/mha manager --config /etc/mha/cluster.yaml --log-format json
# Restart=on-failure

./mha manager --config cluster.yaml --log-format json 2>> /var/log/mha.log
```

manager 在故障转移成功后会退出。先更新 `cluster.yaml` 把新主库标为 primary，用 `check-repl` 验证拓扑，再显式重启 manager。`Restart=on-failure` 对崩溃场景仍然有用，但成功故障转移属于正常退出，不应该在配置还指向旧主时被自动重启。

---

## 6. Hook 事件

当 `hooks.shell_compat: true` 且 `hooks.command` 有值时，mha-go 会在每个事件上以 `sh -c <command>` 执行脚本，并通过环境变量传递上下文。

Hook 只用于告警、审计和兼容回调。VIP/代理切换不由 hook 驱动；请在 [`writer_endpoint`](#7-writer-endpoint-集成) 配置真正的写入口切换。`failover.writer_switched` 事件在 writer endpoint 切换步骤成功后触发。

### 通用环境变量（所有事件）

| 变量 | 说明 |
|------|------|
| `MHA_EVENT` | 事件名（如 `failover.start`） |
| `MHA_CLUSTER` | 集群名 |
| `MHA_NODE_ID` | 涉及的主库节点 |

### 事件目录

| 事件 | 触发时机 | 额外环境变量 |
|------|----------|--------------|
| `monitor.suspect` | 主库探测失败达到阈值 | `MHA_PRIMARY`、`MHA_FAILURE_COUNT` |
| `monitor.dead_confirmed` | 主库确认死亡，触发故障转移 | `MHA_PRIMARY` |
| `failover.start` | 故障转移开始执行 | `MHA_OLD_PRIMARY`、`MHA_CANDIDATE` |
| `failover.fence` | 旧主被 fence（设为只读） | `MHA_FENCED_NODE` |
| `failover.promote` | 候选主已提升 | `MHA_NEW_PRIMARY` |
| `failover.writer_switched` | Writer endpoint 已切换 | `MHA_NEW_PRIMARY`、`MHA_OLD_PRIMARY` |
| `failover.complete` | 故障转移成功 | `MHA_NEW_PRIMARY`、`MHA_OLD_PRIMARY` |
| `failover.abort` | 故障转移被阻断或失败 | `MHA_FAILED_STEP`，失败时有 `MHA_ERROR`，被阻断时有 `MHA_REASON` |
| `switchover.start` | 在线切换开始执行 | `MHA_ORIGINAL_PRIMARY`、`MHA_CANDIDATE` |
| `switchover.complete` | 在线切换成功 | `MHA_NEW_PRIMARY`、`MHA_ORIGINAL_PRIMARY` |
| `switchover.abort` | 在线切换失败 | `MHA_FAILED_STEP`、`MHA_ERROR` |

### Hook 脚本示例

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

## 7. Writer endpoint 集成

当 `writer_endpoint.kind` 为 `vip` 或 `proxy` 时，mha-go 会在新主提升后调用配置的脚本。该步骤负责把写入口转移到新主；它本身并不等同于完整的 fence 机制。脚本通过环境变量获取上下文：

这是 VIP 漂移和代理写入口更新的受支持路径。不要把主 VIP 漂移放进 `hooks.command`；hook 只是生命周期回调，不是 writer endpoint 的主切换入口。

| 变量 | 说明 |
|------|------|
| `MHA_CLUSTER` | 集群名 |
| `MHA_WRITER_ENDPOINT_ACTION` | `precheck`、`switch` 或 `verify` |
| `MHA_WRITER_ENDPOINT_KIND` | `vip` 或 `proxy` |
| `MHA_WRITER_ENDPOINT_TARGET` | `writer_endpoint.target` 的值 |
| `MHA_NEW_PRIMARY_ID` | 新主节点 ID |
| `MHA_NEW_PRIMARY_ADDRESS` | 新主 `host:port` |
| `MHA_NEW_PRIMARY_HOST` | 新主 host |
| `MHA_NEW_PRIMARY_PORT` | 新主 port |
| `MHA_OLD_PRIMARY_ID` | 旧主节点 ID |
| `MHA_OLD_PRIMARY_ADDRESS` | 旧主 `host:port` |
| `MHA_OLD_PRIMARY_HOST` | 旧主 host |
| `MHA_OLD_PRIMARY_PORT` | 旧主 port |

脚本以 `sh -c <command>` 的方式执行。退出码非零将中止操作并报错。

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

## 8. Fencing 模型

Fencing 的目的是在故障转移决策之后，阻止旧主继续接受写入。mha-go 当前在旧主可达时执行 SQL 层的只读 fencing。生产部署应把这当作第一层，而不是全部的隔离手段。

推荐顺序：

1. SQL 只读 fence：旧主可达时设置 `super_read_only=ON` / `read_only=ON`。
2. 写入口 fence：从代理写入池摘除旧主，或把 VIP 从旧主迁走。
3. 基础设施 fence：如已配置且运维上可接受，使用 STONITH、云路由变更、安全组或实例关机。

Writer endpoint 切换和 fencing 不是同一件事：

- fencing 回答：“旧主还能接受写入吗？”
- writer endpoint 切换回答：“新的写入该去哪？”

对故障转移来说，writer endpoint 的 precheck 在 SQL 变更之前执行，而所有 required fencing 必须在 writer endpoint 切换之前完成。如果旧主完全不可达，则由配置的 salvage 策略和运维人员在可用性与一致性之间的取舍决定是否继续。

普通从库不可达不会阻断故障转移。规划阶段已经死亡的从库会被跳过并记录下来，等后续 rejoin。候选新主则不同：它必须 SQL 可达，并且如果 VIP/代理的 precheck 需要 SSH 或其他主机级访问，那这份 precheck 也必须通过才允许提升。

---

## 9. 运行历史

mha-go 不使用 SQLite 或内嵌状态数据库保存历史。运行时 `RunStore` 仅存在于进程内，重启即丢。

要保存运维审计历史，请用结构化日志并将 stderr 重定向到日志文件或 journald：

```bash
./mha manager --config /etc/mha/cluster.yaml --log-format json 2>> /var/log/mha-go/manager.jsonl
```

示例：

```bash
journalctl -u mha-manager --output cat | jq 'select(.cluster=="my-cluster")'
jq 'select(.level=="ERROR" or .level=="WARN")' /var/log/mha-go/manager.jsonl
```

v1 刻意不提供 `admin history` 命令。日志保留、轮转、集中采集交给主机侧日志栈。

---

## 10. Salvage 策略

故障转移时，候选主可能还缺少旧主上已提交但尚未复制出来的事务。Salvage 策略决定 mha-go 如何处理这种情况。

| 策略 | 行为 |
|------|------|
| `strict` | 规划阶段若检出缺失事务就直接阻断。必须由运维人员先手动解决再执行。 |
| `salvage-if-possible`（默认） | 尝试让候选主通过 GTID 指向旧主（或其他 donor）追平。若旧主 SQL 已死但 SSH 可达，则通过 SSH dump 本地 binlog 并应用缺失 GTID。若失败则中止故障转移。 |
| `availability-first` | 与 `salvage-if-possible` 相同，但追平失败仅作为警告记录，故障转移继续。把可用性置于零数据丢失之前。 |

SQL donor 追平使用 `WAIT_FOR_EXECUTED_GTID_SET()`，超时由 `replication.salvage.timeout` 控制（默认 `30s`）。

SSH binlog salvage 需要：

- 在旧主节点配置 `ssh`。
- SSH host-key 校验优先使用 `MHA_SSH_KNOWN_HOSTS` 或 `~/.ssh/known_hosts`。两者都不存在时，当前 SSH 客户端会退化为不校验 host key；生产环境应配置 known hosts。
- 旧主主机安装 `mysqlbinlog`。
- manager 主机安装本地 `mysql` 客户端；如需指定路径，可设置 `MHA_MYSQL_CLIENT_PATH`。
- 正确配置旧主本地 binlog 的 `ssh.binlog_dir` / `ssh.binlog_index` / `ssh.binlog_prefix`。

旧主 GTID 已知时，mha-go 执行 `mysqlbinlog --include-gtids=<missing>`。旧主 SQL 已死且 GTID 未知时，mha-go 执行 `mysqlbinlog --exclude-gtids=<candidate_gtid_executed>`，跳过候选主已经应用过的事务。
