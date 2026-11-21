# OpenDiscord 文档导航（v2.0.0）

OpenDiscord 采用分层 socket 架构：总线层为骨架，桥接层为枢纽，用户层/管理层/数据层经桥接通信。除管理层到桥接使用 HTTP API 外，所有层间通信均为内部 socket 长连接；前端到用户层使用 WebSocket。

本索引按 Diátaxis 四象限归类（**教程 / How-to / 参考 / 解释**），每篇标注读者对象：**新用户 / 部署者 / 开发者**。链接集合与 `docs/` 实际文件一一对应。

## 教程（Tutorial · 跟着做一遍）

| 文档 | 一句话 | 读者 |
|---|---|---|
| [用户指南](user-guide.md) | 从安装客户端到注册登录、收发消息的完整上手路径 | 新用户 |
| [部署手册（单机 embedded）](deploy.md) | 在一台 2C2G 机器上从零到服务可用的分步部署教程 | 部署者 |
| [术语对照表](terminology-dictionary.md) | 产品术语的中英文对照与命名约定（UX 文案域维护） | 全部 |

## How-to（完成具体任务）

| 文档 | 一句话 | 读者 |
|---|---|---|
| [管理员指南](admin-guide.md) | 管理员日常操作：登录管理面、封禁/踢人/举报处理/广播/归档 | 部署者 |
| [路线图](roadmap.md) | 项目方向：已完成、进行中与"明确不做"清单及理由 | 全部 |
| [v1.5 四步版本路线](v1.5-roadmap.md) | v1.5.0-beta.1 → v1.5.0 四步收口计划（UX/文档/README/发布） | 开发者 |
| [v2.0 路线图](v2.0-roadmap.md) | v2.0 版本路线：容量契约与写路径优化范围、审计发现与真实压测数据 | 开发者 |
| [v2.1 写路径批量化规范](v2.1-write-batching-spec.md) | v2.1 写入路径批量化改造的规范与实施记录 | 开发者 |
| [v2.0 架构重设规划（草案）](architecture-reset-v2-plan.md) | 容量缺口转 v2.0 架构重设的规划草稿（未定稿） | 开发者 |

## 参考（Reference · 查事实）

| 文档 | 一句话 | 读者 |
|---|---|---|
| [API 参考](api-reference.md) | 全部 HTTP 端点（方法/路径/鉴权/入参/出参/错误码），未实现端点如实标注 | 开发者 |
| [CLI 参考](cli-reference.md) | `adminctl` / `opendiscordctl` 完整子命令、flag、退出码与已知限制 | 部署者 |
| [WS 协议](ws-protocol.md) | WebSocket 帧格式、kind 号段总表、payload 结构、限流与错误语义 | 开发者 |
| [已知问题](known-issues.md) | 逐条列明已修/保持/延后的已知缺陷、容量缺口与规划中功能 | 全部 |
| [容量契约](capacity-contract.md) | `policy.capacity` 配置、准入限流语义、热改与超限行为 | 部署者 |
| [发布说明 v2.0.0](release-notes-v2.0.0.md) | v2.0.0 容量契约 + 写路径优化的发布内容、升级步骤与诚实数据 | 部署者 |
| [发布说明 v1.5.0](release-notes-v1.5.0.md) | v1.5.0 发布内容与升级步骤 | 部署者 |
| [架构总纲](architecture.md) | 四层 + 桥接的分层结构、kind 号段、全链路时序、目录结构 | 开发者 |
| [发布说明 v1.4.0](release-notes-v1.4.0.md) | v1.4.0 LTS 发布内容、升级步骤与注意事项 | 部署者 |
| [发布说明 v1.1.0](release-notes-v1.1.0.md) | v1.1.0 升级步骤、配置变更、破坏性变更与回滚 | 部署者 |

## 解释（Explanation · 讲为什么）

| 文档 | 一句话 | 读者 |
|---|---|---|
| [总线与桥接层设计](bridge-design.md) | 桥接流水线组件、背压、crash-only 恢复的设计取舍 | 开发者 |
| [用户层与前端设计](user-frontend-design.md) | WS 网关管道、前端分层、sync 引擎、渲染器注册表的设计 | 开发者 |
| [管理层与数据层设计](admin-data-design.md) | 数据操作 kind、DDL 分域、写管道、REST API、RBAC 设计 | 开发者 |
| [限流与安全规范](security-spec.md) | 分层限流、认证、上传安全、运维基线与安全红线（§8.2） | 开发者 |
| [Web UI 设计规范](web-ui-spec.md) | 设计令牌、组件规范、消息区交互、性能指标 | 开发者 |
| [普遍拉取规范](universal-pull-spec.md) | 水位线协议、自适应 AIMD 算法、兼容矩阵与回滚开关 | 开发者 |
| [v3 消息总线设计](v3-bus-design.md) | MailboxHub / DeliveryPlane / 灰度双写的设计与回滚铁律 | 开发者 |
| [1.4 LTS 规划](lts-1.4-plan.md) | 1.4 LTS 三核心、已知问题决策（§5）与 S-1~S-7 验收门槛（§6） | 开发者 |
| [性能优化规范 v1.2](perf-spec-v1.2.md) | v1.2 性能目标、优化方案与压测守门规则 | 开发者 |
| [大群拉取模式规范 v1.2](pull-mode-spec-v1.2.md) | v1.2 引入的按会话水位线拉取模式规范 | 开发者 |
| [SQLite 写路径精调](perf-sqlite-write-path.md) | SQLite 写路径基准测试报告（arm64 2 核环境） | 开发者 |
| [慢消费者降级回滚事后分析](perf-postmortem-v1.2-draft.md) | v1.2"慢消费者降级"机制回滚的 postmortem（草稿） | 开发者 |

## 过程与审查记录（开发者）

这些文档记录特定批次的审查/验收结论，是历史锚点而非现行规范；冲突时以代码与本目录"参考"篇为准。

| 文档 | 一句话 | 读者 |
|---|---|---|
| [服务端审查报告](review-server.md) | 服务端设计文档符合性三轮独立审查原文 | 开发者 |
| [Web Push 部署手册](push-setup.md) | 服务端推送订阅端点与 VAPID 发送的部署与验证手册（v1.5.0-beta.3） | 部署者 |
| [前端审查报告](review-frontend.md) | 前端与用户层文档 vs 代码符合性审查 | 开发者 |
| [安全红线审查报告](review-security.md) | security-spec §8.2 十二条红线逐条审查 | 开发者 |
| [LTS 发布清单](lts-checklist.md) | 三轮审查全部问题清单与当前状态（发布门槛） | 开发者 |
| [1.4 验收锚点](lts-1.4-acceptance.md) | 1.4 验收与转生产的断言/守门执行锚点 | 开发者 |
| [1.4 剩余批次锚点](lts-1.4-remaining.md) | 1.4 剩余批次的实施执行锚点 | 开发者 |
| [v3 P0 依赖核实检查单](v3-p0-checklist.md) | v3 总线设计 §9 P0 依赖逐项核实记录 | 开发者 |
| [架构性能改动全清单](architecture-optimization-menu.md) | L0~L4 全量优化方案分层清单；含自研 appendlog 放弃与 L2-5 普遍拉取实测不成立的最终决策（2026-11-21） | 开发者 |
| [架构全量实现进度诚实评估](implementation-progress-honest-assessment.md) | appendlog 主线从立项到放弃的决策过程复盘与剩余工作真实规模 | 开发者 |
| [v2.0 发布准备核查清单](release-readiness-v2.0.md) | v2.0.0 发布前事实核查：版本、CI、产物、容量边界与上线 checklist | 部署者 |
| [版权与声明合规审计](legal-and-claims-audit.md) | sota-copywriting 版权/声明层：商标风险、第三方许可证、声明可证实性 | 开发者 |
| [CLI UX 就绪审计](cli-ux-readiness.md) | sota-cli-ux：三 CLI 命令树/退出码/dry-run/机器可读输出审计 | 开发者 |
| [前端设计就绪审计](frontend-design-readiness.md) | sota-frontend-design：WCAG 2.2 AA 与系统性 smell 只读审计 | 开发者 |

## 技术栈

- 后端：Go（分层组件，单进程 embedded 或分进程部署）
- 前端：React 18 + TypeScript + Vite
- 协议：Protobuf Envelope（单一契约源 `proto/`；kind 号段双端真源：`server/internal/proto/kinds/kinds.go` 与 `apps/client/src/net/protocol/kinds.ts`）
- 存储：SQLite（热，本机）/ PostgreSQL（冷，外部）

## 设计律

1. 每个组件只做一件事并做好；组件经 channel 管道串联，禁止 god-component
2. 一切皆帧：控制、数据、管理、观测统一为 Envelope 帧
3. 机制与策略分离：可运营变更的常量一律入 `policy.json` / feature flags
4. crash-only：故障即重启，靠 REQ/ACK 补发恢复，不写容错状态机
5. 日志只写 stdout，交给 journald；所有操作有 CLI 等价物

## 质量门禁

提交前必须全绿：

```bash
make lint                       # go vet + gofmt + importlint（文档承诺的 import 红线由脚本强制）
go test ./... -count=1          # 后端
cd apps/client && npm run build && npm test
cd apps/admin  && npm run build && npm test
bash scripts/doccheck.sh        # 文档里的端点/命令/kind 必须能在源码找到
bash scripts/release-check.sh   # 起真服务跑冒烟（healthz/登录/媒体往返）
```

安全相关改动须对照 [限流与安全规范](security-spec.md) 的 §8.2 十二条红线自审。
