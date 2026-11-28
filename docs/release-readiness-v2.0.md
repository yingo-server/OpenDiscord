# v2.0.0 发布准备核查清单（Release Readiness）

> 状态：v2.0.0 已发布（正式版）。本清单为发布前事实核查记录，供复盘与下次发布复用。
> 核查日期：2026-11-21 · 核查方式：git / GitHub Release & Actions API / 文档比对（未本地构建）。

## 1. 版本事实

| 项 | 事实 | 来源 |
|---|---|---|
| 版本 | v2.0.0（正式版，非 prerelease，非 draft） | GitHub Release API |
| Tag | `v2.0.0`，创建于 2026-11-21 07:28:49Z，target: main | GitHub Release API |
| Release URL | https://github.com/yingo-server/UFOT/releases/tag/v2.0.0 | GitHub Release API |
| CI | HEAD `ad5c7712` 的 CI run conclusion = **success**（含 `-race` 全绿） | GitHub Actions API |
| Stress run | HEAD 前一提交 `cc200b53` 的 Stress run = **success** | GitHub Actions API |

## 2. 代码状态（核查时）

- HEAD：`ad5c771` — `docs(arch): record L2-5 universal-pull A/B result (no availability gain)`
- 工作区：除本批文档改动外干净（无未提交代码变更）。
- 注意：HEAD 已领先 v2.0.0 tag（tag 后合入了 stressd 修复 + L2-5 实测文档）。这些改动
  不改变生产路径行为（appendlog 实验代码默认关闭且已删除；stressd 仅影响压测工具），
  但 v2.0.0 产物构建自 tag 时的 main，而非当前 HEAD——下次 patch 版本时应包含这些提交。
- 最新 CI 结论：success（默认分支）。

## 3. 发布资产（9 产物，与 Release API 一致）

1. ufot-v2.0.0-linux-386.tar.gz
2. ufot-v2.0.0-linux-amd64.tar.gz
3. ufot-v2.0.0-linux-arm.tar.gz
4. ufot-v2.0.0-linux-arm64.tar.gz
5. ufot-v2.0.0-linux-riscv64.tar.gz
6. ufot-v2.0.0-windows-386.zip
7. ufot-v2.0.0-windows-amd64.zip
8. ufot-v2.0.0-src.tar.gz
9. SHA256SUMS

## 4. 已知边界（诚实陈述，不夸大）

- **容量未达标**：S-1（500人群×1000在线）、S-2（1000×50人群）门槛 90% 均未达到——
  v2.0.0 实测 S-2 可用性 17.54%（v1.5 基线 16.38%），ACK 吞吐 1197/s（+107%）。
  数据见 [v2.0-roadmap.md §版本](v2.0-roadmap.md)、[lts-1.4-plan.md §9](lts-1.4-plan.md)。
  超限由容量契约显式拒绝兜底，不静默挂起。
- **自研 appendlog 已放弃**：两次 CI A/B（283/s、174/s）远低于 SQLite（1197/s），
  主线不成立，引擎与接线代码已全部删除，SQLite 为唯一生产路径。
  见 [architecture-optimization-menu.md](architecture-optimization-menu.md)、
  [implementation-progress-honest-assessment.md](implementation-progress-honest-assessment.md)。
- **普遍拉取 L2-5 实测不成立**：真实 S-2 数据（run 44907652962）可用性 13.66%（低于
  inline 基线 17.54%）、ACK 945/s；服务端 drop -86% 是真实收益但不转化为可用性。
  现有协议下 SYNC 逐条返回，无法合批，O(1) 下行不可达。见
  [architecture-optimization-menu.md §L2-5](architecture-optimization-menu.md)。
- S-7 逐字节基线仍受 v1.2.1 历史 tag 构建缺陷阻断。

## 5. 上线前 checklist

- [x] 部署文档就绪：[deploy.md](deploy.md)（单机 embedded 分步部署）
- [x] 容量契约文档就绪：[capacity-contract.md](capacity-contract.md)
- [ ] 配置迁移确认：`policy.capacity` 缺省为 0（准入不启用，行为等价 v1.5，兼容无需迁移）；
      如需启用准入限流，按 capacity-contract.md 配置并 SIGHUP 热改验证
- [ ] `write_path.batch_size` 确认为 500（v2.0.0 新默认；旧 policy.json 显式写了 1000 的需手动改）
- [x] 回滚路径：`deploy/release.sh`（见 release-notes-v2.0.0.md §4）
- [x] 已知问题链接：[known-issues.md](known-issues.md)、[release-notes-v2.0.0.md §5](release-notes-v2.0.0.md)

## 6. 判定

**可以发布：是（已发布）**。判定依据（只列事实）：

1. v2.0.0 tag、CI -race 全绿、9 产物齐备且与 release-notes 描述一致。
2. 本版承诺均为"过载行为正确（容量契约 + ACK 诚实）+ 写吞吐翻倍"——均有实测数据支撑。
3. 本版**未承诺容量达标**，release-notes 与 known-issues 均如实记录 S-1/S-2 未达门槛、
   自研 appendlog 放弃、L2-5 实测不成立；文档与代码一致，无夸大表述。
4. 配置向后兼容（`policy.capacity` 缺省 0），回滚路径已文档化。

保留事项（不阻断本版）：HEAD 领先 tag 若干文档/压测工具提交，宜在下一 patch 版本收编。
