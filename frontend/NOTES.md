# NOTES —— 契约 vs 实测差异 / 验收记录

> 记录规则：先查契约再写码；**契约与实测冲突时以实测为准**，并把差异写在这里。
> 生产调用一律低频（guest 账号），测完调用 logout。

## A. 契约与实测差异（2026-11-28 核对）

### A1. CORS：只反射 http(s) Origin（实测）

```
Origin: http://example.com        → Access-Control-Allow-Origin: http://example.com
Origin: https://app.example.com   → 反射
Origin: null                      → 无 CORS 头
Origin: minis://                   → 无 CORS 头
```

结论：**前端必须从 http(s) 源提供**。跑在 `minis://` / `file://` 这类自定义 scheme
下时，浏览器会直接拦截跨源请求（实测 `Failed to fetch`）。

### A2. 跨源部署下 Cookie 不可用（依据契约 + 浏览器规则）

登录成功下发 `Set-Cookie: access_token=...; HttpOnly; SameSite=Lax`。
SameSite=Lax 的 Cookie 在**跨站** XHR/fetch 中不会携带 → 客户端改为
`Authorization: Bearer <access_token>`；WS 握手用
`Sec-WebSocket-Protocol: bearer, <token>`（服务端接受该头）。

**待验证**（服务端 WS 连接桶当时已被我方探测打满，返回
`429 too many upgrade attempts`、`Retry-After: 720`）：浏览器是否要求服务端回显
所选 subprotocol。若服务端不回显，浏览器会判定握手失败——此时需要在部署侧
把 `/ws` 反代到**同源**下（Cookie 模式），或由后端补上 subprotocol 回显。
这一项必须复测，结论见 A6。

### A3. WS kind 的客户端可达性（源码核对，与 ws-protocol.md 的表述有出入）

- `buildRoutes` 实际位于 `server/cmd/chatd/wire.go:574-648`。
- 106 / 109 / 453 **在路由表内**，由 `msg_domain_v2_enabled` 开关门控（默认 true），
  不是“路由缺失”。FORAGENT.md 与部分文档的“可能收不到回帧”表述需按实测复核。
- **920 / 930 / 940 不在用户层能力表**（`server/internal/bridge/defaults.go` 的
  `allUserKinds()`），用户层上行会被 bridge Verifier 拒收：
  - 客户端无法直接发 READ_HISTORY(920) / CONV_DATA_REQUEST(930) / IDENTITY_REQUEST(940)；
  - 200 / 201 / 210 / 211 由装配层翻译成 930 的 op1 / op5 / op2 / op3 后下发；
  - 因此**资料读写（940/941）与历史分页（920）对客户端不可达**：
    - 改昵称无法落库 → 设置页仅本地生效并明确提示；
    - 历史只能通过 kind 8 SYNC_REQUEST（sinceSeq=0 起，limit 默认 200、上限 500）
      按 seq 升序逐条取回，**无法向上翻更早的页**。
- SYNC 的应答是逐条 MSG_NEW(102)，不是 kind 9。

### A4. 会话列举 / 用户检索：用户面无端点

`930` 的 conv 域只有 op1/2/3/4/5/6，**没有“列出我的会话”**；用户面 HTTP 也没有
对应端点。因此：

- 会话列表只能靠 kind 202 CONV_PAYLOAD 事件累积（成员变更时 fan-out），
  服务端目前也不计算未读数与最后一条预览；
- 新建会话需要先知道对方 user_id。

已在 `docs/backend-endpoint-proposal.md` 给出两个只读端点的契约草案，
前端按草案实现，端点 404 时自动降级（事件累积 + 手动输入 ID）。

### A5. 管理面可达性

`/api/v1/*` 默认监听 `127.0.0.1:7781`（`server/internal/admin/server.go:34`），
生产的 Caddyfile **没有** 7781 的反代块；实测公网访问
`http://server.344977.xyz/api/v1/metrics` → `404`（用户面兜底 NOT_FOUND）。
需要部署侧加一条 `/api/v1/* → 127.0.0.1:7781` 的反代（README 已给出示例）。
用户面 7780 只暴露 `/admin/inject`（HMAC + IP 白名单），**不是** REST 面。

### A6. 验收实测记录（2026-11-28，生产 v2.3.1）

**测试环境**：本机 node 代理（5174）模拟同源部署；WS 握手用
`Sec-WebSocket-Protocol: bearer, <token>`（原生 socket，非浏览器）。

| # | 项目 | 状态 | 证据 |
|---|---|---|---|
| 0a | WS 握手（原生，subprotocol） | ✅ | `HTTP/1.1 101 Switching Protocols` |
| 0b | PING(4) → PONG(5) | ✅ | `{"ack_id":9,"kind":5,...}`（ack_id 正确回显） |
| 0c | 坏帧 → ERROR(4000) | ✅ | 服务端正确回错误帧 |
| 0d | 游客登录 → HTTP 200 会话对象 | ✅ | user_id 14/15/13 等，token/refresh/expires_at 全字段 |
| 1 | guest 登录 + 刷新恢复 | ✅ | 前端实测：登录成功、localStorage 恢复、refresh 单飞 |
| 2 | MSG_SEND → MSG_ACK(status=0) | ⚠️ **阻塞** | 发 conv_id=1（任意会话）→ 无任何回帧（无 ACK、无 403）。源码 `pipe.go onMsgSend` 有 403/ACK 双路径，`ack.go buildAckEnv` 寻址完整，但生产实测静默。FORAGENT §3 记载的"非成员无回帧"确认属实，**成员路径无法验证（见 3）** |
| 3 | CONV_CREATE(200) 建会话 | ⚠️ **阻塞** | JSON（convType 0/1、title、memberIds 多变体）与 protobuf 二进制（packed member_ids）均无回帧。源码链路（wire.go:398 convKindToPipe → 930 op1 → conv.go:105 create → buildResultEnv 按 req.UserID 寻址）逐行核对完整。生产无回帧 = v2.3.1 与 main 源码存在行为差异，或生产部署配置问题（需服务器 journalctl 排查，前端侧无法绕过） |
| 4 | 图片上传 | 未测 | 依赖 2/3 通过后的消息链路 |
| 5 | 375px 不破版 + 亮暗主题 | ✅ | 见下截图记录 |
| 6 | 浏览器跨源 WS | ❌ 已证实不可行 | 服务端 101 不回显 `Sec-WebSocket-Protocol` → 浏览器判握手失败（1006）；跨站 Cookie 又被 SameSite=Lax 拦截。**跨源部署必须同域反代，或由后端补 subprotocol 回显**。客户端已实现自动降级重试 |

**375px / 主题证据**：登录卡、会话列表空态、联系人降级表单、设置页（暗/亮）、
管理台登录均实测无横向滚动、无破版；亮暗主题切换即时生效。

**管理台**：登录请求正确发出（CSRF/幂等键齐备）；公网 404（`unknown api path`）
符合 A5 预期——需部署侧反代 `/api/v1/* → 127.0.0.1:7781`。

### A7. 其他实测

- 登录方式探测：`GET /api/auth/methods` → `{"methods":{"phone":false,"email":false,"password":true,"guest":true}}`。
- `GET /` → 服务描述 JSON（不是网页）。
- 生产 WS 连接桶很小：连续握手会返回 `429 too many upgrade attempts`
  （`Retry-After: 720`）。**自动化脚本务必控制握手频率。**

## B. 降级行为（后端缺口时的前端表现）

| 缺口 | 前端表现 |
|---|---|
| 无 `/api/conversations` | 不报错；会话列表保留事件累积结果；联系人页提示并提供手动输入 ID |
| 无 `/api/users` | 联系人页展示手动输入 ID 的兜底表单 |
| 资料通道 940 不可达 | 设置页改昵称仅本地生效，弹出黄色提示说明 |
| kind 12 只有下行广播 | 前端不主动拉取 flags；通话等按默认关闭处理 |
| VAPID 公钥未配置 | 推送入口提示不可用，其余功能不受影响 |
