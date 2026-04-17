# mha-go

[![CI](https://github.com/fanderchan/mha_go/actions/workflows/ci.yml/badge.svg)](https://github.com/fanderchan/mha_go/actions/workflows/ci.yml)
[![Release](https://img.shields.io/github/v/release/fanderchan/mha_go?display_name=tag)](https://github.com/fanderchan/mha_go/releases)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](../LICENSE)

[MySQL MHA](https://github.com/yoshinorim/mha4mysql-manager)（Master High Availability）的 Go 语言重写版。为基于 GTID 的单主 MySQL 复制拓扑提供自动化 **故障转移（failover）** 和 **在线切换（switchover）** 能力。

[English](../README.md)

## 与 Perl MHA 的功能对比

图例：`✓` 表示支持，`-` 表示按设计不支持，`部分` 表示常见路径已实现但还不是完整能力。

| 大项 | 细项 | Perl MHA 0.58 | mha-go |
|------|------|---------------|--------|
| 部署 | 单一自包含二进制 | - | ✓ |
| 部署 | 不依赖 Perl 运行时 | - | ✓ |
| 部署 | 不强制每台 MySQL 主机安装 node 包 | - | ✓ |
| 版本范围 | 旧版本 MySQL 兼容 | ✓ | - |
| 版本范围 | 明确以 MySQL 8.4 为发布基线 | - | ✓ |
| 版本范围 | MySQL 9.7 ER/EA 前向兼容轨道 | - | 部分 |
| 复制模型 | GTID-only 安全模型 | - | ✓ |
| 复制模型 | 非 GTID / 文件位点故障切换 | ✓ | - |
| 拓扑检查 | 一次性复制健康检查 | ✓ | ✓ |
| 拓扑检查 | 基于能力探测的 SQL 拓扑发现 | - | ✓ |
| 故障转移 | 自动主库故障检测 | ✓ | ✓ |
| 故障转移 | 候选优先级 / no-master 控制 | ✓ | ✓ |
| 故障转移 | 执行前生成 typed 有序计划 | - | ✓ |
| 故障转移 | 写操作默认 dry-run | - | ✓ |
| 恢复 | 通过 SSH node 工具做 relay-log/binlog 恢复 | ✓ | - |
| 恢复 | 从 SQL 可访问 donor 做 GTID 追平 | - | ✓ |
| 恢复 | 旧主 SQL 不可达时通过 SSH/node 工具抽取 binlog | ✓ | 计划中 |
| 在线切换 | 在线主库切换 | ✓ | ✓ |
| 写入口 | 外部命令切换 VIP/proxy | ✓ | ✓ |
| 写入口 | promote 前预检写入口切换能力 | - | ✓ |
| 写入口 | 切换后 verify 命令 | - | ✓ |
| 隔离 | SQL read-only fence | 部分 | ✓ |
| 隔离 | 可配置 required/optional fencing steps | - | ✓ |
| Hook | 生命周期 shell 回调 | ✓ | ✓ |
| Hook | Hook 作为 VIP 漂移主入口 | ✓ | - |
| 可观测性 | 结构化日志审计/历史 | - | ✓ |
| 凭据 | env/file/plain 密码引用 | - | ✓ |
| 测试 | Go 单元测试与 CI 静态构建 | - | ✓ |

## 支持的 MySQL 版本

| 版本 | 状态 |
|------|------|
| MySQL 8.4.x | 主力支持（发布基线） |
| MySQL 9.7 ER/EA | 前向兼容目标 |

**不支持** MySQL 5.7、8.0 和 9.6。所有节点必须开启 GTID。

## 快速上手

### 1. 前提条件

**所有 MySQL 节点**需确保 GTID 已启用（`my.cnf`）：

```ini
[mysqld]
gtid_mode                = ON
enforce_gtid_consistency = ON
log_bin                  = ON
log_replica_updates      = ON
```

验证：

```sql
SHOW VARIABLES WHERE Variable_name IN ('gtid_mode', 'enforce_gtid_consistency');
```

### 2. 创建 MHA MySQL 账号

在**主库**上执行（通过 GTID 自动复制到所有从库）：

```sql
CREATE USER IF NOT EXISTS 'mha'@'<你的子网>%'
  IDENTIFIED BY '<强密码>';

-- 健康检查 + 故障转移所需的最小权限
GRANT RELOAD,
      REPLICATION CLIENT,
      REPLICATION SLAVE,
      REPLICATION_SLAVE_ADMIN,
      SYSTEM_VARIABLES_ADMIN,
      SESSION_VARIABLES_ADMIN
  ON *.* TO 'mha'@'<你的子网>%';

FLUSH PRIVILEGES;
```

> **提示**：将 `<你的子网>` 替换为实际网段（如 `192.168.1.%` 或 `10.0.%`）。

### 3. 安装

预编译二进制不依赖 Go；从源码构建需要 Go 1.25+。

下载预编译 Linux 二进制：

```bash
MHA_VERSION=v0.1.4
case "$(uname -m)" in
  x86_64) ASSET="mha_${MHA_VERSION}_linux_amd64" ;;
  aarch64|arm64) ASSET="mha_${MHA_VERSION}_linux_arm64" ;;
  *) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

curl -fL -o mha \
  "https://github.com/fanderchan/mha_go/releases/download/${MHA_VERSION}/${ASSET}"
curl -fL -o SHA256SUMS \
  "https://github.com/fanderchan/mha_go/releases/download/${MHA_VERSION}/SHA256SUMS"
grep " ${ASSET}$" SHA256SUMS | sha256sum -c -
chmod +x mha
```

也可以从源码构建：

```bash
git clone git@github.com:fanderchan/mha_go.git
cd mha_go

# 动态链接
go build -o mha ./cmd/mha

# 静态编译（推荐用于部署）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags="-extldflags=-static" -o mha ./cmd/mha
```

### 4. 配置

复制并编辑示例配置：

```bash
cp examples/cluster-8.4.yaml /etc/mha/cluster.yaml
```

最小配置示例（`cluster.yaml`）：

```yaml
name: my-cluster

topology:
  kind: async-single-primary

replication:
  mode: gtid

nodes:
  - id: db1
    host: 10.0.0.11
    port: 3306
    version_series: "8.4"
    expected_role: primary
    sql:
      user: mha
      password_ref: env:MHA_DB_PASSWORD

  - id: db2
    host: 10.0.0.12
    port: 3306
    version_series: "8.4"
    expected_role: replica
    candidate_priority: 100
    sql:
      user: mha
      password_ref: env:MHA_DB_PASSWORD

  - id: db3
    host: 10.0.0.13
    port: 3306
    version_series: "8.4"
    expected_role: replica
    candidate_priority: 90
    sql:
      user: mha
      password_ref: env:MHA_DB_PASSWORD
```

通过环境变量设置密码：

```bash
export MHA_DB_PASSWORD='你的强密码'
```

### 5. 验证复制健康状态

```bash
./mha check-repl --config /etc/mha/cluster.yaml
```

预期输出：

```
Cluster: my-cluster  mode=async-single-primary  primary=db1  nodes=3
  - db1    role=primary health=alive   addr=10.0.0.11:3306   ro=false
  - db2    role=replica health=alive   addr=10.0.0.12:3306   ro=true
         replica: source=db1 io=true sql=true lag=0s autopos=true
  - db3    role=replica health=alive   addr=10.0.0.13:3306   ro=true
         replica: source=db1 io=true sql=true lag=0s autopos=true
Assessment: OK
```

### 6. 开始使用

```bash
# 在线切换（先 dry-run，再真实执行）
./mha switch --config /etc/mha/cluster.yaml --new-primary db2
./mha switch --config /etc/mha/cluster.yaml --new-primary db2 --dry-run=false

# 故障转移计划和执行
./mha failover-plan --config /etc/mha/cluster.yaml
./mha failover-execute --config /etc/mha/cluster.yaml --dry-run=false

# 启动 HA 监控守护进程
./mha manager --config /etc/mha/cluster.yaml
```

## 子命令

| 命令 | 说明 |
|------|------|
| `check-repl` | 一次性拓扑和复制健康检查 |
| `manager` | 常驻 HA 监控；主库宕机时自动触发故障转移 |
| `switch` | 在线（优雅）切换到指定或最优从库 |
| `failover-plan` | 构建并展示故障转移计划，不执行 |
| `failover-execute` | 构建并执行故障转移计划 |

通用参数：

- `--config <file>` — 集群配置文件（必需）
- `--log-level debug|info|warn|error`（默认 `info`）
- `--log-format text|json`（默认 `text`）
- `--dry-run` — `switch` 和 `failover-execute` 默认为 `true`

## 凭据引用

`sql.password_ref` 字段支持三种格式：

| 格式 | 示例 | 说明 |
|------|------|------|
| `env:变量名` | `env:MHA_DB_PASSWORD` | 从环境变量读取（推荐） |
| `file:路径` | `file:/etc/mha/db.secret` | 从文件读取；自动去除尾部换行 |
| `plain:值` | `plain:s3cr3t` | 明文 — **不建议用于生产环境** |

## 生产部署

### systemd

```ini
# /etc/systemd/system/mha-manager.service
[Unit]
Description=MHA Go Manager
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/mha manager --config /etc/mha/cluster.yaml --log-format json
Restart=on-failure
RestartSec=5s
Environment=MHA_DB_PASSWORD=your-password-here
StandardOutput=journal
StandardError=journal

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now mha-manager
journalctl -u mha-manager -f
```

> **说明**：manager 在成功执行故障转移后会退出。先更新配置以反映新拓扑，用 `check-repl` 验证后再显式重启服务。`Restart=on-failure` 只覆盖崩溃或非零退出。

## 文档

| 文档 | 说明 |
|------|------|
| [操作手册](operations.md) | 完整配置参考、MySQL 前置条件、操作流程 |
| [架构蓝图](mha-go-blueprint.md) | 设计决策和模块职责 |
| [部署指南](deploy-mha-go.md) | 配合 [dbbot](https://github.com/fanderchan/dbbot) 的分步部署 |
| [测试指南](testing.md) | 单元测试、CI 和本地 MySQL 8.4 集成测试 |
| [变更日志](../CHANGELOG.md) | 版本发布历史 |
| [配置示例：MySQL 8.4](../examples/cluster-8.4.yaml) | 完整注释的三节点集群配置 |

## 许可协议

Apache License 2.0 — 详见 [LICENSE](../LICENSE)。
