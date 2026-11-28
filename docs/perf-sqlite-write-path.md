# SQLite 写路径精调（perf-spec 补充模块 D）— 基准报告

环境：arm64 容器（2 核），SQLite（mattn/go-sqlite3, CGO），WAL，数据集为
migrated 热库空表冷启动写入。bench 定义于
`server/internal/data/store/sqlite/perfbench_test.go`；跑法：

```
GOPROXY=off GOCACHE=/tmp/gocache GOMODCACHE=/tmp/gomod \
  go test ./server/internal/data/store/sqlite/ -run XXX -bench '...' -benchtime=200x
```

## 1. PRAGMA 精调（1000 行/批，count=2）

| 配置 | ns/op #1 | ns/op #2 | vs 基线 |
|---|---|---|---|
| **baseline**（当前生产：WAL+NORMAL+wac2000+cache8M） | 70.0M | 74.4M | — |
| +mmap_size=256MB | 80.7M | 94.4M | **-13%~-27%（更慢）** |
| cache_size=-16000 (16MB) | 86.8M | 88.7M | **-19%（更慢）** |
| mmap + cache16 组合 | 91.0M | 88.4M | **-23%（更慢）** |
| wal_autocheckpoint=1000（SQLite 默认） | 83.3M | 85.0M | -13%（更慢） |
| wal_autocheckpoint=2000（当前） | 81.5M | 83.6M | — |
| wal_autocheckpoint=4000 | 82.5M | 81.5M | ≈持平（±2%） |

**采纳：维持现状，不采纳任何新 PRAGMA。** 本机数据全部落在内存缓存
（tmpfs/页缓存），mmap 无 IO 可省、徒增页管理开销；8MB 页缓存对本工作负载
（顺序追加、无大范围随机读）已够；wac 2000→4000 差异 <2%，不满足 ≥10%
采纳门槛。结论与任务假设相反——三个候选 PRAGMA 均"测了没效果"。

## 2. 批大小（组提交吞吐）

| rows/批 | ns/op #1 | ns/op #2 | 每行 |
|---|---|---|---|
| 100 | 13.3M | 13.2M | ~132µs |
| 1000 | 83.2M | 85.8M | ~84µs |
| 5000 | 405.5M | 400.4M | ~80µs |

每行成本单调缓降但 1000→5000 仅省 ~5%，而 5000 行事务的绝对时延 ~400ms
（组提交持写锁期间 reader 全被 WAL 快照挡在外面）。

**读并发下的写（1 writer + 4 readers 持续 ReadHot）：**
265ms / 234ms 每 1000 行批 —— 比无读并发（~84ms/千行）慢约 3 倍，
说明 WAL 读锁竞争是真实成本；**批越大、单事务持锁越久，reader 饥饿越重**
（5000 行事务 = 读者最长阻塞 ~400ms）。甜点在 1000 行附近，与现生产值一致。

**采纳：batch_size 默认保持 1000；MaxWriteBatchSize=8000 上限不动
（5000 行已显示收益枯竭+尾延迟恶化，8000 更不可取）。**

## 3. 组提交矩阵（batch_size × flush_interval_ms，含 p95 ACK 时延）

模拟 flusher：8 发送端把 batch_size 条消息在 flush 窗口内均匀送入，commit
后逐消息计 ACK 时延（enqueue→commit）。

| batch | interval | ns/op(每消息) | p95 ACK µs |
|---|---|---|---|
| 500 | 25ms | 56.4M | **76.9k** |
| 500 | 50ms | 54.6M | **69.8k** |
| 500 | 100ms | 54.2M | 69.4k |
| 1000 | 25ms | 99.7M | 198.7k |
| 1000 | 50ms | 104.9M | 187.0k |
| 1000 | 100ms | 102.6M | 186.6k |
| 2000 | 25ms | 218.8M | 318.4k |
| 2000 | 50ms | 231.6M | 331.2k |
| 2000 | 100ms | 219.9M | 337.2k |
| 4000 | 25ms | 448.2M | 584.6k |
| 4000 | 50ms | 441.8M | 586.3k |
| 4000 | 100ms | 474.2M | 640.0k |

按 p95 优先判据：**batch=500 / interval=50ms** 全场最优（p95 70ms，
每消息吞吐 ~55ns 同 batch 内最优）。interval 25→50 收益明显，50→100 无
进一步收益（发送端已能在 50ms 内凑满 500）。batch 500→1000 p95 翻倍
（70→187ms），2000/4000 直线恶化且吞吐无补偿。

**policy 默认值建议（结论）：**
- `write_path.batch_size` 默认 **1000 → 建议下调至 500**（p95 减半，吞吐
  每消息不变；若运维看重绝对 TPS 而非 ACK p95，可保持 1000）。
- `write_path.flush_interval_ms` 默认 50ms **维持**（25ms 略差，100ms 无收益）。
- 本项为 policy 常量建议，未改 `policy.DefaultWriteBatchSize` 代码 ——
  改默认值影响生产语义，留给 review 决定（见 UNVERIFIED）。

## 4. 采纳/拒绝汇总

| 项 | 决定 | 理由 |
|---|---|---|
| mmap_size=256MB | ❌ 拒绝 | 实测慢 13–27%，≥10% 门槛反向不达标 |
| cache_size=-16000 | ❌ 拒绝 | 实测慢 ~19%，本负载无随机读 |
| mmap+cache16 组合 | ❌ 拒绝 | 两项劣化叠加 |
| wal_autocheckpoint 4000 | ❌ 拒绝 | ±2% 噪声内，非 ≥10% 提升 |
| wal_autocheckpoint 2000（现状） | ✅ 保留 | 优于 1000（默认），4000 无增益 |
| 组提交 batch 500 + interval 50ms | ⚠️ 建议采纳进 policy 默认 | p95 全场最优；代码默认值改动待 review |
| MaxWriteBatchSize=8000 | ✅ 不动 | 上限无害（仅验证允许，不默认） |

## 5. PRAGMA 断言

现有 `TestPragmasApplied`（prune_test.go）覆盖 auto_vacuum /
journal_size_limit / cache_size。本次**未改动任何 PRAGMA**（全部候选被
数据拒绝），故无需新增断言；若未来采纳新 PRAGMA，必须按同一模式补
`row := db.QueryRow("PRAGMA <name>"); row.Scan(&v)` 断言。

## 6. UNVERIFIED（交 CI）

- 本报告所有数字来自 2 核 arm64 容器（数据大概率在页缓存中，fsync 成本
  被低估）。**在带真实磁盘的生产规格上重跑** `BenchmarkPragmaConfigs` 与
  `BenchmarkGroupCommitMatrix` 后再定 PRAGMA/默认值 —— 特别是 mmap：
  真实 IO 下结论可能反转。
- `batch=500` 默认值建议未落地为代码（policy.DefaultWriteBatchSize 仍
  1000），需 review 决定是否接受 p95/TPS 权衡。
- CI 基准（-benchtime 较小）方差 ±10% 属正常；采纳判断均基于 ≥10% 门槛
  双次运行确认。
