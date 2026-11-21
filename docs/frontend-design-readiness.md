# 前端设计就绪审计（AUDIT mode · 只读）

> 本次为**只读审计**，未修改任何代码 / CSS / 组件 / 其他文档，未 commit/push，未跑 build/test。
> 依据 sota-frontend-design SKILL AUDIT mode + 7 个 rules 的 Audit checklist 抽查（rg 静态扫描，未做运行时验证）。

## 1. 设计系统现状

| | client | admin |
|---|---|---|
| Token 层 | `apps/client/tailwind.config.js`（216 行，语义色/间距/层级 token）+ `apps/client/src/ui/styles/index.css` | `apps/admin/src/styles/tokens.css`（110 行，OKLCH 语义层）+ `styles/index.css` |
| 组件规模 | `src/ui/` 27 个 .tsx | `src/ui/` 23 个 .tsx（components + views） |
| 样式策略 | Tailwind 工具类为主 | Tailwind + tokens.css 自定义属性 |

两 SPA 均有独立 token 层、无双轨手工调色板迹象（未逐文件核对字面量泄漏，属待深入项）。

## 2. 系统性 smell grep 结果（apps/client/src + apps/admin/src）

| smell | 计数 | 说明 |
|---|---|---|
| `transition: all` | 0 | ✅ |
| `will-change`（静态 CSS） | 0 | ✅ |
| 物理属性 `margin-left/right` | 0 | ✅（均用逻辑属性/gap） |
| `outline: none` | 1 | admin index.css:21 —— 在 `:focus-visible` 块内用 ring 替代，可接受但建议核对 |
| `:focus` 无 `:focus-visible` | 1 处 | admin index.css:125-127 `.input/.select/.textarea:focus` 单独定义，与 :focus-visible 体系并存 |
| `placeholder=` | 34 | ⚠️ 多处疑似 placeholder 兼作 label，需逐个确认有无可见 label |
| clickable div | 4 | ⚠️ 均为 scrim/遮罩层：client Settings.tsx:270、ChatView.tsx:230、MessageList.tsx:427；admin Shell.tsx:70（已有 aria-hidden，缺键盘关闭路径核对） |

**最严重 5 个 file:line**：
1. `apps/admin/src/styles/index.css:125` — `.input:focus` 覆盖 :focus-visible ring，焦点样式可能不一致
2. `apps/client/src/ui/views/settings/Settings.tsx:270` — modal 容器 div onClick 关闭，无键盘 Esc 等价路径核对
3. `apps/client/src/ui/views/chat/MessageList.tsx:427` — 同上
4. `apps/client/src/ui/views/chat/ChatView.tsx:230` — 全屏 div onClick 关菜单
5. placeholder-as-label 嫌疑最集中处：`apps/admin/src/ui/views/Messages.tsx`（4 处）、`client ConvList.tsx`（3 处）

## 3. WCAG 2.2 AA 三项关键状态

| 项 | 状态 | 证据 |
|---|---|---|
| viewport 缩放（1.4.4） | ✅ 通过 | `client/index.html:7` 与 `admin/index.html:5` 均无 `user-scalable=no`，admin 无 maximum-scale |
| `:focus-visible` 全局样式 | ✅ 存在 | `client/src/ui/styles/index.css:161`、`admin/src/styles/index.css:20`（admin 用 ring 替代 outline） |
| `prefers-reduced-motion` | ✅ 存在 | `client/src/ui/styles/index.css:284` @media 块；`admin/src/styles/index.css:30` + `admin/src/lib/motion.ts:9`（JS 层 matchMedia，动效归零） |

## 4. 发布前清单

**已合规**：viewport 可缩放；全局 focus-visible；reduced-motion 双 SPA 均落实；无 transition:all / will-change / 物理属性残留。

**需跟进（建议，按优先级）**：
1. 逐一核查 34 处 `placeholder=` 是否均有可见 label（rules/04 §1，MEDIUM）
2. admin `.input:focus`（index.css:125）与 `:focus-visible` 统一为单一焦点体系
3. 4 处 clickable div 补键盘等价操作（Esc/焦点陷阱），优先 modal 两处
4. rules/05 其余项（APG 键盘模型、`<dialog>`/showModal、24px 目标）未做运行时验证，建议 Playwright 键盘-only + 200% zoom + reduced-motion 仿真补测
5. 组件内 hex/px 字面量泄漏未逐一 grep，建议纳入 CI smell 检查
