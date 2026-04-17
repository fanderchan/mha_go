# MHA Go 重构蓝图

更新时间：2026-04-17
状态：基线设计文档

## 1. 文档目的

本文档定义新一代 MHA 的 Go 版重构方向，作为后续持续开发的基线。

目标不是逐行复刻 `mha4mysql-manager`/`mha4mysql-node` 0.58，而是：

- 继承 MHA 在异步复制单写拓扑上的核心能力
- 解决 0.58 的主要痛点
- 明确只支持现代版本与现代运维方式
- 为后续扩展到 Group Replication / InnoDB Cluster 预留规范

## 2. 产品边界

### 2.1 当前版本范围

当前版本只支持：

- MySQL `8.4.x`：主力支持版本，测试与生产基线
- MySQL `9.7 ER/EA`：预研和预适配版本，作为前瞻验证目标

不支持：

- MySQL `5.7`
- MySQL `8.0`
- MySQL `9.6`
- 非 GTID 复制模式

说明：

- 截至 2026-04-14，`8.4` 是稳定长期支持主线。
- `9.7` 仍按 `ER/EA` 目标对待，代码必须以能力探测为核心，避免写死版本假设。
- 当前没有稳定 `9.7` 验证环境时，`9.7` 只进入测试蓝图和前向兼容设计，不作为当前发布阻断项。

### 2.2 当前拓扑范围

当前版本只覆盖：

- 异步复制单写架构
- GTID 复制
- 可选半同步复制
- 单主多从
- 可识别多级复制，但 v1 以单层主从为主测试面

未来版本预留但暂不实现：

- Group Replication
- InnoDB Cluster
- 多写拓扑

## 3. 相比 MHA 0.58 要解决的痛点

### 3.1 0.58 的主要问题

- 依赖 Perl、SSH、公钥、node 工具包，部署和维护成本高
- 监控、故障切换、在线切换逻辑分散，执行过程难以审计
- 中途失败时恢复能力弱，更多依赖人工介入
- 观测性不足，缺少结构化事件流和统一历史记录
- 外部 hook 以 shell 参数拼接为主，接口脆弱
- 非 GTID/relay log 恢复逻辑复杂，核心模型被历史兼容拖累
- 对现代版本演进的适配方式偏经验化，而不是能力化

### 3.2 新版本的解决方向

- 单二进制 Go 程序，依赖最小化
- 以清晰状态机驱动监控、故障切换和在线切换
- 以 `GTID-first` 为核心，不再让非 GTID 路径主导架构
- 以能力探测代替大量版本硬编码
- 引入结构化事件日志、运行日志、审计日志、指标
- hook typed 化，shell 兼容作为适配层
- 可选 agent 模式，逐步弱化对裸 SSH 的依赖

## 4. 设计原则

- `GTID-only`
- `State-machine first`
- `Capability-driven`
- `Journaled by structured logs`
- `Agent-optional`
- `Production on 8.4 first`
- `9.7 ER compatibility by detection, not assumption`

## 5. 总体架构

```text
cmd/mha
├─ manager        长驻监控 + 自动故障切换
├─ switch         在线切换
├─ check-repl     拓扑与复制健康检查
├─ failover-plan  故障切换计划
├─ failover-execute 故障切换执行
└─ version        版本信息

internal/
├─ config         配置模型与兼容旧 MHA 配置
├─ capability     版本能力探测
├─ domain         领域对象
├─ topology       拓扑发现与候选主决策
├─ monitor        健康检查与误判抑制
├─ failover       自动故障切换状态机
├─ switchover     在线切换状态机
├─ replication    GTID 复制控制与补数逻辑
├─ fencing        VIP / STONITH / endpoint 切换
├─ hooks          typed hooks + shell 兼容层
├─ state          进程内状态、事件、运行记录
├─ transport      SQL / SSH / Agent RPC
└─ obs            日志、指标、审计、事件查询
```

## 6. 核心模块

### 6.1 `config`

负责：

- 读取 YAML/TOML/JSON 主配置
- 兼容导入旧 MHA `cnf`
- 校验必填项
- 校验互斥项
- 归一化默认值

核心要求：

- 不再以“块名 + Perl 风格参数”作为内部模型
- 旧格式只作为输入适配，不进入核心 domain
- `password_ref` 当前统一采用引用形式，v1 先支持 `env:NAME`、`file:/path`、`plain:value`
- 离线演示和单元测试允许 `static discoverer`，真实拓扑检查走 `sql discoverer`

### 6.2 `capability`

对每个节点探测能力：

- `HasGTID`
- `HasAutoPosition`
- `HasSuperReadOnly`
- `HasSemiSync`
- `HasPerfSchemaReplicationTables`
- `HasClonePlugin`
- `SupportsReplicationChannels`
- `SupportsDynamicPrivileges`
- `SupportsReadOnlyFence`

规则：

- 控制器不直接写 `if version >= x`
- 先看 capability，再决定行为

### 6.3 `domain`

建议对象：

- `ClusterSpec`
- `NodeSpec`
- `ClusterView`
- `NodeState`
- `ReplicaState`
- `CandidateScore`
- `FailoverPlan`
- `SwitchoverPlan`
- `RunRecord`
- `RunEvent`

### 6.4 `topology`

职责：

- 发现当前 writer
- 区分 alive / dead / replica / non-replica
- 判断候选主可用性
- 检查复制过滤与配置一致性
- 识别多级复制
- 输出可供状态机消费的 `ClusterView`

当前实现约束：

- `sql discoverer` 通过只读 SQL 探测节点基础信息、GTID、`SHOW REPLICA STATUS`、半同步状态
- `static discoverer` 仅用于离线 dry-run、示例和测试，不作为生产探测路径
- `check-repl` 在发现拓扑后会执行复制健康评估，区分 error/warn finding
- 候选主排序当前综合 `auto-position`、复制线程状态、source 映射、只读状态、半同步和 lag，而不是只看静态优先级
- `failover-plan` 当前会先获取 lease，再基于 GTID 集合计算 candidate 新鲜度、primary 差集和 donor 建议
- `failover-plan` 当前还会输出 execution gate：primary 是否确认故障、阻断原因、以及建议的 salvage action 列表
- `failover-plan` 当前会生成 typed step outline，覆盖 `confirm`, `fence`, `salvage`, `promote`, `repoint`, `switch-writer-endpoint`, `verify`
- `failover-execute --dry-run` 当前已能消费 typed step outline，并在第一个 blocking step 停止执行
- `failover-execute --dry-run=false` 使用 `MySQLActionRunner`：对旧主做 SQL 只读隔离（可连则 `super_read_only`/`read_only`；旧主已在拓扑中标记为 dead 且不可连则跳过）、补数步骤将候选指向 donor 后 `WAIT_FOR_EXECUTED_GTID_SET`、候选主 `STOP REPLICA` / `RESET REPLICA ALL` / 关闭只读后提升、其余从库 `CHANGE REPLICATION SOURCE TO ... SOURCE_AUTO_POSITION=1` 重指向新主；写入口：`writer_endpoint.kind` 为 `vip`/`proxy` 时需配置 `writer_endpoint.command` 或环境变量 `MHA_WRITER_ENDPOINT_COMMAND` 执行外部脚本；`verify-cluster` 用 SQL 巡检新主可写且从库指向新主

### 6.5 `monitor`

职责：

- 主库健康探测
- 多观察点二次确认
- 网络分区误判抑制
- manager 自身 lease 保护

基本状态机：

```text
Init
-> DiscoverTopology
-> Healthy
-> Suspect
-> SecondaryCheck
-> ReconfirmTopology
-> DeadConfirmed
-> HandoverToFailover
```

实现细节（`internal/controller/monitor`）：

```
Healthy ──probe失败──► Suspect ──达到阈值──► SecondaryCheck
  ▲                       │                       │
  │                    恢复正常                副本IO线程确认主库存活
  └───────────────────────┘                       │
                                             全部失败│
                                                    ▼
                                        ReconfirmTopology ──重新发现后主库存活──► Healthy
                                                    │
                                              主库仍然死亡
                                                    ▼
                                            DeadConfirmed ──► HandleFailover()
```

- **Healthy**：每个 interval 探测一次主库（SQL ping）。失败则进入 Suspect，成功重置 failureCount。
- **Suspect**：继续探测，累计失败次数。达到 `failure_threshold` 后进入 SecondaryCheck；任意一次成功则回到 Healthy。
- **SecondaryCheck**：依次检查各副本的 IO 线程是否仍连接主库；若配置了 `secondary_checks` 则额外询问指定 observer 节点。任意一个确认主库存活则回到 Healthy；全部失败则进入 ReconfirmTopology。
- **ReconfirmTopology**：在 `reconfirm_timeout` 内重新执行完整拓扑发现。发现主库存活则回到 Healthy；主库仍死亡或发现失败则进入 DeadConfirmed。
- **DeadConfirmed**：调用 `FailoverHandler.HandleFailover()`，manager 循环退出。需人工或运维自动化重启 manager 以监控新主库。

### 6.6 `failover`

职责：

- old primary 确认死亡
- old primary fencing
- 候选主选择
- 补数
- 提升新主
- 其他从库重指向
- writer endpoint 切换
- 结果校验

状态机：

```text
LoadSpec
-> SnapshotTopology
-> AcquireLease
-> ConfirmPrimaryDead
-> FenceOldPrimary
-> SelectCandidate
-> RecoverMissingTransactions
-> PromoteCandidate
-> RepointReplicas
-> SwitchWriterEndpoint
-> Verify
-> Complete
```

### 6.7 `switchover`

职责：

- 在线切换前检查
- 拒绝新写入
- 锁原主
- 等待候选主追平
- 切换新主
- 重定向旧主和其他从库

状态机：

```text
Precheck
-> LockOldPrimary
-> WaitCandidateCatchUp
-> PromoteCandidate
-> RepointReplicas
-> RepointOldPrimary
-> SwitchWriterEndpoint
-> Verify
-> Complete
```

说明：不设单独的 `FreezeWrites` 步骤。`LockOldPrimary`（设置 `super_read_only`）已在 MySQL 层阻止新写入，效果等同。代理层的流量切换由末尾的 `SwitchWriterEndpoint` 通过外部脚本完成，两者职责不重叠，无需中间额外步骤。

### 6.8 `replication`

当前只做 GTID 路径。

包含两类逻辑：

- `gtid`: 正常 GTID 自动切换与追平
- `salvage`: 半同步降级或异步窗口下的补数

### 6.9 `fencing`

统一隔离接口：

- `ReadOnlyFence`
- `VIPFence`
- `STONITHFence`
- `CloudRouteFence`
- `ProxyWriterFence`

要求：

- fencing 是一等公民，不是附属脚本
- failover 未完成 fencing 时，不能进入 writer endpoint 切换
- v1 当前 SQL 层只读隔离只能算 `ReadOnlyFence`，不能等同于完整故障隔离
- 真实生产 fencing 应优先支持 typed coordinator，再提供 shell 兼容适配层

推荐实现顺序：

1. `ReadOnlyFence`：旧主可连时设置 `super_read_only=ON` / `read_only=ON`，作为最基础的 MySQL 层保护。
2. `ProxyWriterFence` / `VIPFence`：通过 typed 接口或兼容脚本把写入口从旧主摘除，要求可验证新写入口只指向新主。
3. `STONITHFence` / `CloudRouteFence`：在明确配置后执行电源、云路由、安全组或实例级隔离。
4. `FenceCoordinator`：按配置顺序执行多种 fencing，并记录每个动作的结构化日志；任一必需 fencing 失败时，禁止进入 writer endpoint 切换。

writer endpoint 切换负责“把新写流量导向新主”；fencing 负责“确保旧主不能继续接受写入”。这两个步骤必须分开建模，不能只靠一个 VIP 脚本同时承担全部语义。

### 6.10 `state`

`RunStore` 接口用于单次操作（failover/switchover/monitor session）的**进程内**状态跟踪：每一步的结果写入 `RunRecord`/`RunEvent`，操作结束后由调用方汇总输出。这是内部协调机制，不是持久化数据库。

运维审计（历史记录、事后排查）依赖**结构化日志文件**（stderr JSON/logfmt 重定向到文件），用 `grep`/`jq` 查询即可。不引入 SQLite、嵌入式数据库或额外持久化存储。

约束：

- 不实现 `admin history` 所需的持久化数据库。
- 不增加 `--state-db` / SQLite 运行库。
- 需要保留的历史信息必须写入日志文件或由外部日志系统采集。
- `RunStore` 只服务当前进程内的一次操作协调，不能作为跨进程恢复依据。

当前实现：`MemoryStore`（进程内，重启清空）+ `LocalLeaseManager`（单进程）。

## 7. 半同步与降级异步后的补数策略

这是新版本必须正面解决的问题。

### 7.1 问题

即使开启半同步，也可能发生：

- 半同步超时后自动降级为异步
- 主库本地已提交，但从库未收到事务
- 主库崩溃后，最新事务只存在于旧主本地 binlog

这时如果直接提升最新从库，可能丢事务。

### 7.2 设计目标

在 GTID-only 前提下，补数逻辑必须明确支持三种策略：

#### `strict`

- 不能确认无丢失时，不自动提升
- 需要补数成功后才允许 failover

适合高一致性场景。

#### `salvage-if-possible`

- 先尝试从旧主抽取缺失 GTID 事务
- 抽取成功则应用到候选主
- 抽取失败则中止自动切换

这是推荐默认策略。

#### `availability-first`

- 若旧主不可访问，允许提升最先进从库
- 明确记录“疑似丢失事务窗口”
- 发出高优先级审计与告警

适合以可用性优先的业务。

### 7.3 补数实现思路

补数优先级：

1. 旧主可 SQL 访问：直接查询 GTID、binlog 位点、只读状态
2. 旧主不可 SQL 访问但 agent/SSH 可访问：读取本地 binlog 并抽取差异 GTID 事务
3. 旧主完全不可达：根据策略决定中止或继续

抽象接口：

```go
type TransactionSalvager interface {
    CollectMissingTransactions(ctx context.Context, oldPrimary NodeRef, candidate NodeRef, gap GTIDSet) (ArtifactRef, error)
    ApplyTransactions(ctx context.Context, candidate NodeRef, artifact ArtifactRef) error
}
```

### 7.4 为什么仍然需要补数

因为：

- GTID 只解决“定位和重连”，不自动解决“事务只存在旧主本地”的问题
- 半同步不是绝对安全，只要能降级，就必须设计补数和保守策略

## 8. 候选主选择规则

候选主选择应分为两阶段：

### 8.1 资格过滤

必须满足：

- 可连接
- 非 `no_master`
- 复制线程状态健康
- 延迟在可接受范围内
- `log_bin` 开启
- `read_only/super_read_only` 状态可控
- 复制过滤与业务策略兼容

### 8.2 评分排序

建议维度：

- GTID 最先进
- `candidate_master` 优先
- 半同步状态更优
- 同城/同可用区优先
- 历史故障更少
- 只读切换时延更低

## 9. 管理面与运行面

### 9.1 CLI

当前 v1 CLI：

- `mha manager`
- `mha switch`
- `mha check-repl`
- `mha failover-plan`
- `mha failover-execute`
- `mha version`

后续可评估但非当前目标：

- `mha manager run-once`
- `mha compat import-mha-cnf`
- `mha admin status`

明确不做：

- `mha admin history`：历史审计统一查询结构化日志文件。
- `mha admin resume`：不为 v1 引入持久化状态数据库；中断恢复依赖人工检查日志后重新执行幂等步骤。

### 9.2 管理 API

后续可预留：

- `GET /status`
- `POST /switch`
- `POST /stop`

v1 只实现本地 CLI，REST/gRPC 作为后续扩展。管理 API 不应反向要求引入 SQLite 或内嵌状态数据库。

## 10. hook 规范

不要再让核心状态机直接拼 shell 参数。

内部统一事件：

- `monitor.suspect`
- `failover.start`
- `failover.fence`
- `failover.promote`
- `failover.writer_switched`
- `failover.complete`
- `failover.abort`
- `switchover.start`
- `switchover.complete`

对外支持两种实现：

- typed Go plugin / RPC handler
- shell compatibility adapter

## 11. Group Replication / InnoDB Cluster 预留规范

当前不实现，但必须留接口。

### 11.1 原则

- 不把 GR/Cluster 硬塞进异步复制控制器
- 抽象“拓扑模式”和“writer 管理方式”

### 11.2 预留接口

```go
type TopologyMode interface {
    Name() string
    Discover(ctx context.Context, cluster ClusterSpec) (*ClusterView, error)
    Validate(ctx context.Context, view *ClusterView) error
    SupportsManualPromotion() bool
    SupportsExternalWriterEndpoint() bool
}
```

首批模式：

- `AsyncSinglePrimaryMode`
- `GroupReplicationSinglePrimaryMode`
- `GroupReplicationMultiPrimaryMode`
- `InnoDBClusterMode`

### 11.3 需要提前考虑的点

- GR 自带主选举，不能照搬异步复制的提升逻辑
- InnoDB Cluster 的 writer 切换更依赖 metadata 和 Router
- endpoint 切换和 fencing 责任边界不同
- 监控维度不再只是 `SHOW REPLICA STATUS`

## 12. 测试策略

### 12.1 支持矩阵

必须持续测试：

- MySQL `8.4.x`
- MySQL `9.7 ER/EA`（有可用环境时执行）

其中：

- `8.4` 是发布阻断矩阵
- `9.7 ER/EA` 是前瞻兼容矩阵；当前没有环境时保留在测试蓝图，不阻断发布

### 12.2 必测场景

- 主库 crash
- manager 到主库网络隔离
- 单从库延迟
- 多从库延迟不一致
- candidate 不可提升
- old primary fencing 失败
- 半同步正常切换
- 半同步降级为异步后的补数成功
- 半同步降级为异步后的补数失败
- old primary 完全不可达时的严格模式
- online switchover 中断后的人工恢复 runbook
- hook 失败
- 重复执行 failover/switchover 关键步骤的幂等性
- fencing 失败后不得切换 writer endpoint

## 13. 分阶段开发计划

### Phase 1

- 配置模型
- capability 探测
- 拓扑发现
- `check-repl`
- 基础 journal

### Phase 2

- manager 监控循环
- suspect/secondary check/reconfirm
- 基础 failover 状态机

### Phase 3

- GTID failover
- 基础 `ReadOnlyFence`
- writer endpoint 切换
- 结构化日志审计

### Phase 4

- online switchover
- 人工 recover runbook
- shell hook 兼容层

### Phase 5

- 半同步降级后的补数逻辑
- typed fencing coordinator
- agent/SSH binlog salvage
- 双 manager / 分布式 lease 评估（后续路线，不作为 v1 目标）

### Phase 6

- GR/Cluster 模式实现

## 14. 明确不做的事

当前版本不做：

- 非 GTID 支持
- 5.7/8.0/9.6 兼容
- 为历史 MHA node 工具包继续补功能
- 把 shell 脚本作为核心接口
- 默认强依赖 SSH
- SQLite、内嵌数据库或 `admin history`
- 双 manager / 分布式 lease

## 15. 当前结论

本项目的架构路线应固定为：

- `8.4 first`
- `9.7 ER pre-adaptation in test blueprint`
- `GTID-only`
- `semi-sync aware`
- `async gap salvage capable`
- `state-machine driven`
- `journaled by structured log files`
- `single-manager by default, close to Perl MHA operating model`
- `GR/Cluster extension ready`

后续如与本文档冲突，必须先更新本文档，再改实现。
