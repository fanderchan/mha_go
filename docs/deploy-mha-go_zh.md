# mha-go 部署指南

[English](deploy-mha-go.md)

适用于通过 dbbot `master_slave.yml` playbook 部署的一主两从 MySQL 8.4.x 或
9.7.0 GTID 拓扑。下文示例使用 dbbot 默认测试环境：`192.168.161.11/12/13`
上的 MySQL 9.7.0。

## 前提条件

| 项目 | 要求 |
|---|---|
| MySQL 版本 | 8.4.x 或 9.7.0 ER/EA（GTID ON，`gtid_mode=ON`，`enforce_gtid_consistency=ON`） |
| 拓扑 | 一主两从，由 `master_slave.yml` 部署（存在 `master_slave_finish.flag`） |
| 用户 | `admin` 账号具备 `SUPER`/`SYSTEM_VARIABLES_ADMIN` 权限，用于授权 |
| 网络 | 中控机（manager_ip）可 TCP 3306 访问全部三节点 |
| 操作系统 | x86_64 Linux；glibc ≥ 2.17（静态编译二进制，无额外依赖） |

## 架构

```
   ┌─────────────────────────────────┐
   │  192.168.161.11  (db1 / manager)│  ← primary + 中控
   │  mysql3306  running             │
   │  /usr/local/bin/mha             │
   │  /etc/mha/cluster.yaml          │
   └────────────┬────────────────────┘
                │  replication (GTID)
       ┌────────┴────────┐
       ▼                 ▼
192.168.161.12         192.168.161.13
  (db2 / replica)      (db3 / replica)
```

> manager 进程只在 `manager_ip` 节点运行，mha 二进制需安装在该节点。

## 步骤一：构建 mha 二进制

在有 Go 1.25+ 的构建机上执行：

```bash
git clone git@github.com:fanderchan/mha_go.git
cd mha_go
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
  go build -ldflags="-extldflags=-static" -o mha ./cmd/mha
```

生成的 `mha` 静态链接，可直接部署到任意 x86_64 Linux。

## 步骤二：部署二进制

```bash
scp mha root@<manager_ip>:/usr/local/bin/mha
ssh root@<manager_ip> "chmod +x /usr/local/bin/mha && mha --help"
```

## 步骤三：创建 mha MySQL 账号

在 **primary** 上执行（会自动通过 GTID 复制到所有从库）：

```sql
CREATE USER IF NOT EXISTS 'mha'@'<subnet>%'
  IDENTIFIED BY '<password>';

GRANT SELECT, RELOAD, PROCESS, SUPER,
      REPLICATION CLIENT, REPLICATION SLAVE,
      REPLICATION_SLAVE_ADMIN,
      SYSTEM_VARIABLES_ADMIN,
      SESSION_VARIABLES_ADMIN
  ON *.* TO 'mha'@'<subnet>%';

FLUSH PRIVILEGES;
```

dbbot 默认值：
- 用户名：`mha`
- 密码：`Dbbot_mha@8888`
- 主机范围：`192.168.161.%`（按实际子网修改）
- `master_slave.yml` 已创建的复制源账号：`repl` / `Dbbot_repl@8888`，主机范围同样是 `192.168.161.%`

`cluster.yaml` 里要把管理账号和复制账号分开配置。`mha` 用于巡检和编排节点；
`repl` 会在 mha-go 重指向复制源时写入 `SOURCE_USER` / `SOURCE_PASSWORD`。

验证是否已复制到从库：

```bash
mysql -u admin -p -e "SHOW GRANTS FOR 'mha'@'192.168.161.%';"
```

## 步骤四：编写集群配置

创建 `/etc/mha/cluster.yaml`（以本次测试环境为例）：

```yaml
name: <cluster-name>

topology:
  kind: mysql-replication-single-primary
  single_writer: true
  allow_cascading_replicas: false

controller:
  id: manager-1
  lease:
    backend: local-memory
    ttl: 15s
  monitor:
    interval: 2s
    failure_threshold: 3
    reconfirm_timeout: 5s

replication:
  mode: gtid
  semi_sync:
    policy: disabled           # dbbot master_slave.yml 默认是纯异步复制
    wait_for_replica_count: 0
    timeout: 5s
  salvage:
    policy: salvage-if-possible
    timeout: 30s

writer_endpoint:
  kind: none                   # 生产环境改为 vip 或 proxy

nodes:
  - id: db1
    host: 192.168.161.11
    port: 3306
    version_series: "9.7"      # MySQL 8.4.x 使用 "8.4"
    expected_role: primary
    candidate_priority: 0
    sql:
      user: mha
      password_ref: plain:Dbbot_mha@8888
      replication_user: repl
      replication_password_ref: plain:Dbbot_repl@8888

  - id: db2
    host: 192.168.161.12
    port: 3306
    version_series: "9.7"      # MySQL 8.4.x 使用 "8.4"
    expected_role: replica
    candidate_priority: 100    # 最高优先级，优先提升
    sql:
      user: mha
      password_ref: plain:Dbbot_mha@8888
      replication_user: repl
      replication_password_ref: plain:Dbbot_repl@8888

  - id: db3
    host: 192.168.161.13
    port: 3306
    version_series: "9.7"      # MySQL 8.4.x 使用 "8.4"
    expected_role: replica
    candidate_priority: 90
    sql:
      user: mha
      password_ref: plain:Dbbot_mha@8888
      replication_user: repl
      replication_password_ref: plain:Dbbot_repl@8888
```

`password_ref` 支持三种形式：
- `plain:<value>` — 明文（仅测试）
- `env:<VAR>` — 从环境变量读取
- `file:</path/to/file>` — 从文件读取（推荐生产）

## 步骤五：验证拓扑

```bash
mha check-repl --config /etc/mha/cluster.yaml
```

期望输出示例：

```
Cluster: <name>  mode=mysql-replication-single-primary  primary=db1  nodes=3
  - db1    role=primary health=alive   addr=192.168.161.11:3306   ro=false sro=false
  - db2    role=replica health=alive   addr=192.168.161.12:3306   ro=true sro=true
         replica: source=db1 io=true sql=true lag=0s autopos=true
  - db3    role=replica health=alive   addr=192.168.161.13:3306   ro=true sro=true
         replica: source=db1 io=true sql=true lag=0s autopos=true
Assessment: OK
```

## 步骤六：在线切换（switchover）

在线切换不中断业务，适合主动运维（机器下线、升级等）：

```bash
# Dry-run（不执行 MySQL 写操作）
mha switch --config /etc/mha/cluster.yaml --new-primary db2 --dry-run

# 真实执行
mha switch --config /etc/mha/cluster.yaml --new-primary db2
```

## 步骤七：failover 计划（紧急预案）

```bash
# 查看 failover 步骤和阻断原因
mha failover-plan --config /etc/mha/cluster.yaml

# 强制执行（primary 已确认死亡时使用）
mha failover-execute --config /etc/mha/cluster.yaml
```

## 常驻监控模式

```bash
# 前台运行（测试）
mha manager --config /etc/mha/cluster.yaml

# systemd 管理（生产）
systemctl start mha-manager
systemctl enable mha-manager
```

参见下节 systemd 单元文件配置。

## systemd 单元文件

`/etc/systemd/system/mha-manager.service`：

```ini
[Unit]
Description=dbbot MHA Go Manager
After=network.target mysql3306.service
Wants=mysql3306.service

[Service]
Type=simple
User=mysql
Group=mysql
ExecStart=/usr/local/bin/mha manager --config /etc/mha/cluster.yaml --log-format json
Restart=on-failure
RestartSec=10s
StandardOutput=journal
StandardError=journal
SyslogIdentifier=mha-manager

[Install]
WantedBy=multi-user.target
```

```bash
systemctl daemon-reload
systemctl enable --now mha-manager
journalctl -u mha-manager -f
```

manager 在成功故障转移后会正常退出。此时先更新 `/etc/mha/cluster.yaml` 中的主从角色，用 `mha check-repl --config /etc/mha/cluster.yaml` 验证新拓扑，再显式执行 `systemctl restart mha-manager`。`Restart=on-failure` 只用于崩溃或非零退出，不应在配置仍指向旧主时自动重启监控。

## 常见问题

### mha 用户权限不足（RELOAD/SUPER 缺失）

从库缺权限通常因复制线程暂停导致 GRANT 未同步。处理步骤：

```bash
# 1. 检查从库复制状态
mysql -h <replica> -u admin -p -e "SHOW REPLICA STATUS\G" | grep Running

# 2. 若 IO/SQL 线程停止，重启
mysql -h <replica> -u admin -p -e "START REPLICA;"

# 3. 等待 GTID 同步后再验证权限
mysql -h <replica> -u admin -p -e "SHOW GRANTS FOR 'mha'@'...%';"
```

### glibc 版本不兼容

症状：`/lib64/libc.so.6: version 'GLIBC_2.32' not found`

原因：二进制在高版本 glibc 系统上动态编译。

解决：使用 `CGO_ENABLED=0` 静态编译（见步骤一）。

### switchover 后复制线程停止

在线切换执行 `RESET REPLICA ALL` + `CHANGE REPLICATION SOURCE TO` 时，
如果旧 primary 的锁未释放会导致线程卡住。处理：

```bash
mysql -h <node> -u admin -p -e "SET GLOBAL super_read_only=0; START REPLICA;"
```

## 参考

- 架构设计：[mha-go-blueprint_zh.md](mha-go-blueprint_zh.md)
- 操作手册：[operations_zh.md](operations_zh.md)
- 配置示例：[../examples/cluster-test.yaml](../examples/cluster-test.yaml)
