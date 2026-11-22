# UFOT 前端 WinUI3/Fluent 风格重写计划

状态：待执行 · 日期：2026-11-22 · 基线：v2.0.8

## 目标
保持全部现有功能（登录四方式、聊天、媒体、通话、设置、封禁页），只重写视觉层：
从「森林翡翠 + Tailwind 令牌」改为 **WinUI3 / Fluent Design 风格**——Mica/Acrylic
材质、Fluent 圆角与阴影、Segoe UI Variable 字体、NavigationView 布局、亚克力侧栏。
品牌名统一 UFOT（Unfettered Freedom Of Thought）。

## 约束（铁律）
- 只改 UI 层：`apps/client/src/ui/**`、`apps/client/src/ui/styles/index.css`、
  `tailwind.config.js`、`index.html`、i18n 文案；**不动** `net/`/`core/`（协议、状态、认证）
- 后端域名默认走 env：`VITE_API_BASE` 显式指定；未指定时生产仍用 location.host 自拼
- 隐藏内部技术提示：错误文案不暴露 kind 号、错误码、协议名、driver 名等
- 全平台兼容最稳：不依赖实验性 CSS（backdrop-filter 有 @supports 回退）
- 构建验证全交 CI，本地禁止 build

## 阶段
1. 设计令牌与主题层：重写 index.css（Mica/Acrylic 背景、亮/暗主题、Semantic Colors）
   + tailwind.config.js（Fluent 圆角/阴影/字体族）；不碰组件
2. 外壳与导航：App.tsx 改为 NavigationView 风格（左侧 Acrylic 导航栏 + 内容区），
   保留 router 与所有 view 挂载点
3. 组件层：Button/Input/Avatar/Bubble/Toast/Skeleton → Fluent 风格；
   MessageList/Composer/ConvList/Settings/Login 各视图视觉重写，功能接线不变
4. 文案与品牌：i18n 全面检查隐藏技术提示；index.html 标题/favicon/品牌字标同步
5. env 配置：bootstrap.ts 支持 VITE_API_BASE（含 /ws 拼接规则）；文档说明

## 执行记录
- 2026-11-22：计划书落盘。阶段1+2（令牌+外壳）交 Agent `eca960f3` 执行中。
- 并行完成：VITE_API_BASE env 配置（bootstrap.ts 的 resolveApiBase + resolveWsUrl、
  两处 AuthApi 创建点）；隐藏技术提示（auth-error.ts 未知 code 不再透传后端 message）。
- 阶段3（commit 42e567c）：22 组件/视图 Fluent 视觉重写，CI 绿。
- 阶段4a（commit 252e69f）：改用微软官方 @fluentui/react-components v9.75.2 +
  @fluentui/react-icons —— Button/IconButton/Input/Avatar/Skeleton/Toast 内部转发官方组件，
  FluentProvider 接入，37 图标官方化（LogoMark 品牌资产保留）。tsc 绿。
- 阶段4b（Agent 执行中）：视图层剩余 28 个手写 button + 5 个 input/textarea 全部官方化。

## 说明
- 阶段1-3 是「照 Fluent 设计语言手写 CSS」，阶段4 起按用户要求改为「官方组件库实现」。
- npm install 报 5 个传递依赖漏洞（audit 因 registry 限流未能详列），未强制 fix 以免破坏性升级。

## 验收
- CI go -race + web client/admin 全绿
- 功能不回退：登录/聊天/媒体/通话/设置的现有 e2e 与单测通过
- 视觉：截图核对 Fluent 特征（Mica、圆角 8、Acrylic 侧栏、Segoe UI）
