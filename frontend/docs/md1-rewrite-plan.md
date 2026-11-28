# UFOT Web 客户端 · MD1 视觉重写方案

> “MD1” = Material Design One（经典 Material Design / Material 1，Android 5.0 Lollipop 风格）
> 目标：保留现有全部功能与协议逻辑，只重写视觉层（CSS + 少量组件结构）。

## MD1 视觉基线

| 维度 | MD1 规范 |
|---|---|
| 色彩 | 主色 + 强调色（accent）；亮/暗主题；彩色 AppBar（主色）；状态栏深色叠层 |
| 阴影 | 卡片与浮动按钮的分层阴影（`0 2px 4px rgba(0,0,0,.2)` / `0 6px 12px rgba(0,0,0,.2)`） |
| 圆角 | 卡片 2px；小组件 2–4px；头像圆形；FAB 圆形 |
| 排版 | Roboto（英文）/ Noto Sans SC（中文）；字号 12/14/16/20/24/34；字重 400/500 |
| 组件 | 彩色顶部 AppBar + 汉堡/操作图标；FAB（正红圆钮 + 白色 plus 图标）；卡片列表；涟漪反馈；Snackbar；Tabs 指示条；Dialog 带动画 |
| 交互 | 轻触有 ripple；页面切换淡入；列表 item 触碰有高亮；Snackbar 从底部弹出 |

## UFOT 适配

- 主色：近黑 `#202124`（UFOT 品牌）或正蓝 `#3b82f6`；强调色：正红 `#e5484d`（FAB / 关键动作）；警告黄 `#f5a524`、信息蓝 `#3b82f6` 仅语义
- AppBar：亮色主题用主色蓝条，暗色主题用深灰条，UFOT 字标白色
- 会话列表：MD1 卡片列表（左侧头像，右侧两行文字），未读数用彩色 badge
- 聊天气泡：MD1 风格——自己蓝色实底、对方浅灰底，带小圆角（2px 显老派，8px 显现代；MD1 用 4–8px）
- 输入区：MD1 TextField 下划线风格（底部边框，聚焦变主色），发送 FAB
- 设置页：MD1 卡片 + 开关（Switch）
- 弹窗：MD1 Dialog（圆角 2px，阴影，按钮右对齐）

## 改动范围

- `src/styles/tokens.css`：MD1 色板 + 阴影 + 字体 + 尺寸刻度
- `src/styles/base.css`：Roboto/Noto Sans SC 字体栈、ripple 动画
- `src/styles/ui.css`：按钮/图标按钮/输入/标签页/徽标/对话框/Snackbar 全部重写
- `src/styles/features.css`：会话卡片、气泡、输入区、设置卡片、管理台重写
- `src/styles/layout.css`：AppBar / 导航抽屉 / 三栏布局重写
- 少量组件结构：AppShell（AppBar）、Composer（FAB 发送）、设置页 Switch

## 不改

- 所有 TS/TSX 业务逻辑、协议层、store、env、netlify.toml、CI
- 后端、部署脚本、安装流程

## 验收

- 375px 不破版；亮/暗主题；首页/会话/设置/管理台截图对比 MD1 基线；tsc 全绿；浏览器实测登录/刷新/发消息
