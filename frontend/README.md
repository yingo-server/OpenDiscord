# UFOT Web 客户端

浏览器客户端，对接 UFOT 后端。后端地址只从 `VITE_*` 读取；示例见 `.env.example`，实际部署地址不入库。当前只保留登录、会话、设置三个可用页面。

## 目录分层

```
src/
├─ app/        装配层：路由、启动引导（会话恢复 → 探测登录方式 → 建 WS）
├─ core/       内核层：env、HTTP/WS 编解码与连接、auth/conv/media 领域服务、zustand store
├─ ui/         设计系统：primitives 与 layout（AppShell）
├─ features/   功能层：auth / chat / settings
└─ styles/     tokens / base / ui / layout / features（MD1 视觉）
```

依赖方向单向：`app → features → ui → styles`，`features → core`，`core` 不反向依赖。

## 环境变量

复制 `.env.example` 为本地 env 文件（不入库）后按部署填写。常用三项：

| 变量 | 说明 |
|---|---|
| `VITE_API_BASE` | HTTP API 根；同域部署留空，用 `location.origin` |
| `VITE_WS_URL` | WebSocket 全 URL；留空时由 `VITE_API_BASE` 推导 |
| `VITE_VAPID_PUBLIC_KEY` | Web Push 公钥；留空则推送入口不可用 |

`VITE_ADMIN_BASE` 仍被解析，但当前前端没有管理台页面，暂无使用方。CI 构建不注入任何 `VITE_*`，产物默认适配同域部署；跨域部署在托管时注入并重新构建。

### 同域部署（推荐）

前端与后端同站点：Cookie 自动携带，WS 同源握手。站点配置以 `../deploy/Caddyfile` 和 `../docs/deploy.md` 为准。

### 跨域部署

跨源 WS 已实测不可行：服务端 101 不回显 `Sec-WebSocket-Protocol`，浏览器判握手失败；跨站 Cookie 又被 `SameSite=Lax` 拦截。部署时把 `/ws` 和 `/api/*` 反代到同源，不要使用跨域 Cookie 模式。

## 构建与发布

CI（`.github/workflows/ci.yml`）在 `v*` tag 上执行 `npm ci && npm run build`，产物上传为 workflow artifact `web-dist`（保留 7 天），当前不附加到 GitHub Release。下载解压到静态目录即可；HashRouter 不需要服务端 rewrite。

```bash
sudo install -d -o ufot -g ufot /var/lib/ufot/frontend
sudo tar -xzf ufot-web-*-dist.tar.gz -C /var/lib/ufot/frontend
```

## 响应式

| 断点 | 布局 |
|---|---|
| ≥1024px | 侧边导航 + 会话列表/详情 |
| 768–1023px | 导航收窄，列表与详情并存 |
| <768px | 单栏 + 底部标签栏（会话、我的） |

尺寸集中在 `src/styles/tokens.css`；最小点击区 40px（移动 48px），动效遵循 `prefers-reduced-motion`。视觉规范见 `docs/md1-rewrite-plan.md`（已完成）。

## 已知限制

- 会话列表依赖 `CONV_PAYLOAD` 事件累积；列表底部可按已知会话号直接进入。
- 资料读写（940/941）与历史分页（920）对用户端不可达；昵称仅本地生效。
- 跨源 WS 不可行，必须同域反代。
- 证据与降级行为见 `NOTES.md`。
