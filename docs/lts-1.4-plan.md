# OpenDiscord 1.4 LTS 规划

> 状态：规划稿 · 目标：处理全部已知问题，重点性能。
> 三个核心：**普遍拉取模式（Universal Pull）**、**参数自适应（Adaptive Control）**、**v3 消息总线启用**。
> 参考硬件 2 核 4 GB。所有容量数字来自 CI 实测，不估算。

## 0. 定义与边界

1. **LTS**：1.4 发布后进入 18 个月维护期，只收安全与缺陷修复，不引入投递模型变更。
2. **全部已知问题**：`docs/known-issues.md` 12 条 + 容量段，逐条给出「修 / 自动化 / 明确不做并记录理由」的决策（§4）。不允许以「v1.2 计划」含糊带过。
3. 纪律继承两轮事故教训：
   - 客户端行为先行合入并验证，服务端特性才允许启用；
   - 任何新机制必须先用计数器证明前提成立，再用同一计数器证明收益；
   - flag 默认关闭的中间态 + 逐字节回归常驻；
   - protobuf wire 跨帧不自描述，禁止拼帧合批。

## 1. 性能现状（基线，CI 实测）

| 场景 | 当前可用性 | p95 | 判决 |
|---|---|---|---|
| 512 连接 × 10 人群 | 94.5% | 115ms | 稳定工作点 |
| 1000 × 50 人群 | 14~24% | ~470ms | inline 扇出瓶颈 |
| 1000 × 500 人群（v1.2 阈值拉取） | 65.0% | 301ms | 提升 32 倍但未达 95% |

根因已定位：**inline 逐成员扇出**（O(成员数) 帧）+ **ACK 通道与扇出共抢出路**。
v1.2 只对 >100 人群启用拉取，50 人群仍 inline —— 这是 1.4 要消除的。

## 2. 核心一：普遍拉取模式（Universal Pull）

**定义**：所有会话默认走「水位线通知 + 客户端批量拉取」，取消 `pull_mode_threshold` 的分流语义；inline 逐成员推送只作为 `universal_pull_enabled=false` 的回滚路径保留（逐字节一致）。

要点：
- 客户端 `pull-scheduler.ts` 已实现（singleflight + jitter + min_interval + 兜底轮询），**协议零改动**；
- 服务端水位线生成点从「fanout 阈值分支」改为「所有会话统一分支」；
- 小群实时性由自适应控制器保证（§3），不再靠 inline；
- 大群消息照常落库、seq 照常分配，只换通知方式——回滚安全性的根基；
- 202 全量快照在 >100 人会话继续 `members_omitted`；≤100 人保持现状（12~16KB 安全）。

## 3. 核心二：参数自适应（Adaptive Control）

**定义**：水位线频控参数不再是静态 flag，而由控制器按实时负载动态给出，每个会话独立。

采集信号（全部已有计数器，不新增探针）：
- 扇出队列深度 / 丢弃计数、ACK 率、p95、在线连接数、每会话消息速率、credit PAUSED 数。

控制量（每会话）：
- `watermark_batch`（硬界 1~200）、`watermark_interval_ms`（硬界 10~2000）。

算法（**AIMD + 双维**，防止振荡）：
- 无拥塞（队列 < soft、ACK 率 > 98%）：interval 加性减小 → 小群趋近 10ms，实时性接近 inline；
- 拥塞（队列 > firm 或 ACK 率 < 95%）：interval 乘性增大 → 大群聚合到 2s；
- 变更速率限制：每次调整 ≤ 当前值 25%，冷却 1s；
- SIGHUP / 管理 API 可覆盖为静态值（自适应关闭时有确定行为）；
- 所有参数变更写入 SIGUSR1 dump + 审计事件，可回放。

**约束**：控制器是纯策略层，机制层只读取参数；参数有硬上下界，越界拒载；控制器 panic 不影响投递（旁路失败回退静态默认值）。

## 4. 核心三：v3 消息总线启用

v3 的 P0~P3d-2b-1 已全部落盘但**默认关闭、零行为影响**：
- MailboxHub（per-uid 有序日志 + mailbox_seq + 去重 LRU + SQLite 落库）
- DeliveryPlane（credit 流控 + PAUSED + 保命帧 + 多设备独立 cursor）
- fanout 灰度双写旁路、客户端 kind 15/16/17、连接生命周期钩子机制（未注册）

1.4 要完成：
1. P3d-2b-2：注册钩子 + 灰度 uid 关闭旧直投（**一次性切换，杜绝双投递窗口**）+ kind 15 上行 ACK 路由 + 背压统计上报；
2. P4：`v3_bus_enabled=true` 灰度 5%→25%→50%→100%，每档 S-1~S-6 压测守门；
3. 100% 后一个版本周期删除旧直投代码（1.4 内完成，LTS 不留双路径）。

**与普遍拉取的关系**：v3 mailbox 对数据帧有序可靠；普遍拉取的水位线作为 mailbox 的「指针帧」条目（§2.4 of v3-bus-design.md）。两者叠加后：
- 小群：mailbox 直接携带数据帧（自适应 10ms 水位线，实时）；
- 大群：mailbox 只携带水位线，客户端批量拉；
- 回执/控制帧：mailbox 有序；
- typing/presence：无序旁路（低价值）。

## 5. 已知问题逐条决策（12 条 + 容量）

| # | 问题 | 1.4 决策 | 归属批次 |
|---|---|---|---|
| 1 | 冷库 PG driver 需部署层注册 | **修**：发布 `chatd-pg` 构建变体（内嵌 pgx driver），启动时自检并落审计；`require_audit_store` 行为不变 | OPS-1 |
| 2 | 短信/邮件/图形验证码供应商未接 | **修**：可插拔适配器（HTTP 通用 webhook + 阿里云/腾讯云示例配置），生产未配仍 fail-closed | OPS-2 |
| 3 | Windows ARM/ARM64 无产物 | **明确不做**：记录理由（SQLite cgo 无 mingw arm64 工具链），CI 显式 skip；提供源码构建文档 | DOC |
| 4 | split 多进程部署未实现 | **修**：删除指向不存在二进制的 systemd 单元（不留会启动失败的运维指引），文档标注 embedded-only | OPS-1 |
| 5 | 群通话/屏幕共享 | **明确不做**：1v1 通话完整；自建 SFU 超出 2C2G 带宽预算，LTS 不引入 | DOC |
| 6 | 移动端无推送 | **修**：PWA service worker + Web Push（VAPID），可配置厂商通道接口缝 | FE-2 |
| 7 | 通话需自备 TURN | **保持自备 + 自动化**：管理界面一键测试 TURN 连通性，deploy 模板不变 | OPS-2 |
| 8 | 群人数上限 100 | **由普遍拉取 + 自适应解决**：默认上限提到 1000，硬上限保留（64KB 帧）；容量由控制器守门 | CORE |
| 9 | 编辑/转发/@提及 服务端未实现 | **修**：106 编辑、转发元数据、@ 提及广播 | FE-1 |
| 10 | 举报 kind 453 无 handler | **修**：453 → sys_reports 入库 + 审计 | FE-1 |
| 10b | 会话列表未虚拟滚动 | **修**：复用消息列表虚拟滚动方案 | FE-1 |
| 11 | auto_vacuum 需手工转换 | **自动化**：启动时检测 `auto_vacuum=0` 自动生成一次性迁移计划，`opendiscordctl upgrade` 执行 | OPS-1 |
| 12 | 旧 thumb_data 需手工迁移 | **自动化**：并入 `opendiscordctl upgrade`，dry-run 默认 | OPS-1 |
| 容量 | 扇出写放大 | **由三核心根治**，验收门槛见 §6 | CORE |

## 6. 验收门槛（1.4 发布守门）

| # | 场景 | 门槛 |
|---|---|---|
| S-1 | 500 人群 × 1000 在线 | 可用性 ≥95%、p95 ≤100ms、`Conn.Dropped` 数据帧 = 0 |
| S-2 | 1000 × 50 人群 | 可用性 ≥90%（当前 14~24%） |
| S-3 | 512 × 10 人群 | 可用性 ≥99%、p95 ≤80ms，不劣于 v1.1.1 |
| S-4 | 慢消费者注入（50/1000 慢连接） | 其余 950 连接 ≥99%、慢连接重连续传无 gap |
| S-5 | 重启风暴 | 续传 QPS ≤800、无 5xx、cursor 不回退 |
| S-6 | 自适应振荡 | 负载阶跃 3 次，参数收敛且不跨硬界，p95 不出现 >2× 抖动 |
| S-7 | flag 回滚 | `universal_pull_enabled=false` + `v3_bus_enabled=false` → 行为逐字节同 v1.2.1 |

## 7. 分批路线（每批 60 分钟、窄任务、CI 绿才进下一批）

| 批次 | 内容 | 文件域 |
|---|---|---|
| **SPEC** | 《普遍拉取 + 自适应控制规范》（本规划 §2/§3 细化到协议/算法/测试） | docs/ |
| **CORE-1** | 自适应控制器（新增，纯逻辑 + 单测） | bus/adaptive/ |
| **CORE-2** | 普遍拉取服务端分流（改 fanout/watermark，flag 默认关闭） | data/pipe/ |
| **CORE-3** | v3 P3d-2b-2 接线 + 灰度切换（一次性，含双投递保护） | cmd/chatd/ + user/app/ |
| **FE-1** | 编辑/转发/@/举报/会话列表虚拟滚动 | apps/client + pipe |
| **FE-2** | PWA + Web Push | apps/client |
| **OPS-1** | PG 变体 / 删除死 units / upgrade 自动化 | deploy + cmd/opendiscordctl |
| **OPS-2** | 供应商适配器 + TURN 自检 | authsvc + admin |
| **DOC** | known-issues 清零、CHANGELOG 1.4.0、容量表刷新、明确不做清单 | docs/ |
| **REL** | S-1~S-7 全量压测 → 打 v1.4.0 tag → 全矩阵发布 | CI |

## 8. 代理任务锚点（任务书短，要求写在这里，代理用 file_read 完整读）

- **§A 规范代理**：读本文件 §2/§3/§6 + `docs/pull-mode-spec-v1.2.md` + `docs/v3-bus-design.md`，产出 `docs/universal-pull-spec.md`。必须含：水位线协议在普遍模式下的字段/频率、自适应 AIMD 的伪代码与边界、与 v1.2 客户端的兼容矩阵、S-6/S-7 的可测试断言、回滚开关清单。**只写规范，不改代码**。
- **§B 功能代理（FE-1）**：读 §5 第 9/10/10b 行，实现 106 编辑、转发元数据、@ 广播、453 举报入库、会话列表虚拟滚动。只碰 `server/internal/data/pipe/` 与 `apps/client/src/ui/views/conv-list/`。
- **§C 运维代理（OPS-1）**：读 §5 第 1/4/11/12 行，实现 `chatd-pg` 构建变体、删除死 units、`opendiscordctl upgrade` 自动化。只碰 `deploy/` 与 `server/cmd/opendiscordctl/`。

三批文件域无交集，可并行。全部 60 分钟预算，回来按时/规模判定。
