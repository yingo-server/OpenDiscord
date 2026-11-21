# OpenDiscord 已知问题（v1.4.0 发布稿）

> 版本：**v1.4.0（已发布，2026-11-18）**。CI 全绿；v1.4 四项实验性 flag 已转生产默认开启
> （`universal_pull_enabled` / `adaptive_control_enabled` / `v3_bus_enabled` / `v3_bus_percent=100`，
> 另 `msg_domain_v2_enabled` 默认开启，见 `server/internal/pkg/featureflags/flags.go` `Defaults()`）。
> 基线 1.1.0 的 12 条 + 容量段逐条给出三态结论：**已修 / 保持（写明理由） / 延后 v1.5**。
> 纪律：凡未经 S-1~S-7 压测守门的性能声明一律不写成"达标"。v1.4 首轮压测
> PASS=0 / FAIL=6 / BLOCKED=8，**S-1/S-2 容量缺口未解决**，随 v1.4.0 知情发布一并交付，
> 转入 v1.5 架构重设专项；详见 `docs/lts-1.4-plan.md §9` 与 `docs/architecture-reset-v2-plan.md`。

## 状态速览

| # | 问题 | 状态 |
|---|---|---|
| 1 | 冷库 PG driver 需部署层注册 | 已修（`chatd-pg` 变体，CI 全绿） |
| 2 | 短信/邮件/图形验证码供应商未接 | 已修（webhook 适配器，CI 全绿） |
| 3 | Windows ARM/ARM64 无产物 | 保持（明确不做，理由见下） |
| 4 | split 多进程部署未实现 | 已修（删除死 systemd 单元，标注 embedded-only） |
| 5 | 群通话/屏幕共享 | 保持（明确不做，理由见下） |
| 6 | 移动端无推送 | v1.5 已修（Web Push/VAPID：订阅端点 + 每条消息下发；未配 VAPID 则 fail-closed） |
| 7 | 通话需自备 TURN | 已修（自动化部分：管理面 TURN 自检，CI 全绿） |
| 8 | 群人数上限默认 100 | 保持（默认值不动；理由：S-1/S-2 容量缺口未解决前不放大默认值） |
| 9 | 编辑/转发/@提及 服务端 | 已实现（`msg_domain_v2_enabled` 默认开启；处理分支在 `data/pipe`，经压测守门前不宣称达标） |
| 10 | 用户举报 kind 453 | v1.5 已修（端到端：服务端 onReport v1.4 在位，本版前端发帧接线） |
| 10b | 会话列表未虚拟滚动 | 已修（ConvList 接 react-virtual，CI 全绿） |
| 11 | auto_vacuum 需手工转换 | 已修（`opendiscordctl upgrade` 自动化，CI 全绿） |
| 12 | 旧 thumb_data 需手工迁移 | 已修（并入 `opendiscordctl upgrade`，dry-run 默认，CI 全绿） |
| 容量 | 扇出写放大（S-1/S-2 缺口） | **未解决**：v1.4 三核心已默认开启，但未达 S-1~S-7 门槛；转入 v1.5/v2.0 架构重设专项 |

另：**CORE-3（v3 总线接线）代码已落盘、待 CI/压测验证**——`AttachV3Deliver` 已在
`newAssembly` 末尾调用，`AddConnLifecycle` 已注入 user 层 Server，自适应控制循环
（adaptive.go）已接 `SetAdaptiveStore`，kind 15 上行已进 Router，SIGUSR1 dump 已含
Plane 背压指标。双投递保护按 uid 单一路径判定。**但**：`v3_bus_enabled=false`
默认关闭，控制器热生效单测与灰度切换全链路不双投递断言未写，最后一轮编译
未复跑（交 CI），全部未经 S-1~S-7 压测。状态见 §0。

## 0. 1.4 三核心（新，代码在位·待验证）

三条主线的机制代码已全部落盘，**全部默认关闭、零行为影响**：

- **普遍拉取（CORE-2）**：`universal_pull_enabled=false` 时 fanout 分流与 v1.2.1
  逐字节一致（T-SV-5 常驻回归锁定）。flag-on 后所有会话统一走水位线分支。
- **自适应控制器（CORE-1）**：AIMD + 双维控制器（含 SafeTick panic 旁路）在位；
  chatd 控制循环（adaptive.go）已接线，`adaptive_control_enabled=false`（默认）时
  不启动/不写/保持静态 `watermark_batch=50 / watermark_interval_ms=2000`；开启时
  SafeTick→AdaptiveWrite 越界拒载。热生效单测未写，交 CI/压测。
- **v3 消息总线（CORE-3）**：MailboxHub / DeliveryPlane / 灰度双写旁路 / kind 15/16/17
  客户端 / 连接钩子适配全部在位；**接线四项（钩子注册、灰度关旧直投、kind 15 上行、
  背压指标上报）已落盘**。`v3_bus_enabled=false` 默认关闭；开启时按 uid 单一路径
  判定（DeliveryEnabled / LegacyDirectEnabled 共用），关闭旧逐成员直投，无双投递窗口。
  全链路不双投递的 e2e 断言未写，交 CI/压测。

**状态更新（v1.4.0 已发布）**：以上 §0 三段为转生产前的收敛记录。四个 flag
（`universal_pull_enabled` / `adaptive_control_enabled` / `v3_bus_enabled` / `v3_bus_percent=100`）
现已在 v1.4 默认开启；CORE-1/2/3 单测与 cmd 层 e2e（`core3_test.go`、`v3deliver_test.go` 等）
在 CI `-race` 全绿，热生效/不双投递断言已补齐。唯一未过的是容量门槛 S-1~S-7
（见 `lts-1.4-plan.md §9`），属**性能限制**而非"未接线/未验证"——S-1/S-2 容量缺口仍未解决。

## 部署与依赖

### 1. 冷库 Postgres driver 需部署层注册 —— 已修

v1.4 提供 `chatd-pg` 构建变体（内嵌 pgx driver），启动时自检并落审计事件；
`require_audit_store=true` 的拒启语义不变。普通 `chatd` 架构约定（不 import
任何 driver）保持不变。CI 全绿，正式转为已修。

### 2. 短信 / 邮件 / 图形验证码供应商未接入 —— 已修

v1.4 落地通用 HTTP webhook 适配器（POST JSON、3s 超时、非 2xx 即失败），
`deploy/providers.example.toml` 提供阿里云/腾讯云/通用 webhook 三段示例（不含密钥）。
生产未配置时保持 fail-closed：不发验证码、第 2 条 OTP 起强制图形码并拒绝——
这是有意行为，不是缺陷。

### 3. Windows ARM / ARM64 不提供构建产物 —— 保持（明确不做）

理由不变：SQLite 需要 cgo，Windows on ARM/ARM64 没有可用的 mingw 工具链，
为一个边缘平台引入交叉编译矩阵不值得。CI 显式 skip 而非静默跳过；
源码构建路径（`GOOS=windows GOARCH=arm64 CGO_ENABLED=1`）文档化。
若未来换纯 Go SQLite 驱动可重开评估（v1.5+）。

### 4. split 多进程部署未实现 —— 已修（收敛为明确不做）

v1.2 预留的 `opendiscord-{bridge,user,data,admin}.service` 指向不存在二进制的
systemd 单元已删除，不留会启动失败的运维指引。文档明确 chatd 为 embedded-only
单进程；真正的水平扩展需求由部署方自行拆分（见 roadmap 明确不做清单）。

## 功能边界

### 5. 群通话 / 屏幕共享未实现 —— 保持（明确不做）

1v1 通话完整（含忙线、超时、ICE 失败处理）。自建 SFU 超出 2C2G 带宽预算，
LTS 不引入；云 SFU 集成留待 v1.5+ 按需求评估。

### 6. 移动端无推送 —— v1.5 已修（Web Push / VAPID）

前端 `public/manifest.webmanifest` + `public/sw.js` + `src/net/push/` 订阅封装
（production-only 注册，dev/mock 不注册）。**v1.5 补齐服务端**：`POST /api/push/subscribe`
订阅登记（Cookie 鉴权、fail-closed）+ `pushsvc` 的 VAPID ES256 JWT / aes128gcm 下发，
并接到数据层 flusher 的每条已提交消息（对收件人 `Notify`）。配置 `VAPID_PRIVATE_KEY` /
`VAPID_SUBJECT` / 前端 `VITE_VAPID_PUBLIC_KEY`，任缺一则 no-op（订阅仍可登记，不发通知）。
配置步骤见 `docs/push-setup.md`。

### 7. 通话需要自备 TURN 服务器 —— 已修（自动化部分）

保持自备 TURN（带宽成本由部署方决定）不变。v1.4 新增管理面一键连通性自检
（`GET /api/v1/diagnostics/turn`，读配置 turn URL 做 TCP 探测，3s 超时，仅管理面可见），
不再要求部署方手工 telnet 排查。硬化模板 `deploy/coturn/turnserver.conf` 不变。

### 8. 群人数上限（默认 100，硬上限 1000）—— 保持

**默认值仍为 100，本版未放大到 1000。** 理由：放大默认值的前提是普遍拉取 +
自适应经 S-1~S-2 压测证明可用性达标，而 S-1/S-2 容量缺口仍未解决（见 §0 与
`architecture-reset-v2-plan.md`）。在验证完成前放大默认值，等于向 2 核用户承诺实测撑不住的规模
（v1.1.1 降级教训）。验证通过后再改默认值（改动是一行 policy 默认 + 文档）。硬上限 1000（64KB 帧约束）不变。
容量核算公式与实测表见下「容量」段，仍然有效。

### 9. 消息编辑 / 转发 / @提及 服务端实现 —— 已实现（flag 默认开启）

服务端 106 编辑（时间窗内软更新 + 广播）、109 转发元数据、@ 提及定向通知
已在 `data/pipe` 落地，由 `msg_domain_v2_enabled` 门控，**v1.4 起默认 true**。
客户端 106/107/108 渲染路径已接线。S-1~S-7 压测守门前不宣称性能达标。

### 10. 用户举报（REPORT kind 453）—— v1.5 已修（端到端）

453 → `sys_reports` 入库 + 审计 + ACK 的处理代码在 v1.4 已落地
（`data/pipe/msgdomain.go` `onReport`，与第 9 条同一 flag `msg_domain_v2_enabled` 门控），
用户层上行白名单（`bridge/defaults.go`）与 wire 转换亦已放行 453。**v1.5 补齐前端**：
`MessageList` 举报菜单发 453 帧（不再弹"即将推出"），成功/失败有明确提示，
PII 只传 msg_id + 枚举原因（detail 不采集自由文本）。

### 10b. 会话列表未虚拟滚动 —— 已修

`ConvList.tsx` 已接 `@tanstack/react-virtual`（复用消息列表方案），
无 flag 门控，随构建生效。单测在位，CI 全绿，正式转为已修。

## 运维注意

### 11. 升级到 v1.1 必须做一次热库 auto_vacuum 转换 —— 已修（自动化）

`opendiscordctl upgrade`（v1.4 新增）启动检测 `auto_vacuum=0` 自动生成一次性
迁移计划（停服 → 备份 → `PRAGMA auto_vacuum=INCREMENTAL; VACUUM;` → 校验返回 2），
`--run` 执行、默认 dry-run。手工路径 `docs/deploy.md §7.4` 保留为回退手段。

### 12. 旧版 `thumb_data` 需要手工迁移 —— 已修（自动化）

并入 `opendiscordctl upgrade`（默认 dry-run，`--run` 执行）。独立的
`opendiscordctl migrate thumbs --dry-run/--run` 仍可用。

## 容量：扇出写放大是主要约束 —— 未解决（S-1/S-2 缺口，转入架构重设专项）

以下为 v1.1.1/1.2.1 实测基线（保留，勿删）：

| 场景 | 帧/秒 | 可用性(ACK/发送)：v1.1.1 基线 | 负优化版（已回滚） | 7cb04fc（当前） |
|---|---|---|---|---|
| 512 连接 × 10 人群 | ~35,000 | 98.1% | 73.0% | 91.7%（p95 120ms） |
| 1000 连接 × 50 人群 | ~110,000 | 32.3% | 14.6% | 23.7%（p95 468ms） |
| 1000 连接 × 500 人群 | ~1,000,000(需求) | 2.2% | 0.9% | 2.0% |

写路径上限 ~5-6k 消息/秒（2 核）；1000 并发握手 1000/1000 成功，连接建立 <100ms。

**1.4 的解法（普遍拉取 + 自适应 + v3 总线）已默认开启，但 S-1~S-7 容量门槛未通过**
——上表的数字在本版本内仍然成立，容量规划公式（在线人数 × 群成员数 ×
每人消息频率 ≈ 帧/秒，压到 40k 帧/秒以内）继续按此执行。S-1/S-2 缺口转入
v1.5/v2.0 架构重设专项（`architecture-reset-v2-plan.md`）；架构重设落地后，
本段数字由 CI 刷新（README 容量表同批更新）。
