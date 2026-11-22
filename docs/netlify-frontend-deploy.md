# Netlify 前端部署与后端域名配置

## 两种部署方式

**方式 A：前后端同域（当前生产）**
前端由 Caddy 在 80 端口反代，前端与 WS 同域，零配置：`resolveWsUrl()` 用
`location.host` 自动拼 `ws://当前域名/ws`。

**方式 B：前端在 Netlify、后端在源站（迁移场景）**
两者不同域，必须在**构建期**注入后端地址。前端支持两个 Vite 变量：
- `VITE_API_BASE`：后端 HTTP 根，如 `https://server.344977.xyz`
  → 认证 API 走 `${VITE_API_BASE}/api/auth/*`，WS 自动推导 `wss://server.344977.xyz/ws`
- `VITE_WS_URL`：显式 WS 地址（最高优先级，覆盖上面推导）

## Netlify 构建配置

`frontend` 分支是 CI 已构建好的静态产物（无 build command），**构建期变量无法在 Netlify 生效**。
两种解决：
1. 在 Netlify 用「从源码构建」而非 frontend 分支：Build command `npm ci && npm run build`
   （在 apps/client 目录），环境变量填 `VITE_API_BASE`。
2. 保持 frontend 分支部署，但在 GitHub Actions 的 web job 里从 Secret 读 `VITE_API_BASE`
   注入构建——需配置仓库 Secret `VITE_API_BASE`。

## CORS 注意

前端跨源访问后端时，后端 authsvc 的 `PublicOrigin` 需设为 Netlify 域名（精确匹配 CORS）；
WS 握手不受 CORS 限制，但浏览器会校验 Cookie 的 SameSite/Secure 属性。
生产建议：前后端同域或用反代统一域名，避免跨源复杂度。
