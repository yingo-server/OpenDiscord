# FORAGENT — UFOT 后端事实速查（面向开发前端/客户端的 AI）

> 本文件是**事实清单**，不是任务书。它描述这个服务是什么、有什么、行为如何；
> 不包含任何对客户端实现的要求、建议或风格偏好。客户端怎么做，由客户端的开发者决定。
>
> 逐字事实来源（需要更细的字段/行为时查这三份）：
> - `docs/client-contract.md` — 客户端契约（env 解析、认证、媒体、推送、错误码、限流）
> - `docs/api-reference.md` — HTTP 端点全集 + 生产实测响应样例（实测于生产 2026-11-23/28）
> - `docs/ws-protocol.md` — WS 帧协议（Envelope 字段表、85 个 kind 全表、payload 形状）
>
> 版本基线：v2.3.1（当前生产运行版本）。生产实例：`http://server.344977.xyz`（HTTP + WS `/ws`）。
> 允许直接调用生产 API 验证行为；不允许 SSH。

---

## 1. 服务形态（事实）

- UFOT 是自托管 IM 的**纯后端**：同一进程、同一端口提供 HTTP API 与 WebSocket。仓库不含任何前端/客户端代码。
- `GET /` 返回 JSON 服务描述符（不是网页）：
  `{"service":"UFOT","role":"backend","surface":{"http_api":"/api/*","websocket":"/ws","health":"/healthz","auth":"/api/auth/methods"}}`
- 未知路径返回 JSON 404：`{"error":"not found","code":"NOT_FOUND"}`。
- `/admin/` 等 UI 页面路径**不存在**。管理面 `/api/v1/*` 在独立回环端口（7781），公网不可达，用户客户端不使用。

## 2. 端点清单（事实）

### 认证域 `/api/auth/*`（全部 POST，除注明外）

| 端点 | 开关门控 | 成功响应 |
|---|---|---|
| `GET /api/auth/methods` | —（公开，不鉴权） | `{"methods":{"phone":bool,"email":bool,"password":bool,"guest":bool}}` |
| `POST /api/auth/phone/code` | `auth.methods.phone` | `{"ok":true,"challenge_id":"...","channel":"sms","expires_in":300,"resend_after":60}` |
| `POST /api/auth/phone/verify` | `auth.methods.phone` | 会话对象 |
| `POST /api/auth/email/link` | `auth.methods.email` | `{"ok":true,"challenge_id":"...","channel":"email","expires_in":600,"resend_after":60}` |
| `POST /api/auth/email/poll` | `auth.methods.email` | 已点→会话对象；未点→`409 {"error":"pending","code":"PENDING","status":"pending"}` |
| `POST /api/auth/password` | `auth.methods.password` | 会话对象 |
| `POST /api/auth/guest` | `auth.methods.guest` | 会话对象 |
| `POST /api/auth/refresh` | — | 会话对象（access/refresh 均轮换） |
| `POST /api/auth/logout` | — | `{"ok":true,"revoked":bool}` |
| `POST /api/auth/password/set` | —（需登录，与登录开关无关） | 会话对象 |

**当前生产开关状态**（实测 2026-11-28）：`phone:false, email:false, password:true, guest:true`。
开关在 policy.json（`auth.methods`），SIGHUP 热更即时生效；被关闭的端点在解析参数前返回
`403 {"error":"<method> login is disabled","code":"METHOD_DISABLED"}`。

**会话对象**（登录成功返回；`expires_at` 毫秒、`expires_at_unix` 秒，除 `display_name` 外可能缺省）：

```json
{"user_id":123,"device_id":"g_...","access_token":"<JWT>","token":"<== access_token>",
 "refresh_token":"<JWT>","expires_at":1795533858563,"expires_at_unix":1795533858,
 "session_id":"g_...","guest":true,"is_new_user":true,"profile":{"display_name":"..."}}
```

鉴权传递（事实）：登录后服务端 `Set-Cookie: access_token=...; HttpOnly; Path=/`（SameSite=Lax）。
HTTP 请求带 Cookie；`Authorization: Bearer <access_token>` 也被接受。
**WS 握手只读 Cookie**（也接受 `Sec-WebSocket-Protocol: bearer, <token>` 头）；URL query 传 token 被服务端拒绝（不存在该路径）。

### 媒体域 `/api/media/*`

| 端点 | 方法 | 事实 |
|---|---|---|
| `/api/media/upload` | POST multipart，字段名 `file` | 需登录 Cookie；成功 `201 {"media_id","mime","mtype","size"}`；同字节重复上传返回 200（内容寻址） |
| `/api/media/{id}` | GET | 需登录；200 字节流；非上传者/不存在一律 404 |

硬约束（服务端强制）：图片 8MB / 音频 2MB / 文件 20MB；魔数白名单 JPEG/PNG/WEBP/GIF/OGG/MP3；
扩展名与魔数不一致 → 415；单用户每日 50 次；用户总配额 500MB。WS presign（kind 300-304）未实现，媒体只有 HTTP 面。

### 推送域 `/api/push/*`

| 端点 | 事实 |
|---|---|
| `POST /api/push/subscribe` | body `{"subscription":{"endpoint":"https://...","keys":{"p256dh":"<base64url 65B>","auth":"<base64url 16B>"}}}` → 204 无 body；非法密钥 400；同 endpoint 幂等 |
| `POST /api/push/test` | 带 Cookie，无 body → `{"sent":N}` |

事实：订阅只存内存，服务重启即失；服务端不提供 VAPID 公钥下发端点（公钥由部署方分发）；
推送 body 只含通用文案+convId，不含消息正文。

### 其他

- `GET /healthz` → `{"ok":true,"mode":"embedded","conns":N,"uptime_ms":N,"db":"ok"}`（无鉴权）。

## 3. WebSocket（事实）

- 端点 `ws(s)://<host>/ws`，握手时鉴权（失败 401 不建连）。帧上限 64KB，超限回 `ERROR{code:4000}` 后以 1003 关闭。
- 两种线格式，服务端按对端首帧格式自动回同格式：文本帧 = JSON（envelope 字段 snake_case，payload 为 **base64** 字符串）；二进制帧 = protobuf。payload 内部 JSON 是 protojson camelCase（与 envelope 的 snake_case 不同——这是事实，不是笔误）。
- Envelope 关键字段：`id`(uint64 单调), `kind`(uint32), `ack_id`, `request_id`(string), `conv_id`/`user_id`(二选一，同设报错), `payload`。**`id` 必须是数字**（字符串 id 会收到 `ERROR{4000}` 并被断连）。
- 心跳：服务端每 25s 发 WS Ping；读超时 60s。客户端常见做法是发 kind 4 PING（回 kind 5 PONG）——是否跟随由客户端决定。
- 请求-响应关联：响应帧带与请求相同的 `id`/`request_id`；错误也是帧（kind 10）。

### kind 号段（85 个常量，真源 `server/internal/proto/kinds/kinds.go`）

系统层（0-99）：1 HELLO、2 AUTH、3 AUTH_RESULT、4 PING、5 PONG、6 RESUME、7 RESUME_RESULT（⚠️ 无生产者）、8 SYNC_REQUEST、9 SYNC_RESULT（⚠️ SYNC 实际回 102）、10 ERROR、11 KICKED、12 FEATURE_FLAGS、13 RATE_LIMITED、14 WATERMARK、15/16/17 MAILBOX_ACK/PAUSE/RESUME、18 CLIENT_CAPS。

消息层（100-199）：101 MSG_SEND、102 MSG_NEW、103 MSG_ACK、104 MSG_READ、105 MSG_DELIVERED、106 MSG_EDIT、107 MSG_DELETE、108 MSG_RECALL、109 MSG_FORWARD、110 MSG_SYNC、111 MSG_SEARCH（⚠️ 无处理器）。

会话层（200-299）：200 CONV_CREATE、201 CONV_UPDATE、202 CONV_PAYLOAD、210 MEMBER_ADD、211 MEMBER_REMOVE、212 MEMBER_UPDATE、213 ROLE_SET、220 MUTE（⚠️ 无处理器）、221 PIN（⚠️ 无处理器）、230 TYPING、231 DRAFT（⚠️ 无处理器）。

媒体层（300-304）：MEDIA_PRESIGN 系列（⚠️ 全部未实现，媒体走 HTTP）。

通话层（350-354）：CALL_OFFER/ANSWER/CANDIDATE/BUSY/HANGUP——**当前生产 `call_enabled=false`**，开启前发这些 kind 会收到 403 语义的错误。

在线状态（400-402）：400 PRESENCE_UPDATE（客户端可上行）、401/402（服务端下行；客户端上行会被拒绝，回 ERROR 4003）。

资料/安全（450-453、940-941）：450 PROFILE_UPDATE（⚠️ 无处理器——资料更新实际走 940）、451 BLOCK / 452 UNBLOCK（⚠️ 无处理器）、453 REPORT（已实现）；940 IDENTITY_REQUEST / 941 IDENTITY_RESULT（资料读写唯一通道，payload 为 base64 的用户 JSON）。

管理/内部（500-502、902-990）：服务端下发或内部使用；客户端收到未知 kind 时自行决定处理方式（服务端不会因客户端忽略而断连）。

### 服务端当前已知行为差异（事实，实测+源码）

- kind 106/109/453：客户端发出后**可能收不到任何应答帧**（路由缺失，帧被回环）；文档声称已实现，与生产行为不符——实现客户端时以实测为准。
- 非成员发 MSG_SEND：源码有 `ERROR{403}` 路径，生产实测无回帧。
- MSG_SEND 超 8KB payload：被**静默丢弃**（无 ACK 无 ERROR），客户端只能等超时。
- kind 7/9 无生产者；SYNC 补拉的实际应答是 102 逐条重发。
- `retry_after_ms`（kind 13 与 HTTP 429）当前为服务端硬编码值（500ms），非精确退避建议。

## 4. 错误与限流（事实）

- HTTP 错误体两种字段形态并存：`{"error":"...","code":"..."}`（认证/媒体/推送）与 `{"code":"...","message":"..."}`（部分早期端点）。两种都可能出现。
- HTTP 429 带重试信息：`retry_after_ms`（毫秒）与 `retry_after`（秒）字段。
- WS 限流帧：kind 13 `{"scope":"conn","limit":1,"retry_after_ms":500}`。
- 主要服务端限流点（当前生产值）：登录方式探测 60/min/IP；验证码发送 60s 间隔、单号 5/h、10/day、单 IP 5/h；媒体上传 50 次/日；消息 60 条/min/用户；WS 连接桶存在（重连风暴会触发 429/封禁窗口）。
- 密码登录连续失败 5 次锁定（15m→1h→24h 递增）→ `429 ACCOUNT_LOCKED`。
- refresh 重放检测：旧 refresh token 再次使用 → `401 REFRESH_REPLAY`，该设备整族令牌吊销。并发 refresh（多标签页同用一族）会触发同一结果——这是当前服务端行为。

## 5. CORS（事实）

- 当前生产配置 `cors_mode = "allow_any"`：任何合法 http(s) Origin 被反射，带 `Access-Control-Allow-Credentials: true`，方法 `GET, POST, OPTIONS`，头 `Content-Type, Authorization`，预检缓存 600s。
- CORS 由装配层统一应用于**全部** `/api/*`（auth、media、push、404 兜底）；预检 OPTIONS 由服务端直接应答（204）。
- WS 握手不受 CORS 约束；服务端校验 Origin，当前 `allow_any_origin = true`（放行任意合法 Origin）。
- 若部署方改 `cors_mode` 为 `allow_list`，只 `public_origin` + `cors_origins` 列表内的 Origin 会收到头。客户端不应假设当前模式不变。

## 6. 相关文件索引（事实）

| 文件 | 内容 |
|---|---|
| `docs/client-contract.md` | 客户端契约总入口（认证流程/媒体/推送/错误码/限流） |
| `docs/api-reference.md` | 全部 HTTP 端点 + 生产实测响应样例 |
| `docs/ws-protocol.md` | Envelope 字段表、85 kind 全表与状态、payload 形状 |
| `docs/push-setup.md` | 推送订阅三步 + VAPID 运维侧 |
| `docs/deploy.md` | 部署（服务器只跑后端） |
| `docs/architecture.md` | 四层架构与 ADR（为什么后端不托管 UI） |
| `CHANGELOG.md` | 版本历史 |
