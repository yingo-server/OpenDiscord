# OpenDiscord 开发文档

OpenDiscord 采用分层 socket 架构：总线层为骨架，桥接层为枢纽，用户层/管理层/数据层经桥接通信。除管理层到桥接使用 HTTP API 外，所有层间通信均为内部 socket 长连接；前端到用户层使用 WebSocket。

## 文档索引

| 文档 | 内容 |
|---|---|
| [架构总纲](architecture.md) | 分层架构、kind 号段、全链路时序、目录结构、部署形态 |
| [总线与桥接层设计](bridge-design.md) | 桥接流水线组件、背压、crash-only 恢复、ctl 工具 |
| [用户层与前端设计](user-frontend-design.md) | WS 网关管道、前端分层、sync 引擎、渲染器注册表 |
| [管理层与数据层设计](admin-data-design.md) | 数据操作 kind、DDL 分域、写管道、REST API、RBAC |
| [Web UI 设计规范](web-ui-spec.md) | 设计令牌、组件规范、消息区交互、性能指标 |
| [限流与安全规范](security-spec.md) | 分层限流、认证、上传安全、运维基线、合规 |
| [API 参考](api-reference.md) | 全部 HTTP 端点（方法/路径/鉴权/入参/出参/错误码） |
| [CLI 参考](cli-reference.md) | `adminctl` / `opendiscordctl` / `opchat` 完整参数与退出码 |
| [WS 协议](ws-protocol.md) | 帧格式、kind 号段、各 kind payload、时序、限流语义 |
| [路线图](roadmap.md) | 已完成 / v1.2 计划 / 明确不做及理由 |
| [1.4 LTS 规划](lts-1.4-plan.md) | 普遍拉取 / 参数自适应 / v3 总线三核心、12 条已知问题决策（§5）、S-1~S-7 验收门槛（§6） |
| [普遍拉取规范](universal-pull-spec.md) | 水位线协议、自适应 AIMD 算法与边界、兼容矩阵、S-6/S-7 断言、回滚开关清单 |
| [v3 消息总线设计](v3-bus-design.md) | MailboxHub / DeliveryPlane / 灰度双写 / kind 15-17、接线与回滚铁律 |
| [v1.1 升级说明](release-notes-v1.1.0.md) | 升级步骤、配置变更、破坏性变更、回滚 |
| [已知问题](known-issues.md) | 已知缺陷、影响范围与计划修复版本 |
| [LTS 发布清单](lts-checklist.md) | 三轮审查的全部结论与当前状态（发布门槛） |
| [审查报告](review-server.md) | 协议贯通 / 性能 / 安全三轮独立审查原文 |

## 技术栈

- 后端：Go（分层组件，单进程 embedded 或分进程部署）
- 前端：React 18 + TypeScript + Vite
- 协议：Protobuf Envelope（单一契约源 `proto/`）
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
