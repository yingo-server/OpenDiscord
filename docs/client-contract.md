# UFOT 客户端契约（Client Contract）

> 本文件面向**外部客户端**（Web / 桌面 / 移动，任何技术栈）的实现者：接入本服务所需的
> 全部契约——环境变量解析、认证流程、WS 帧协议入口、媒体上传、推送订阅、错误码与限流语义。
> UFOT 是纯后端服务，仓库不含任何客户端代码；客户端是独立程序，通过自身配置指向本服务。
>
> 实测入口：生产服务器 http://server.344977.xyz
> - HTTP API：http://server.344977.xyz/api/...
> - WebSocket：ws://server.344977.xyz/ws
> 允许直接调用生产 API 验证行为；不允许 SSH。

## 0. 接入总览

服务对外暴露三个面（同一个进程、同一端口，通常经 Caddy 反代）：

| 面 | 路径 | 用途 |
|---|---|---|
| 服务描述 | `GET /` | JSON 服务描述符（surface 发现），不是网页 |
| HTTP API | `/api/*` | 认证 /api/auth/*、媒体 /api/media/*、推送 /api/push/*、健康 /healthz |
| WebSocket | `/ws` | 业务实时帧（见 [ws-protocol.md](ws-protocol.md)） |

`GET /` 返回：

```json
{"service":"UFOT","role":"backend","surface":{"http_api":"/api/*","websocket":"/ws","health":"/healthz","auth":"/api/auth/methods"}}
```

客户端应把 API base 与 WS url 作为自己的配置项暴露（环境变量或等价机制），
不假设两者同源。

## 1. 环境变量与地址解析

客户端需要两个可配置地址。推荐变量名（框架用 Vite 等构建期注入时同理）：

| 变量 | 含义 | 默认（回退） |
|---|---|---|
| `API_BASE` | 后端 HTTP 根 | 空 = 同源 `location.origin` |
| `WS_URL` | 后端 WebSocket 全 URL | 由 API_BASE 推导；再回退同源拼 `/ws` |

解析规则（多端实现必须一致）：

```
API_BASE = env.API_BASE || location.origin
WS_URL   = env.WS_URL
         || (API_BASE ? scheme(API_BASE)+'://'+host(API_BASE)+'/ws'
                      : (location.protocol==='https:'?'wss':'ws')+'://'+location.host+'/ws')
```

CORS：跨域客户端依赖服务端 CORS 行为，见 [api-reference.md §1.2](api-reference.md)（`cors_mode`
三模式；默认 `allow_any` 反射任意合法 Origin 且带 `Allow-Credentials`）。
WS 握手不受浏览器 CORS 约束，但服务端校验 Origin（`allow_any_origin = true` 默认放行）。

## 2. 认证（HTTP）——四种登录方式

统一响应头 `Content-Type: application/json; charset=utf-8`、`Cache-Control: no-store`。
错误响应体统一为 `{"error":"<可展示文案>","code":"<机器码>"}`（部分历史端点为
`{"code":"...","message":"..."}`，两者都可能出现，实现需兼容两种字段名）。
message/error 面向终端用户，不含 SQL/栈信息。

### 2.1 登录方式开关：先探测，再渲染

四种登录方式（手机验证码 / 邮箱魔法链接 / 账号密码 / 游客）各有一个开关：
`policy.json` 的 `auth.methods`，**默认全部 false**（fail-closed）。

客户端登录前必须先调用：

```
GET /api/auth/methods    （公开、不鉴权、按 IP 限流 60/min）
→ 200 {"methods":{"phone":false,"email":false,"password":true,"guest":true}}
```

只展示 `true` 的方式；调用被关闭方式的端点会得到
`403 {"error":"...","code":"METHOD_DISABLED"}`（在解析任何参数之前返回）。
`policy.json` 热更（SIGHUP）即时生效——客户端不应长期缓存探测结果。

### 2.2 端点表

| 端点 | 请求体 | 成功响应 |
|---|---|---|
| POST /api/auth/phone/code | `{"phone":"+8613800138000","captcha_token":"..."?}` | `{"ok":true,"challenge_id":"...","channel":"sms","expires_in":300,"resend_after":60}` |
| POST /api/auth/phone/verify | `{"phone":"...","code":"123456","challenge_id":"...","device_id":"..."?}` | 会话对象（见 2.4） |
| POST /api/auth/email/link | `{"email":"a@b.c","captcha_token":"..."?}` | `{"ok":true,"challenge_id":"...","channel":"email","expires_in":600,"resend_after":60}` |
| POST /api/auth/email/poll | `{"email":"a@b.c","challenge_id":"...","device_id":"..."?}` | 链接未点 → `409 {"error":"pending","code":"PENDING","status":"pending"}`（可轮询重试）；已点 → 会话对象 |
| POST /api/auth/password | `{"account":"手机/邮箱/用户名","password":"...","device_id":"..."?}` | 会话对象 |
| POST /api/auth/guest | `{"nickname":"可选","device_id":"..."?}` | 会话对象 |
| POST /api/auth/refresh | `{"refresh_token":"...","device_id":"..."?}` | 会话对象（access/refresh 均轮换） |
| POST /api/auth/logout | `{}` | `{"ok":true,"revoked":bool}` |
| POST /api/auth/password/set | `{"password":"..."}`（需登录，≥10 字符） | 会话对象 |

四个登录端点（phone/code、email/link、password、guest）受 `auth.methods` 开关门控（§2.1）。
完整字段级说明与实测样例见 [api-reference.md §2](api-reference.md)。

### 2.3 鉴权传递

- 登录成功后服务端下发 `Set-Cookie: access_token=...; HttpOnly; Path=/`（SameSite=Lax）。
- 后续 HTTP 请求带 Cookie 即可；部分端点也接受 `Authorization: Bearer <access_token>`。
- **WS 握手只认 Cookie**（禁止 `?token=` query 传递）。非浏览器客户端需自行在
  WS upgrade 请求中携带 `Cookie: access_token=...` 头。
- refresh_token 在内存/安全存储中轮换管理；检测到旧 refresh 重放 → `401 REFRESH_REPLAY`，
  整族令牌吊销，客户端必须回到登录态。

### 2.4 会话对象（登录成功返回，实测于 v2.1.0 生产 2026-11-23）

```json
{"user_id":123,"device_id":"g_...","access_token":"<JWT>","token":"<== access_token>",
 "refresh_token":"<JWT>","expires_at":1795533858563,"expires_at_unix":1795533858,
 "session_id":"g_...","guest":true,"is_new_user":true,"profile":{"display_name":"游客xxxx…"}}
```

注意：`expires_at` 是**毫秒**、`expires_at_unix` 是**秒**（与早期文档相反，以实测为准）。
除 `display_name` 外的字段可能因 omitempty 缺省（真源：`authsvc/tokens.go` sessionPayload）。

## 3. WebSocket 帧协议

一切实时业务（消息收发、会话、资料更新、通话信令）都在 WS 帧上。本节只列入口约定，
**帧格式、kind 全表、payload 形状以 [ws-protocol.md](ws-protocol.md) 为唯一协议文档**。

- 单连接，文本帧 = JSON；握手带 Cookie，失败服务端下发 kind 10 ERROR 后关闭。
- 心跳：客户端每 20s 发 kind 4 PING，服务端回 kind 5 PONG；60s 无心跳服务端断开。
- 请求/响应关联：请求帧带 `id`，响应帧回同一 `id`；错误也是帧（kind 10 ERROR）。
- 能力协商：连接后发 kind 18 CLIENT_CAPS `{"pull_capable":true}`，服务端改用水位线 +
  客户端拉取，下行帧量大幅降低（见 [universal-pull-spec.md](universal-pull-spec.md)）。
- 客户端**必须忽略未知 kind**（服务端后续会新增号段）。
- 资料更新没有 HTTP 端点：走 kind 940 IDENTITY_REQUEST / 941 IDENTITY_RESULT
  （payload 为 base64 的用户 JSON，见 ws-protocol.md）。

关键客户端必须处理的帧：`10 ERROR`、`11 KICKED`、`12 FEATURE_FLAGS`、`13 RATE_LIMITED`、
`14 WATERMARK`（→ 发 kind 8 SYNC_REQUEST 补缺口）、`103 MSG_ACK`（status 非 0 = 失败）。

## 4. 媒体上传（HTTP 直传）

- `POST /api/media/upload`：`multipart/form-data`，字段名 **`file`**，需登录 Cookie →
  `201 {"media_id":"...","mime":"image/png","mtype":1,"size":N}`（内容寻址：同字节重复上传返回 200）。
- `GET /api/media/{id}`：需登录 Cookie，200 字节流；非上传者/不存在均 404。
- 约束：图片 8MB / 音频 2MB / 文件 20MB；魔数白名单（JPEG/PNG/WEBP/GIF/OGG/MP3）；
  单用户每日 50 次。
- WS presign（kind 300-304）未实现，媒体一律走本 HTTP 面。
- 错误码：`413 FILE_TOO_LARGE` / `415 UNSUPPORTED_TYPE` / `429 RATE_LIMITED` / `500 INTERNAL`。

## 5. 推送订阅（Web Push）

客户端需要做的三件事：

1. **申请订阅**：在用户授权通知后，通过浏览器 Push API（或平台等价机制）取得
   PushSubscription（endpoint + keys.p256dh + keys.auth）。服务端 VAPID 公钥的
   分发方式由部署方决定（本服务不提供公钥下发端点）。
2. **把订阅上报**：`POST /api/push/subscribe`
   `{"subscription":{"endpoint":"https://...","keys":{"p256dh":"<base64url 65B>","auth":"<base64url 16B>"}}}`
   → 成功 **204 无 body**；非法密钥 → `400 {"code":"BAD_REQUEST","error":"invalid p256dh"}`。
   同 endpoint 重复上报幂等（只更新）。
3. **失败语义**：对任何非 2xx 只记日志、不重试风暴（fail-closed）；推送不可用不得影响
   WS 长连接与页内通知。

服务端测试入口：`POST /api/push/test`（带 Cookie，无 body）→ `{"sent":N}`。
运维侧（VAPID 密钥生成、环境变量）见 [push-setup.md](push-setup.md)。

## 6. 错误码约定

- HTTP 错误体：`{error, code}`（认证/媒体域）或 `{code, message}`（早期域）——两种字段名都要兼容。
- 用户面错误码全集（BAD_REQUEST / INVALID_CREDENTIALS / ACCOUNT_LOCKED / OTP_* / PENDING /
  REFRESH_* 等）见 [api-reference.md §6](api-reference.md)。
- 管理面 REST 错误体为 `{code:int, reason:string, request_id:string}`（客户端一般不调用）。
- WS 帧层错误：kind 10 ERROR，protobuf Error{code, reason}（4000=bad frame、4290=rate limit），
  见 [ws-protocol.md §6](ws-protocol.md)。

## 7. 限流语义

- HTTP 429 响应体带 `retry_after_ms`（毫秒）：`{"error":"...","code":"RATE_LIMITED","retry_after_ms":500}`。
  客户端应按 `retry_after_ms` 退避，不做固定间隔轮询。
- WS 限流是帧：kind 13 `{"scope":"conn","limit":1,"retry_after_ms":500}`；严重超限服务端
  以 kind 10 ERROR{4290} 断连，客户端按重连退避处理。
- 主要限流点：登录方式探测 60/min/IP；验证码发送 60s 间隔 + 单号 5/h + 10/day +
  单 IP 5/h（详见 security-spec §1.5.2）；媒体上传 50 次/日；消息 60 条/min/用户。
- 密码登录连续失败 5 次锁定（15m/1h/24h 递增）→ `429 ACCOUNT_LOCKED`。

## 8. 明确不属于客户端契约的内容

- 服务端不托管任何 UI：`GET /` 返回 JSON 描述符，`/admin/` 等页面路径不存在。
- 管理面 `/api/v1/*`（回环端口 + RBAC + CSRF）是独立管理程序的契约，用户客户端不调用。
- 客户端的技术栈、目录结构、内部状态机由客户端团队自定；本契约只约束线上字节——
  URL、请求/响应形状、帧格式、错误码与限流语义。实现时建议用生产 API/WS 抓真实报文校准。
