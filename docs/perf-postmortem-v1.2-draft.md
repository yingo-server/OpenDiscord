# perf-spec v1.2「慢消费者降级」回滚 — 事后分析（草稿）

状态：draft · 日期：2025 · 范围：perf-spec v1.2 模块 2c（sendqueue 慢消费者降级）

## 摘要

v1.2 上线 sendqueue 优化后，500 人群压测出现严重回归：

| 指标 | v1.1.1 | v1.2（降级开启） |
|---|---|---|
| 可用性 | 2.2% | **0.9%** |
| p95 延迟 | 4.2s | **8.8s** |

定位为「慢消费者降级」（LAGGING + RESYNC 补拉）在客户端侧完全没有退路，
已整体回滚；其余优化（环容量 1024、drain-then-batch 写循环、控制帧遵守
jsonMode）保留。15ms safety tick 改为可选（`Config.SendTick`，默认关闭）。

## 根因链（证据）

1. **降级协议依赖一个不存在的对端行为。** `sendqueue.push()` 环满时置
   `lagging=true`，后续所有 inline 帧静默丢弃，并向控制通道投递
   `resyncFrame()`（Envelope kind=14），期望客户端收到 RESYNC 后自行发
   SYNC(kind 8) 补拉缺口。
2. **前端从未实现 RESYNC handler。** `apps/client/src/main.tsx` 中没有任何
   kind 14 的分发注册，`apps/client` 的 `kinds.ts` 也没有该常量。客户端收到
   RESYNC 后既不报错也不动作。
3. **压测客户端也不发 SYNC。** 降级设计假设的退路（客户端主动补拉）在压测
   负载里完全不存在——连接一旦进入 LAGGING，没有任何机制能使其退出。
4. **ACK 蒸发放大损伤。** 连接 LAGGING 期间，`Conn.Write` 对每帧走
   「drop + 计数」路径；压测期间约 **6.9 万个 MSG_ACK** 在 `push()` 处凭空
   消失。发送方等不到 ACK → 重试风暴 → p95 4.2s→8.8s、可用性 0.9%。
5. **writeBatch 是逐帧循环**（`gorillaWriter.writeBatch` 内 for 循环调用
   `WriteMessage`），并未真正合批，降级方案声称的「单慢 socket 不拖累全组」
   收益从未兑现——即降级只有代价、没有收益。

## 教训

> **降级协议不能依赖对端尚未实现的行为。** 服务端新增任何「通知客户端做 X」
> 的机制前，必须先确认客户端已实现 X，并有灰度开关与退出机制。没有退路的
> 降级不是降级，是把丢帧合法化。

补充教训：

- 新增协议 kind 必须与客户端 codec 同步交付（`kinds.ts` ↔ `kinds.go`），
  并有跨端一致性检查。
- 「静默丢弃」语义改变（丢弃范围从「新帧」扩大到「一切帧含 ACK」）必须
  有压测指标守门，而不是只看服务端 CPU/内存。

## 本次回滚范围

- `server/internal/user/net/ws/sendqueue.go`：删除 `lagging`、`resyncFrame()`
  及 `kinds.KindResync` 引用；`push()` 简化为 `bool`，环满回到「静默丢弃 +
  dropped 计数」（v1.1.1 语义）。
- `server/internal/proto/kinds/kinds.go`：删除 `KindResync Kind = 14` 与
  注册；14 号重新未分配。
- `server/internal/user/net/ws/acceptor.go`：`Conn.Write`/`WriteWait` 恢复
  「drop + 计数」；注释同步更新。
- 测试：新增 `sendqueue_test.go` 锁定环满静默丢弃语义。

## 保留的优化与取舍

- **环容量 1024**：保留（原 4096，省内存）。静默丢弃语义与 v1.1.1 一致，
  风险已被压测验证为可接受。
- **drain-then-batch 写循环结构**：保留。虽然 `writeBatch` 当前仍是逐帧写，
  结构保留给后续真正的合批（单 syscall/帧聚写）。
- **15ms safety tick → 可选**（`Config.SendTick`，app 层 `app.Config.SendTick`
  透传）。取舍：压测显示该 tick 在队列几乎恒空时以 ~66 次/秒/连接空转唤醒
  写协程，是纯开销；notify 路径已覆盖所有入队场景（push/signal），丢失唤醒
  风险仅在 `WriteWait` 轮询重试等极端路径存在，故默认 0 = 关闭。如需恢复旧行为
  设 `SendTick = 15ms`。
- **控制帧遵守 jsonMode（`frameWireMsg`）**：保留。这是真修复——否则 JSON
  客户端会收到解析不了的 protobuf 控制帧。
- 模块 1（历史加载）、模块 3（多值 INSERT）未触碰。

## v1.2 大群拉取模式如何避免重演

1. **客户端先行**：任何水位线/补拉通知机制，客户端 handler 必须先合入并
   有端到端测试（`apps/client` 注册分发 + `kinds.ts` 常量），再上服务端。
2. **复用已有通道**：优先复用已实现的 ERROR(kind 10)/RATE_LIMITED(kind 13)
   下行通道携带水位线信息，而不是新开 kind；若确需新 kind，走双端同 PR。
3. **降级必须有退出条件与灰度开关**：LAGGING 状态可被「队列排空」「超时」
   「客户端 SYNC 到达」任一条件解除，并受 feature flag 控制，默认关闭。
4. **指标守门**：压测可用性/p95 与 ACK 丢失计数（`Conn.Dropped`）作为上线路
   门槛，任何一者回退即自动阻断发布。
