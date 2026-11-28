# UFOT Web 客户端

对接 UFOT 纯后端（生产实例 `http://server.344977.xyz`）的浏览器客户端。
后端地址**只来自 env**，代码里不硬编码任何域名。

## 目录分层

```
src/
├─ app/        ① 装配层：路由、启动引导（会话恢复 → 探测登录方式 → 建 WS）
├─ core/       ② 内核层：与 UI 无关的领域能力
│   ├─ config/ env 解析（契约 §1 公式）
│   ├─ net/    http 客户端、WS 帧编解码、WS 连接层、kind 常量
│   ├─ domain/ auth / conv / media / admin 领域服务
│   └─ store/  zustand：auth / conv / msg / bridge / ui
├─ ui/         ③ 设计系统：primitives（无业务）+ layout（外壳）
├─ features/   ④ 功能层：auth / chat / contacts / settings / admin
└─ styles/     设计令牌 + 基础 + 控件 + 布局 + 功能页
```

依赖方向单向：`app → features → ui → styles`，`features → core`，`core` 不反向依赖。

## 环境变量

复制 `.env.example` 为 `.env.production`（不入库）后按部署填写。关键三项：

| 变量 | 说明 |
|---|---|
| `VITE_API_BASE` | HTTP API 根；**同域部署留空**（同源 `location.origin`） |
| `VITE_WS_URL` | WebSocket 全 URL；同域部署留空（由 API_BASE 推导） |
| `VITE_ADMIN_BASE` | 管理面基址（需反代到后端 7781） |

> `frontend/.env.*` 在 `.gitignore` 中，域名只存在于部署者本机——源码零硬编码。
> CI 构建的 Release 产物**不携带任何 env**：所有 `VITE_*` 留空，回退到同源 `location.origin`，
> 因此该产物直接适配"同域托管"形态；跨域部署者在托管时注入 env 重新构建，或经反代同域化。

### 同域部署（推荐）

后端与前端同站点（Caddy 站点块示例见 `docs/deploy-plan.md`）：
Cookie 自动携带（SameSite=Lax 即可）、WS 同源握手，无任何跨域问题。

### 跨域部署（不推荐）

前端与后端不同站时，`SameSite=Lax` 的 Cookie 不会被跨站请求携带，
且后端 WS 握手只认 Cookie（红线：不接受 URL query token）——
跨域形态需要后端将 Cookie 改为 `SameSite=None; Secure`（仅 HTTPS 下可行），
或前端经反代同域化。优先选择同域部署。

## 部署

**构建只允许 CI**：推 tag `v*` → GitHub Actions 产出 `ufot-web-<ver>-dist.tar.gz`（挂在 Release）。
服务器下载解压到静态目录即可（纯静态文件，HashRouter 无需服务端 rewrite）。

```bash
# 服务器侧（分用户：文件归 ufot 属主）
sudo install -d -o ufot -g ufot /var/lib/ufot/frontend
sudo tar -xzf ufot-web-*-dist.tar.gz -C /var/lib/ufot/frontend
```

完整站点块（前端静态 + API/WS/管理面反代 + HTTPS）见 `docs/deploy-plan.md`。

### 反向代理要求

管理面 `/api/v1/*` 在后端默认只监听回环 `127.0.0.1:7781`，需要显式暴露，例如：

```caddy
handle /api/v1/* {
    reverse_proxy 127.0.0.1:7781
}
```

（同时保留后端原有的 CSRF 双提交与 RBAC 校验；建议再加 IP 白名单。）

## 响应式

| 断点 | 布局 |
|---|---|
| ≥1024px | 侧栏 + 会话列表 + 详情 三栏 |
| 768–1023px | 列表 ⇄ 详情 两栏（路由驱动） |
| <768px | 单栏 + 底部标签栏，输入区贴安全区 |

尺寸统一用 rem，根字号 `clamp(14px, 0.4vw + 13.2px, 18px)`，支持 200% 缩放；
最小点击区 40px（手机 44px）；动效遵循 `prefers-reduced-motion`。

## 已知限制

见 `NOTES.md`（契约与实测差异、后端缺口、降级行为）。

## 目录约定

- 后端补充端点草案：`docs/backend-endpoint-proposal.md`
- 验收证据与实测记录：`NOTES.md`
