# 大群拉取模式规范（v1.2 · Pull Mode）

> 依据：`perf-spec-v1.2.md` §4 早期设想 + `perf-postmortem-v1.2-draft.md` 事故教训 + 代码核实。
> 所有指标以 **2 核 4 GB** 为参考硬件。本规范是规范，不改代码；所有"现状"均已在仓库中核实（引文件行号处为核实点）。
>
> **铁律（从事故继承，违反即重演）：**
> 1. **客户端先行**。服务端任何水位线发送逻辑，必须在客户端 handler 合入、单测与 mock 端到端验收通过之后才能启用。服务端代码可以提前合入，但**必须处于 flag 关闭态**（见 §6.4 中间态）。
> 2. **新 kind 双端同 PR 可验证**。`kinds.ts`（客户端）与 `kinds.go`（服务端）必须同步登记；kind 14 曾因客户端无 handler 被回滚（`kinds.go` 30-31 行注释），本规范复用 14 号时必须以"客户端 handler 已有测试"为前置条件。
> 3. **指标守门**。压测可用性 / P95 / `Conn.Dropped` ACK 丢失计数任一回退即阻断发布。

## 0. 现状与问题（已核实，不重新调研）

### 0.1 容量事实

| 场景 | 帧需求 | 可用性 | 结论 |
|---|---|---|---|
| 512 连接 × 10 人群 | ~35k 帧/s | 98.1% | inline 在预算内 |
| 1000 连接 × 50 人群 | ~110k 帧/s | 23.7% | 已超预算 |
| 1000 连接 × 500 人群 | 250k 帧/s（需求） | 2.2% | 扇出爆炸，**inline 物理不可行** |

帧扇出预算 ≈ **40,000 帧/s**（`perf-spec-v1.2.md` §0.2 瓶颈 B）。500 人群 × 1000 在线 × 0.5 msg/s 的写路径仅 500 msg/s（瓶颈 A 不触发），唯一出路是**把每条消息的 O(在线成员) 次推送换成 O(1) 次水位线 + 客户端按需拉取**。

### 0.2 已有的可用积木（全部已实现，本规范只做接线）

| 积木 | 位置 | 状态 |
|---|---|---|
| 920/921 READ_HISTORY，`since_seq` + `before_seq` 双方向 | `proto/chat.proto:532`；`pipe/wire/wire.go:72-78`（BeforeSeq）；`pipe/readrouter.go:43,95`（降序读路径 + 测试） | 服务端已落盘 |
| kind 8/9 SYNC 补拉（conv_id/since_seq/limit → 逐帧 Envelope） | `proto/chat.proto:79-90` | 已实现 |
| 客户端 sync 引擎：seq 收敛 / gap 缓冲 / tombstone / 去重 / single-writer `ingest` 唯一写入口 | 客户端侧实现（前端已移出仓库；契约见 client-contract.md §3） | 契约不变量：**不得破坏** |
| 客户端 history-loader：三段式 + singleflight + exhausted + stale 丢弃 | 客户端侧实现 | 已实现（契约） |
| feature flag 机制：原子快照 + FEATURE_FLAGS(911) 广播 + SIGHUP 热重载 | `server/internal/pkg/featureflags/flags.go` | 已实现 |
| fanout 异步扇出面：per-conv 100ms 聚合窗 + broadcaster workers + 有界丢弃 | `server/internal/data/pipe/fanout.go` | 已实现，水位线挂接点 |

### 0.3 大群下的 202 快照问题（一并解决）

`ConvPayload`（`proto/chat.proto:229`）含 `repeated Member members`。JSON 形态下每成员约 120–160 字节（user_id/role/joined_at/muted_until/字段名），500 人 ≈ **60–80 KB，直接超 64KB 单帧上限**；protobuf 形态约 30–40 KB，逼近上限且随成员数线性增长。**大群绝不能继续推全量 202 事件**，替代方案见 §3.4。

## 1. 协议设计

### 1.1 水位线帧：新增 kind 14 `WATERMARK`

14 号在 RESYNC 回滚后重新未分配（`kinds.go:30-31` 注释明确），处于系统层 0-99 号段，语义归类正确。**登记约束**：`kinds.go` 增 `KindWatermark Kind = 14` + `register(KindWatermark, "WATERMARK")`；`apps/client/src/net/protocol/kinds.ts` 同步增加 `WATERMARK = 14`。双端常量一致性由 CI 既有检查守住（事故补充教训第 1 条）。

**Protobuf 形态**（`proto/chat.proto` 新增消息，字段只用 1-4，5-9 保留——与本文件所有消息的惯例一致）：

```protobuf
message Watermark { // 14 服务端 → 客户端，拉取模式会话的最新 seq 通知
  uint64 conv_id    = 1; // 会话 ID
  uint64 latest_seq = 2; // 该会话当前已分配的最大 seq（落库后必已可拉）
  uint32 reason     = 3; // 0=周期(批次推进) 1=重连/首次 2=flag切换回 inline 前最后一次
  uint32 member_count = 4; // 发送时会话成员数（客户端展示与诊断用，不参与逻辑）
  reserved 5 to 9;
}
```

**JSON 形态**（控制帧遵守 jsonMode，事故保留项）：

```json
{"kind":14,"convId":123,"latestSeq":8842,"reason":0,"memberCount":500}
```

字段语义约束（可测试）：
- `latest_seq` 只能引用**已完成 seq 分配且已可被 920/8 读到**的值（即在落库提交之后生成，§3.1）。客户端拉取 [lastSeq+1, latest_seq] 必须非空、不 404。
- `reason` 仅诊断与测试断言用，客户端逻辑不得依赖其取值。
- 帧大小：protobuf ≈ 24 字节，JSON ≈ 70 字节。**这是"20 字节水位线"设想的落地形态**，允许 ±10 字节。

### 1.2 阈值切换规则

```
len(members) <= pull_mode_threshold (默认 100，flag 可调): inline push（现状不变）
len(members) >  pull_mode_threshold:                       拉取模式
```

**取 100 的依据（数字可复算）：**
- inline 模式的帧需求 = Σ(每会话 在线数 × 该会话消息速率)。预算 40,000 帧/s。
- 目标场景 0.5 msg/s：100 成员 ≈ 50 帧/s/会话（全员在线），单会话余量充足；但 1000 连接均匀分布在 10 个 100 人群时总需求 = 1000×0.5×100 / 100 × … = 5,000 帧/s，仍在预算内并留 8 倍余量给回执（104/105，回执同样走会话扇出）。
- 阈值只能看到单会话成员数，看不到「每连接同时加入的会话数」这一乘积（50 人群 1000 连接 = 23.7% 正是乘积超预算）。因此**阈值只做粗分流，50–100 人边界场景由压测门（§4.1 S-2/S-3）守护，压测不过则调低阈值**。这是有意为之的简单性取舍：v1.2 不做 per-conv 动态切换（见 §6.6）。
- 64KB 快照约束：≤100 成员的 202 全量快照 JSON ≈ 12–16 KB，安全；>100 才会出现 §0.3 问题，与阈值天然对齐。

切换是**按会话、按消息**在服务端发送时判定的（§3.1），客户端**不需要也不允许**根据 member_count 自行切换行为。

### 1.3 水位线发送频率（双向约束）

服务端对每个拉取模式会话维护 `lastWatermarkSeq` 与 `lastWatermarkAt`：

- **推进触发**：消息落库后，若 `latest_seq - lastWatermarkSeq >= watermark_batch`（默认 **10**），立即广播一条水位线（聚合窗 ≤100ms，合并进 fanout 既有聚合器）。
- **时间触发**：距上次水位线 ≥ `watermark_interval`（默认 **500ms**）且 seq 有推进 → 广播。
- **防饿死**：即使 seq 长期只增长 1 条/秒（低于上述两阈值），`watermark_interval` 兜底保证客户端**至多 500ms + 1 条消息的延迟**感知新消息；客户端侧另有 10s 兜底轮询（§2.2），双保险。
- **无推进不发**：seq 没变绝不发水位线（纯心跳由 kind 4/5 Ping/Pong 负责，不混用）。
- 上界可复算：500 人群 0.5 msg/s → 每 500ms 至多 1 条水位线/会话 = **2 帧/s/会话**，对比 inline 的 500 帧/s/会话，降幅 250 倍。1000 个 500 人群在线的水位线总量 ≤ 2,000 帧/s < 40k 预算。

两个参数均为 feature flag（§3.3），压测可调。

### 1.4 与 202 快照的关系

- 拉取模式会话（成员数 > 阈值）**不再向成员推送全量 202 事件**。
- `ConvPayload` 增加 `bool members_omitted = 7; uint32 member_count = 8;`（原 reserved 7 to 9 中取号，兼容旧客户端——旧客户端收到的 members 为空 + 新字段被忽略，行为等同"成员列表为空"，与现状"大群快照超限被拒"相比不是回退）。
- 成员列表获取改为按需：客户端对拉取模式会话通过 930/931 ConvDataRequest 拉取成员（分页，op 语义沿用现有会话数据域）。**v1.2 只要求"不推全量"，930 分页成员拉取若现网未实现则为前置依赖，列入实施计划 P0 检查项。**
- 会话元数据（title/version）变更仍推 201 CONV_UPDATE（低频、小帧，不受本规范影响）。

## 2. 客户端设计（先行部分）

### 2.1 水位线状态机

新增模块 `apps/client/src/net/sync/pull-scheduler.ts`（纯逻辑件，依赖注入，可单测——与 history-loader 同风格）：

```
onWatermark(w):
  1. conv 未加载 / 不在会话列表 → 记录 (convId → latestSeq) 到 pendingWatermark 表，返回
     （不触发拉取；由会话列表 hydrate 时消费）
  2. latestSeq <= engine.lastSeq(convId) → 丢弃（去重：重复水位线/乱序迟到）
  3. latestSeq > lastSeq → 进入调度（§2.2），拉取范围 [lastSeq+1, latestSeq]
```

拉取结果回到引擎的路径**只有一条**：

```
8 SYNC {conv_id, since_seq=lastSeq+1, limit=200}
  → 921 frames → 逐帧映射为 StoredMessage → engine.ingest(convId, msgs)
```

引擎不变量不动（`engine.ts` 文件头 1-5 条）：single-writer、先校验后落库、去重、gap 缓冲、tombstone 不推进 lastSeq。**pull-scheduler 只产生请求，绝不直接写 cache/sink**——这是对事故教训"第二套写入路径"的结构性防御。

收敛判定：`engine.lastSeq >= latestSeq`（水位线消费完成）；gap 无法在 [lastSeq+1, latestSeq] 内闭合时由引擎既有 `onGap` 路径走全量重放，水位线标记为 stale 丢弃。

### 2.2 惊群防护（500 人同时收到水位线）

**合流（单飞）**：每 conv 一个 in-flight 拉取，复用 history-loader 已验证的 singleflight 模式：

```
schedulePull(convId, targetSeq):
  若该 conv 已有 in-flight pull → 更新其 targetSeq（追赶语义），返回
  否则创建 pull 任务，延迟 jitter 后执行
```

**抖动（jitter）**：水位线到达后延迟 `random(0, 2000ms)` 再发 8 SYNC。500 人同批收到水位线时，拉取请求在 2 秒内均匀摊开 → 服务端瞬时拉取 QPS 上界 = 在线数/2s ≈ **500 QPS（目标场景 0.5 msg/s 下实际 ≤ 100 QPS，因为水位线本身已被 500ms 窗摊开）**，远低于 920 既有读路径容量。

**最小间隔**：同一 conv 两次拉取间隔 ≥ `pull_min_interval`（默认 **1000ms**）。高频会话（多条水位线在间隔内到达）合并为一次拉更大范围的请求——这就是"追赶语义"，保证收尾收敛而不是每条消息一次往返。

**收尾**：拉取返回 `has_more=true` 时立即续拉（不受 jitter 约束，受 min_interval 约束），直到 `lastSeq >= targetSeq` 或 `has_more=false`。

**兜底轮询**：会话处于前台且 lastSeq < 已知 latestSeq 超过 10s → 无视 jitter 直接拉（防水位线丢失饿死）。后台会话不做兜底轮询（省电，靠重连 Resume 收敛）。

### 2.3 与 sync 引擎的接线点

| 接线点 | 约束 |
|---|---|
| `engine.ingest` | 唯一写入口，pull-scheduler 与 102 MSG_NEW handler、全量重放共用，**不新增入口** |
| `engine.lastSeq` 读取 | pull-scheduler 只读快照值用于构造 since_seq；竞态无害（拉多了引擎去重，拉少了下一轮水位线/兜底覆盖） |
| `onGap` | 引擎既有路径不变；pull-scheduler 不感知 gap |
| dispatch 注册 | `apps/client/src/net/protocol/` 注册 kind 14 handler + `kinds.ts` 常量（事故根因 #2 的直接修复动作） |

### 2.4 与上滑历史加载（920 beforeSeq）的并发协调

两个方向的拉取**允许同时进行**，因为它们操作不相交的 seq 区间，靠以下规则保证不冲突：

1. **方向与游标分离**：前向用 8 SYNC（sinceSeq = lastSeq+1，游标是引擎 lastSeq）；后向用 920 beforeSeq（游标是 UI 当前最旧已加载 seq），互不读写对方游标。
2. **落库同门**：920 结果同样经 `engine.ingest` 写入；历史消息 seq < lastSeq，被引擎既有"seq <= lastSeq 且非既有 seq 变更"路径正常接受/去重，**不推进 lastSeq**（引擎不变量 5 的推广，需在引擎测试中显式锁定该行为——见 §4.2 T-CL-5）。
3. **通道复用不互斥**：singleflight 键不同（pull: `pull:<convId>`，history: `hist:<convId>:<beforeSeq>`），互不吞并。
4. **退让规则**：同一 conv 同时存在前向 in-flight 与后向 920 在途时，前向优先续拉（保证用户看到的最新消息先齐），后向 920 按其既有"同时最多 1 个在途"约束排队。可实现为简单的 conv 级信号量，不改 history-loader 对外接口。

## 3. 服务端设计（后行部分）

### 3.1 水位线生成点

- **生成点在 fanout 层，不在 conv 写层**：消息落库（seq 已分配）后走既有 dispatcher → fanout 队列 → 聚合器（`fanout.go` 既有 per-conv window 结构）→ broadcaster。broadcaster 解析成员集后判定：
  - `len(members) <= threshold` → 现有逐成员 emit（102 MSG_NEW），零改动；
  - `len(members) > threshold` → **不**逐成员 emit 102，改为在 per-conv 聚合窗内生成一条 Watermark（§1.3 频控），广播到该会话在线成员。
- 水位线只发在线成员；离线成员由 Resume(kind 6，含 last_seq 补拉) 收敛，无需水位线。
- **大群消息照常落库、seq 照常分配**——拉取模式只改变"通知方式"，存储与 seq 语义零变化。这是回滚安全性的根基：关掉 flag 后一切回到现状。

### 3.2 阈值判断位置

判断读取 `featureflags.Snapshot()["pull_mode_threshold"]`（int，0 = 禁用）。放在 broadcaster 成员集解析之后（每会话每窗一次，不在热路径逐帧读——Snapshot 是 atomic.Value 零拷贝读，成本可忽略，但仍在窗粒度缓存以免 500 帧 500 次读）。

### 3.3 feature flag 与灰度

新增 flag（`featureflags/flags.go` Defaults 增补，经 911 FEATURE_FLAGS + SIGHUP 热重载，机制现成）：

| flag | 类型 | 默认 | 含义 |
|---|---|---|---|
| `pull_mode_threshold` | int | **0**（禁用） | 成员数 > 该值启用拉取；0=全量 inline，回滚即改此值 |
| `watermark_batch` | int | 10 | §1.3 推进阈值 |
| `watermark_interval_ms` | int | 500 | §1.3 时间阈值 |
| `watermark_jitter_ms` | int | 2000 | 客户端抖动上界（服务端仅用于压测配置下发，逻辑在客户端） |

灰度方案：阈值从 0 → 1000（只有超大群生效）→ 500 → 200 → 100，每档压测 §4.1 场景 2+3 守门，任一指标回退即回退阈值。**客户端 capability 不作为服务端开启条件**（服务端无法可靠区分客户端版本），替代为：灰度期内由压测与内部账号验证双端行为后再放开。

### 3.4 202 快照替代

- 拉取模式会话的 conv 创建/成员变更事件：推 `ConvPayload{members_omitted:true, member_count:N}`（§1.4），成员列表按需 930 分页拉。
- 920/8 对拉取模式会话**无任何特殊化**：消息本来就在库里，读路径不变。这也是本方案优于"服务端大群缓存"类方案的原因——没有新增服务端状态。

## 4. 验收标准（可测试）

### 4.1 服务端压测（CI，20s 稳态，`-no-ratelimit` + `--conv-members`）

| # | 场景 | 门槛 |
|---|---|---|
| S-1 | 500 人群 × 1000 在线 × 0.5 msg/s（拉取模式） | 可用性 ≥95%，P95 ≤100ms，p99 ≤200ms，p99/p95 ≤2.0，下行 ≤3k 帧/s（水位线+ACK+控制帧），`Conn.Dropped` = 0 |
| S-2 | 50 人群 × 1000 在线 | 可用性 ≥95%，P95 ≤100ms（不得低于现状 23.7%，目标为达到 inline 水准） |
| S-3 | 10 人群 × 512 在线（threshold=100 下不受影响） | 可用性 ≥99%，P95 ≤80ms——**不低于 v1.1.1**，证明分流无回归 |
| S-4 | 1000 并发握手（回归） | 握手 P95 <100ms 不回退 |
| S-5 | 拉取风暴注入：S-1 下服务端重启后全员重连 | 920/8 QPS 有上界：≤ 在线数/(jitter 窗) + min_interval 约束，实测断言 ≤ 800 QPS 且无 5xx |

### 4.2 客户端单测（mock，全链路：水位线→拉取→收敛）

| # | 用例 |
|---|---|
| T-CL-1 | 收水位线 latestSeq=lastSeq+5 → 构造 8 SYNC since_seq 正确；结果经 ingest 后 lastSeq 收敛到 latestSeq |
| T-CL-2 | 重复/乱序水位线（latestSeq ≤ lastSeq）→ 丢弃，零请求 |
| T-CL-3 | 500 条并发水位线同 conv → singleflight 合并为 1 个 in-flight + 追赶 targetSeq，min_interval 被尊重 |
| T-CL-4 | jitter：水位线到达与请求发出间隔 ∈ [0, 2000ms]，分布断言（统计 100 样本均值在 [700,1300ms]） |
| T-CL-5 | 前向拉取与后向 920 并发：920 返回 seq < lastSeq 的历史 → ingest 接受入库且 **lastSeq 不回退不推进** |
| T-CL-6 | 拉取返回 has_more=true → 自动续拉至收敛；中途 gap → onGap → 全量重放路径可用 |
| T-CL-7 | 会话未加载时收到水位线 → 记 pendingWatermark，hydrate 后消费 |
| T-CL-8 | kind 14 常量 `kinds.ts === kinds.go`（CI 一致性检查，事故补充教训） |

### 4.3 服务端单测

| # | 用例 |
|---|---|
| T-SV-1 | threshold=100：101 人群发消息 → 0 条 102 逐成员帧、≥1 条 Watermark(latest_seq=正确值)；99 人群 → 行为与现状逐字节一致 |
| T-SV-2 | 频控：1s 内 100 条消息 → 水位线 ≤ ⌈100/10⌉ + ⌈1000/500⌉ 条；1 条消息/1s → 恰好 1 条（防饿死） |
| T-SV-3 | seq 未推进的窗 → 0 条水位线 |
| T-SV-4 | flag 热改 threshold：500 人群从 200→100 → 新消息回到 102 逐成员推（回滚路径实测） |
| T-SV-5 | Watermark.latest_seq 引用的消息可被 920 since_seq 读到（生成时序约束） |
| T-SV-6 | 202 members_omitted：旧客户端（忽略未知字段）解析不报错 |

### 4.4 压测脚本改造需求

`scripts/stress`（workload.go/wire.go）现状 mixed 负载假设逐成员 102 下推，**不支持拉取模式**。需增加：

1. workload 增加 `--pull-mode`：客户端在收到 kind 14 后按 §2.1 状态机发 8 SYNC 并消费 921（当前压测端不处理 14——与事故根因 #3"压测客户端不发 SYNC"同构，**必须先修**，否则压测会人为复现 LAGGING 式死锁）；
2. 统计口径增加：下行帧/s 拆分（水位线 vs 消息 vs 控制帧）、920/8 客户端 QPS、收敛延迟（水位线 latestSeq → lastSeq 追平的 P95）；
3. 场景 S-5（重启风暴）作为独立子命令。

## 5. 风险与回滚

| 风险 | 场景 | 缓解 | 回滚开关 |
|---|---|---|---|
| 拉取风暴 | 水位线丢失/客户端 bug 导致全员反复拉 | singleflight + jitter + min_interval + 兜底轮询仅在"已知落后"时触发；服务端 RATE_LIMITED(13) 兜底 | `pull_mode_threshold=0` 热改；客户端编译期开关 `enablePullMode`（服务端停发后客户端调度器自然静止，无请求产生） |
| seq 断层 | 水位线推进但 8/920 拉取失败 | 引擎 gap buffer + onGap 全量重放既有路径；兜底 10s 轮询 | 同上；客户端重连 Resume(kind 6) 是最终收敛手段 |
| 离线堆积（first pull 巨大） | 用户离线 1 周后上线，水位线指向数千条差距 | Resume 路径本来就按 last_seq 分页补拉（现有能力）；客户端单次 8 limit=200 + has_more 续拉受 min_interval 限速；服务端 920 读路径已有 singleflight/热冷路由 | 无需服务端开关；若压测显示读路径过载，调大 `watermark_jitter_ms` 摊平 |
| pending 拉取队列内存膨胀 | 万级会话的水位线/待拉任务驻留 | pendingWatermark 表按 conv 去重（每 conv 最多 1 个 targetSeq，O(会话数) 常数）；in-flight 每 conv ≤1 | 客户端上限：pending 任务数 > 1000 时丢弃最旧的（靠重连收敛），计数埋点 |
| 服务端水位线逻辑 bug 污染 inline 群 | 阈值判定错误波及小群 | 分流判定是纯读分支，102 路径零改动；T-SV-1 锁定 99 人群行为不变 | `pull_mode_threshold=0` |
| 压测复现 RESYNC 式死锁 | 压测端无 handler | §4.4 第 1 条为压测改造前置验收项 | — |

## 6. 实施计划

### 6.1 P1 客户端（先行）——预估 3–4 人日

1. `kinds.ts` + dispatch 注册 kind 14（0.5d）
2. `pull-scheduler.ts` 状态机 + singleflight/jitter/min_interval（1.5d）
3. 引擎接线 + T-CL-1…T-CL-7 单测（1d）
4. mockBridge 增加水位线注入（0.5d）
**验收门槛**：T-CL 全绿；服务端零改动（中间态下客户端收到水位线即工作——可由 mock/压测端模拟发送验证）。**未达门槛，P2 不启动。**

### 6.2 P2 压测改造（可与 P1 并行）——预估 2 人日

§4.4 三项 + S-5 子命令。**门槛**：`--pull-mode` 下 mock 服务端水位线→拉取→ACK 统计正确。

### 6.3 P3 服务端——预估 3–4 人日

1. proto：Watermark 消息 + ConvPayload 字段 7/8 + kinds.go 登记（0.5d）
2. fanout 分流 + 频控 + flag（1.5d）
3. 202 members_omitted + 930 分页成员（依赖核实，若无则 +1d）
4. T-SV 全部单测（1d）
**验收门槛**：T-SV 全绿；S-3 回归不劣化。

### 6.4 中间态：「服务端合入但不启用」

P3 合入主干时 `pull_mode_threshold` 默认 **0**——服务端代码在位、行为与主干逐字节一致（T-SV-1 的 99 人群用例在主干 CI 常驻锁定）。此后按 §3.3 灰度推进：0 → 1000 → 500 → 200 → 100，每档跑 S-1/S-2/S-3/S-5，全部达标后进入下一档；任一档回退即停留在上一档并开 issue。

### 6.5 依赖检查项（P0，开工前核实）

- [ ] 930/931 会话数据域是否已支持分页成员拉取（§1.4 前置）
- [ ] Resume(kind 6) 对拉取模式会话的 last_seq 补拉路径实测
- [ ] CI 是否已有 kinds.ts ↔ kinds.go 一致性检查；无则本规范补上（0.5d，计入 P1）

### 6.6 明确不做

- ❌ per-conv 动态阈值（按实时帧率自动切换）——v1.2 用静态阈值 + 压测守门
- ❌ 按成员活跃度差异化推送（部分 inline 部分拉取）——一致性代价大于收益
- ❌ 服务端为拉取模式新建任何缓存/状态——读路径复用 920/8，零新增状态
- ❌ 把水位线塞进既有 202 version 字段或 ERROR 通道复用——语义污染，不如一个干净的新 kind（客户端 handler 先行已消除事故根因）
- ❌ 后台会话兜底轮询——省电，靠 Resume 收敛
