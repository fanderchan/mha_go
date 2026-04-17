# 变更日志

[English](CHANGELOG.md)

## v0.1.4 - 2026-04-17

- 扩展 MySQL 8.4 Docker 集成测试：故障转移后重新加入已恢复的旧主库。
- 补充旧主恢复后的最终拓扑检查。

## v0.1.3 - 2026-04-17

- 扩展 MySQL 8.4 Docker 集成测试：停止当前主库，执行真实的故障转移到 `db3`。
- 新增故障转移后的写入与复制验证。
- 新增手动触发的 GitHub Actions 集成测试工作流。
- 中英文 README 均链接到变更日志。

## v0.1.2 - 2026-04-17

- 在 Docker 集成测试中加入真实的 MySQL 8.4 在线切换路径。
- 将集成测试所用的 `mha` 二进制放到 Docker 网络内运行，使 SQL 巡检地址与复制源地址一致。
- 增加发布期版本注入，`mha version` 可输出 tag。
- 文档补充说明：`RESET REPLICA ALL` 需要 `RELOAD` 权限。
- README 下载示例升级到 `v0.1.2`。

## v0.1.1 - 2026-04-17

- 新增本地 MySQL 8.4 Docker 集成烟雾测试。
- 新增测试指南，覆盖单元检查、CI 与集成测试用法。
- README 补充 CI 与发布徽章。
- 修正 MySQL 8.4 动态权限名称 `REPLICATION_SLAVE_ADMIN`。

## v0.1.0 - 2026-04-17

- MySQL MHA 的 GTID-only Go 重写版首次发布。
- 新增 CI 与发布工作流。
- 发布 Linux amd64 与 arm64 静态二进制。
