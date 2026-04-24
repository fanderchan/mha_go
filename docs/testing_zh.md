# 测试指南

[English](testing.md)

本项目有三层测试：

- 控制器、拓扑、复制、状态、hook、配置等包级别的单元测试。
- GitHub Actions CI 覆盖格式检查、模块一致性、`go vet`、单元测试，以及 Linux 静态编译。
- 本地 MySQL 8.4 集成烟雾测试，在 Docker 内起一套 GTID 单主拓扑运行。
- MySQL 9.7 ER/EA 真实主机验证轨道，当前使用 dbbot 三节点实验环境手工执行，后续再自动化。

## 本地单元检查

跑一遍 CI 同款检查：

```bash
gofmt -l .
go mod tidy
git diff --exit-code -- go.mod go.sum
go vet ./...
go test ./...
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags="-extldflags=-static" -o /tmp/mha ./cmd/mha
```

## MySQL 8.4 集成测试

集成测试使用 Docker Compose 和官方 `mysql:8.4` 镜像，拓扑：

- `db1`：主库
- `db2`：从库，提升优先级最高
- `db3`：从库

脚本会开启 GTID，按 `SOURCE_AUTO_POSITION=1` 配置复制，在主库创建 `mha` SQL 账号，等待两台从库应用种子数据，然后依次执行：

- `mha check-repl`
- `mha switch --new-primary db2 --dry-run`（dry-run 模式）
- `mha switch --new-primary db2`（对真实 Docker 拓扑执行）
- 切换后再跑一次 `mha check-repl`
- `mha failover-plan --candidate db3`
- `mha failover-execute --candidate db3 --dry-run`：确认新主库仍存活时被正确阻断
- 停掉 `db2`（切换后的当前主库）
- `mha failover-plan --candidate db3`
- `mha failover-execute --candidate db3`
- 故障转移后在 `db3` 写数据，并在 `db1` 验证复制
- 重启已恢复的旧主库 `db2`，用 GTID auto-position 把它重新挂到 `db3` 下，再次验证三节点完整拓扑

`mha` 二进制在 Docker 网络内执行，所以同一组节点地址既适用于 SQL 巡检，也适用于 `CHANGE REPLICATION SOURCE TO`。

生成的 Docker 配置使用 `salvage.policy: availability-first`，因为该测试会故意停止当前主库，而一次性容器没有提供进入已停止旧主机的 SSH binlog salvage 路径。生产示例仍保持更保守的 `salvage-if-possible` 默认值，除非已配置 SSH/agent salvage。

从仓库根目录运行：

```bash
./test/integration/mysql84/run.sh
```

常用环境变量：

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `MYSQL_IMAGE` | `mysql:8.4` | 要测的 MySQL 镜像。发布阻断矩阵请保持在 8.4.x。 |
| `MHA_IT_RUNNER_IMAGE` | 同 `MYSQL_IMAGE` | 在 Docker 网络内运行静态 `mha` 二进制的镜像。 |
| `MYSQL_ROOT_PASSWORD` | `rootpass` | 一次性容器里的 root 密码。 |
| `MHA_IT_PASSWORD` | `mha_it_pass_123` | 复制出去的 `mha` SQL 账号的密码。 |
| `MHA_IT_BIN` | 临时目录内构建 | 指定一个已有的 Linux amd64 `mha` 二进制用于测试，跳过本地构建。 |
| `MHA_IT_KEEP` | `0` | 设为 `1` 可在测试结束后保留容器和生成的配置用于调试。 |
| `MHA_IT_PROJECT` | 自动生成 | Docker Compose project 名称。 |

当 `MHA_IT_KEEP=1` 时，脚本退出前会打印出 Docker Compose project 名和临时工作目录。

## 覆盖矩阵

用这张表判断当前 Docker 测试已经证明了什么，以及生产式发布前还需要手工补测什么。

标记说明：

- `已覆盖`：由 `test/integration/mysql84/run.sh` 在真实 MySQL 容器上执行。
- `单测`：只由 Go 单元/包测试覆盖。
- `手工`：当前自动化 Docker 流程没有覆盖。

| 场景 | Docker 8.4 覆盖 | 其他自动化覆盖 | 手工补测建议 |
|------|-----------------|----------------|--------------|
| 构建静态 `mha` 二进制 | 已覆盖 | CI 构建 | 基础 Linux amd64 构建无需额外补测。 |
| 启动 3 节点 MySQL 8.4 GTID 拓扑 | 已覆盖 | - | 如需验证安装包、网络、防火墙，仍需在真实主机复测。 |
| 配置 GTID auto-position 复制 | 已覆盖 | - | 生产账号和权限模型要单独确认。 |
| `mha check-repl` SQL 拓扑发现与评估 | 已覆盖 | 拓扑评估/发现映射单测 | 每套真实目标拓扑维护前都应跑一次。 |
| 在线切换 dry-run | 已覆盖 | executor 单测 | 无 writer endpoint 的基础切换无需额外补测。 |
| 在线切换真实执行，无 writer endpoint | 已覆盖 | switchover controller/executor/verify 单测 | 按生产负载特征再跑一轮。 |
| 候选主追平与切换后复制 | 已覆盖 | GTID set 单测 | 长事务、明显延迟场景需要手工补。 |
| 主库仍存活时生成 failover plan | 已覆盖 | failover controller 单测 | 基础阻断门禁无需额外补测。 |
| 主库仍存活时 failover execute 被阻断 | 已覆盖 | failover executor 单测 | 基础阻断门禁无需额外补测。 |
| 停主后真实故障转移到存活从库 | 已覆盖 | failover controller/executor/verify 单测 | 带生产 fencing 和 endpoint 配置时要复测。 |
| 旧主恢复后 rejoin | 脚本用手工 SQL 覆盖 | - | 仍需要手工 runbook；当前没有 `mha rejoin` 命令。 |
| `availability-first` 故障转移机制 | 已覆盖 | best-effort salvage 失败继续执行的 executor 单测 | 用业务一致性期望验证该策略是否可接受。 |
| `salvage-if-possible` 且 donor SQL 可达 | 手工 | SQL salvager 和 GTID 单测 | 制造缺失 GTID gap，确认 donor 追平成功/失败行为。 |
| `strict` salvage 策略 | 手工 | failover plan 单测 | 确认 strict 模式会阻断你预期的运维场景。 |
| 旧主 SQL 已死时 SSH binlog salvage | 手工 | SSH 命令构造单测 | 配好 SSH、本地 binlog 路径、`mysqlbinlog`、manager 侧 `mysql` 和 known_hosts 后测试。 |
| 半同步启用且健康 | 手工 | 评估逻辑单测 | 加载 semi-sync 插件，验证 preferred/required 策略。 |
| 半同步降级到 async | 只覆盖 warning 形态 | 评估逻辑单测 | 手工制造 async gap，并验证所选 salvage 策略。 |
| Writer endpoint `vip`/`proxy` precheck/switch/verify | 手工 | writer endpoint 命令单测 | 验证真实 VIP/proxy 脚本、幂等性和失败处理。 |
| required/optional 外部 fencing 步骤 | 手工 | fencing coordinator 单测 | 验证 STONITH/cloud/proxy 命令，以及 required 失败会阻断。 |
| 旧主 SQL 可达时的 failover read-only fence | 手工 | SQL admin 与 fencing 单测 | 补测 SQL 可达但需要切换的近似 split-brain 场景。 |
| manager 监控循环自动故障转移 | 手工 | monitor 状态机单测 | 运行 `mha manager`，kill/isolate 主库，确认自动交接。 |
| manager 与主库网络隔离 | 手工 | monitor 状态机单测 | 用防火墙或 network namespace；Docker stop 不能等价覆盖。 |
| 从库延迟 / 延迟不均 / 候选主选择 | 手工 | 候选主评分单测 | 注入延迟并验证候选主排序和阻断行为。 |
| 候选主不能提升 / 中途步骤失败 | 手工 | executor 失败路径单测 | 通过 SQL 权限或外部命令失败验证 abort 状态和日志。 |
| 真实 hook 通知系统 | 手工 | shell dispatcher 单测 | 验证副作用、失败处理和 dry-run 期望。 |
| MySQL 9.7 ER/EA | 真实主机手工验证 | 版本归一化单测 | 使用 dbbot 三节点实验环境记录 `check-repl`、在线切换 dry-run、主库存活时 failover 阻断和 manager 启动。 |

## 手工测试用例模板

每个手工用例建议记录：

1. 范围：MySQL 版本、拓扑、半同步设置、salvage 策略、writer endpoint 和 fencing 配置。
2. 基线：`mha check-repl --config <file>`、`SHOW REPLICA STATUS\G`、行数或业务一致性标记、当前写入口目标。
3. Dry-run：完整 `mha switch` 或 `mha failover-execute` 命令，以及 plan 输出。
4. 故障或动作：具体故障注入或维护动作，包含时间点。
5. 执行：命令输出、退出码、结构化日志、hook/script 输出。
6. 验证：新主可写、预期从库都以 GTID auto-position 指向新主、旧主状态、写入口目标、业务读写检查。
7. 清理：rejoin 或重建步骤、最终 `check-repl`、是否需要人工数据对账。

## MySQL 9.7 ER/EA 验证计划

MySQL 9.7 ER/EA 属于前向兼容目标。当前手工验证环境是 dbbot 三节点实验环境：

- `192.168.161.11`：主库和 manager
- `192.168.161.12`：从库
- `192.168.161.13`：从库

先用 dbbot `master_slave.yml` 部署 MySQL 拓扑，再按本项目 README 或 dbbot
`mha_go.yml` role 部署 mha-go。

先跑一遍和 8.4 集成测试一样的场景：

- `check-repl`
- dry-run 和真实切换
- 新主存活时故障转移被阻断
- 主库停机后的真实故障转移
- 旧主恢复后用 GTID auto-position 重新加入拓扑
- manager systemd 启动和日志

9.7 特有的检查只能通过 capability 探测加进来。不要引入会削弱 8.4 发布基线的版本分支逻辑。

## CI

CI 定义在 `.github/workflows/ci.yml`，每次 push 到 `main` 以及每个 PR 都会跑。发布构建定义在 `.github/workflows/release.yml`，匹配 `v*` tag 时触发。

Docker 集成测试故意保持手动触发，定义在 `.github/workflows/integration.yml`。发布前需要验证 MySQL 行为时，从 GitHub Actions UI 手动运行即可。

## GitHub 仓库设置

当前环境有 SSH push 权限，但还没有用于仓库设置的 GitHub API 凭据。以下内容请在 GitHub UI 里一次性配好：

- About 描述：`GTID-only Go rewrite of MySQL MHA for MySQL 8.4 and 9.7 single-primary replication failover`
- Topics：`mysql`、`mha`、`gtid`、`failover`、`replication`、`high-availability`、`golang`
- `main` 分支保护：合并前强制 PR、要求 `Go test` 状态检查通过、要求分支与 base 保持同步、禁用 force push、禁用分支删除。
