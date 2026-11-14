# OpenDiscord WebSocket 协议参考（1.0）

> 只描述 `proto/chat.proto`、`server/internal/proto/kinds/kinds.go`、`server/internal/user/net/frame/codec.go`、`server/internal/user/net/ws/acceptor.go`、`server/internal/data/pipe/wire/wire.go` 中**真实存在**的帧结构与语义。kind 号段逐条取自 `kinds.go`（共 **77** 个常量），状态列标注是否已有处理分支。

---

## 1. 传输

- **端点**：`GET /ws`（挂在用户面 `listen` 端口，默认 `127.0.0.1:8080`）。非 `/ws` 路径交给同端口的 `ExtraRoutes`（`/api/auth/*`、`/healthz`、SPA）。
- **协议**：WebSocket（生产经 Caddy → WSS）。握手阶段完成鉴权，失败直接 `401` 不建连（`acceptor.go`）。
- **鉴权**：**只从 Cookie `access_token` 取 token**（`tokenFromRequest`）。也支持非浏览器客户端用 `Sec-WebSocket-Protocol: bearer, <token>` 头承载。**禁止把 token 放 URL query（红线，`security-spec §2.3`）**。
- **同源校验**：有 `Origin` 时必须与 Host 或配置的 `public_origin` 精确匹配（不前缀/不通配）；无 `Origin`（原生客户端）放行。
- **帧格式嗅探**：二进制帧=protobuf，文本帧=JSON；服务端按对端首帧格式回帧（`Conn.jsonMode` atomic 记录）。两种格式走**同一套**限额与校验。
- **帧上限 64KB**：`frame.MaxFrameBytes = 64 << 10`（`codec.go`；`acceptor.go` `SetReadLimit` 同源）。超限 → 回 `ERROR{code=4000}` 后按 Close 1003 关闭。
- **心跳**：服务端每 `25s` 发 WS Ping（`HeartbeatInterval` 默认 25s）；读超时 `60s`（`ReadTimeout`）。

---

## 2. Envelope 完整字段表

所有面共用唯一外层帧 `Envelope`（`proto/chat.proto`）。JSON 线格式（`frame.jsonEnvelope`）为 **snake_case**；二进制为 protobuf。

| JSON 字段 | proto 字段号 | 类型 | 说明 |
|-----------|-------------|------|------|
| `id` | 1 | uint64 | 单调帧 id |
| `ack_id` | 2 | uint64 | 被确认帧的 id |
| `kind` | 3 | uint32 | 消息类型（见 §4 号段表），必填，`0` 视为坏帧 |
| `version` | 4 | uint32 | 协议版本 |
| `request_id` | 5 | string | 请求-应答配对（幂等键） |
| `flags` | 6 | uint32 | 位标志；`bit0` = PriorityFlag（高优桶） |
| `sent_at` | 7 | fixed64 | 发送端毫秒时间戳 |
| `conv_id` | 10 | uint64 | target oneof：目标会话（与 `user_id` 二选一） |
| `user_id` | 11 | uint64 | target oneof：目标用户 |
| `payload` | 15 | bytes | 按 kind 解码的消息体；JSON 线格式里为 **base64** |

- 保留字段号：8、9、12、13、14（严格模式：JSON 出现未知字段即拒绝；protobuf 只能出现白名单号明文校验通过）。
- target oneof：`conv_id` 与 `user_id` **不能同时非零**（`decodeJSON` 直接报 `both conv_id and user_id set`）。

---

## 3. 两种线格式（同一帧示例）

同一帧 `MSG_SEND(kind=101)` 发到会话 `42`，payload 为 `{"text":"hi","clientMsgId":"c-1"}`：

**JSON 文本帧**（浏览器默认；外层 snake_case，payload 为 base64）：

```json
{"id":1,"kind":101,"conv_id":42,"payload":"eyJ0ZXh0IjoiaGkiLCJjbGllbnRNc2dJZCI6ImMtMSJ9"}
```

**protobuf 二进制帧**（原生客户端）：字段 1=`id`(varint 1)、字段 3=`kind`(varint 101)、字段 10=`conv_id`(varint 42)、字段 15=`payload`(length-delimited = 上述 JSON 明文字节)。两种编码经同一 `frame.Decode` 校验后进入同一处理链。

> 注意：**外层 envelope 是 snake_case，但 payload 内部结构由各 kind 定义**。数据层把 payload 解码为 `wire.*` 结构体，其 JSON tag 是 **protojson camelCase**（`clientMsgId`、`convId`、`sentAt`…），与 envelope 的 snake_case 不同。payload 对编解码层是不透明 `[]byte`，只在对应 kind 的 handler 里解析。

---

## 4. kind 号段总表（77 个常量）

> 唯一真源：`server/internal/proto/kinds/kinds.go`。「状态」依据是否有实际处理分支（网关本地 / 数据 pipe / 下行投递）。

**0–99 系统层**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 1 | HELLO | system | 实现（本地记录客户端版本 + 能力 pull_capable） |
| 2 | AUTH | system | 实现（本地幂等确认→AUTH_RESULT） |
| 3 | AUTH_RESULT | system | 实现（本地应答） |
| 4 | PING | system | 实现（本地→PONG） |
| 5 | PONG | system | 实现（本地） |
| 6 | RESUME | system | 部分（本地白名单仅免限流，转发桥接；数据层无 handler） |
| 7 | RESUME_RESULT | system | ⚠️ 未实现（无生产者） |
| 8 | SYNC_REQUEST | system | 实现（数据层 onSyncRequest→补拉 MSG_NEW） |
| 9 | SYNC_RESULT | system | ⚠️ 未实现（常量存在，SYNC 实际回 102 而非 9） |
| 10 | ERROR | system | 实现（错误应答） |
| 11 | KICKED | system | 实现（下行 executeKicked：写帧+断连） |
| 12 | FEATURE_FLAGS | system | 实现（本地回 flag 快照） |
| 13 | RATE_LIMITED | system | 实现（conn 桶降级 NACK） |
| 14 | WATERMARK | system | 实现（拉取模式水位线；普遍拉取下发给具备 pull_capable 的客户端） |
| 15 | MAILBOX_ACK | system | 实现（v3 累积 ACK） |
| 16 | MAILBOX_PAUSE | system | 实现（v3 诊断） |
| 17 | MAILBOX_RESUME | system | 实现（v3 续传） |
| 18 | CLIENT_CAPS | system | 实现（C→S 能力上报：payload JSON {"pull_capable":bool}；旧客户端能力协商，见 lts-1.4-acceptance §B） |

**100–199 消息层**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 101 | MSG_SEND | message | 实现（数据层 onMsgSend→ACK） |
| 102 | MSG_NEW | message | 实现（ACK 后 fan-out / SYNC 补拉） |
| 103 | MSG_ACK | message | 实现（下行投递） |
| 104 | MSG_READ | message | ⚠️ 未实现（无限流之外的处理分支） |
| 105 | MSG_DELIVERED | message | ⚠️ 未实现 |
| 106 | MSG_EDIT | message | ⚠️ 未实现 |
| 107 | MSG_DELETE | message | ⚠️ 未实现 |
| 108 | MSG_RECALL | message | ⚠️ 未实现（WS；管理端召回走 REST） |
| 109 | MSG_FORWARD | message | ⚠️ 未实现 |
| 110 | MSG_SYNC | message | ⚠️ 未实现 |
| 111 | MSG_SEARCH | message | ⚠️ 未实现 |

**200–299 会话层**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 200 | CONV_CREATE | conv | 实现（装配翻译→930 op1） |
| 201 | CONV_UPDATE | conv | 实现（→930 op5 getMeta） |
| 202 | CONV_PAYLOAD | conv | 实现（成员变更事件 fan-out） |
| 210 | MEMBER_ADD | conv | 实现（→930 op2 addMember） |
| 211 | MEMBER_REMOVE | conv | 实现（→930 op3 removeMember） |
| 212 | MEMBER_UPDATE | conv | ⚠️ 未实现（无翻译分支） |
| 213 | ROLE_SET | conv | ⚠️ 未实现 |
| 220 | MUTE | conv | ⚠️ 未实现 |
| 221 | PIN | conv | ⚠️ 未实现 |
| 230 | TYPING | conv | ⚠️ 未实现（有限流规则无处理） |
| 231 | DRAFT | conv | ⚠️ 未实现 |

**300–349 媒体层**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 300 | MEDIA_PRESIGN_REQUEST | media | ⚠️ 未实现（媒体走 HTTP `/api/media/*`） |
| 301 | MEDIA_PRESIGN_RESULT | media | ⚠️ 未实现 |
| 302 | MEDIA_READY | media | ⚠️ 未实现 |
| 303 | MEDIA_EXPIRE | media | ⚠️ 未实现 |
| 304 | MEDIA_THUMB | media | ⚠️ 未实现 |

**350–399 通话预留**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 350 | CALL_OFFER | call | ⚠️ 未实现（flag 关闭） |
| 351 | CALL_ANSWER | call | ⚠️ 未实现 |
| 352 | CALL_CANDIDATE | call | ⚠️ 未实现 |
| 353 | CALL_BUSY | call | ⚠️ 未实现 |
| 354 | CALL_HANGUP | call | ⚠️ 未实现 |

**400–449 presence**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 400 | PRESENCE_UPDATE | presence | ⚠️ 未实现（有限流规则无处理） |
| 401 | PRESENCE_SUBSCRIBE | presence | ⚠️ 未实现 |
| 402 | PRESENCE_UNSUBSCRIBE | presence | ⚠️ 未实现 |

**450–499 用户层**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 450 | PROFILE_UPDATE | user | ⚠️ 未实现 |
| 451 | BLOCK | user | ⚠️ 未实现 |
| 452 | UNBLOCK | user | ⚠️ 未实现 |
| 453 | REPORT | user | ⚠️ 未实现 |

**500–599 管理下行**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 500 | ADMIN_BROADCAST | admin | 部分（注入链路存在，按 user_id 投递） |
| 501 | ANNOUNCE | admin | ⚠️ 未实现（无生产者） |
| 502 | MAINTENANCE | admin | ⚠️ 未实现（无生产者） |

**900–909 层间控制（不出 Bridge）**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 902 | INTERNAL_HELLO | internal.control | 实现（bridge 握手） |
| 903 | INTERNAL_SUBSCRIBE | internal.control | 实现（bridge 订阅） |
| 904 | INTERNAL_FRAME_ACK | internal.control | 实现（bridge ack） |
| 905 | INTERNAL_HEARTBEAT | internal.control | 实现（bridge 心跳） |

**910–919 层间系统**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 910 | SHUTDOWN_NOTICE | internal.system | 实现（SIGTERM 排空广播） |
| 911 | FLAG_SYNC | internal.system | 实现（flag 重下发） |
| 912 | CONFIG_UPDATE | internal.system | 实现（SIGHUP 热重载广播） |
| 913 | TOPOLOGY | internal.system | 实现（拓扑帧，管理面） |

**920–929 数据读**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 920 | READ_HISTORY | internal.read | 实现（数据层 onReadHistory） |
| 921 | READ_HISTORY_RESULT | internal.read | 实现（下行结果） |

**930–939 会话数据**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 930 | CONV_DATA_REQUEST | internal.convdata | 实现（数据层 onConvData） |
| 931 | CONV_DATA_RESULT | internal.convdata | 实现（下行结果） |

**940–959 身份数据**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 940 | IDENTITY_REQUEST | internal.identity | 实现（数据层 onIdentity） |
| 941 | IDENTITY_RESULT | internal.identity | 实现（下行结果） |

**960–969 安全域**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 960 | SECURITY_REQUEST | internal.security | 实现（管理面注入：封禁/审查） |
| 961 | SECURITY_RESULT | internal.security | 实现（下行结果） |

**970–979 系统 flag**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 970 | FLAG_GET | internal.flags | 实现（数据层 onFlag 读） |
| 971 | FLAG_SET | internal.flags | 实现（数据层 onFlag 写） |
| 972 | FLAG_RESULT | internal.flags | 实现（下行结果） |

**980–989 运维作业**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 980 | JOB_TRIGGER | internal.jobs | 实现（数据层 onJobTrigger→jobber） |
| 981 | JOB_STATUS | internal.jobs | 实现（状态下行） |

**990–999 诊断**

| kind | 名称 | 层 | 状态 |
|------|------|----|------|
| 990 | DIAG_LOOPBACK | internal.diag | 实现（环回压测帧） |

> 600–899 为全预留，无命名常量，禁止占用。

---

## 5. 已实现 kind 的 payload 结构

payload 在 JSON 线格式下是 base64；解析后按下列结构（`wire.go` 的 JSON tag 为 protojson camelCase，与 `chat.proto` 语义一一对应）。

### 5.1 MSG_SEND（101）— `MessagePayload`

| 字段(JSON) | 类型 | 说明 |
|-----------|------|------|
| `clientMsgId` | string | 客户端幂等键（dedupguard，重放命中回原 ACK） |
| `text` | string | 文本 |
| `media` | array\<Media\> | 媒体数组，元素见下 |
| `replyTo` | uint64 | 引用消息 id |
| `mentions` | uint64[] | @ 的用户 id（proto 字段 5） |

`Media`：`{mediaId, mtype, name, size, mime, width, height, durationMs}`。

- 非成员发送 → `ERROR{code=403}`（"not a member"）。
- 超 20 msg/s（MSG_SEND 桶 `Rate:20, Burst:40, Capacity:100`）→ `RATE_LIMITED` 或断连（验收项）。
- writech 满 → `ERROR{code=503 "busy"}`（D-1 红线，不阻塞）。

### 5.2 MSG_ACK（103）— `MsgAck`

| 字段 | 类型 | 说明 |
|------|------|------|
| `msgId` | uint64 | 服务端分配的消息 id（snowflake） |
| `seq` | uint64 | 会话内连续 seq |
| `clientMsgId` | string | 回显客户端幂等键（用于配对） |
| `status` | uint32 | 状态码 |

### 5.3 MSG_NEW（102）— `MsgNew`

| 字段 | 类型 | 说明 |
|------|------|------|
| `msgId` | uint64 | snowflake |
| `seq` | uint64 | 会话内连续 seq（客户端据此校验 gap） |
| `senderId` | uint64 | 发送者 |
| `content` | bytes | 原始 MessagePayload（base64） |
| `sentAt` | fixed64 | 发送时间戳 |

### 5.4 SYNC_REQUEST（8）— 复用 `ReadHistory` 结构

| 字段 | 类型 | 说明 |
|------|------|------|
| `convId` | uint64 | 会话 id |
| `sinceSeq` | uint64 | 已收到的最大 seq（补拉起点） |
| `limit` | uint32 | 上限 |
| `coldMerge` | bool | 热/冷合并提示 |

- 响应**不是**包裹帧：把 `sinceSeq` 之后的每条消息作为独立 `MSG_NEW(102)` 按请求者 user_id 逐帧下发（`onSyncRequest` → `rr.catchup` 升序读）。
- 非成员 → `ERROR{code=403}`。

### 5.5 READ_HISTORY（920）→ READ_HISTORY_RESULT（921）

请求 `ReadHistory {convId, sinceSeq, limit, coldMerge}`；应答 `ReadHistoryResult {convId, messages:[MsgNew], fromCold, hasMore}`。

### 5.6 CONV 系列

- 上行 `CONV_CREATE(200)`（`ConvCreate {title, convType, memberIds}`）由装配翻译为 `CONV_DATA_REQUEST(930) op=1`，数据层回 `CONV_DATA_RESULT(931)`，并向每个成员 fan-out `CONV_PAYLOAD(202)`。
- 上行 `CONV_UPDATE(201)` → `930 op=5`（getMeta）；`MEMBER_ADD(210)`/`MEMBER_REMOVE(211)` → `930 op=2/3`（`MemberOp {convId, userIds, role}`）。
- `ConvPayload {convId, title, convType, createdAt, members:[Member{userId,role,joinedAt,mutedUntil}]}`。

### 5.7 其它已实现内部帧

`DataResult {op, ok, error, body, requestId}`（931/941/961/972 通用应答体）；`FlagRequest {key,value,write,requestId}`（970/971）、`FlagResult {flags,requestId}`（972）；`JobTrigger {job,dryRun,requestId}`（980）、`JobStatus {job,state,at,detail}`（981）。`version` / `requestId` 等用于请求-应答配对。

---

## 6. 时序（文字）

### 6.1 登录 → 握手 → 发消息 → ACK → 对端 MSG_NEW

```
客户端                      chatd(usergw)              数据层(pipe)
  |-- POST /api/auth/phone/verify -->|                        |
  |<-- 200 {access_token,...}+Cookie |                        |
  |-- GET /ws (Cookie access_token)->|  (验证→建连)             |
  |<-- 101 Switching Protocols ------|                        |
  |-- MSG_SEND(101){text,clientMsgId}->|  限流→Wrap→转发  ----->| onMsgSend
  |                                  |                        | admit+seqassign
  |                                  |<---- MSG_ACK(103){msgId,seq,clientMsgId} --|
  |<-- MSG_ACK(103) ------------------|                        |
  |                                  |<---- MSG_NEW(102) to 对端(uid) ---| fan-out
  对端 <-- MSG_NEW(102){msgId,seq,...}--|
```

### 6.2 断线重连 → SYNC 补拉

```
客户端                           chatd                       数据层
  |-- GET /ws (新连接, 同 Cookie) -->|                         |
  |   (检测 seq 出现 gap)            |                         |
  |-- SYNC_REQUEST(8){convId,sinceSeq,limit}->| 转发 ---------->| onSyncRequest
  |                                  |                         | memberCheck→catchup 升序
  |                                  |<---- 逐帧 MSG_NEW(102)（sinceSeq 之后）----|
  |<-- MSG_NEW(102) ... (多帧) -------|                         |
  |   (补齐后 seq 连续, 恢复正常)     |                         |
```

> 若 ACK 丢失或网络抖动导致 seq 跳号，客户端必须发 SYNC_REQUEST 补拉，而不是假设丢消息。

---

## 7. 限流与错误（`security-spec §1.2.3` L1–L4）

网关两级令牌桶（`limit/limiter.go`）判定后由 `router` 执行：

| 级别 | 触发 | 服务端动作 | 帧语义 |
|------|------|-----------|--------|
| **L1 软超限** | 单桶瞬时超（typing/presence 类 `Silent=true`） | **静默丢弃** | 无回帧 |
| **L2 NACK** | MSG 类超限（`Throttle`） | 回 `RATE_LIMITED(13)`（`scope:"conn", retry_after_ms:500`） | 客户端指数退避（基准 500ms，上限 8s） |
| **L3 降级** | 用户级聚合连续超限 | 高优帧降级、priority 桶拒绝，仅保 ack/ping | 视具体桶 |
| **L4 断连** | 惩罚触发 或 conn 内 60s 超限帧占比 > 50%（`Kill`） | 写 `ERROR{code=4290}` 后 `Close`（1008） | 客户端 60s 内禁重连 |

其它错误帧：

| 帧 | code | 触发 |
|----|------|------|
| `ERROR` | 4000 | 坏帧（解码失败/未知字段/kind=0）→ 1003 关闭 |
| `ERROR` | 403 | 非成员访问会话（MSG_SEND / SYNC / READ_HISTORY） |
| `ERROR` | 503 | 写队列满（busy，D-1 不阻塞） |
| `ERROR` | 4290 | 速率超限断连（L4） |
| `KICKED` | — | 管理端踢下线：下发 `Kicked{reason,bannedUntil}` 并断连 |
| `RATE_LIMITED` | — | L2 降级 |

默认桶（`limit.DefaultConfig`）：`MSG_SEND 20/s`、`MSG_READ/DELIVERED 10/s`、`SYNC_REQUEST 2/s`、`MEDIA_PRESIGN 1/30s`、`TYPING/PRESENCE 1/s`（Silent）、未列出 kind 兜底 `10/s`；`PING/PONG` 完全豁免。

---

## 8. 客户端实现清单（前端怎么接）

1. **鉴权**：登录后浏览器自动持有 `access_token` Cookie；`new WebSocket(`${wss}://${host}/ws`)` 即可，**不要**在 URL 拼 token。
2. **线格式**：浏览器发**文本帧 JSON**（snake_case envelope + base64 payload）；服务端按此格式回文本帧。原生客户端可发 protobuf 二进制帧。
3. **心跳**：响应服务端 25s Ping（浏览器 WebSocket 自动回 Pong）；应用层可另发 `PING(4)`，收 `PONG(5)`（带上 `ack_id` 与 `server_ms`）。
4. **seq 单调校验**：对每条 `MSG_NEW`/`MSG_ACK` 校验会话内 `seq` 连续；发现 gap 立即发 `SYNC_REQUEST(8){convId, sinceSeq}` 补拉。
5. **ACK 配对**：用 `MSG_ACK.clientMsgId` 匹配本地待确认消息，替换临时 id 为 `msgId`。
6. **tombstone 处理**：消息删除/召回（108/107 等）当前 **⚠️ 未实现**；客户端应先按"未收到即不存在"处理，预留 tombstone 分支。
7. **断线重连**：重连后凭 Cookie 重新握手，若本地 seq 已老化，发 SYNC_REQUEST 补齐；收到 `KICKED` 帧则停止重连并提示封禁原因。
8. **限流退避**：收 `RATE_LIMITED` 按 `retry_after_ms` 指数退避；连接被 1008 关闭后 60s 内不重连。
9. **错误处理**：`ERROR.code` 决定 UI（403 无权限、503 稍后重试、4000 协议错误需升级客户端）。

---

## 9. 未实现清单（协议层）

- `SYNC_RESULT(9)`、`RESUME_RESULT(7)`：常量存在但无生产者（SYNC 实际回 102，RESUME 仅转发）。
- 消息类 `104/105/106/107/108/109/110/111`：无用例处理（编辑/删除/转发/回执/WS 搜索/WS 召回）。
- 会话类 `212 MEMBER_UPDATE`、`213 ROLE_SET`、`220 MUTE`、`221 PIN`、`230 TYPING`、`231 DRAFT`：无翻译/处理分支。
- 媒体 WS `300–304`：媒体一律走 HTTP，WS presign 未接。
- 通话 `350–354`、presence `400–402`、用户 `450–453`：常量已定义，无处理器（call flag 关闭）。
- 管理下行 `501 ANNOUNCE`、`502 MAINTENANCE`：无生产者（仅 `500 ADMIN_BROADCAST` 有注入链路）。
- 高风险：以上未实现 kind 若客户端上行，网关会因"能力外/无 handler"被丢弃或转 `ERROR`——**客户端不应假设其可用**。
