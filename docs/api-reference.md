# UFOT API 参考

> 描述版本：v2.1.0。本文档只描述**代码中真实存在**的接口；每个端点的响应示例均为
> **生产服务器实测真实输出**（实测于 v2.1.0 生产 2026-11-23，示例中的 token 已脱敏，
> 但结构与字段名保持原样）。字段真源为源码 struct 的 json tag（见每节"源码"行）；
> 与源码 struct 不一致处以实测为准并在文中标注。
>
> 路由真源：`server/cmd/chatd/assemble.go buildHTTP()`（用户面 mux）、
> `server/internal/authsvc/authsvc.go routes()`、`server/internal/mediasvc/mediasvc.go Routes()`、
> `server/internal/pushsvc/pushsvc.go Routes()`。

---

## 1. 概览

UFOT 有 **两个 HTTP 面 + 一个 WebSocket 面**，全部由同一个 `chatd` 进程内的真实组件提供。

| 面 | 监听配置项 | 默认地址 | 作用 |
|----|-----------|---------|------|
| 用户面 HTTP | `listen` | `127.0.0.1:8080` | `/api/auth/*`、`/api/media/*`、`/healthz`、`/ws`、`GET /`（服务描述符） |
| 管理面 injector | `admin_listen` | `127.0.0.1:7780` | `POST /admin/inject`（唯一帧注入入口）+ `GET /healthz` |
| 管理面 REST | `admin_rest_listen` | `127.0.0.1:7781` | `/api/v1/*`（RBAC）+ `GET /health` |
| WebSocket | 复用 `listen` | `127.0.0.1:8080/ws` | 业务实时帧（见 `ws-protocol.md`） |

> 注意：`config/chatd.toml` 里**只显式写了 `admin_listen = 127.0.0.1:7780`**，没有写 `admin_rest_listen`；REST 面默认 `7781`（见 `server/internal/admin/server.go`）。CLI 内置默认端口也是 `7781`——两者一致，但**与 `admin_listen` 的 7780 不同**，详见 `cli-reference.md` 已知限制。
>
> `admin_listen` 校验 loopback（`chatd/admin.go`：`isLoopbackHost`），非 loopback 直接启动失败。生产只能经 Caddy 进入。

### 1.1 鉴权方式总表

| 面 | 鉴权 | 传递方式 | 说明 | 源码 |
|----|------|---------|------|------|
| 用户面 | access_token | Cookie `access_token`（HttpOnly, SameSite=Lax） | 登录成功同时下发 body token + Cookie；WS 握手只认 Cookie | `authsvc/tokens.go` `authsvc.go:78` |
| 用户面（部分） | access_token | `Authorization: Bearer` | `/api/auth/password/set` 等 `authenticate()` 接受的端点 | `authsvc/handlers.go` |
| WS | access_token | Cookie `access_token`（**禁止 query token**，红线） | 握手前校验，失败 401 不建连 | `user/net/ws/acceptor.go` `tokenFromRequest` |
| 管理面 | Admin session token | `Authorization: Bearer <token>` 或 Cookie `oc_admin_session` | login 返回 `token`+`csrf_token`；Bearer 优先 | `admin/middleware/context.go` `TokenFromRequest` |
| 管理面 | CSRF 双提交 | Header `X-CSRF-Token` == Cookie `oc_admin_csrf` | **严格模式：所有非安全方法（含 Bearer 的 CLI）都必须带** | `admin/middleware/csrf.go` |
| 管理面 | 幂等 | Header `Idempotency-Key` | (admin, method, path, key) 首次响应 5 分钟内逐字节重放 | `admin/handler/idempotency.go` |

> Idempotency-Key 仅对**写方法**生效；缺失时直接透传（不报错）。超 128 字节 → 400 `invalid_idempotency_key`。两个 CLI 会自动生成 v4 UUID。

---

## 2. 认证域 `/api/auth/*`（10 端点）

挂载在用户面（`listen`）同一 mux；`POST` only，预检走 `OPTIONS`（不 405）。
成功统一返回**会话对象**（除非另有说明），失败返回 `{error, code, status?, retry_after_ms?}`。

### 2.0z 登录方式开关（公开查询）

> 四个登录端点（phone/code、email/link、password、guest）均受本开关**门控**：
> 被关闭的方式在解析任何参数之前直接 `403 METHOD_DISABLED`。
> 客户端契约视角的完整描述见 [client-contract.md §2.1](client-contract.md)。

`GET /api/auth/methods` —— **不鉴权**（客户端尚未登录也要能问），但**按源 IP 限流**
（60 次/分钟/IP，超限 429 `{"error":"too many requests","code":"RATE_LIMITED"}`，
带 `retry_after_ms`；`Cache-Control: no-store`）。

```json
HTTP 200
{ "methods": { "phone": false, "email": false, "password": true, "guest": true } }
```

开关来源是 `policy.json` 的 `auth.methods` 段，**四项默认全部 false**（fail-closed）：

```json
"auth": { "methods": { "phone": false, "email": false, "password": false, "guest": false } }
```

- 被关闭的登录方式，其端点在**解析任何参数之前**直接返回
  `403 {"error":"<method> login is disabled","code":"METHOD_DISABLED"}`；
  `POST /api/auth/password/set`（已登录后改密）不受开关约束。
- `policy.json` 热更（SIGHUP）即时生效，无需重启。
- 客户端据此只展示可用的登录方式，避免用户点了必然 403 的入口。

### 1.2 CORS 行为（跨域客户端必读）

CORS 由 `chatd.toml` 三个键控制（`server/internal/authsvc/authsvc.go`）：

| 键 | 取值 | 行为 |
|----|------|------|
| `cors_mode` | `allow_any`（**默认**） | 反射任意合法 http(s) Origin，`Access-Control-Allow-Credentials: true`；为独立前端跨域部署的默认姿态 |
| `cors_mode` | `allow_list` | 只对 `public_origin` 与 `cors_origins`（逗号分隔，精确匹配、忽略大小写）回 CORS 头；UI 与后端同域托管时建议收紧到此模式 |
| `cors_mode` | `off` | 不回任何 CORS 头（纯同域部署） |
| `allow_any_origin` | `true`（默认） | WS 握手放行任意合法 Origin（独立前端默认）；设 `false` 收紧 |

注意事项：

- CORS 只作用于用户面 HTTP（`/api/auth/*`、`/api/media/*`、`/api/push/*`）；`OPTIONS` 预检统一处理（不 405）。
- Cookie 是 HttpOnly + SameSite=Lax，跨域浏览器客户端必须 `credentials: 'include'` 并依赖服务端回 `Allow-Credentials: true`。
- WS 握手不受浏览器 CORS 约束，但服务端校验 Origin（见 `allow_any_origin`）；非浏览器客户端可不受此限制。
- `allow_any` 模式下非 http(s) 的 Origin（含垃圾值）不反射、不带凭证返回。



**会话对象字段**（`authsvc/tokens.go` sessionPayload）：

| 字段 | 类型 | 说明 |
|------|------|------|
| `user_id` | int64 | 用户 id |
| `device_id` | string | 设备 id（服务端签发或复用） |
| `access_token` | string | 访问令牌（短期，默认 15m） |
| `token` | string | == access_token（客户端兼容别名） |
| `refresh_token` | string | 刷新令牌（默认 30d） |
| `expires_at` | int64 | access 过期（unix 秒） |
| `expires_at_unix` | int64 | 同上（冗余兼容） |
| `session_id` | string | 会话 id |
| `guest` | bool | 是否游客 |
| `is_new_user` | bool | 是否新建用户 |
| `profile` | object | `{display_name}`，可选 |

### 2.0 游客登录（真实会话对象）

`POST /api/auth/guest`，请求体 `{}`（可选 `device_id`、`nickname`）。

**实测于 v2.1.0 生产 2026-11-23**（token 截断脱敏）：

```json
HTTP 200
{
  "user_id": 20,
  "device_id": "g_53d196d74f6ba71e0a245c5deaa4d82e",
  "access_token": "eyJhbGciOiJFZERTQSIsImtpZCI6ImsxIiwidHlwIjoiSldUIn0.…",
  "token": "eyJhbGciOiJFZERTQSIsImtpZCI6ImsxIiwidHlwIjoiSldUIn0.…",
  "refresh_token": "eyJhbGciOiJFZERTQSIsImtpZCI6ImsxIiwidHlwIjoicmVmcmVzaCJ9.…",
  "expires_at": 1795533858563,
  "expires_at_unix": 1795533858,
  "session_id": "g_53d196d74f6ba71e0a245c5deaa4d82e",
  "guest": true,
  "is_new_user": true,
  "profile": { "display_name": "游客ba13fc5c…" }
}
```

> 实测差异标注：源码 `tokens.go` 中 `expires_at` 注释为 unix 秒、`expires_at_unix` 为"冗余兼容"，
> 实测 `expires_at` 是 **毫秒**（1795533858563），`expires_at_unix` 才是秒 —— 以实测为准。
> 成功同时下发 `Set-Cookie: access_token=…`（HttpOnly），后续端点与 WS 握手都用它。

### 2.0a refresh（真实轮换响应）

`POST /api/auth/refresh`，body `{"refresh_token":"…"}`。**实测**：返回与 guest 相同结构的会话对象，
`guest:false, is_new_user:false`，access/refresh 均轮换为新值，`session_id`（device_id）不变；HTTP 200。
缺 `refresh_token` → 400 `{"error":"refresh_token required","code":"BAD_REQUEST"}`。

### 2.0b logout（实测）

`POST /api/auth/logout`（带 Cookie），body `{}` → **实测** `{"ok":true,"revoked":false}`，HTTP 200。
（guest 设备无密码凭证可吊销，`revoked:false` 为正常输出。）

### 2.0c 错误路径实测

```json
// POST /api/auth/password {"account":"x@y.z","password":"wrongwrong1"}
HTTP 400  {"error":"account must be a phone number, an email or a username","code":"BAD_REQUEST"}

// POST /api/auth/phone/code {}（缺手机号）
HTTP 400  {"error":"invalid phone number","code":"BAD_REQUEST"}

// POST /api/auth/phone/verify {"phone":"+8613800000000","code":"000000","challenge_id":"353d…"}
HTTP 401  {"error":"authentication failed","code":"OTP_INVALID"}

// POST /api/auth/password/set {"password":"short"}（带 Cookie）
HTTP 400  {"error":"password must be at least 10 characters","code":"WEAK_PASSWORD"}
```

### 2.1 手机验证码

| 方法 | 路径 | 鉴权 | 请求体 | 成功响应 |
|------|------|------|--------|---------|
| POST | /api/auth/phone/code | 无 | `phone`、`captcha_token?`、`captcha?` | 200 `{ok, challenge_id, channel:"sms", expires_in, resend_after}` |
| POST | /api/auth/phone/verify | 无 | `phone`、`code`、`challenge_id?`（别名 `token?`）、`device_id?` | 200 会话对象 |

- 发送节流（`security-spec §1.5.2`）：60s 间隔、单号 5/h + 10/day、单 IP 5/h、当天第 2 次起要求 captcha、24h 内被 3 个不同 IP 命中则冻结。
- dev 模式（`APP_ENV=dev` + `dev_otp=true`）固定验证码 `123456`。

### 2.2 邮箱魔法链接

| 方法 | 路径 | 鉴权 | 请求体 | 成功响应 |
|------|------|------|--------|---------|
| POST | /api/auth/email/link | 无 | `email` | 200 `{ok, challenge_id, channel, expires_in, resend_after, link?}`（`link` 仅 dev） |
| POST | /api/auth/email/poll | 无 | `email`、`challenge_id?`（别名 `token?`）、`device_id?` | 200 会话对象；**未点链接 → 409 `{status:"pending", code:"PENDING", error:"pending"}`** |

> `email/poll` 用 **409 Conflict**（不是 202）表达"pending"，因为客户端 `EmailAuthProvider.verify` 把 `code=PENDING` 当作可重试信号（`authsvc/otp.go:410`）。

**实测于 v2.1.0 生产 2026-11-23**：

```json
// POST /api/auth/phone/code {"phone":"+8613800000000"}
HTTP 200
{"ok":true,"challenge_id":"353d50ae0b2911eaf3817b9ae370a690","channel":"sms","expires_in":300,"resend_after":60}

// POST /api/auth/email/link {"email":"a@b.co"}
HTTP 200
{"ok":true,"challenge_id":"e69508bbd00c976603dda8a59dca0453","channel":"email","expires_in":600,"resend_after":60}

// POST /api/auth/email/poll {"email":"a@b.co","challenge_id":"deadbeef"}（链接未点击）
HTTP 409
{"error":"pending","code":"PENDING","status":"pending"}
```

### 2.3 账户密码 / 游客 / 密码设置 / 刷新 / 登出

| 方法 | 路径 | 鉴权 | 请求体 | 成功响应 |
|------|------|------|--------|---------|
| POST | /api/auth/password | 无 | `account`、`password`、`captcha?`、`captcha_token?`、`device_id?` | 200 会话对象 |
| POST | /api/auth/password/set | Bearer/Cookie | `password` | 200 会话对象（重置密码） |
| POST | /api/auth/guest | 无 | `device_id?`、`nickname?` | 200 会话对象（`guest=true`） |
| POST | /api/auth/refresh | refresh_token | `refresh_token`、`device_id?` | 200 会话对象（轮换 refresh） |
| POST | /api/auth/logout | Bearer/Cookie | 无 | 200 |

- 密码登录账号锁定：连续失败 5 次 → 15m/1h/24h 递增，返回 `ACCOUNT_LOCKED`（429）。
- `password/set` 密码长度 `< 10` 字符 → 400 `WEAK_PASSWORD`（`minPasswordLen=10`）。
- `refresh` 检测到 refresh 令牌重放（旧令牌再用）→ 401 `REFRESH_REPLAY`，整族令牌吊销。
- 密码错误 / 账号不存在统一 `INVALID_CREDENTIALS`（防枚举，等时 argon2id 假校验）。

---

## 3. 媒体域 `/api/media/*`（2 端点）

| 方法 | 路径 | 鉴权 | 请求体 | 成功响应 |
|------|------|------|--------|---------|
| POST | /api/media/upload | access_token | `multipart/form-data`，字段名 **`file`** | 201 `{media_id, size, mime, mtype}` |
| GET | /api/media/{id} | access_token | — | 200 二进制字节流（`Content-Type` = 存储 mime） |

**实测于 v2.1.0 生产 2026-11-23**：

```json
// POST /api/media/upload （208 字节真 PNG，字段 file=@t.png;type=image/png，带 Cookie）
HTTP 201
{"media_id":"e5zJzrwbMgupb2sapED-OOCM6gcDYxob","mime":"image/png","mtype":1,"size":208}

// GET /api/media/e5zJzrwbMgupb2sapED-OOCM6gcDYxob （带 Cookie）
HTTP 200  Content-Type: image/png  （字节流）

// 未带 Cookie 上传：
HTTP 401  {"code":"UNAUTHORIZED","error":"unauthorized"}
// 未带 Cookie 下载：同上 401 UNAUTHORIZED
```

**上传约束**（`mediasvc/mediasvc.go`）：

- 大小上限：图片 `8MiB`、音频 `2MiB`、其它文件 `20MiB`；整体硬顶 `maxFileBytes = 20MiB`。
- 类型按**魔数**判定（JPEG/PNG/WEBP/GIF/OGG/MP3），未知类型 → **415** `UNSUPPORTED_TYPE`。
- 可执行/脚本黑名单扩展名（`.exe/.sh/.js/.svg/.html/...`）直接拒绝。
- 单用户每日上传上限 `50` 次 → 超限 **429** `RATE_LIMITED`。
- 内容寻址：同账号重复上传相同字节返回 200（同 `media_id`）。
- `mtype` 取值：`1`=image、`2`=audio、`4`=file。
- 访问 `GET /api/media/{id}`：非上传者 / 不存在均返回 **404**（不泄露存在性）。

---

## 4. 健康检查与服务描述符

**`GET /`（服务描述符）**——后端不托管 UI，根路径返回 JSON 而非网页；客户端可用它做 surface 发现：

```json
HTTP 200
{"service":"UFOT","role":"backend","surface":{"http_api":"/api/*","websocket":"/ws","health":"/healthz","auth":"/api/auth/methods"}}
```

（非 `/` 的未知路径 → `404 {"error":"not found","code":"NOT_FOUND"}`。）

**用户面 /healthz 实测于 v2.1.0 生产 2026-11-23**：

```json
// GET /healthz
HTTP 200
{"conns":0,"db":"ok","mode":"embedded","ok":true,"uptime_ms":145081572}

// GET /api/nope （未知 API 路径，buildHTTP catch-all）
HTTP 404
{"code":"NOT_FOUND","error":"unknown api path"}
```

注意字段序为 map 编码序（字母序）；`uptime_ms` 为进程启动以来的毫秒数。

---

## 4a. Web Push 域 `/api/push/*`（2 端点，已实现）

源码：`server/internal/pushsvc/pushsvc.go Routes()`。鉴权与 media/authsvc 同契约：`access_token` HttpOnly Cookie。
订阅保存在内存（重启丢失，浏览器会重新订阅）；无 VAPID 环境变量时 Sender 降级为 no-op（订阅照常登记）。

| 方法 | 路径 | 鉴权 | 请求体 | 成功响应 |
|------|------|------|--------|---------|
| POST | /api/push/subscribe | access_token | `{"subscription":{"endpoint","keys":{"p256dh","auth"}}}` | **204 无响应体** |
| POST | /api/push/test | access_token | 无 | 200 `{"sent":N}` |

**实测于 v2.1.0 生产 2026-11-23**：

```json
// POST /api/push/subscribe（p256dh 必须是 base64url 可解码的 65 字节非压缩 P-256 公钥，auth 16 字节）
HTTP 204  （无 body）

// p256dh 非法（长度/编码不符）：
HTTP 400  {"code":"BAD_REQUEST","error":"invalid p256dh"}

// POST /api/push/test（带 Cookie，无订阅或订阅者离线）
HTTP 200
{"sent":0}
```

> 4xx/5xx 时客户端 fail-closed：只记日志不重试（见 client-contract.md §5）。

| 方法 | 路径 | 面 | 鉴权 | 成功响应 |
|------|------|----|------|---------|
| GET | /healthz | 用户面 | 无 | 200 `{ok, mode, uptime_ms, conns?, db:"ok"}`；DB 不可用 → 503 `{ok:false, db:"unavailable"}` |
| GET | /healthz | 管理 injector(`admin_listen`) | 无 | 200 `{ok:true}` |
| GET | /health | 管理 REST(`admin_rest_listen`) | 无（Public 路由） | 200（admin handler `handleHealth`） |

> 三个健康端点形态不同，**不要混用**。用户面 `/healthz` 会真 ping 热库（2s 超时）。

---

## 5. 管理面 REST `/api/v1/*`

> 全部在 `admin_rest_listen`（默认 `7781`）。鉴权：`Authorization: Bearer`（或 `oc_admin_session` Cookie）；**所有非 GET 还需 `X-CSRF-Token` 双提交**；写方法建议带 `Idempotency-Key`。
> 路由真源：`server/internal/admin/handler/api.go` `Routes()`。
> 统一成功响应为 JSON；统一错误体：`{code:int(HTTP状态码), reason:string, request_id:string}`（`admin/middleware/context.go` `WriteError`）。

路由按权限（RBAC）分组，权限常量见 `admin/middleware/rbac.go`。角色：`super`(0)/`ops`(1)/`review`(2)。

### 5.1 认证（3 路由）

| 方法 | 路径 | 权限 | 请求体 / 查询 | 成功响应 |
|------|------|------|--------------|---------|
| POST | /api/v1/login | Public | `{username, password, totp}` | 200 `{token, csrf_token, expires_at, admin:{admin_id,username,role}}` + Set-Cookie |
| POST | /api/v1/logout | 需登录 | — | 200 `{ok:true}` |
| GET | /api/v1/whoami | 需登录 | — | 200 `{admin_id, username, role, permissions[], expires_at}` |

- 登录失败（含锁定）统一 401 `invalid_credentials`（`handler/auth.go`），且仍审计。
- `whoami` 返回 `permissions` 完整权限清单。

### 5.2 用户 users（5 路由，`user.read` / `user.ban` / `user.kick`）

| 方法 | 路径 | 权限 | 请求体 / 查询 | 成功响应 |
|------|------|------|--------------|---------|
| GET | /api/v1/users | user.read | 查询 `status`(-1..3)、`limit`(默认50)、`offset`、`q` | 200 `{items:[UserRecord], total}` |
| GET | /api/v1/users/{id} | user.read | — | 200 UserRecord |
| POST | /api/v1/users/{id}/ban | user.ban | `{level(1..3,默认1), reason, days, until}` | 200 结果 |
| POST | /api/v1/users/{id}/unban | user.ban | `{reason?}`（可为空体） | 200 结果 |
| POST | /api/v1/users/{id}/kick | user.kick | `{reason?}`（可为空体） | 200 结果 |

> ban/unban/kick 均为 `HighRisk`，走 op-rate-limit + 幂等 + CSRF + 注入 `POST /admin/inject`。

### 5.3 举报 reports（3 路由，`report.read` / `report.handle`）

| 方法 | 路径 | 权限 | 请求体 / 查询 | 成功响应 |
|------|------|------|--------------|---------|
| GET | /api/v1/reports | report.read | 查询 `status`(-1..3)、`limit`、`offset` | 200 `{items:[Report], total}` |
| GET | /api/v1/reports/{id} | report.read | — | 200 Report；不存在 404 |
| POST | /api/v1/reports/{id}/handle | report.handle | `{action("claim|resolve|reject"), note}` | 200 Report |

### 5.4 消息 messages（2 路由，`message.search` / `message.recall`）

| 方法 | 路径 | 权限 | 请求体 / 查询 | 成功响应 |
|------|------|------|--------------|---------|
| GET | /api/v1/messages/search | message.search | 查询 `q`、`conv_id`、`limit`、`offset` | 200 `{items:[MessageRecord], total}` |
| POST | /api/v1/messages/{id}/recall | message.recall | `{reason?}`（可为空体） | 200 结果 |

### 5.5 特性开关 flags（2 路由，`flag.read` / `flag.write`）

| 方法 | 路径 | 权限 | 请求体 / 查询 | 成功响应 |
|------|------|------|--------------|---------|
| GET | /api/v1/flags | flag.read | — | 200 `{items:[{key,value,version}]}` |
| PUT | /api/v1/flags | flag.write | `{key, value}` 或批量 `{flags:{k:v,...}}` | 200 `{items:[{key,value,version}]}` |

### 5.6 审计 audit（1 路由，`audit.read`）

| 方法 | 路径 | 权限 | 请求体 / 查询 | 成功响应 |
|------|------|------|--------------|---------|
| GET | /api/v1/audit | audit.read | 查询 `since_id`(≥0)、`limit`(默认500, max1000) | 200 `{items:[AuditRecord], next_since_id, has_more}` |

### 5.7 广播 broadcast（1 路由，`broadcast.send`）

| 方法 | 路径 | 权限 | 请求体 / 查询 | 成功响应 |
|------|------|------|--------------|---------|
| POST | /api/v1/broadcast | broadcast.send | `{text, conv_ids?, expires_at?}`；`?dry_run=true` 走预览 | 200 预览（无副作用）或发送结果 |

- `dry_run=true`（body 或 query）→ 只估算影响，返回 `{estimated_reach,...}`，不注入帧。
- 幂等：同 `Idempotency-Key` 重放返回**相同 body**（验收项 "idempotent replay"）。

### 5.8 归档 archive（2 路由，`archive.read` / `archive.run`）

| 方法 | 路径 | 权限 | 请求体 / 查询 | 成功响应 |
|------|------|------|--------------|---------|
| GET | /api/v1/archive/status | archive.read | — | 200 归档账本状态 |
| POST | /api/v1/archive/run | archive.run | `{dry_run?}`；或 `?dry_run=true` | 200 运行结果 |

### 5.8a 诊断 diagnostics（1 路由，复用 `metrics.read` 权限）

| 方法 | 路径 | 权限 | 请求体 / 查询 | 成功响应 |
|------|------|------|--------------|---------|
| GET | /api/v1/diagnostics/turn | metrics.read | — | 200 TURN 连通性自检结果（读配置 turn URL 做 TCP 探测，3s 超时，仅管理面可见；`admin/handler/api.go` `handleTURNCheck`） |

> v1.4 新增（OPS-2）。管理员不再需要手工 telnet 排查 TURN 可达性。

### 5.9 指标 metrics（1 路由，`metrics.read`）

| 方法 | 路径 | 权限 | 请求体 / 查询 | 成功响应 |
|------|------|------|--------------|---------|
| GET | /api/v1/metrics | metrics.read | — | 200 `text/plain; version=0.0.4`（Prometheus 文本）+ `X-Request-ID` |

### 5.10 其它 injector 面（1 路由）

| 方法 | 路径 | 面 | 鉴权 | 说明 |
|------|------|----|------|------|
| POST | /admin/inject | admin_listen(7780) | HMAC（bridge `adminapi`） | 唯一非 socket 的帧注入入口；REST 层经此把管理帧送 bus |

> 该端口上的 `/api/admin/*` 返回 404 `{error:"admin REST is served on admin_rest_listen", code:"WRONG_SURFACE"}`（`chatd/admin.go`），**不是**业务端点。

**管理路由计数**：`/api/v1` 下 **21 条**（auth 3 + users 5 + reports 3 + messages 2 + flags 2 + audit 1 + broadcast 1 + archive 2 + metrics 1 + diagnostics/turn 1），另有 `/health` = 22 条 mounted 路由（`api.go Routes()`）。验收表覆盖全部业务路径 + review 角色 403 探测。

---

## 6. 错误码全集

### 6.1 用户面 / 认证域（`authsvc/*.go`）

认证错误体：`{error, code, status?, retry_after_ms?}`。`code` 为大写枚举：

| code | HTTP | 来源 | 含义 |
|------|------|------|------|
| BAD_REQUEST | 400 | `errBadRequest` | 参数缺失/非法 |
| INTERNAL | 500 | `errInternal` | 内部错误 |
| WEAK_PASSWORD | 400 | `handlers.go:151` | 密码 < 10 字符 |
| INVALID_CREDENTIALS | 401 | `handlers.go:71` | 账号或密码错误 |
| ACCOUNT_LOCKED | 429 | `handlers.go:59/69` | 登录失败次数过多锁定 |
| BANNED | 403 | `login.go:111` | 账号被封禁 |
| CAPTCHA_REQUIRED | 400 | `otp.go` | 需人机验证 |
| CAPTCHA_INVALID | 400 | `otp.go` | 人机验证失败 |
| PENDING | 409 | `otp.go:419` | 邮箱链接未点击（可重试） |
| OTP_INVALID | 401 | `otp.go:217/220/270` | 验证码错误 |
| OTP_EXPIRED | 401 | `otp.go` | 验证码过期 |
| OTP_USED | 401 | `otp.go` | 验证码已用 |
| OTP_LOCKED | 401/429 | `otp.go` | 验证码尝试过多锁定 |
| OTP_VERIFY_LOCKED | 429 | `otp.go` | 校验锁定 |
| OTP_RESEND_TOO_SOON | 429 | `otp.go` | 重发过快 |
| OTP_HOURLY_LIMIT | 429 | `otp.go` | 每小时限额 |
| OTP_DAILY_LIMIT | 429 | `otp.go` | 每日限额 |
| OTP_FROZEN | 429 | `otp.go` | 24h 冻结 |
| IP_RATE_LIMIT | 429 | `otp.go` | 单 IP 限额 |
| EMAIL_TOKEN_USED | 401 | `otp.go` | 邮件令牌已用 |
| INVALID_TOKEN | 401 | `tokens.go` | access token 无效 |
| UNAUTHENTICATED | 401 | `tokens.go` | 未认证 |
| INVALID_REFRESH | 401 | `tokens.go` | refresh 无效 |
| REFRESH_EXPIRED | 401 | `tokens.go` | refresh 过期 |
| REFRESH_REVOKED | 401 | `tokens.go` | refresh 已吊销 |
| REFRESH_REPLAY | 401 | `tokens.go:265/309` | refresh 重放（旧令牌再用） |

### 6.2 媒体域（`mediasvc/mediasvc.go`）

错误体：`{error, code}`。

| code | HTTP | 含义 |
|------|------|------|
| FILE_TOO_LARGE | 413 | 超过尺寸上限 |
| UNSUPPORTED_TYPE | 415 | 魔数不在白名单 |
| RATE_LIMITED | 429 | 每日上传上限（50/天） |
| INTERNAL | 500 | 落盘/入库失败 |

### 6.3 管理面（`admin/handler/errors.go` + `admin/middleware/*`）

错误体统一 `{code:int(HTTP), reason:string, request_id:string}`。`reason` 即下表：

| reason | HTTP | 来源 | 含义 |
|--------|------|------|------|
| invalid_argument | 400 | `errors.go` | 请求体/参数非法 |
| invalid_idempotency_key | 400 | `idempotency.go` | Idempotency-Key > 128 字节 |
| invalid_credentials | 401 | `handler/auth.go` | 管理登录失败/锁定 |
| unauthenticated | 401 | `middleware/authn.go`,`rbac.go` | 无/无效 token |
| csrf_invalid | 403 | `middleware/csrf.go` | CSRF 双提交缺失或不匹配 |
| permission_denied | 403 | `middleware/rbac.go`,`errors.go` | 角色无该权限 |
| not_found | 404 | `errors.go` | 目标不存在 |
| conflict | 409 | `errors.go` | 状态冲突（如重复处理） |
| request_canceled | 408 | `errors.go`,`idempotency.go` | 客户端取消 |
| rate_limited | 429 | `middleware/ratelimit.go` | 操作限流 |
| internal_error | 500 | `errors.go` | 未归类的内部错误 |
| bridge_unavailable | 503 | `errors.go` | 桥接/注入不可达（带 `Retry-After: 5`） |
| not_configured | 503 | `errors.go` | 依赖未接线 |
| timeout | 504 | `errors.go` | 上下文超时 |

> WS 帧层还有另一套错误码（`ERROR{code}`，如 4000=bad frame、4290=rate limit、403=not member），见 `ws-protocol.md §6`。

---

## 7. WS 帧协议实测（v2.1.0 生产 2026-11-23）

握手：`GET /ws`，带 `Cookie: access_token=…`（也接受 `?token=`，但浏览器客户端只用 Cookie）。
`HTTP/1.1 101 Switching Protocols` 即建连成功（实测响应含 `Server: Caddy`，生产经 Caddy 反代）。

帧格式（`server/internal/user/net/frame/codec.go`）：首字节 `{` = JSON 形态，否则 protobuf。
JSON 形态字段：`id, ack_id, kind, version, request_id, flags, sent_at, conv_id, user_id, payload`；
**`payload` 为 base64**（protobuf bytes 的 JSON 编码；客户端把 payload JSON 序列化后再 base64）。
`conv_id` 与 `user_id` 互斥（同设 → bad frame 断连）；未知 JSON 字段 → `DisallowUnknownFields` 拒绝。

**实测回包（全部为真实生产输出）**：

```text
① PING(kind=4, payload=protobuf Ping{client_ms=1795533159802})
→ 文本帧 {"kind":5,"ack_id":<发送帧id>,"payload":"CXq9HQ6iAQAAEeS4HQ6iAQAA"}
  payload 解出 protobuf Pong{client_ms=1795533159802, server_ms=1795533158628}
  （ack_id 回显发送帧的 id；不回 request_id）

② 发送 {"kind":4,"bogus":1}（未知字段）
→ {"kind":10,"payload":"CKAfEj5iYWQgZnJhbWU6IGZyYW1lOiBiYWQgZnJhbWU6IGpzb246IGpzb246IHVua25vd24gZmllbGQgImJvZ3VzIg=="}
  payload = protobuf Error{code:4000, reason:"bad frame: frame: bad frame: json: unknown field \"bogus\""}
  随后连接以 1003 关闭（资源立即回收，acceptor readLoop）。

③ 发送 payload 非法 base64
→ kind 10, Error{code:4000, reason:"bad frame: … json: illegal base64 data at input byte 0"} + 1003 断连

④ CONV_CREATE(kind=200) / MSG_SEND(kind=101)（非成员会话）
→ 实测无任何回帧（见下方"投递条件"说明），连接保持打开。
```

> **实测差异标注**：非成员/无效会话的 MSG_SEND 按源码 `data/pipe/msg.go` 应回
> `ERROR{code:403, reason:"not a member of conversation"}`（kind 10，按 user_id 寻址），
> 但生产实测未收到回帧。下行投递按 `user_id` 定位连接（`router.deliver`），回包
> 形状以本节 ①② 的真实输出为准；CONV_CREATE 的正常应答为 kind 931
> `{"op","ok","error?","body?","requestId"}`（源码 `data/pipe/pipe.go onConvData`，
> 见 `api_contract_test.go` 的 PASS 用例）。
>
> 服务端主动心跳：acceptor 每 HeartbeatInterval 发 WS Ping 控制帧；客户端 Pong 续读超时。
> 限流超限（kind 13 RATE_LIMITED / 4290 断连）与 ERROR{503,"SHUTDOWN"} 排空广播见源码。

**WS kind 全表（85 个，真源 `server/internal/proto/kinds/kinds.go`）见 `docs/ws-protocol.md`。**

---

## 8. 未实现 / 受限端点清单

| 项 | 状态 | 说明 |
|----|------|------|
| `GET/POST /api/admin/*`（injector 端口的旧 RBAC 路径） | ⚠️ 未实现 | `chatd/admin.go` 注释：`/api/admin/*` 在 injector 端口返回 `WRONG_SURFACE`；RBAC REST 实际在 `admin_rest_listen` 的 `/api/v1/*`。 |
| 管理面 RBAC REST 挂在 `/api/admin/*` 的旧设计 | ⚠️ 未实现 | 旧注释说返回 `501 NOT_IMPLEMENTED`，实际已改为 `/api/v1/*` 且已实现；此处仅记录历史差异。 |
| 冷库 Postgres 读路径 | ⚠️ 未实现 | `cold_dsn=""` 默认不连接；读退化为纯热库（`chatd/config.go` 注释）。 |
| WS 媒体 presign（kinds 300/301 MEDIA_PRESIGN_*） | ⚠️ 未实现 | 常量已定义，但数据层/网关未接处理；媒体走 HTTP `/api/media/*`。 |
| 消息编辑/删除/转发/搜索/回执类 WS kind（106/107/109/110/111 等） | ⚠️ 部分 | 106/107/108/104/105/109 在 `data/pipe` 有处理分支；106/109（及 453 举报）由 `msg_domain_v2_enabled` 门控（编译默认 true，pipe 层回退 false，DB flag 值优先——生产实际行为以 sys_feature_flags 为准）；110/111 仍无 handler 分支。 |
| 用户举报 453 REPORT 上行链路 | ⚠️ 部分（flag 门控） | 数据层 `onReport` 处理分支与 `sys_reports` 入库代码在位（`data/pipe/msgdomain.go`，`msg_domain_v2_enabled` 门控，编译默认 true / pipe 回退 false）。 |
| 通话 kinds 350–354 | ⚠️ 默认关闭（flag 门控） | 处理分支已实现（`data/pipe/call.go` `onCall`），由 `call_enabled` 门控，**默认 false**；开启前上行回 403 "calls disabled"。 |
| Presence kinds 400–402 | ⚠️ 部分 | 400 由客户端上报、数据层处理；401/402 为**服务端产生的连接生命周期事件**（连接建立/断开向同会话对端广播，上行发送回 `ERROR 4003`），非客户端订阅机制（`kinds.go` 注释）。 |
| SMS 短信发送（`SMSSender`） | ⚠️ 未实现 | `authsvc/otp.go` 注释：未接线；dev 用固定码，生产经 v1.4 webhook 适配器下发（未配置时 fail-closed）。 |
| 推送订阅端点 `/api/push/subscribe`、`/api/push/test` | ✅ 已实现（v1.5） | 见 §4a；Notify 钩子已接数据层 flusher，VAPID 未配置时 fail-closed no-op。 |
| 媒体异步病毒扫描（ClamAV） | ⚠️ 未实现 | `mediasvc` 注释：MVP 行出厂即用（status=1），1.1 才有扫描流水线。 |
| Captcha 强制校验 | ⚠️ 未实现 | `authsvc/handlers.go` 注释：字段接受但未强制（TODO-CAPTCHA）。 |
