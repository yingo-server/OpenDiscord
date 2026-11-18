# OpenDiscord v1.4.0 发布说明（LTS · 知情发布）

发布日期：2026-11-18
Tag：`v1.4.0`（commit `095ef48`）
性质：**知情发布（informed release）** —— 代码转生产完成、CI 全绿；容量验收未全达，性能缺口已知并转入 v1.5 架构重设专项。

## 本版核心：四条实验性特性转生产默认

| Flag | 旧默认 | v1.4.0 默认 | 作用 |
|---|---|---|---|
| `universal_pull_enabled` | false | **true** | 所有会话统一走水位线（kind 14）+ 客户端批量拉取，取消 100 成员阈值分流 |
| `adaptive_control_enabled` | false | **true** | AIMD 自适应控制器按队列/ACK 信号写 `effective[cid]`，动态调 batch/interval |
| `v3_bus_enabled` | false | **true** | MailboxHub + DeliveryPlane + credit 流控成为默认投递路径 |
| `v3_bus_percent` | 0 | **100** | 全量 uid 按哈希分桶进 v3 灰度 |

配套：`msg_domain_v2_enabled`（编辑/转发/@/举报）同步默认 true；旧客户端通过
`CLIENT_CAPS(18)` / `Hello.pull_capable` 上报能力，无能力连接自动保持 inline 102，
逐字节兼容 v1.2.1 老客户端（S-7 回滚语义，见下）。

## 代码质量

- CI `-race` 全绿：`go test ./... -race -count=1 -p 1` + web client/admin 构建。
- 本轮修复的真实生产 bug：
  - 自适应控制循环哨兵 `cid=1` 与首个真实会话 id 撞车，每秒覆盖 `effective[1]` → 改 `1<<62`。
  - v3 双投递保护误伤 reply-lane 帧（931/ACK/控制帧）→ 豁免非 mailbox 帧。
  - gorilla websocket e2e 读 deadline 状态污染 → 改单读者 goroutine。
- 压测编排/脚本修复 9 处真实 bug（dispatch/constrain/APP_ENV/no-boot/证据字段/panic 等），均为测试基础设施，未改产品逻辑。

## 已知容量限制（知情发布的核心）

v1.4 首轮双规格压测（2c2g / 4c16g，S-1~S-7）结果 **PASS=0 / FAIL=6 / BLOCKED=8**。
完整归因见 `docs/lts-1.4-plan.md §9`。摘要：

| 场景 | 4c16g 实测 | 门槛 | 结论 |
|---|---|---|---|
| S-3（512×10 人群） | 93.86% | 94.5% | 差 0.64%，接近 |
| S-2（1000×50 人群） | 16.38%（带限流 36.77%） | 90% | 数量级缺口 |
| S-1（500 人群×1000 在线） | 0.93% | 95% | 数量级缺口 |

**不掩盖、不篡改**：上述数字原样保留。瓶颈定位在"消息落库 + 大群扇出"路径
（presence/receipt/typing 均 100%，唯 `msg_send` 不足）；S-1 负载为
~6933 msg/s × 500 成员扇出 ≈ 346 万帧/s + 同速率 SQLite 写入，超出当前
单进程 SQLite 架构的物理包络。带生产限流重测 S-2 仍仅 36.77%，确认是吞吐
天花板而非延迟 bug。

**本次发版不等同于"容量达标"**：发布的是"代码转生产完成 + 已知限制透明化"，
容量门槛转入 **v1.5 架构重设专项**。

## BLOCKED 项

- S-4/S-5/S-6：证据字段/工具 panic 已修，待重跑即可判定（非产品缺陷）。
- S-7：`v1.2.1` 历史 tag 自身 `go build` 失败（`callConnCleanups` 未随 tag 提交），
  逐字节基线无法构建；不重打历史 tag，如实保留 BLOCKED。

## 升级与回滚

- 从 v1.2.1 升级：配置兼容；新 flag 默认开启即为 v1.4 行为。
- 回滚到 v1.2.1 行为：SIGHUP 热改四个 flag 为 false / percent=0（逐字节基线），
  无需换二进制。
- 部署见 `docs/deploy.md`；发布脚本 `deploy/release.sh`（原子切换 + 健康检查 + 失败回滚）。

## 产物

tag 推送触发 `.github/workflows/ci.yml` 的 tag-only jobs，构建 9 个产物
（5 Linux 架构 + 2 Windows + src + SHA256SUMS）并自动创建 GitHub Release。
