# 测试指南

[English](testing.md)

本项目有三层测试：

- 控制器、拓扑、复制、状态、hook、配置等包级别的单元测试。
- GitHub Actions CI 覆盖格式检查、模块一致性、`go vet`、单元测试，以及 Linux 静态编译。
- 本地 MySQL 8.4 集成烟雾测试，在 Docker 内起一套 GTID 单主拓扑运行。
- 预留的 MySQL 9.7 ER/EA 验证计划（在稳定环境可用前先保留在测试蓝图里）。

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
- `mha switch --new-primary db2`（dry-run 模式）
- `mha switch --new-primary db2 --dry-run=false`（对真实 Docker 拓扑执行）
- 切换后再跑一次 `mha check-repl`
- `mha failover-plan --candidate db3`
- `mha failover-execute --candidate db3`：确认新主库仍存活时被正确阻断
- 停掉 `db2`（切换后的当前主库）
- `mha failover-plan --candidate db3`
- `mha failover-execute --candidate db3 --dry-run=false`
- 故障转移后在 `db3` 写数据，并在 `db1` 验证复制
- 重启已恢复的旧主库 `db2`，用 GTID auto-position 把它重新挂到 `db3` 下，再次验证三节点完整拓扑

`mha` 二进制在 Docker 网络内执行，所以同一组节点地址既适用于 SQL 巡检，也适用于 `CHANGE REPLICATION SOURCE TO`。

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

## MySQL 9.7 ER/EA 验证计划

MySQL 9.7 ER/EA 属于前向兼容目标，在稳定测试环境就绪前不作为发布阻断项。

一旦有 9.7 环境可用，先跑一遍和 8.4 集成测试一样的场景：

- `check-repl`
- dry-run 和真实切换
- 新主存活时故障转移被阻断
- 主库停机后的真实故障转移
- 旧主恢复后用 GTID auto-position 重新加入拓扑

9.7 特有的检查只能通过 capability 探测加进来。不要引入会削弱 8.4 发布基线的版本分支逻辑。

## CI

CI 定义在 `.github/workflows/ci.yml`，每次 push 到 `main` 以及每个 PR 都会跑。发布构建定义在 `.github/workflows/release.yml`，匹配 `v*` tag 时触发。

Docker 集成测试故意保持手动触发，定义在 `.github/workflows/integration.yml`。发布前需要验证 MySQL 行为时，从 GitHub Actions UI 手动运行即可。

## GitHub 仓库设置

当前环境有 SSH push 权限，但还没有用于仓库设置的 GitHub API 凭据。以下内容请在 GitHub UI 里一次性配好：

- About 描述：`GTID-only Go rewrite of MySQL MHA for MySQL 8.4 and 9.7 single-primary replication failover`
- Topics：`mysql`、`mha`、`gtid`、`failover`、`replication`、`high-availability`、`golang`
- `main` 分支保护：合并前强制 PR、要求 `Go test` 状态检查通过、要求分支与 base 保持同步、禁用 force push、禁用分支删除。
