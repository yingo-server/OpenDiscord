# UFOT v1.5.0 发布说明

发布日期：2026-11-21
Tag：`v1.5.0`（正式版，非 prerelease）
基线：v1.4.0

## 本版主题：工程完善（文案 / 文档 / 功能补齐）

v1.5 是 v1.4 转生产之后的工程完善版本：三个 sota skill 由 Agent 执行审计与重写，
补齐两个已记录的功能缺口。容量限制仍如实保留，架构级重设另见
`docs/architecture-reset-v2-plan.md`。

## 1. UX 文案重写（sota-ux-writing）

- 客户端文案字典重写：错误消息写明「后果 / 原因 / 下一步」，按钮动词+宾语，
  空态给出下一步动作，术语全库一致（ISO 24495-1 平实语言）。
- 管理后台硬编码中文全部外置到 `apps/admin/src/ui/i18n/`，组件统一走 `t()` +
  具名占位符，硬编码中文清零。
- 新增 `docs/terminology-dictionary.md`：36 条术语的界面/文档/API/CLI 用词基线
  （一个概念一个名字）。

## 2. 技术文档重写（sota-docs-workflow）

- `docs/README.md` 按 Diátaxis 重组为导航（tutorial / how-to / reference /
  explanation），34 个链接与 docs/ 实际 34 篇一一对应。
- 事实校正：`api-reference`（补 diagnostics/turn、路由计数）、`cli-reference`
  （10 个子命令 + 退出码）、`ws-protocol`（84 个 kind 对齐代码）、
  `known-issues`（v1.4 已发布、CI 全绿，缺口如实保留）。
- 新增 `docs/push-setup.md`（推送 VAPID 配置 how-to）。

## 3. README 重写（sota-copywriting）

- 三层读者入口（新用户 / 部署者 / 开发者）+ 收益先行的定位陈述。
- v1.4 容量实测表与所有事实（9 产物、基准数据、链接集合）原样保全。
- 诚实标注：S-1/S-2 容量缺口与推送/举报（本版已补齐）状态清晰。

## 4. 功能补齐

### 移动端系统推送（Web Push / VAPID）
- 新增 `POST /api/push/subscribe`（Cookie 鉴权，订阅登记，fail-closed）+
  `server/internal/pushsvc/`：VAPID ES256 JWT（RFC 7519）+ aes128gcm 加密 +
  失效订阅清理（404/410）。
- 接线到数据层 flusher：每条已提交消息对收件人触发 `Notify`（无订阅/无 VAPID
  配置时 no-op，绝不影响投递热路径）。系统与浏览器支持详见 `docs/push-setup.md`。
- 配置：服务端 `VAPID_PRIVATE_KEY` / `VAPID_SUBJECT`，前端 `VITE_VAPID_PUBLIC_KEY`；
  任一缺失即 fail-closed（订阅仍可登记，不发通知）。

### 消息举报
- 服务端 453 REPORT 上行链路已在 v1.4 实现（`msgdomain.go:onReport` → sys_reports
  + 审计 + ACK），本版补齐前端：MessageList 举报菜单发送 453 帧（不再弹"即将推出"），
  成功/失败均有明确提示，PII 只传 id + 枚举原因。

## 质量与已知边界

- CI：go test -race + web client/admin 全绿；本版修了 3 个 CI 捕获的回归
  （admin 文案断言、gofmt、VAPID JWT payload 编码）。
- 容量：S-1（500人群×1000在线 0.93%）、S-2（1000×50 16.38%）在 raw ceiling 下
  仍未达门槛，v1.4 知情发布的边界延续；架构重设见 `docs/architecture-reset-v2-plan.md`。
- S-7（flag 回滚逐字节）仍受 v1.2.1 历史 tag 构建缺陷阻断（见 `docs/lts-1.4-plan.md §9`）。

## 升级

从 v1.2.1 / v1.4.0 升级：配置兼容；新功能默认禁用（推送未配置 VAPID 即 no-op），
不影响既有行为。部署见 `docs/deploy.md`，回滚见 `deploy/release.sh`。
