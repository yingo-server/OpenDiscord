# 消息总线架构 v3（v3-bus-design）

> 状态：design draft · 日期：2026-11 · 前置依据：`pull-mode-spec-v1.2.md`（已落地）、`perf-postmortem-v1.2-draft.md`、`architecture.md`（v1 总纲）、`bridge-design.md`。
> 参考硬件：2 核 4 GB；帧扇出预算 40,000 帧/s（perf-spec-v1.2 §0.2）。
> 所有"现状"引证均对应仓库实际代码；本规范不改代码。

## 0. 为什么需要 v3：现有投递模型的三个结构性问题

### 0.1 现状投递链路（已核实）

```
data 层 flusher 落库 → pipe emit → fanoutPlane.Enqueue(fanoutJob)
  → per-conv 聚合窗(100ms) → broadcast → memberCache 解析成员集
  → 逐成员 fanoutToPut → Bridge Bus(topic conv.{cid}) → user 层 router 查 session 表
  → 同一 uid 的全部连接 × Conn.Write（非阻塞，写入 per-conn 发送缓冲环）
  → 环满 = 静默丢弃 + dropped 计数（acceptor.go:115-126）
```

### 0.2 问题一：投递语义按"会话"组织，但物理信道按"连接"组织 —— 中间多一次 O(N×M) 展开

每条消息在 fanout 层按成员集展开为 O(成员数) 帧，再在 user 层按设备数再次展开为 O(连接数) 次 `Conn.Write`。两次展开都发生在**发送侧**，接收侧无法表达"我已读到 seq X"。v1.2 拉取模式用水位线把大群的 O(N) 推送降为 O(1)，但**小群（≤100 人）仍走逐成员 inline**，且水位线/ACK/typing/presence/receipt 各走各的频控逻辑，分散在 `fanout.go` 的 `shouldEmitWatermark` / `isTypingKind` / `isReceiptKind` 等十余个判定函数里，每加一种帧都要重新回答"谁收、聚不聚合、丢了怎么办"。

### 0.3 问题二：per-conn 环是唯一背压点，且丢帧语义是"合法化静默丢失"

环（1024）满即丢，`Conn.Dropped` 只是计数。事故（v1.2 慢消费者降级回滚）证明：任何"环满→通知客户端补拉"的方案若依赖对端未实现行为就是灾难；但反过来，**环满→丢弃→什么都不发生**同样不可持续——ACK 蒸发、103 MSG_ACK 丢失引发发送方重试风暴（事故根因链第 4 条）。v3 需要**背压向上游传播**而不是在最后一跳静默解决。

### 0.4 问题三：投递状态没有单一真相源

seq 真相在 data 层（per-conv seqalloc），投递进度真相在**客户端** lastSeq，服务端对"某个 uid 是否已收到某 seq"零感知；Resume(kind 6) 只能全量按 last_seq 补拉。断线重连、多设备、水位线丢失三条路径各自为政，收敛靠兜底轮询。v3 要把"投递进度"本身变成总线的一等公民。

## 1. v3 设计总览

### 1.1 一句话定义

> **v3 消息总线 = 以 (user_id) 为主键的单写者有序投递日志（mailbox log），配 per-user 交付游标（delivery cursor），所有下行帧（消息/回执/水位线/控制）统一经总线按游标推进；连接只是游标的消费者，环满不再丢帧而是暂停游标推进并向上游传播背压。**

三个替代关系：

| 现状（v1.2） | v3 |
|---|---|
| pipe→fanout 按 conv 成员集展开 O(N) 帧 | fanout 只产出**每 conv 一条** canonical 记录 + 水位线，投递展开移入总线 mailbox 阶段（仍 O(N) 但单点、有序、可测） |
| user 层查 session 表 × 每 conn `Conn.Write` 直塞环 | 总线按 uid 聚合为**每 uid 一个有序 mailbox**，连接订阅 mailbox，按游标消费 |
| per-conn 环满静默丢弃 | mailbox 与 per-conn 发送缓冲之间引入**信用（credit）流控**：缓冲满→连接请求暂停→mailbox 暂停推进→上游 fanout 聚合窗自然拉长（背压传播），**永不静默丢数据帧** |

### 1.2 总体结构

```
┌ data 层（不变）────────────────────────────┐
│ seqalloc→dedup→flusher 落库（seq 真相源）      │
│ emit: 每 conv 一条 canonical env → fanout     │
└──────────────┬───────────────────────────┘
               ▼
┌ bus 层（v3 新核心）─────────────────────────┐
│ fanoutPlane（保留，瘦身）：聚合窗/水位线/阈值分流    │
│   产出 canonical frames（102/103/104/105/14/20x）  │
│        ▼                                          │
│ MailboxHub（新）：                                 │
│   mailbox[uid] = 单写者有序日志（含控制帧）          │
│   职责：按 uid 去重排序、游标跟踪、离线暂存          │
│        ▼                                          │
│ DeliveryPlane（新）：                              │
│   conn[uid×dev] 订阅 mailbox，credit 流控          │
│   环满 → pause → 背压回 mailbox → 回聚合窗         │
└──────────────┬───────────────────────────┘
               ▼
┌ user 层（瘦身）───────────────────────────┐
│ 删除 per-uid session 表直投逻辑；Conn 只剩     │
│ codec + limiter(上行) + 发送缓冲 + ACK 上报     │
└───────────────────────────────────────┘
```

分层铁律不变：一切皆帧（mailbox 内条目也是 Envelope）、机制策略分离（阈值/窗口全走 feature flag）、crash-only（mailbox 只落 SQLite 一张表，重启即恢复）、import-lint 禁止 user↔bus 反向依赖。

## 2. Mailbox 设计

### 2.1 MailboxLog[uid]

```
mailbox[uid] 条目 = { mailbox_seq, envelope, state }
  mailbox_seq  uint64 —— 该 uid 的投递序号，总线分配，单调递增，重启不回退（存 SQLite）
  envelope     原始下行帧（102/103/104/105/14/20x/5xx 管理帧）
  state        pending → inflight(已发未ACK) → acked / expired
```

**关键决策：mailbox 序号与会话 seq 分离。** conv seq 是存储真相（不变），mailbox_seq 是投递真相（新增）。二者在条目内通过 `envelope` 自带字段关联（102 有 seq，103 有 msg_id，14 有 latest_seq）。这样做的理由：

- 会话 seq 属于 conv 维度（一 conv 一条线），投递属于 user 维度（一 user 一条线），跨会话交错，强行复用会话 seq 会在多群场景产生空洞歧义（这正是现状客户端 gap 判定复杂化的根源）；
- Resume / 重连恢复从"按 conv 逐个 last_seq 补拉"变成"按 mailbox_seq 一条线续传"，协议从 O(会话数) 次往返降为 O(1)。

### 2.2 写入路径（单写者）

- 每个在线 uid 恰好一个 mailbox，写入只来自 bus 层 dispatcher（fanout 产出线），**无第二个写入口**——对事故"第二套写入路径"教训的结构性继承；
- mailbox_seq 分配与条目追加在同一把 per-uid mutex 内完成，追加即按 (mailbox_seq, conv_id) 有序；
- 去重：总线级 `delivery_key = hash(uid, kind, msg_id|seq)` LRU（容量 4096/uid），fanout 重试或 bridge 重投不产生重复条目。

### 2.3 存储：SQLite 单表（热库分域 `mailbox`）

```sql
CREATE TABLE mailbox_log (
  mailbox_seq INTEGER PRIMARY KEY,   -- 每独立自增（per-uid 逻辑序 = 全局自增 + per-uid 游标表推导）
  uid INTEGER NOT NULL,
  kind INTEGER NOT NULL,
  conv_id INTEGER,
  envelope BLOB NOT NULL,            -- 帧序列化形态
  acked INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL
);
CREATE INDEX idx_mailbox_uid_unacked ON mailbox_log(uid, mailbox_seq) WHERE acked=0;
```

- **写入复用 flusher 组提交**：mailbox 条目随 data 层写管道同事务落库（同 SQLite WAL，零额外 fsync 成本）；
- **ACK 即水位标记**：客户端 ACK（kind 105 DELIVERED 语义扩展，见 §4.2）→ 更新 `acked`；后台 compaction job（复用既有 trim job 单元）删除 `acked=1 且 created_at < now-24h` 的条目；
- 未 ACK 条目天然就是**离线暂存**：uid 下线后条目留在表里，重连 Resume 按 mailbox_seq 续传，不再需要"按 conv last_seq 逐群补拉"；
- 容量上界：保留策略 acked 24h + 未 acked 7d（策略走 policy.json，代码零常量）。

### 2.4 与 v1.2 拉取模式的关系：水位线成为 mailbox 的一种条目

- 大群（>threshold）消息**不进 mailbox**（否则 O(N) 问题原样复活），只发 Watermark(14) 条目进 mailbox（每 conv 每 500ms 至多 1 条）；
- 客户端对 14 的处理完全沿用 `pull-scheduler.ts` 现状（singleflight + jitter + min_interval + 兜底轮询），**零改动**；
- 即：v3 中"mailbox 携带数据帧"只覆盖小群与回执，"mailbox 携带指针帧"覆盖大群——与 v1.2 的 inline/pull 分流完全兼容，threshold 语义不变，`pull_mode_threshold=0` 依旧是一键回滚到全 inline。

## 3. DeliveryPlane 与信用流控

### 3.1 连接订阅模型

```
conn(uid, dev) HELLO/AUTH 完成 → SUBSCRIBE mailbox://uid@dev (cursor=Resume给点或0)
DeliveryPlane 按 mailbox_seq 顺序向该 conn 发送 cursor+1..head 的条目
每 conn 维护: cursor(已确认), inflight(窗口内已发未确认), credit(剩余发送额度)
```

### 3.2 信用流控（替代"环满静默丢弃"）

```
credit 初始 = 512（flag: delivery_credit）
每发 1 条: credit--；客户端每 ACK 一批(n条): credit += n
credit == 0 → conn 进入 PAUSED：
  1. 该 conn 停止拉取 mailbox 新条目（不丢、不标记）
  2. 若该 uid 全部 conn 均 PAUSED → mailbox 进入 BACKPRESSURED：
     fanout 聚合窗对该 uid 产出的数据帧暂停下发、在窗内合并（typing/presence/receipt 合并语义现状保留）
     消息本体已在 SQLite，绝不丢
  3. 上行不受影响（limiter 与下行解耦），慢消费者不拖累发送方——修复事故"降级只有代价没有收益"的结构根源
心跳(Ping 4/5)与 ERROR(10)/KICKED 不占 credit、不受 PAUSED 影响（保命帧）
PAUSED 持续 >60s → 判死：发 kind 10 SLOW_CONSUMER 关闭连接，客户端重连走 Resume 续传
```

**与事故教训对照**：这里"通知客户端做 X"（ACK 一批推进 credit）的机制**客户端先行合入**——v3 客户端 ACK 逻辑是既有 sync 引擎 `ingest` 成功后的自然产物（现状已回 104/105），服务端只需把"批 ACK 推进 credit"作为纯服务端状态机，客户端零新行为依赖，满足"降级协议不能依赖对端尚未实现的行为"铁律。

### 3.3 顺序语义

- 同一 conn 内 mailbox_seq 严格有序（sync 引擎 seq 校验现状受益）；
- PAUSED 期间客户端可能乱序到达吗？不会——窗口内 inflight 条目按 seq 发出，暂停只发生在窗口边界；
- 客户端 `engine.ingest` 不变量（single-writer/去重/gap 缓冲）完全不动，gap 由"续传窗口"天然闭合。

### 3.4 多设备

- mailbox 每 uid 一份，每设备独立 cursor/credit——弱设备（后台标签页）PAUSED 不阻塞其他设备；
- `expired` 状态：设备 7 天未消费的未 ACK 条目转 expired（策略项），避免单僵尸设备撑爆未读队列。

## 4. 协议变更（双端同 PR，铁律 2）

### 4.1 新增/扩展 kind

| kind | 名称 | 方向 | 说明 |
|---|---|---|---|
| 15 | `MAILBOX_ACK` | C→S | `{cursor: uint64}` —— 确认已消费到 mailbox_seq=cursor（替代/兼容现状 104/105 对 conv seq 的 ACK，二者并存，v3 灰度期 105 优先） |
| 16 | `MAILBOX_PAUSE` | S→C | `{resume_at_ms?}` 仅诊断用；正常暂停靠 credit==0 停发，不发帧 |
| 17 | `MAILBOX_RESUME` | C→S | `{cursor}` 重连/续传请求，语义 = RESUME(kind 6) 的 mailbox 形态；kind 6 保留兼容 |

protobuf 形态（字段 1-3 用满，4-9 保留，与项目惯例一致）：

```protobuf
message MailboxAck   { uint64 cursor = 1; uint32 acked_count = 2; reserved 3 to 9; }
message MailboxResume{ uint64 cursor = 1; uint32 limit = 2; reserved 3 to 9; }
```

号段登记：15-19 为 0-99 系统层新增号段（14 WATERMARK 已占用）；`kinds.go` + `kinds.ts` 同 PR 登记，CI 一致性检查（P0 前置，与 v1.2 §6.5 同项）。

### 4.2 kind 15 与现有 104/105 的关系

- 105 DELIVERED（per-msg ACK）保留：它同时承担发送方回执与 credit 推进；
- kind 15 是**累积 ACK**（TCP 式），用于：Resume 续传时一次确认、credit 批量推进、服务端 compaction 依据；
- 服务端以 max(105 累计推导, 15.cursor) 为生效 cursor——两者乱序无害，幂等。

## 5. fanoutPlane 瘦身与背压传播

### 5.1 fanout 职责收窄为"产线"，不再管"投递"

保留：per-conv 聚合窗、水位线频控（`shouldEmitWatermark`）、threshold 分流、receipt/typing/presence 合并。
删除：`fanoutToPut` 的逐成员直投 + put 回调穿透（user 层 Conn.Write 的一切触达）。fanout 产出统一进入 `hub.Offer(uid, env)`。

### 5.2 背压链路

```
conn credit==0 ──► mailbox BACKPRESSURED ──► 聚合窗对该 uid 暂停出队（窗内合并）
                        │
                        └─窗满(超过 4× 默认窗容量)──► fanout Enqueue 返回 false
                                                    ──► data 层 emit 退避重试（指数退避，上限 1s）
                                                    ──► 数据本体已在 SQLite，只是通知延迟，永丢不存在
```

指标新增 `Bus.BackpressureDepth`（BACKPRESSURED mailbox 数）与 `Bus.PausedConns`，压测守门见 §7。

## 6. feature flag 与灰度

| flag | 默认 | 含义 |
|---|---|---|
| `v3_bus_enabled` | **false** | 总开关；false 时 user 层走现状直投路径（两路径并存同一代码库，`hub.Offer` 变 no-op） |
| `delivery_credit` | 512 | 每 conn 初始信用 |
| `mailbox_ack_window` | 128 | 服务端建议的批 ACK 粒度 |
| `mailbox_retention_unacked_h` | 168 | 未 ACK 保留时长（7d） |
| `mailbox_retention_acked_h` | 24 | 已 ACK 保留时长 |
| `slow_consumer_close_s` | 60 | PAUSED 判死时长 |

灰度：`v3_bus_enabled` 按 uid 百分比灰度（policy.json 策略 + 911 热重载，机制现成）5% → 25% → 50% → 100%。**客户端先行**：kind 15/17 handler + `kinds.ts` 常量先行合入并有 mock 端到端测试（铁律 1）；客户端不识别 15 的旧版本在服务端灰度名单外不受影响（未灰度 uid 仍走 104/105）。回滚 = flag 热改 false，mailbox 表保留不删（compaction 停止即可），双路径切换在连接粒度（新连接按灰度判定），切换零停机。

## 7. 验收标准

### 7.1 压测（CI，`-no-ratelimit`，沿用 stress 框架）

| # | 场景 | 门槛 |
|---|---|---|
| S-1 | 500 人群 × 1000 在线 × 0.5 msg/s | 可用性 ≥95%，P95 ≤100ms；下行 ≤3k 帧/s（水位线 + credit 控制帧） |
| S-2 | 1000 连接 × 50 人群（现状 23.7% 可用性的场景） | 可用性 ≥90%（**较现状 23.7% 翻 4 倍**，mailbox 聚合收益的直接证明） |
| S-3 | 10 人群 × 512 在线（inline 回归） | 可用性 ≥99%，P95 ≤80ms，不劣于 v1.1.1 |
| S-4 | **慢消费者**：1000 连接中注入 50 个带宽 1/10 的慢连接 | 慢连接 PAUSED 后：其余 950 连接可用性 ≥99%、P95 不回退；`Conn.Dropped` 数据帧 = 0；慢连接重连后 Resume 续传完整（无 gap） |
| S-5 | 重启风暴：S-1 下服务端重启全员重连 | mailbox 续传 QPS ≤ 在线数/jitter窗，实测 ≤800 QPS 且无 5xx；mailbox 表恢复后 cursor 不回退 |
| S-6 | 1000 并发握手回归 | 握手 P95 <100ms |

`Conn.Dropped` 计数语义变更：v3 下该计数**只允许**计"保命帧以外的 PAUSED 暂停次数"，数据帧静默丢失必须为 0——这是事故 ACK 蒸发教训的直接守门指标。

### 7.2 客户端单测（mock，先行验收）

| # | 用例 |
|---|---|
| T-CL-1 | kind 15 发送：ingest 收敛后 cursor 正确上报 |
| T-CL-2 | Resume(kind 17) 传 cursor=lastConsumed，续传帧经 engine.ingest 无 gap |
| T-CL-3 | kinds.ts ↔ kinds.go 15/16/17 常量一致（CI） |
| T-CL-4 | 旧版本客户端（无 15 handler）在非灰度路径全量回归绿 |

### 7.3 服务端单测

| # | 用例 |
|---|---|
| T-SV-1 | mailbox_seq 单调：并发 Offer 同 uid 1000 条 → 序号无重复无空洞 |
| T-SV-2 | credit 流控：credit=512 发 1000 条 → 488 条滞留 mailbox，0 条丢弃；ACK 批量推进后继续发出 |
| T-SV-3 | BACKPRESSURED 传播：全 conn PAUSED → 聚合窗暂停 → emit 退避；恢复 credit → 全量收敛 |
| T-SV-4 | 崩溃恢复：kill -9 后重启 → mailbox 表恢复 cursor，S-5 场景无重复投递（去重 LRU 生效） |
| T-SV-5 | flag 回滚：`v3_bus_enabled=false` 热改 → 新连接全部走旧路径，旧路径行为逐字节同主干 |
| T-SV-6 | compaction：acked 24h + unacked 7d 边界删除正确，活跃条目不误删 |
| T-SV-7 | 灰度 5%：同 uid 稳定落同一路径（按 uid 哈希，非按连接随机） |

## 8. 风险与回滚

| 风险 | 缓解 | 回滚 |
|---|---|---|
| mailbox 表成为 SQLite 写放大新热点 | 与 flusher 同事务组提交（零额外 fsync）；压测 S-3 锁定写路径 P95 不回退 | flag off，表冻结 |
| 双路径长期并存维护成本 | 100% 灰度后一个版本周期删除旧直投代码（列入 roadmap）；import-lint 保证 user 层不再触碰 fanout 内部 | — |
| credit 协议 bug 导致死锁（双方互相等） | 保命帧（4/5/10）不占 credit；`slow_consumer_close_s` 判死是最终逃生；T-SV-2/S-4 常驻 CI | flag off |
| mailbox 内存膨胀（在线 uid 多） | 每 uid 条目仅 HEAD-cursor 差量驻内存（LIFO 释放），SQLite 是权威态；pending 上限 4096/uid 超限靠 DB 读回 | — |
| 客户端旧版本无 15 handler | 按 uid 灰度隔离（T-SV-7）；客户端先行铁律 | 灰度名单回退 |

## 9. 实施计划

| 阶段 | 内容 | 预估 | 门槛 |
|---|---|---|---|
| P0 | 依赖核实：911 flag 热重载可用于 uid 百分比灰度；kinds CI 一致性检查；mailbox 分域迁移骨架（`store/migrations/hot/mailbox/0001.sql`） | 1d | 检查单全勾 |
| P1 | 客户端先行：kinds.ts 15/16/17 + ACK cursor 上报 + Resume 续传 + T-CL 全绿（服务端零改动，mock 验证） | 2d | T-CL 全绿，否则 P2 不启动 |
| P2 | 服务端 bus 层：MailboxHub + mailbox_seq 分配 + SQLite 表 + 去重 LRU + T-SV-1/4/6 | 3d | T-SV-1/4/6 绿 |
| P3 | DeliveryPlane credit 流控 + 背压传播 + fanout 瘦身 + T-SV-2/3/5/7 | 3d | S-3 不劣化 + T-SV 全绿 |
| P4 | 压测改造（S-2/S-4 场景 + 指标拆分）+ 灰度上线 5%→100% | 2d | §7.1 全部门槛达标 |

总计 9-11 人日。中间态铁律同 v1.2 §6.4：主干合入时 `v3_bus_enabled=false`，旧路径行为有 T-SV-5 常驻锁定。

## 10. 明确不做

- ❌ mailbox 跨机分布式日志（Kafka 化）——单机 SQLite 已覆盖目标规模，v3 是**总线架构**不是存储引擎重构；
- ❌ per-conv 投递日志（mailbox 按 conv 分）——多群交错与 Resume O(N) 问题即由此而来；
- ❌ 服务端推送级"已读回执追踪"（per-recipient read state 在线计算）——已读仍是客户端 105 上报，总线只存 acked 位，不做查询面；
- ❌ 把 typing/presence 也强排序进 mailbox——低价值帧走独立无序旁路（现状 isTyping/isPresence 判定保留），mailbox 有序性只服务数据帧；
- ❌ QoS 0/1/2 三档投递语义——两档足够：有序可靠（mailbox）与尽力而为（旁路）。
