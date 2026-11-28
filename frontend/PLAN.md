# UFOT Web 客户端方案（历史记录，已停止维护）

本文件记录 2026-11 的初始方案，不再更新。当前代码与页面以 `frontend/README.md`、`frontend/NOTES.md` 和 `src/app/router.tsx` 为准。

文中的 `/contacts`、`/admin/*` 页面、BrowserRouter、Vite 7 与后端端点草案均已删除或失效。当前只保留登录、会话、设置三块；视觉为 MD1；核心可用性链路是 `MSG_SEND → MSG_ACK → MSG_NEW → SYNC`。
