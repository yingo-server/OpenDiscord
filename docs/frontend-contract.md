# UFOT 前端契约（给重写者的唯一依据）

> 本文件是前端重写的完整契约。后端是已上线生产服务（server.344977.xyz），契约以真实行为为准。
>
> 实测入口：生产服务器 http://server.344977.xyz
> - HTTP API：http://server.344977.xyz/api/...
> - WebSocket：ws://server.344977.xyz/ws
> - 已部署的旧前端（可观察真实报文）：http://server.344977.xyz/
> 允许直接调用生产 API 验证行为；不允许 SSH。

## 0. 品牌与视觉基线
- 名称：UFOT（Unfettered Freedom Of Thought）；口号 They demanded unfettered freedom of thought.
- 配色：纯黑白（黑白灰 + 至多一个强调色；高级感来自留白/层次/字重对比）
- 字体：系统栈（Segoe UI / -apple-system / PingFang SC / Noto Sans SC），不引入字体文件
- 气质：克制、精密、密度高但不拥挤；不用 emoji、不堆渐变、不用超大圆角

## 1. 环境变量优先，回退默认值
所有可变点从环境变量读，没有则回退下表（构建期 Vite import.meta.env）：
| 变量 | 含义 | 默认（回退） |
|---|---|---|
| VITE_API_BASE | 后端 HTTP 根 | 空 = 同源 location.origin |
| VITE_WS_URL | 后端 WebSocket 全 URL | 由 VITE_API_BASE 推导；再回退 location.host 拼 /ws |
| VITE_DEFAULT_LANG | 默认语言 | zh-CN |

解析规则（必须一致）：
  API_BASE = env.VITE_API_BASE || location.origin
  WS_URL  = env.VITE_WS_URL
          || (API_BASE ? scheme(API_BASE)+'//'+host(API_BASE)+'/ws'
                        : (location.protocol==='https:'?'wss':'ws')+'://'+location.host+'/ws')
开发模式（import.meta.env.DEV）额外允许 VITE_DEV_WS_URL 指向本地后端。

## 2. HTTP API（全部 POST，除注明外）
统一响应头 Content-Type: application/json; charset=utf-8，Cache-Control: no-store。
错误响应体统一为 {"code":"<机器码>","message":"<可展示文案>"}；message 面向终端用户，不得含 driver/SQL/栈信息。

### 2.1 认证 /api/auth/*（错误体实测为 {"error":"<文案>","code":"<机器码>"}）

**登录方式开关**：四种登录方式各有一个开关（policy.json 的 `auth.methods`，默认全部 false）。
前端应先调用 `GET /api/auth/methods`（公开、不鉴权、按 IP 限流 60/min）拿到
`{"methods":{"phone":b,"email":b,"password":b,"guest":b}}`，只渲染可用方式；
被关闭的方式会返回 `403 {"error":"...","code":"METHOD_DISABLED"}`。

| 端点 | 请求体 | 成功响应 |
|---|---|---|
| POST /api/auth/phone/code | {"phone":"+8613800138000","captcha_token":"..."?} | {"ok":true,"challenge_id":"...","channel":"sms","expires_in":300,"resend_after":60} |
| POST /api/auth/phone/verify | {"phone":"...","code":"123456","challenge_id":"...","device_id":"..."?} | 会话对象（见 2.3） |
| POST /api/auth/email/link | {"email":"a@b.c","captcha_token":"..."?} | {"ok":true,"challenge_id":"...","channel":"email","expires_in":600,"resend_after":60} |
| POST /api/auth/email/poll | {"email":"a@b.c","challenge_id":"...","device_id":"..."?} | 链接未点 → 409 {"error":"pending","code":"PENDING","status":"pending"}；已点 → 会话对象 |
| POST /api/auth/password | {"account":"手机/邮箱/用户名","password":"...","device_id":"..."?} | 会话对象 |
| POST /api/auth/guest | {"nickname":"可选","device_id":"..."?} | 会话对象 |
| POST /api/auth/refresh | {"refresh_token":"...","device_id":"..."?} | 会话对象（轮换） |
| POST /api/auth/logout | {} | {"ok":true,"revoked":bool} |
| POST /api/auth/password/set | {"password":"..."}（需登录，≥10 字符） | {"ok":true} |

### 2.2 媒体 /api/media/*（实测 2026-11-23）
- POST /api/media/upload：multipart 字段名 file，返回 **201** {"media_id":"...","mime":"image/png","mtype":1,"size":208}
- GET /api/media/{id}：取文件（需登录 Cookie），200 字节流（实测 Content-Type: image/png）；未登录 401 {"code":"UNAUTHORIZED","error":"unauthorized"}
- 上限：图片 8MB / 音频 2MB / 文件 20MB；魔数白名单（JPEG/PNG/WEBP/GIF/OGG/MP3）；每日 50 次
- WS presign（kind 300-304）未实现，媒体一律走本 HTTP 面

### 2.3 会话对象（登录成功返回，实测于 v2.1.0 生产 2026-11-23）
{"user_id":123,"device_id":"g_...","access_token":"<JWT>","token":"<== access_token>",
 "refresh_token":"<JWT>","expires_at":1795533858563,"expires_at_unix":1795533858,
 "session_id":"g_...","guest":true,"is_new_user":true,"profile":{"display_name":"游客xxxx…"}}

源码真源：authsvc/tokens.go sessionPayload（display_name 之外的字段 omitempty 时可能缺省）。
注意 expires_at 是毫秒、expires_at_unix 是秒（与早期文档相反，以实测为准）。

鉴权方式：登录后服务端下发 Set-Cookie: access_token=...; HttpOnly; Path=/。
后续 HTTP 请求带 Cookie 即可；WebSocket 握手也用 Cookie（浏览器自动带）。
access_token 也存内存（需要 Bearer 的场合），但不要放 localStorage。

### 2.4 推送 /api/push/*（实测 2026-11-23）
- POST /api/push/subscribe  body {"subscription":{"endpoint":"https://...","keys":{"p256dh":"<base64url 65B>","auth":"<base64url 16B>"}}}  成功 **204 无 body**
- POST /api/push/test  带 Cookie，无 body → {"sent":N}（200）；无有效订阅时 sent:0
- 错误体为 {"code":"BAD_REQUEST","error":"invalid p256dh"}（注意是 error 字段承载文案）

### 2.5 管理面 /api/admin/v1/*
独立管理 SPA 用（TOTP 登录），仅监听回环地址，外网 403。本项目用户端不调用它们。
端点：auth/login、metrics、users、messages、reports、flags、audit、archive、broadcast、login-history

### 2.6 资料更新（无 HTTP 端点，走 WebSocket）
服务端 **没有** /api/profile/update 这个 HTTP 端点（不要猜）。
唯一路径：WS kind 940 IDENTITY_REQUEST，payload：
  {"op":1,"userId":<当前 uid>,"body":"<base64 的 store.User JSON>"}
- op=1 = OpUpsertUser；body 是 store.User 的 JSON（字段以 server/internal/data/store/api.go 的 User 定义为准）
- op=2 = OpGetUser（读回资料，body 同格式）
- 服务端回 kind 941 IDENTITY_RESULT，payload {"op":N,"ok":bool,"error":"...","body":"<base64>","requestId":""}
- 失败时 ok=false，必须提示用户并撤销本地乐观更新

## 3. WebSocket 协议（ws://.../ws）
- 单连接，一切皆帧。文本帧 = JSON UTF-8。
- 帧结构：{"kind":<int>,"id":"<request id>","conv_id":<int>,"payload":<any>,"target":{...},"ack_id":<int>}
- 握手：浏览器带 Cookie 连 /ws；服务端校验 access_token，失败下发 kind 10 ERROR 后关闭。
- 心跳：客户端每 20s 发 kind 4 PING，服务端回 kind 5 PONG；60s 无心跳服务端断开。
- 响应关联：请求帧带 id，响应帧回同一 id；错误也回同一 id（kind 10 ERROR）。
- 能力协商：连接后建议发 kind 18 CLIENT_CAPS，payload {"pull_capable":true}；
  服务端据此改用水位线 + 客户端拉取，下行帧量大幅降低。
- 未知 kind 必须忽略而不是报错。

### 3.1 全部 kind 号（服务端真值）
1 HELLO   2 AUTH      3 AUTH_RESULT   4 PING    5 PONG    6 RESUME
7 RESUME_RESULT  8 SYNC_REQUEST  9 SYNC_RESULT  10 ERROR  11 KICKED
12 FEATURE_FLAGS  13 RATE_LIMITED  14 WATERMARK  15 MAILBOX_ACK
16 MAILBOX_PAUSE  17 MAILBOX_RESUME  18 CLIENT_CAPS
101 MSG_SEND  102 MSG_NEW  103 MSG_ACK  104 MSG_READ  105 MSG_DELIVERED
106 MSG_EDIT  107 MSG_DELETE  108 MSG_RECALL  109 MSG_FORWARD  110 MSG_SYNC  111 MSG_SEARCH
200 CONV_CREATE  201 CONV_UPDATE  202 CONV_PAYLOAD  210 MEMBER_ADD
211 MEMBER_REMOVE  212 MEMBER_UPDATE  213 ROLE_SET  220 MUTE  221 PIN
230 TYPING  231 DRAFT
300 MEDIA_PRESIGN_REQUEST  301 MEDIA_PRESIGN_RESULT  302 MEDIA_READY
303 MEDIA_EXPIRE  304 MEDIA_THUMB
350 CALL_OFFER  351 CALL_ANSWER  352 CALL_CANDIDATE  353 CALL_BUSY  354 CALL_HANGUP
400 PRESENCE_UPDATE  401 PRESENCE_ONLINE  402 PRESENCE_OFFLINE
450 PROFILE_UPDATE  451 BLOCK  452 UNBLOCK  453 REPORT
500 ADMIN_BROADCAST  501 ANNOUNCE  502 MAINTENANCE
902 INTERNAL_HELLO  903 INTERNAL_SUBSCRIBE  904 INTERNAL_FRAME_ACK
905 INTERNAL_HEARTBEAT  910 SHUTDOWN_NOTICE  911 FLAG_SYNC  912 CONFIG_UPDATE
913 TOPOLOGY  920 READ_HISTORY  921 READ_HISTORY_RESULT
930 CONV_DATA_REQUEST  931 CONV_DATA_RESULT
940 IDENTITY_REQUEST  941 IDENTITY_RESULT
960 SECURITY_REQUEST  961 SECURITY_RESULT
970 FLAG_GET  971 FLAG_SET  972 FLAG_RESULT
980 JOB_TRIGGER  981 JOB_STATUS  990 DIAG_LOOPBACK
（90x/9xx 是服务端内部帧，用户端不需处理，但必须忽略未知 kind。）

### 3.2 关键 payload 形状
- 101 MSG_SEND：{"conv_id":N,"id":"client-msg-id","payload":{"text":"...","reply_to":"...","mentions":[uid],"media":[id]}}
- 102 MSG_NEW：{"conv_id":N,"id":"msg-id","seq":N,"payload":{"sender_id":N,"text":"...","created_at":N,"reply_to":"","mentions":[],"media":[],"kind":"text"}}
- 103 MSG_ACK：{"id":"<回显请求 id>","payload":{"status":0,"msg_id":"...","seq":N}}
  status 非 0 表示失败，必须当失败处理（不得当成功）
- 104 MSG_READ / 105 MSG_DELIVERED：{"conv_id":N,"seq":N}
- 230 TYPING：{"conv_id":N,"payload":{"typing":true}}
- 13 RATE_LIMITED：{"scope":"conn","limit":1,"retry_after_ms":500}
- 10 ERROR：{"code":"...","message":"..."}
- 14 WATERMARK：{"conv_id":N,"payload":{"latest_seq":N}} → 客户端应发 8 SYNC_REQUEST 拉缺口
- 8 SYNC_REQUEST：{"conv_id":N,"payload":{"since_seq":N,"limit":200}} → 9 SYNC_RESULT 返回消息数组
- 200 CONV_CREATE：{"payload":{"type":"direct"|"group","member_ids":[uid],"title":"..."}}
- 453 REPORT：{"conv_id":N,"payload":{"target_msg":"msg-id","reason":1,"detail":""}}

重要：形状以实测为准。上表是契约说明，实现时用生产 API/WS 抓真实报文校准。

## 4. 功能清单（必须全部可用）
1. 登录：手机验证码 / 邮箱魔法链接 / 账号密码 / 游客（四个 tab；游客无需填任何字段）
2. 会话列表：置顶/静音/未读数/搜索/新建会话（单聊、群聊）
3. 聊天：收发消息、编辑、撤回、删除、转发、回复、@提及、已读/送达状态、输入中提示、乐观发送与失败重试
4. 媒体：图片/语音/文件上传与展示（缩略图、波形）
5. 通话：WebRTC 1v1（offer/answer/candidate、接听/拒绝/挂断/静音/免提）
6. 设置：改昵称/头像/密码、主题（亮/暗）、清除缓存、退出登录
7. 全局：断线重连与指数退避、离线补拉、未读角标、网络状态条

## 5. 工程要求
- 分层清晰：api/（HTTP+WS 传输）、domain/（类型与纯逻辑）、state/（store）、ui/（组件与页面）、platform/（env/存储/时间）。
  层间单向依赖，禁止反向 import；跨层只经公开接口。
- 易维护：一个职责一个文件；单文件 < 300 行；类型集中定义；禁止 any（用 unknown + 类型守卫）。
- 鲁棒：WS 重连退避；请求超时与取消；ErrorBoundary 包页面；加载/空/错误三态齐全；乐观更新可回滚。
- 简洁：优先平台原生能力（Intl、matchMedia、ResizeObserver、IntersectionObserver）；不引重量级依赖。
- 性能：长列表虚拟化；重渲染 memo 化；图片懒加载；首屏 bundle 友好。
- 测试：关键逻辑（env 解析、WS 帧编解码、重连退避）有单测。

## 6. 交付方式
- 产出目录：apps/client（npm run build 出静态站，产物嵌进 chatd 二进制）
- 只改 apps/client，不动 server/、协议、数据库
- 完成后 npm run build 必须通过；tsc --noEmit 零错误

## 7. 自检清单（交付前逐条确认）
- 四个登录方式都能真实登录成功
- 消息能发出、能在另一端收到、失败会重试
- 断网后恢复能自动重连并补拉
- 刷新页面能恢复会话（Cookie/refresh token）
- 切换到窄屏（375px）布局不破
- 亮/暗主题都可用，对比度达标
