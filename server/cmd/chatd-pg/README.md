# chatd-pg — chatd 冷库（PostgreSQL）构建变体

对应 1.4 规划 §5 第 1 行（OPS-1）：「冷库 PG driver 需部署层注册 → 发布 `chatd-pg` 构建变体（内嵌 pgx driver），启动时自检并落审计」。

## 用途

主干 `chatd` 按架构红线**不携带任何 postgres driver**（`server/cmd/chatd/assemble.go` 第 8 步：driver 由部署层注入）。配置了 `cold_dsn` 但没有部署层注册 driver 时，主干会打 ERROR 降级为 hot-only（`require_audit_store=true` 时直接拒绝启动）。

`chatd-pg` 就是那个部署层：

1. blank import `github.com/jackc/pgx/v5/stdlib` 注册 database/sql 的 `"postgres"` driver；
2. 读取与 chatd 相同的配置（`config/chatd.toml`，即 openchat.toml 等价物；只认 `cold_dsn` / `require_audit_store`，未知键忽略）；
3. 启动自检（详见下）；
4. 可选移交标准 chatd（`CHATD_BIN=/path/to/chatd ./chatd-pg …`），driver 已在本进程注册，assemble 第 8 步会真正打开冷库；`CHATD_BIN` 未设置时只跑自检后退出 0（CI / 冒烟模式）。

## 构建命令

```sh
# 主仓内 cmd（根 go.mod 统一管理 pgx 依赖）
go build -o dist/chatd-pg ./server/cmd/chatd-pg

# 或经仓库 Makefile
make build-pg
```

## 与默认 chatd 的差异

| | chatd（默认） | chatd-pg |
|---|---|---|
| postgres driver | 无（红线：不携带） | pgx/v5/stdlib 内嵌 |
| cold_dsn 配置但无 driver | ERROR 降级 hot-only 继续 | **拒绝启动**（本变体存在的意义就是提供 driver；若 import 被误删必须显式失败） |
| 自检 | 无 | Ping 冷库 + 经生产写路径 `pg.ColdDB.Append` 落一条 `ops.startup_selfcheck` 审计事件到 `sys.audit_log`（哈希链） |
| 其余行为 | — | 完全一致：不解析/改写任何其他配置；移交时原样透传 argv |

## 启动自检行为

- `cold_dsn` 为空：打印 `driver_registered=true` 与 no-op 通过，正常移交/退出。
- `cold_dsn` 非空但 `"postgres"` 不在 `sql.Drivers()`：**明确报错退出**（指明 blank import 缺失或损坏），不静默降级。
- `cold_dsn` 非空且 driver 已注册：`Ping` 冷库（10s 超时），随后经 `store.AuditJournal` 唯一写入口 Append 一条启动事件——既验证连通性，也验证审计链路端到端可写。失败即退出非零。
