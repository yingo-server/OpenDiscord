# UFOT Web 客户端 · MD1 视觉重写（已完成）

“MD1”在本仓库指 2026-11 采用的经典 Material 风格：彩色 AppBar、主色/强调色、分层阴影、2px 卡片圆角、Roboto/Noto Sans SC、涟漪反馈。不是 Android 官方版本名。

已落地：`src/styles/tokens.css`（色板、阴影、字号、8dp 网格）、`base.css`（字体栈、涟漪）、`ui.css`（按钮、输入、Tabs、Snackbar、Dialog）、`layout.css`（导航、列表、详情）、`features.css`（会话卡片、气泡、输入区）；发送按钮为 FAB。

验收（2026-11-28）：375px 无横向滚动，亮/暗主题可用，`npm run typecheck` 通过。管理台与联系人页面已删除，不再纳入截图验收。聊天与连接状态以 `../NOTES.md` 为准。
