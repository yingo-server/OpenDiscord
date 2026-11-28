# 普遍拉取 + 自适应控制规范（Universal Pull / Adaptive Control · 1.4 LTS）

> 状态：SPEC 批次产出（规划稿 `docs/lts-1.4-plan.md` §8「§A 规范代理」）。
> 依据：`lts-1.4-plan.md` §2 / §3 / §6（含 §0 纪律、§4 与 v3 关系、§7 批次边界）、`docs/pull-mode-spec-v1.2.md`、`docs/v3-bus-design.md`。
> **本文件只写规范，不改任何代码、flag 默认值、proto、测试或 CI。** 文中出现的「现状/核实点」只用于锁定兼容基线；「目标形态」是后续 CORE-1/CORE-2/CORE-3/REL 批次的实施依据。
> 参考硬件 2 核 4 GB。所有容量数字以 CI 实测为准，**规范不估算容量**；soft/firm/控制周期等阈值一律由 policy 配置给出，不在本文硬编码新数字。

## 0. 范围、纪律与定义

**普遍拉取模式（Universal Pull）**：所有会话默认走「水位线通知 + 客户端批量拉取」；`pull_mode_threshold` 的**交付分流语义被取消**（不再用成员数决定 inline 还是拉取）。inline 逐成员推送只作为 `universal_pull_enabled=false` 的回滚路径保留，且必须逐字节一致。小群实时性由自适应控制器保证（§2），不再靠 inline；大群消息照常落库、seq 照常分配，只换通知方式——这是回滚安全性的根基。

**参数自适应（Adaptive Control）**：水位线频控参数不再是静态 flag，而由控制器按实时负载**每会话独立**给出；控制器是纯策略层，机制层只读取参数。

**继承的纪律（违反即阻断发布）**：

1. 客户端行为先行合入并验证，服务端特性才允许启用；`flag 默认关闭的中间态 + 逐字节回归常驻`。本规范下 `universal_pull_enabled` 在中间态默认 false，启用前必须已有 v1.2.1 客户端的 kind 14 handler + `pull-scheduler.ts` 全链路测试（v1.2 已落盘）。
2. 任何新机制必须先用**同一组已有计数器**证明前提成立，再用它证明收益；不新增探针（§2.1）。
3. `protobuf wire 跨帧不自描述，禁止拼帧合批`：水位线永远一帧一 Envelope，不得把多条水位线/数据帧拼进一个 payload、不得跨帧拼接字节流。
4. 一切皆帧；机制/策略分离；crash-only（水位线节流状态是纯加速结构，重启丢状态至多多发一条水位线）。
5. v3 灰度/回滚是**一次性切换，杜绝双投递窗口**；100% 灰度后一个版本周期删除旧直投代码，LTS 不留双路径。
6. 禁止按成员活跃度差异化推送、禁止后台会话兜底轮询、禁止服务端为拉取模式新建任何消息缓存/状态（v1.2 §6.6 的「明确不做」继续有效）。

**术语**：`机制层` = fanout / 水位线生成与发送 / v3 投递；`策略层` = 自适应控制器；`effective[cid]` = 机制层每会话读取的原子参数快照；`静态默认值` = v1.2.1 tag 的 Defaults（见 §5）；`hard bound` = 参数硬上下界；`soft/firm` = policy 里已有的队列水位。

§A 交付项对照：水位线字段/频率 → §1；AIMD 伪代码与边界 → §2；v1.2 客户端兼容矩阵 → §3；S-6/S-7 可测试断言 → §4；回滚开关清单 → §5。

## 1. 普遍模式下的水位线协议（字段与频率）

### 1.1 帧形态与字段（协议零改动）

沿用 v1.2 的 **kind 14 `WATERMARK`**，`kinds.go` / `kinds.ts` 已登记（14；15/16/17 已被 v3 占用）。**普遍模式不新增 kind、不新增 capability 协商、不改 Envelope 结构、不改 8 SYNC / 920 / 930 读路径**。

| 项 | 规范 |
|---|---|
| Envelope | `kind=14`，`conv_id` 与 target uid 寻址；按 v1.2 §3.1 只投在线成员、排除发送者；离线成员由 RESUME(kind 6) 收敛 |
| protobuf（`chat.proto` 已落盘） | 字段 1 `conv_id uint64`、2 `latest_seq uint64`、3 `reason uint32`、4 `member_count uint32`；`reserved 5 to 9`，1.4 不得占用保留号 |
| JSON（jsonMode，事故保留项） | `{"kind":14,"convId":123,"latestSeq":8842,"reason":0,"memberCount":500}`；handler 允许 camelCase / snake_case 双形态解析，不出新形态 |
| 大小 | protobuf ≈24B、JSON ≈70B（v1.2 §1.1 口径），允许 ±10B；一帧一条，禁止拼帧合批 |
| `latest_seq` | 只能引用**已完成 seq 分配且 group-commit 落库、920/8 已可读**的值；客户端按 `[lastSeq+1, latest_seq]` 拉取必须非空、不 404。生成点在 flusher 提交之后 |
| `reason` | `0`=批次/时间推进、`1`=该 conv 首条水位线、`2`=保留槽位。**1.4 明确不为回滚补发 reason=2 帧**：回滚就是停止发 14，否则破坏 S-7 逐字节一致。客户端逻辑不得依赖 reason 取值 |
| `member_count` | 仅展示/诊断；客户端不得据此切换行为（v1.2 §1.2 硬约束继续有效） |
| 序 | seq 未推进绝不发；重复/迟到（`latest_seq <= lastSeq`）由客户端 `pull-scheduler` 丢弃；心跳仍走 4/5，不混用 |

### 1.2 生成点与 202 快照

- 服务端水位线生成点从「fanout 阈值分支」改为**所有会话统一分支**：消息落库 → dispatcher → fanout 队列 → per-conv 聚合窗 → 统一产出 kind 14。`universal_pull_enabled=false` 时整个统一分支不执行，回到 v1.2.1 的 `len(members) > pull_mode_threshold(100)` 判定路径。
- **取消阈值交付分流 ≠ 取消 202 快照保护**。202 `CONV_PAYLOAD` 仍按固定成员数判定：`>100` 人 `members_omitted=true` + `member_count`（字段 7/8）+ 930/931 分页成员拉取；`≤100` 人保持全量快照（12~16KB，64KB 帧上限内安全）；201 CONV_UPDATE 低频小帧不受影响。该判定不得再读 `pull_mode_threshold`。
- 群人数默认上限提到 1000，硬上限保留（64KB 帧），容量由控制器与 §6 门槛守门；任何越过容量上限的场景按 fail-closed 拒绝，不得靠估算放宽。
- v3 叠加时（§1.5）：水位线作为 mailbox 的**指针帧**条目（`v3-bus-design.md` §2.4），`mailbox_seq` 与 conv seq 分离；大群消息本体不进 mailbox（避免 O(N) 复活），小群 mailbox 可携带数据帧。小群实时性由自适应 10ms 水位线保证，回执/控制帧 mailbox 有序，typing/presence 保持无序旁路。
- 「小群 mailbox 携带数据帧」是 v3 总线 per-uid 单写者有序日志的原生投递，**不等于** legacy inline 逐成员 `Conn.Write`；两者严禁同时写入同一连接造成双投递窗口。客户端 `engine.ingest` 仍是唯一写入口，重复到达靠既有去重/gap 缓冲闭合。

### 1.3 普遍模式下的水位线频率

每会话维护 `lastWatermarkSeq` / `lastWatermarkAt`（内存加速态，重启最多多发一条）。参数改为每会话 `effective[cid] = {watermark_batch, watermark_interval_ms}`，由 §2 控制器给出；控制器关闭/失败时回退静态默认值。

1. **推进触发**：`latest_seq - lastWatermarkSeq >= batch` → 立即产出水位线（合并进既有 ≤100ms per-conv 聚合窗）。
2. **时间触发**：`now - lastWatermarkAt >= interval` 且 seq 有推进 → 产出水位线（防饿死：低速率会话感知延迟 ≤ interval + 1 条消息）。
3. **无推进不发**：seq 未变 / 迟到低 seq → 0 条；首条水位线 reason=1。
4. **上界（可复算）**：单会话每秒水位线 ≤ `⌈msg_rate / batch⌉ + ⌈1000 / interval_ms⌉`；每会话独立，不做全局拼帧。
5. **小群 / 大群**：无拥塞时 interval 加性减小趋近硬下界 10ms、batch 趋近 1，实时性接近 inline；拥塞时 interval 乘性增大趋近硬上界 2000ms、batch 趋近 200，聚合通知。**不允许用成员数直接选参数**，只允许用 §2.1 的实时信号。
6. 客户端侧参数**协议零改动**：`jitter ∈ [0, 2000ms)`、`pull_min_interval = 1000ms`、`8 SYNC {conv_id, since_seq=lastSeq+1, limit=200}`、singleflight 键 `pull:<convId>`、`pending` 上限 1000；v1.2 §2.2 的「前台已知落后 >10s 兜底轮询」在规划 §2 中声明已实现，但仓库 v1.2.1 的 `pull-scheduler.ts` 仅落盘 singleflight / jitter / min_interval / pending 四项——**本规范不补代码**，把「10s 兜底轮询是否已接线」列为 CORE-2 启用前必须复核的前置项；未落实不得改写成服务端新行为。

### 1.4 客户端收敛路径（不变式）

`handleWatermark(14) → pullScheduler.onWatermark`：`latestSeq <= engine.lastSeq` 丢弃；未加载会话记 `pendingWatermark`；否则 `schedulePull` 合并同 conv in-flight 并更新 `targetSeq`（追赶语义）。拉取经 8 SYNC，服务端 `onSyncRequest` 按现状逐条以 102 MSG_NEW 重发，经既有 dispatch → `engine.ingest` 收敛；`pull-scheduler` **只产生请求，绝不直接写 cache/sink**。与 920 beforeSeq 历史拉取方向/游标/singleflight 键分离，920 结果同样经 `engine.ingest`，历史 seq 不回退也不推进 `lastSeq`。

### 1.5 与 v3 总线的叠加（边界）

| 帧/会话 | 普遍拉取（v3 off） | v3 on（灰度 uid） | 禁止 |
|---|---|---|---|
| 水位线 14 | fanout 统一产出，按 uid 投递 | mailbox 的**指针帧**条目，按 `mailbox_seq` 有序，每 conn 独立 cursor/credit | 同一 uid 同时走旧直投与 mailbox（双投递窗口） |
| 消息 102 | 小群也不再 inline 推送；8 SYNC 拉取经 `engine.ingest` | 小群 mailbox 可携带数据帧（自适应 10ms 水位线），大群仍只带指针帧 | 把大群数据帧铺进 mailbox 复活 O(N) |
| 回执/控制 | 既有路径 | mailbox 有序；kind 15 累积 ACK 推进 credit | ACK 通道与扇出共抢出路（v1.2 根因） |
| typing/presence | 无序旁路 | 无序旁路（低价值），不进 mailbox 强排序 | — |
| 保命帧 4/5/10 | 不占 credit | PAUSED 下仍可达，不占 credit | — |

切换语义：`v3_bus_enabled` 按 uid 灰度（5%→25%→50%→100%），**一次性切换**——同一 uid 的新连接自某时刻起只走 mailbox，旧直投在该 uid 上关闭；不允许「两路径同时写同一 uid」。回滚见 §5。

## 2. 自适应 AIMD 控制器（伪代码与边界）

### 2.1 输入信号（全部已有计数器，不新增探针）

控制器每 `control_period`（policy 配置项，机制层已有 ticker 周期，不新增探针）读一次**同一组计数器**；先证明前提、再证明收益，二者必须是同名计数器：

| 信号 | 已有来源 | 用途 |
|---|---|---|
| 扇出队列深度 / 丢弃计数 | `FanoutStats.Enqueued / DroppedQueue / DroppedReceipts / OutboundDropped`、`Egress.Stats()[].Len / Cap / DroppedBestEffort / NACKed`、`Forwarder.Stats().Retry / Dropped` | soft/firm 判定、拥塞检测 |
| ACK 率 | `bridge/ack.Tracker` 的 in-flight / 重发 / 判死与 `delivery.Stats().Inflight`、`Egress.Stats()[].NACKed`、压测 `opAgg.Acked/Attempted`（`scripts/stress/report.go`）推导；灰度期 kind 15 cursor 推进同样计入 | >98% / <95% 双阈值 |
| p95 | `scripts/stress/report.go` 的 `percentile(samples, 0.95)`（与 S-1~S-7 同一 p95 计数器口径） | 拥塞旁证 + S-6 抖动守门 |
| 在线连接数 | SIGUSR1 dump `status.conns` / `Session().Count()` | 负载阶跃检测 |
| 每会话消息速率 | `FanoutStats.Enqueued` 增量 / 已落库 seq 增量（机制层既有计数，按 cid 聚合） | 判定是否逼近 `batch` 推进触发 |
| credit PAUSED 数 | `delivery.Stats().PausedConns / Backpressured`（v3 off 时为 0） | 拥塞的直接证据 |

**前提证明**：启用普遍拉取前，同一计数器必须显示 inline 在 1000×50 人群 14~24% 可用性、ACK 通道受挤；**收益证明**：启用后同一计数器在 S-2 达 ≥90%、`Conn.Dropped` 数据帧 = 0。任一不成立即停留在 flag-off 中间态。

### 2.2 控制量、硬界与拒载

`effective[cid]` 只有两项，均为整数：

```
watermark_batch        ∈ [1, 200]       硬界（规划 §3）
watermark_interval_ms  ∈ [10, 2000]     硬界（规划 §3）
```

- **越界拒载**：SIGHUP / 911 / 管理 API / 控制器给出的值不在硬界内，或类型无法归一化为 int（复用 `featureflags.ToInt` 语义），一律拒绝并保留上一次 `effective[cid]`；必须同时写审计事件与 SIGUSR1 dump（拒绝也记录）。
- **静态默认值**：控制器关闭、panic、旁路失败时的确定行为 = v1.2.1 tag 的 Defaults：`watermark_batch=50`、`watermark_interval_ms=2000`（`watermark_jitter_ms=2000` 仍只作压测/诊断下发，机制层不读它做频控）。`universal_pull_enabled` 是独立总开关，`adaptive_control_enabled` 是策略层开关；两者都 off 时走 v1.2.1 静态行为。
- 参数**每会话独立**、不写入全局 flag 表；机制层只从 `effective[cid]` 原子快照读两个值，不调用控制器、不判拥塞、不写审计。
- SIGHUP / 管理 API 静态覆盖优先于控制器输出：覆盖值同样受硬界约束，并落审计事件；撤销覆盖后控制器在下一控制周期恢复输出（不得跳变超 25%/1s 规则，见 §2.3）。

### 2.3 AIMD + 双维伪代码

「双维」= A（无拥塞）加性减 interval、M（拥塞）乘性增 interval，同时另一维 `batch` 与 interval **同向**调整（interval 减则 batch 减，interval 增则 batch 增），避免一个维度抵消另一个；两维都受同一速率限制与冷却。窗口用已有计数器的**增量**，`soft` / `firm` / `control_period` / 加性步长 / 乘性系数均为 policy 配置项（不在规范里新造常量）。

```
// 每会话状态：eff = {batch, interval}; 上一控制周期的信号快照 prev; 上次变更时刻 lastChangeAt
// ACK_RATE_NO_CONGESTION = 98% ; ACK_RATE_CONGESTED = 95% （规划 §3 给定）
// BATCH_MIN=1, BATCH_MAX=200, INTERVAL_MIN_MS=10, INTERVAL_MAX_MS=2000
// RATE_LIMIT = 25%（每次调整 ≤ 当前值 25%）, COOLDOWN = 1s（规划 §3 给定）

onControlTick(now, signals[cid], global):
    if !adaptive_control_enabled:
        eff[cid] = staticDefaults()                 // 50 / 2000，确定行为
        return
    if hasStaticOverride(cid):
        eff[cid] = checkedStaticOverride(cid)       // SIGHUP / 管理 API；越界拒载
        return
    if now - lastChangeAt[cid] < COOLDOWN: return   // 冷却，防振荡

    qDepth   = signals[cid].queueDepth              // 已有 Fanout/Egress/Forwarder 计数增量
    qDropped = signals[cid].droppedDelta
    ackRate  = signals[cid].ackRate                  // 已有 ACK Tracker / kind15 cursor 推导
    paused   = global.pausedConns + global.backpressured
    p95      = signals[cid].p95                   // 与 S-6 同一 p95 计数器
    // p95NotRegressed = (p95 <= 2 * steadyP95)，steadyP95 来自同场景稳态窗口
    congested = (qDepth > firm) || (qDropped > 0) || (ackRate < 95%) || (paused > 0)
    healthy   = (qDepth < soft) && (ackRate > 98%) && (qDropped == 0) && (paused == 0) && (p95NotRegressed)

    old = eff[cid]
    if congested:                                  // M：乘性增大（大群聚合到 2s）
        interval = ceil(old.interval * beta)     // beta>1，policy 给出
        batch    = ceil(old.batch    * beta)
    else if healthy:                              // A：加性减小（小群趋近 10ms，实时性接近 inline）
        interval = old.interval - alphaMS         // alphaMS>0，policy 给出
        batch    = old.batch    - alphaBatch
    else:
        return                                    // 中间带/信号不足：保持，不振荡

    // 变更速率限制：每次 ≤ 当前值 25%（两维分别判；任一超界即整体钳到边界）
    interval = clampRate(old.interval, interval, 0.25)
    batch    = clampRate(old.batch,    batch,    0.25)
    interval = clamp(interval, 10, 2000)
    batch    = clamp(batch,    1,  200)
    if interval == old.interval && batch == old.batch: return

    if !validateHardBounds(batch, interval):        // 越界拒载
        audit("adaptive.reject", cid, old, batch, interval, reason="out_of_bounds")
        return
    eff[cid] = {batch, interval}                  // 原子发布，机制层下一窗读到
    lastChangeAt[cid] = now
    audit("adaptive.change", cid, old, eff[cid], signalsSnapshot)   // 可回放
    dump["adaptive"][cid] = {old, new, signalsSnapshot, at, override=false} // SIGUSR1 dump
```

补充约束：

- `congested` 优先于 `healthy`；冲突信号（如队列低但 ACK 率 <95%）按拥塞处理。计数器缺失/窗口样本不足 → 保持现值，不猜、不新增探针。
- 「每次调整 ≤ 当前值 25%」对减与增双向生效，且 `clampRate` 先于硬界 clamp；在 10ms / 2000ms 边界允许只移动一维，另一维保持。
- 冷却 1s 是**每会话**的，不是全局闸门；静态覆盖/回滚动作的生效不受冷却延迟（但覆盖值必须合法），否则紧急回滚会被策略层拖住。
- 所有参数变更写入 **SIGUSR1 dump（`status_dump` 的 `adaptive` 子表）+ 审计事件**，字段含 `cid / old / new / 触发信号快照 / at / override / version`，可离线回放；审计 detail 只放 id/枚举/数值，**禁止放消息内容**（沿用 `pipe.audit` 约束）。
- **控制器 panic 不影响投递**：策略层旁路失败（panic / 超时 / 无信号）→ 机制层继续用最后一次合法 `effective[cid]`，连续失败则回退静态默认值并写 `adaptive.fallback` 审计；绝不因策略层异常丢数据帧或改 seq。
- 控制器不得读写 DB、不得发帧、不得改 flag；纯函数式 `(signals, prev) → eff`，便于 CORE-1 单测注入假时钟/假计数。

### 2.4 前提/收益的可测断言（同一计数器）

| 断言 | 口径 |
|---|---|
| T-AD-1 | 注入无拥塞信号（队列 < soft、ACK >98%、无 PAUSED）→ interval 单调下降、batch 单调下降，最终分别钳在 10ms / 1，不击穿硬界 |
| T-AD-2 | 注入拥塞信号（队列 > firm 或 ACK <95%）→ 两维乘性上升，最终钳在 2000ms / 200；再次转健康可回落，不死锁 |
| T-AD-3 | 任一维单次变更 ≤ 旧值 25%；1s 内重复 tick 只有一次变更；边界处另一维可不动 |
| T-AD-4 | 越界值（0 / 201 / 2001 / 非整数字符串）→ 拒载，`effective` 保持旧值且各产生一条 `adaptive.reject` 审计 |
| T-AD-5 | 控制器 panic / 静态覆盖 / `adaptive_control_enabled=false` → 回退静态默认 `50/2000`，投递帧数、seq 与 flag-off 基线一致 |
| T-AD-6 | 每次变更在 SIGUSR1 dump 与审计事件各出现一次，字段可回放；审计 detail 无消息正文 |
| T-AD-7 | 前提/收益使用同一计数器名与窗口：inline 侧 1000×50 场景显示队列丢弃/ACK 回退，普遍拉取侧 S-2 ≥90% 且 `Conn.Dropped` 数据帧 = 0 |

## 3. 与 v1.2 客户端的兼容矩阵

**基线定义**：v1.2.1 tag（服务端默认 `pull_mode_threshold=100`、客户端 kind 14 handler + `pull-scheduler.ts` 已落盘、v3 代码在位但 `v3_bus_enabled=false`）。v1.2.0 客户端已有 kind 14 / pull-scheduler，但无 kind 15/16/17 与 `mailbox` 游标状态机；v1.1.1 及更早客户端**无 kind 14、无 pull-scheduler**。`Hello.capabilities` / `client_version` 在 proto 中已存在但仓库未用作可靠版本门（v1.2 规范明文：客户端 capability 不作为服务端开启条件），因此 1.4 **不引入新 capability 协商**。

| 客户端 | 收普遍模式 kind 14（所有群） | 8 SYNC / 920 拉取与 202 快照 | v3 mailbox（kind 15/16/17、mailbox_seq、credit PAUSED） | 1.4 处置 |
|---|---|---|---|---|
| **v1.2.1（最低兼容版本）** | ✅ 已注册 `handleWatermark` → `pullScheduler.onWatermark`；`latestSeq<=lastSeq` 丢弃、未加载记 pending、singleflight + jitter 0~2000ms + min_interval 1000ms、8 SYNC limit=200（10s 兜底轮询状态见 §1.3 第 6 项注） | ✅ 8 SYNC 逐条 102 replay 经 `engine.ingest`；920 beforeSeq 历史不回退 `lastSeq`；202 `lastSeq` 触发补拉；>100 人 `members_omitted` 由忽略未知字段得到空成员列表，成员按需 930/931 | ✅ 已注册 15/16/17 handler + `mailbox.ts` 游标状态机 + `mockBridge` 端到端测试 | **全功能目标客户端**，零协议改动。服务端只允许对已确认具备此行为的连接/内部账号启用普遍拉取；中间态用压测与内部账号验证，不靠服务端猜版本 |
| **v1.2.0** | ✅ 有 14 + pull-scheduler（同 v1.2.1 的拉取语义） | ✅ 同上 | ❌ 无 15/17 handler、无 mailbox 游标状态机；未知 kind 由 dispatch 计数丢弃（不崩） | 普遍拉取可用；**不得进入 `v3_bus_enabled` 灰度桶**（按 uid 灰度隔离 / 名单排除），否则 mailbox ACK 不推进 credit → PAUSED 判死 |
| **v1.1.1 及更早** | ❌ 无 14 handler、无 pull-scheduler；kind 14 被 dispatch 当作 unhandled 计数丢弃，仅 RESUME / 202 快照 `lastSeq` 补拉可用，无法被 14 唤醒（10s 兜底轮询见 §1.3 注） | ⚠️ 202 `members_omitted` 未知字段被忽略 → 等价「成员列表为空」（现状大群快照超 64KB 被拒，非回退） | ❌ 同左 | **不支持普遍拉取**。小群也不再 inline ⇒ 会漏消息直到重连 RESUME / 前台按会话列表 `lastSeq` 补拉。启用普遍拉取前必须：客户端先行发布覆盖 / 强制升级，或按连接/uid 隔离到 v1.2 阈值路径；S-7 回滚后此类客户端行为与 v1.2.1 完全一致 |
| **压测客户端 `scripts/stress`** | ✅ `--pull-mode` 下 kind 14 → 8 SYNC → 收敛延迟采样（`pull.go` / `pull_test.go`） | ✅ 拆帧统计：下行水位线 vs 消息 vs 控制帧、920/8 QPS、收敛 p95 | ⚠️ 本规范不要求压测端模拟 mailbox cursor；v3 灰度压测归 CORE-3/REL | S-6/S-7 必须用 `--pull-mode` 跑，否则人为复现「压测端不发 SYNC」式死锁 |
| **服务端回滚态** | 0 条 kind 14（或仅 v1.2.1 阈值路径的原样 14） | 逐成员 102 原样 | `hub.Offer` no-op / 旧直投唯一路径 | 见 §5；任何客户端都回到 v1.2.1 的既有行为 |

**兼容铁律**：

1. 普遍拉取对 v1.2.1 客户端是**协议零改动**：不得新增 kind、不得改 `Watermark` 字段号、不得改 Envelope、不得让客户端按 `member_count` 或 `reason` 切换行为。
2. 服务端 **feature flag / policy 覆盖不得下发成客户端新行为**；`watermark_jitter_ms` 继续只作压测/诊断读取。
3. 旧客户端收到 14 时的「静默丢弃」是 dispatch 既有行为，不得用服务端广播新帧去补偿；兼容性风险只能通过客户端先行发布 + 灰度隔离解决。
4. v3 灰度按 uid 哈希（同 uid 稳定落同一路径，非按连接随机）；旧版本客户端不进灰度桶，回滚 = 灰度名单回退 + `v3_bus_enabled=false`。
5. `kinds.ts ↔ kinds.go` 一致性检查常驻 CI（14 / 15 / 16 / 17 一一对应），避免重演 kind 14 无 handler 被回滚的事故。

## 4. S-6 / S-7 可测试断言

> 断言均按「先证明前提、再证明收益、同一计数器」写入；「逐字节」= 逐帧 Envelope 的字节序列（kind / conv_id / target uid / payload / request_id）与基线比对；**不跑 build/test** 是本次 SPEC 批次的纪律，以下是交给 CI/REL 批次的可执行判据。

### 4.1 S-6 自适应振荡（规划 §6）

场景：负载阶跃 3 次（无拥塞 → 拥塞 → 无拥塞，或 3 次独立阶跃注入），持续时间覆盖 ≥3 个控制周期；用 §2.1 的同一组计数器记录每周期输入与 `effective[cid]` 输出。

| ID | 可测试断言 |
|---|---|
| S6-1 | 每次阶跃后 `effective[cid]` 在有限周期内**收敛**：连续 N 个控制周期（N 由 policy 给出）内 `interval` / `batch` 的符号不再反复翻转；收敛值落在 `[10,2000]` / `[1,200]` |
| S6-2 | 任一周期两维的单次变化 `abs(new-old) <= 0.25*old`；任意两次参数变更间隔 ≥1s（冷却）；硬界处不出现越界值 |
| S6-3 | 3 次阶跃内 p95 采样最大值 ≤ `2 ×` 同场景稳态 p95（规划「不出现 >2× 抖动」）；`Conn.Dropped` 数据帧 = 0；无 5xx |
| S6-4 | 每个 `adaptive.change` / `adaptive.reject` / `adaptive.fallback` 事件在 SIGUSR1 dump 的 `adaptive` 子表与审计事件各出现一次，含旧/新值、信号快照、时间戳，可按序回放复算出同一 `effective[cid]` |
| S6-5 | 注入控制器 panic / 静态覆盖 → 参数回退静态默认 `50/2000`，投递帧数、seq 分配、可用性不劣于关闭控制器的对照运行 |
| S6-6 | 前提/收益证据：同一 `FanoutStats` / `Egress.Stats` / `AckTracker` / p95 计数器在基线（阈值拉取）与普遍拉取下各记录一份，S-2 达 ≥90% 且队列丢弃/ACK 回退计数不增长 |

### 4.2 S-7 flag 回滚（规划 §6）

配置：`universal_pull_enabled=false` + `v3_bus_enabled=false`（并置 `v3_bus_percent=0`），与 **v1.2.1 tag 默认配置**（`pull_mode_threshold=100`、`watermark_batch=50`、`watermark_interval_ms=2000`、`watermark_jitter_ms=2000`、v3 off）同负载同 seed 双跑。

| ID | 可测试断言 |
|---|---|
| S7-1 | 下行帧逐字节一致：≤100 人群（含 99 人群回归用例）逐成员 102 的帧数、顺序、target uid、payload、request_id 与 v1.2.1 逐字节相同；101/>100 人群的 kind 14 字段/频控/扇出收件人与 v1.2.1 逐字节相同 |
| S7-2 | `universal_pull_enabled=false` 生效后不再产生「所有会话统一分支」的水位线；自适应控制器不写 `effective[cid]`、不产生 `adaptive.change` 审计；**回滚不补发 reason=2 帧**（补发即破坏逐字节一致） |
| S7-3 | v3 侧：`hub.Offer` no-op / 灰度双写旁路零帧；无 kind 15/16/17 下行、无 mailbox 表写入、无 credit PAUSED；10/4/5 保命帧行为不变 |
| S7-4 | 消息落库与 seq 分配逐字节同基线：同一输入下 `seq` 序列、920 READ_HISTORY 与 8 SYNC 应答内容、202 `CONV_PAYLOAD`（含 `members_omitted` 判定）完全一致；`engine.ingest` 唯一写入口不新增路径 |
| S7-5 | 计数器对照：`FanoutStats.Enqueued/DroppedQueue/OutboundDropped`、`Egress.Stats`、`Forwarder.Stats`、`Conn.Dropped`、`Session().Count()`、SIGUSR1 dump 的 `flags_ver` 及 99 人群回归用例（v1.2 T-SV-1）全部与基线一致或在确定性允许范围内；`flags_ver` 增量本身不得引起行为差异 |
| S7-6 | 回滚/再启用热改（SIGHUP / 911 / 管理 API）各跑一次：切换窗口内无双投递（同一 uid 同一帧不出现两份）、无 gap、无重复 seq；v1.1.1 客户端（无 14 handler）在回滚态不再因普遍模式收到小群 14 帧，>100 人群仍按 v1.2.1 阈值路径原样发 14 并被静默丢弃，与基线一致 |
| S7-7 | S-1~S-7 全量压测前，99 人群逐字节回归用例在 flag-off 中间态**常驻**；任一档指标回退即停留在上一档并开 issue（继承 v1.2 §6.4 灰度纪律） |

### 4.3 其余门槛的引用（不重述）

S-1（500 人群 × 1000 在线：可用性 ≥95%、p95 ≤100ms、`Conn.Dropped` 数据帧 = 0）、S-2（1000 × 50 人群 ≥90%）、S-3（512 × 10 人群 ≥99% / p95 ≤80ms 不劣于 v1.1.1）、S-4（慢消费者 50/1000：其余 950 ≥99%、慢连接重连续传无 gap）、S-5（重启风暴：续传 QPS ≤800、无 5xx、cursor 不回退）按规划 §6 原文执行；本规范只补充它们与 S-6/S-7 共用同一组计数器与 `--pull-mode` 压测端。

## 5. 回滚开关清单

**总原则**：开关是热改（SIGHUP / 911 FEATURE_FLAGS / 管理 API），不重启、不改代码；每个开关必须能单独把行为拉回 v1.2.1 的确定性形态。静态覆盖与自适应关闭时一律回退 v1.2.1 静态默认值 `watermark_batch=50` / `watermark_interval_ms=2000`。

| # | 开关 / 覆盖 | 默认（中间态） | 开启后行为 | 回滚动作与验收 |
|---|---|---|---|---|
| 1 | `universal_pull_enabled` | **false** | 所有会话统一水位线分支；取消 `pull_mode_threshold` 交付分流语义（202 快照仍按 `>100` 固定保护） | 置 false → 立即回到 v1.2.1 `len(members)>100` 阈值路径；**不补发 reason=2**；S7-1/S7-2 常驻 |
| 2 | `pull_mode_threshold` | **100**（v1.2.1 静态默认，回滚路径专用） | 普遍模式开启期间该 flag 不再决定交付分流；**保留仅为 S-7 回滚基线** | 回滚态读它；设 `0` = 全量 inline（v1.2 事故期的二级逃生开关，逐字节同 v1.1.1 的 inline 分支）；不得在普遍模式启用期用它灰度 |
| 3 | `adaptive_control_enabled` | **false** | 每会话 AIMD 输出 `effective[cid]` | 置 false → 冻结/清空 `effective[cid]`、回退 `50/2000`；控制器 panic 同路径；T-AD-5 / S6-5 |
| 4 | 静态覆盖（SIGHUP / 911 / 管理 API 写入 `watermark_batch` / `watermark_interval_ms`） | 无覆盖 | 覆盖值优先于控制器，仍受 `[1,200]` / `[10,2000]` 硬界与越界拒载 | 删除覆盖（nil 语义回默认）→ 下一控制周期恢复控制器输出；撤销/设置均写审计 + dump |
| 5 | `watermark_jitter_ms` | **2000**（仅压测/诊断下发） | 客户端 jitter 上界不变；服务端不得拿它做频控 | 保持 `2000`；压测摊平拉取风暴时可热调，但不得改变客户端协议语义 |
| 6 | `v3_bus_enabled` | **false** | 灰度 uid 走 mailbox（水位线作指针帧）、旧直投在该 uid 一次性关闭 | 置 false → `hub.Offer` no-op、旧直投唯一路径、mailbox 表冻结不删；S7-3 / v3 T-SV-5 |
| 7 | `v3_bus_percent` | **0** | 5%→25%→50%→100%，按 uid 哈希分桶，每档 S-1~S-6 守门 | 回退到上一档或 `0`；同 uid 稳定落同一路径；旧客户端不进桶（§3）；任一档回退即停档开 issue |
| 8 | `delivery_credit` / `mailbox_ack_window` / `slow_consumer_close_s` / retention 策略 | v3 设计默认值（512 / 128 / 60s / 24h+7d） | credit 流控、PAUSED 判死、compaction | 属 v3 §6 既有开关，不在普遍拉取规范内新造；回滚时一并保持默认，避免双路径参数漂移 |
| 9 | 客户端编译期 `enablePullMode`（v1.2 规范 §5 列为开关，v1.2.1 代码中未落盘） | 由客户端发布决定 | 关闭后客户端调度器静止，服务端即使误发 14 也不产生 8 SYNC | 与服务端 flag 无关的最后一道逃生；**启用前须核实该开关/等效静止手段是否已落盘**，不得作为普遍拉取的启用依据 |
| 10 | 群人数上限（policy `group.max_members`，硬上限 1000 / 64KB 帧） | 1.4 目标默认 1000（当前 policy 默认 100，属 CORE/DOC 批次实施项） | 容量由控制器与 §6 门槛守门 | 压测不过 → 回退到 v1.2.1 的 `100` 并 fail-closed；不得靠估算放宽 |

**回滚次序（一次操作完成，杜绝双投递窗口）**：`v3_bus_percent→0` / `v3_bus_enabled=false` → `universal_pull_enabled=false` → `adaptive_control_enabled=false` + 清静态覆盖 → 复核 `pull_mode_threshold=100` → 复核 S7-1~S7-7。**不删 mailbox 表、不删数据、不补发帧**；消息本体与 seq 始终在库里，只换通知方式，这是回滚安全的根基。

## 6. 明确不做（规范边界）

- ❌ 新增 kind / 改 `Watermark` 字段号 / 改 Envelope / 把多条水位线拼帧合批（protobuf wire 跨帧不自描述）；
- ❌ 服务端按客户端版本或 capability 猜测行为；新增 capability 协商；
- ❌ 按成员活跃度差异化推送、后台会话兜底轮询、按成员数直接选频控参数；
- ❌ 控制器直接读写 DB / 发帧 / 改 flag / 新增探针；策略层异常影响投递；
- ❌ 为拉取模式新建服务端消息缓存或第二套 `engine.ingest` 写入口；
- ❌ 同一 uid 长期双投递：100% 灰度后一个版本周期删除旧直投代码（1.4 内完成），LTS 不留双路径；删除前旧直投只作 `universal_pull_enabled=false` / `v3_bus_enabled=false` 的回滚路径；
- ❌ 本 SPEC 批次改任何代码、flag 默认值、proto、测试、CI 或部署配置；不跑 build/test、不装依赖。

## 7. 批次交接（非本文件实现）

| 批次 | 从本规范取什么 |
|---|---|
| CORE-1 | §2 纯策略层 + T-AD-1~7（`bus/adaptive/`，新增单测，flag 默认关） |
| CORE-2 | §1.2/§1.3 统一分支 + `effective[cid]` 读取 + 202 固定保护（`data/pipe/`，`universal_pull_enabled` 默认关，逐字节回归常驻） |
| CORE-3 | §1.5 / §5 一次性切换 + kind 15 上行 ACK 路由 + 背压统计（`cmd/chatd/` + `user/app/`） |
| REL | §4 S-6/S-7 + 规划 §6 S-1~S-5（含 `--pull-mode` 压测端、同一计数器前提/收益证据、审计/dump 回放）→ 打 v1.4.0 |
| DOC / OPS / FE | 与本规范无文件域交集；容量表、known-issues、CHANGELOG 按规划 §5/§7 各自推进 |
