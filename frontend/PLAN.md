# UFOT Web 客户端 —— 前端方案（apps/frontend → frontend/）

> 版本基线：后端 v2.3.1（生产 http://server.344977.xyz）
> 事实来源：FORAGENT.md、docs/client-contract.md、docs/api-reference.md、docs/ws-protocol.md + 生产实测
> 纪律：不本地构建（只提交源码）；只写 `frontend/`（+ `apps/frontend/README.md` 指针）；不 commit/push 之外的目录由用户审查后处理

---

## 0. 定位与技术选型

| 项 | 选择 | 理由 |
|---|---|---|
| 框架 | React 19 + TypeScript 5 + Vite 7 | 生态成熟、类型安全、Vite 构建快（CI 构建） |
| 路由 | react-router 7（BrowserRouter + SPA fallback 由部署层保证） | 语义化路径 `/chats/:convId` |
| 状态 | Zustand（按域分 store） | 轻量、无样板、与 WS 事件流易组合 |
| 样式 | CSS Modules + 全局 token（CSS 变量），无 UI 框架 | 黑白基调 + 三色强调，框架会带来自带风格 |
| 图标 | Font Awesome 6 Free（npm 自托管，禁止 CDN） | 需求指定 awesome 图标；离线可用，无 emoji |
| HTTP | fetch + 包装器（credentials: 'include'） | 服务端 Cookie 为 HttpOnly + SameSite=Lax |
| WS | 原生 WebSocket + 自研帧编解码层 | 必须严格遵守 envelope snake_case / payload base64 / id 数字 |
| 存储 | localStorage（会话令牌、设备 ID、草稿、主题、已读水位） | 刷新可恢复；令牌敏感项标注 |

### 视觉基调（对齐 docs/brand-guidelines.md）
- 底色：暗色 `#0a0a0a`；亮色 `#ffffff` / `#f5f5f5` 分层
- 文字：主 `#ffffff` / 亮色 `#111111`；次要 `#cfcfcf` / `#6b6b6b`
- 强调色（仅用于语义状态与关键动作，不做装饰）：
  - 正红 `#e5484d` → 危险操作（删除、封禁、撤回、错误）
  - 正黄 `#f5a524` → 警告（未读、离线、降级、待处理）
  - 正蓝 `#3b82f6` → 信息/主操作（自己发送的消息气泡、链接、选中态）
- 禁止：emoji、渐变、发光边框、装饰性图标徽章

---

## 1. 目录分层（结构清晰、分层）

```
frontend/
├─ index.html
├─ package.json / tsconfig.json / vite.config.ts / .env.production / .env.development
├─ NOTES.md                       # 契约 vs 实测差异、验收证据
└─ src/
   ├─ app/                        # ① 装配层：路由表、Provider、全局错误边界、启动引导
   │   ├─ router.tsx
   │   ├─ providers.tsx
   │   └─ boot.tsx                # 探测 /api/auth/methods、恢复会话、建 WS
   ├─ core/                       # ② 内核层：与 UI 无关的领域能力（可单测）
   │   ├─ config/env.ts           # 契约 §1 的 env 解析公式
   │   ├─ net/http.ts             # fetch 包装 + 错误归一（{error,code} 与 {code,message} 双形态）
   │   ├─ net/ws/codec.ts         # envelope 编解码（snake_case + payload base64 + 数字 id）
   │   ├─ net/ws/client.ts        # 连接、心跳、退避重连、断线补拉
   │   ├─ domain/auth.ts          # 四种登录 + refresh 轮换 + logout
   │   ├─ domain/conv.ts          # 会话创建/成员/元数据
   │   ├─ domain/msg.ts           # 发送/ACK 配对/编辑/撤回/转发/回执/输入中
   │   ├─ domain/media.ts         # 上传（图片/音频/文件）+ 尺寸魔数白名单前置校验
   │   ├─ domain/presence.ts      # 在线状态
   │   ├─ domain/admin.ts         # 管理面 REST（/api/v1/*，独立鉴权域）
   │   └─ store/                  # zustand：auth / conn / conv / msg / ui
   ├─ ui/                         # ③ 设计系统：无业务
   │   ├─ primitives/ (Button, Input, Select, Dialog, Sheet, Tabs, Toast, Icon, Avatar, Badge, Spinner, EmptyState, Skeleton)
   │   └─ layout/ (AppShell, NavRail, ConversationList, ChatPanel, TopBar, BottomTabs)
   ├─ features/                   # ④ 功能层：按业务域组织页面与局部组件
   │   ├─ auth/ (LoginPage, GuestForm, PasswordForm, PhoneForm, EmailForm)
   │   ├─ chat/ (ConversationListPage, ChatPage, MessageList, MessageBubble, Composer, MediaBubble, ReplyQuote, TypingIndicator, ReadReceipts, MessageActions, MemberPicker)
   │   ├─ contacts/ (ContactsPage, NewConversationPage)
   │   ├─ settings/ (SettingsPage, ProfileSection, SecuritySection, ThemeSection, PushSection, ConnectionSection)
   │   └─ admin/ (AdminLoginPage, AdminShell, UsersPage, ReportsPage, MessagesPage, FlagsPage, AuditPage, BroadcastPage, ArchivePage, DiagnosticsPage)
   ├─ styles/ (tokens.css, base.css, light.css, dark.css)
   └─ main.tsx
```

依赖方向严格单向：`app → features → ui → styles`；`features/core` 可用 `core`；`core` 不引用 `features`/`ui`；`ui` 不引用 `core`。

---

## 2. 页面清单与信息架构

| # | 路由 | 页面 | 关键能力（后端映射） |
|---|---|---|---|
| 1 | `/login` | 登录 | 动态 tab（游客 / 密码 / 手机验证码 / 邮箱魔法链接），按 `GET /api/auth/methods` 只渲染可用项；设备 ID 持久化 |
| 2 | `/chats` | 会话列表 | 搜索、置顶、静音标记、未读计数、最后一条预览、最后时间、在线状态点；新建单聊/群聊 |
| 3 | `/chats/:convId` | 会话 | 消息收发/编辑/撤回/删除/转发/回复/@提及/已读送达/输入中/乐观发送与失败重试；历史向上翻页；未读分割线 |
| 4 | `/contacts` | 联系人 | 用户目录（能力取决于后端，见 §5 风险）；新建会话入口 |
| 5 | `/me` | 我的 | 昵称（kind 940）、改密（`POST /api/auth/password/set`）、主题、推送订阅、连接诊断、登出 |
| 6 | `/admin/*` | 管理台 | 独立鉴权（`/api/v1/login` + CSRF 双提交 + Idempotency-Key）：用户/封禁/踢下线、举报队列、消息检索与召回、特性开关、审计流、广播、归档、指标、TURN 自检 |

导航外壳（≥1024px）：
```
┌──────┬──────────────────┬────────────────────────────┐
│ 侧栏 │ 会话列表 320px     │ 会话主体（自适应）           │
│ 72px │ 搜索/新建          │ 顶栏 + 消息流 + 输入区        │
└──────┴──────────────────┴────────────────────────────┘
```

---

## 3. 全平台 / 全尺寸适配矩阵

| 断点 | 布局 | 输入区 | 列表 | 触控目标 |
|---|---|---|---|---|
| ≥1440px | 三栏（72 + 340 + flex） | 单行 + 侧边工具条 | 常驻可滚动 | ≥40px |
| 1024–1439px | 三栏（64 + 300 + flex） | 单行 | 常驻 | ≥40px |
| 768–1023px | 两栏（280 + flex），侧栏收为图标 | 单行，工具条折叠为 + 菜单 | 常驻 | ≥40px |
| <768px | 单栏路由切换（列表 ⇄ 详情） | 双行（文本区 + 工具条）+ 安全区内边距 | 底部 Tab 切换 | ≥44px |
| 横屏矮屏（<600px 高） | 紧凑顶栏 44px，输入区压缩 | 单行 | — | ≥40px |

尺寸刻度（唯一真源，CSS 变量）：
```
--sp-1 4 / --sp-2 8 / --sp-3 12 / --sp-4 16 / --sp-5 24 / --sp-6 32
--fs-xs 12 / --fs-sm 13 / --fs-md 14 / --fs-lg 16 / --fs-xl 20 / --fs-2xl 24
--r-s 8 / --r-m 12 / --r-l 16
--icon-sm 14 / --icon-md 16 / --icon-lg 20
--hit-min 40px（移动 44px）
```

兼容性硬规则：
- 所有尺寸用 `rem`，根字号随视口在 14–18px 间夹逼 → 支持 200% 浏览器缩放不破版
- 消息气泡最大宽度：桌面 `min(62%, 720px)`，移动 `82%`
- 长文本/URL/连续 ASCII 强制 `overflow-wrap:anywhere`；代码块横向滚动
- 列表项统一 `min-height:64px`（移动 72px），头像 40/48px 两档
- 输入区 `position:sticky` + `env(safe-area-inset-bottom)`，键盘弹出（`visualViewport`）不遮挡
- `prefers-reduced-motion` 关闭过渡；`@media (hover:hover)` 才启用 hover 态
- 抽屉/弹窗在 <768px 走底部 Sheet（拇指可达），≥768px 走居中 Dialog

---

## 4. WS 客户端状态机（core/net/ws）

```
idle → connecting → open ⇄ reconnecting(指数退避 0.5s→1→2→4→8s，±20% 抖动，60s 内不再重连)
          ↓ 401/403         ↓ kind 11 KICKED（停重连，提示封禁）
        auth-failed        ↓ kind 13 RATE_LIMITED（按 retry_after_ms 退避）
                          ↓ kind 14 WATERMARK → 发 kind 8 SYNC_REQUEST 补缺口
```
- 帧：`id` 严格自增数字；`payload` JSON → UTF-8 → base64；envelope 用 snake_case
- 心跳：应用层每 20s 发 kind 4 PING，收到 kind 5 PONG 更新延迟
- seq 连续性：每会话维护 `lastSeq`，收到 MSG_NEW(102) 发现跳号 → 发 kind 8 `{convId,sinceSeq,limit}`
- ACK 配对：`MSG_ACK.clientMsgId` ↔ 本地乐观消息；`status≠0` 标红并可重发
- 未知 kind 静默忽略（服务端会加号段）
- 帧上限 64KB：媒体不经 WS 走 HTTP

---

## 5. 后端能力映射与已知风险

| 功能 | 后端 | 客户端做法 |
|---|---|---|
| 四种登录 | ✅ 齐 | 动态 tab |
| 发送/ACK/新消息 | ✅ 101/103/102 | 乐观发送 + ACK 替换 |
| 已读/送达 | ✅ 104/105 | 双勾状态 |
| 输入中 | ✅ 230 | 节流 1s（对齐服务端桶） |
| 在线状态 | ✅ 400 上行 / 401/402 下行 | 会话列表圆点 |
| 撤回/删除 | ✅ 107/108 | tombstone 渲染 |
| 编辑/转发/举报 | ⚠️ 文档称实现，但上行路由疑似缺失（FORAGENT 记录 106/109/453 可能无回帧） | 先按契约实现 UI+协议，实测不通则降级为「已发送但服务端未回执」提示，并记 NOTES.md |
| 消息搜索 | ❌ 用户面无（仅管理面 REST） | 列表内本地过滤 + 提示；不承诺全局搜索 |
| 会话列表 | ⚠️ 用户面 HTTP 无列举端点 | 依赖 kind 202 CONV_PAYLOAD 事件累积 + 本地持久化；**需用户拍板**（见提问 Q3） |
| 用户目录/新建会话 | ⚠️ 无列举用户端点 | 输入 user_id / 已有会话 id 兜底；**需拍板**（Q3） |
| 通话 | ⚠️ 生产 `call_enabled=false` | UI 隐藏，仅在 flag 打开时启用 |
| 推送 | ✅ 但服务端不下发 VAPID 公钥 | 公钥走前端 env `VITE_VAPID_PUBLIC_KEY`，缺省则关闭推送开关并说明 |
| 媒体 | ✅ HTTP 直传（图片 8MB / 音频 2MB / 文件 20MB，魔数白名单 JPEG/PNG/WEBP/GIF/OGG/MP3） | 上传前本地预校验，失败给明确原因 |
| 管理面 | ⚠️ `/api/v1/*` 只在回环 7781，公网 404（已实测） | **需拍板**（Q2） |

---

## 6. env 配置

```env
# frontend/.env.production
VITE_API_BASE=http://server.344977.xyz
VITE_WS_URL=ws://server.344977.xyz/ws
VITE_ADMIN_BASE=http://server.344977.xyz     # 管理面反向代理后的前缀（见 Q2）
VITE_VAPID_PUBLIC_KEY=
```
解析公式严格照 client-contract §1：`API_BASE = env || location.origin`；`WS_URL = env || scheme(API_BASE)+'://'+host(API_BASE)+'/ws'`。

---

## 7. 验收自检（写入 NOTES.md 附证据）

1. 游客登录成功 → 刷新页面会话恢复（refresh token 轮换）
2. 发消息收到 MSG_ACK(status=0)；第二个连接收到 MSG_NEW
3. 断网 30s 重连 → SYNC 补拉无丢失
4. 上传 <8MB 图片并在消息中显示
5. 375px 宽度不破版；亮/暗主题可用
6. 键盘导航可达、焦点可见；`prefers-reduced-motion` 生效

## 8. 不做（明确排除）

- 不本地构建、不出 `dist/`
- 不改 `server/`、`deploy/`、`docs/`、`.github/`、`Makefile`、根目录文件
- 不做 emoji、不做渐变装饰
