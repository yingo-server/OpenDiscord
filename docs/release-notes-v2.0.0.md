# OpenDiscord v2.0.0 发布说明

发布日期：2026-11-21
Tag：`v2.0.0`（正式版，非 prerelease）
基线：v1.5.0

## 本版主题：容量契约 + 写路径优化（架构重设第一步）

v2.0 是 `docs/architecture-reset-v2-plan.md` 的首个落地版本。它解决的是
"过载时用户看到消息一直转圈"和"写路径每消息一次 SQLite 写事务"两个确定性问题，
并给出显式的容量契约。**本版不宣称容量达标**——物理天花板仍在，见下文诚实数据。

## 1. 容量契约（V2-0）：超限明确拒绝，不再静默挂起

- **ACK 不撒谎**：客户端 MSG_ACK `status!=0` 现在把消息标记失败，不再当作发送成功。
- **拒绝即失败**：RATE_LIMITED(kind13) / ERROR(kind10) 触发 in-flight 消息立即失败
  （红点），不再是 10 秒 ACK 超时才报错；RATE_LIMITED 携带触发帧的 `AckId`
  （现成 Envelope 字段，零协议改动）做请求配对。
- **全局准入 QPS**：`policy.capacity.admission_qps`（0=禁用，SIGHUP 热改）提供进程级
  写操作准入桶，超限返回明确拒绝；`max_concurrent_conns` 为预留轴。
- 配置与运维：`docs/capacity-contract.md`。

## 2. 写路径优化（V2-1）：吞吐翻倍

- **seq 批量分配**：原来每条 `msg_send` 在 dispatch goroutine 内做一次
  `AllocateSeq`（独立 SQLite 写事务），与 group-commit 批事务争同一写锁。
  现在由 flusher 对整批按 cid 聚组、每 cid 一次预留连续 seq 段再批量 INSERT。
  N 条消息从 N+1 次写事务降为「每 cid 一次 + 批事务一次」。
- **batch 默认值 1000→500**：仓库内历史实测（docs/perf-sqlite-write-path.md）显示
  batch=500 的 p95 ≈70ms vs 1000≈187ms，全场最优。
- 守住：durability（ACK 仍在 commit 后）、seq 单调、dedup 幂等。

## 3. 真实压测数据（诚实，不加工）

S-2 场景（1000×50 人群），4c16g，raw ceiling 无节流，30 秒：

| 指标 | v1.5 基线 | v2.0.0 |
|---|---|---|
| 可用性 | 16.38% | **17.54%** |
| ACK 吞吐 | ≈579/s | **1197/s（+107%）** |
| p95 ACK | — | 471ms |
| 已确认消息数 | ≈17k | 35909 |

**结论**：写吞吐确实翻倍、可用性略超基线，但绝对可用性距门槛 90% 仍是数量级差距。
单进程 SQLite + 满速扇出的物理天花板未被突破。v2.0 的价值是"过载行为正确 + 写吞吐
翻倍 + 容量可显式管控"，**不是容量达标**。架构级突破（分片 / 会话 Actor / 冷热分层）
留待 V2-2+ 独立版本。

## 4. 升级

从 v1.5.0 升级：配置兼容；`policy.capacity` 缺省为 0（准入不启用，行为等价 v1.5）；
`write_path.batch_size` 默认 500（可在 policy.json 覆盖）。部署见 `docs/deploy.md`，
回滚见 `deploy/release.sh`。

## 5. 已知边界

- S-1（500人群×1000在线）、S-2 容量门槛仍未达标（见 docs/lts-1.4-plan.md §9、
  docs/v2.0-roadmap.md）；超限由 V2-0 的显式限流兜底，不再静默挂起。
- S-7 逐字节基线仍受 v1.2.1 历史 tag 构建缺陷阻断。
- 下一步（V2-2）：热库按 conv_id 分片 / 会话 Actor / dedup 批内隔离 / 扇出解耦，
  属独立架构版本，不捆绑本版。
