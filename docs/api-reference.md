# OpenDiscord API 参考（1.0）

> 描述版本：v1.4.0（2026-11 复核）。本文档只描述**代码中真实存在**的接口。
> 所有字段名、路径、错误码均取自源码（见每节"源码"行）。凡代码未实现的能力一律标注 **⚠️ 未实现**。
>
> 验收基线：`server/cmd/chatd/api_contract_test.go` 跑出 **PASS=53 / FAIL=0 / SKIP=0**（2026-10-31 复核）。

---

## 1. 概览

OpenDiscord 有 **两个 HTTP 面 + 一个 WebSocket 面**，全部由同一个 `chatd` 进程内的真实组件提供。

| 面 | 监听配置项 | 默认地址 | 作用 |
|----|-----------|---------|------|
| 用户面 HTTP | `listen` | `127.0.0.1:8080` | `/api/auth/*`、`/api/media/*`、`/healthz`、`/ws`、SPA 静态资源（同端口） |
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

## 2. 认证域 `/api/auth/*`（9 端点）

挂载在用户面（`listen`）同一 mux；`POST` only，预检走 `OPTIONS`（不 405）。
成功统一返回**会话对象**（除非另有说明），失败返回 `{error, code, status?, retry_after_ms?}`。

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

**上传约束**（`mediasvc/mediasvc.go`）：

- 大小上限：图片 `8MiB`、音频 `2MiB`、其它文件 `20MiB`；整体硬顶 `maxFileBytes = 20MiB`。
- 类型按**魔数**判定（JPEG/PNG/WEBP/GIF/OGG/MP3），未知类型 → **415** `UNSUPPORTED_TYPE`。
- 可执行/脚本黑名单扩展名（`.exe/.sh/.js/.svg/.html/...`）直接拒绝。
- 单用户每日上传上限 `50` 次 → 超限 **429** `RATE_LIMITED`。
- 内容寻址：同账号重复上传相同字节返回 200（同 `media_id`）。
- `mtype` 取值：`1`=image、`2`=audio、`4`=file。
- 访问 `GET /api/media/{id}`：非上传者 / 不存在均返回 **404**（不泄露存在性）。

---

## 4. 健康检查

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

## 7. 验收状态

`server/cmd/chatd/api_contract_test.go` 用真实装配（真 SQLite 热库、真 admin 库、真传输、真 WS acceptor、真 REST 监听器）驱动全部 1.0 面：

- **PASS=53 / FAIL=0 / SKIP=0**。
- 覆盖：`/healthz`、auth 9 端点（含 wrong/replay/409 边界）、media 6 探测（png/.bin/>20MB/no-auth/uploader/other-user）、admin 全业务路由（含 review 角色 403、broadcast 幂等重放、archive dry_run）、WS 握手（无/有 cookie）、CONV_CREATE→CONV_DATA_RESULT(931)+CONV_PAYLOAD(202)、PING→PONG、MSG_SEND→MSG_ACK、SYNC_REQUEST→MSG_NEW、非成员 MSG_SEND→ERROR 403、`>20 msg/s` 触发 RATE_LIMITED 或断连。

复跑命令：

```
export GOCACHE=/tmp/gocache GOTMPDIR=/tmp TMPDIR=/tmp GOMODCACHE=/tmp/gomod GOFLAGS=-mod=mod GOPROXY=direct
go test ./server/cmd/chatd/ -run TestAPIContract -count=1 -v
```

---

## 8. 未实现 / 受限端点清单

| 项 | 状态 | 说明 |
|----|------|------|
| `GET/POST /api/admin/*`（injector 端口的旧 RBAC 路径） | ⚠️ 未实现 | `chatd/admin.go` 注释：`/api/admin/*` 在 injector 端口返回 `WRONG_SURFACE`；RBAC REST 实际在 `admin_rest_listen` 的 `/api/v1/*`。 |
| 管理面 RBAC REST 挂在 `/api/admin/*` 的旧设计 | ⚠️ 未实现 | 旧注释说返回 `501 NOT_IMPLEMENTED`，实际已改为 `/api/v1/*` 且已实现；此处仅记录历史差异。 |
| 冷库 Postgres 读路径 | ⚠️ 未实现 | `cold_dsn=""` 默认不连接；读退化为纯热库（`chatd/config.go` 注释）。 |
| WS 媒体 presign（kinds 300/301 MEDIA_PRESIGN_*） | ⚠️ 未实现 | 常量已定义，但数据层/网关未接处理；媒体走 HTTP `/api/media/*`。 |
| 消息编辑/删除/转发/搜索/回执类 WS kind（106/107/109/110/111 等） | ⚠️ 未实现（除下述例外） | kind 已注册、能力表列出；106/107/108/104/105/109 在 `data/pipe` 有处理分支，**但 106/109（及 453 举报）由 `msg_domain_v2_enabled` 门控、107/108 召回为持久化处理**；110/111 仍无 handler 分支。 |
| 用户举报 453 REPORT 上行链路 | ⚠️ 规划中（v1.5 步骤 3 实现） | 数据层 `onReport` 处理分支与 `sys_reports` 入库代码在位（`data/pipe/msgdomain.go`，`msg_domain_v2_enabled` 门控），但用户层上行白名单未放行 453，前端举报按钮保持占位。 |
| 通话 kinds 350–354 | ⚠️ 默认关闭（flag 门控） | 处理分支已实现（`data/pipe/call.go` `onCall`），由 `call_enabled` 门控，**默认 false**；开启前上行回 403 "calls disabled"。 |
| Presence kinds 400–402 | ⚠️ 部分 | 400 由客户端上报、数据层处理；401/402 为**服务端产生的连接生命周期事件**（连接建立/断开向同会话对端广播，上行发送回 `ERROR 4003`），非客户端订阅机制（`kinds.go` 注释）。 |
| SMS 短信发送（`SMSSender`） | ⚠️ 未实现 | `authsvc/otp.go` 注释：未接线；dev 用固定码，生产经 v1.4 webhook 适配器下发（未配置时 fail-closed）。 |
| 推送订阅端点（前端 `net/push` 期望的订阅上行） | ⚠️ 未实现（规划中，v1.5 步骤 3 实现） | 前端 `apps/client/src/net/push/index.ts` 的 VAPID 订阅封装已就位，但服务端尚无对应订阅接收端点与 push 事件下发；推送 fail-closed。 |
| 媒体异步病毒扫描（ClamAV） | ⚠️ 未实现 | `mediasvc` 注释：MVP 行出厂即用（status=1），1.1 才有扫描流水线。 |
| Captcha 强制校验 | ⚠️ 未实现 | `authsvc/handlers.go` 注释：字段接受但未强制（TODO-CAPTCHA）。 |
