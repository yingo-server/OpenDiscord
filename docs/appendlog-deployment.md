# appendlog 消息存储（V3 实验性，v3.0 接线）

状态：**停止主线** —— 两次 CI 压测均未达 SQLite 基线，默认**不启用**。

> 2026-11-21 决策：自研 appendlog 未能超越 SQLite，保留为实验性技术储备，
> 生产默认仍为 SQLite（appendlog_dir 空）。**勿在生产启用**。

## 实测结论（S-2 4c16g raw ceiling，30s）

| 配置 | ACK 吞吐 | 可用性 | p95 |
|---|---|---|---|
| SQLite（v2.0 基线） | **1197/s** | **17.54%** | 471ms |
| appendlog sync_window=0（每条 fsync） | 283/s | 4.11% | 3025ms |
| appendlog sync_window=50ms（批量 fsync） | 174/s | 2.51% | 4016ms |

**根因**：appendlog 的 group-commit 窗口实现让提交者在协调通道上等待满窗口
（50ms/批 → 理论上限 20/s/shard），而非"立即写、延迟 fsync"；即使修此缺陷，
SQLite WAL+synchronous=NORMAL 已是极成熟的写入路径，追平/超越的把握不足。
实测数据与理论预期（×5~20）严重背离，遵守客观规律：**不发布、不切默认**。

## 保留价值
- 引擎实现本身完整（接口、恢复、dedup、overlay、单测），是一份可复用的技术资产；
- 接线（hybrid store）与 CI A/B 框架留存，后续若有人修复 group-commit 可快速重测。

## 若要恢复该主线（未来）
必须先重写 writer goroutine：请求到达即执行 fn 写 buffer 并立即返回，窗口只合并
fsync（不阻塞 submit）；再跑同样的 A/B 压测，超不过 SQLite 1197/s 就彻底放弃。

## 是什么

用自研 **append-only 日志**（分片顺序写 + 内存 seq 分配 + 稀疏索引）替代 SQLite
作为**消息热存储**。其他域（identity/conv/media/sys/report/audit）仍在 SQLite。
设计目标：突破单 SQLite 写锁的天花板（理论写吞吐 ×5~20）。

## 启用

`config/chatd.toml`：

```toml
appendlog_dir = "/var/lib/opendiscord/appendlog"   # 非空即启用；空=纯 SQLite（v2 行为）
appendlog_shards = 4                                # 0=CPU 核数；每分片独立写锁
```

重启生效（非 SIGHUP 项——存储引擎切换需要完整排空重启）。

## 关键语义

- **seq**：每 cid 在内存计数器分配，启动时从日志重放重建；不再每条 `UPDATE conv_seq`（这是吞吐提升来源）。
- **durability**：默认每条批写 `fsync`（与 v2 "ACK=已落库"一致）；引擎支持批量 fsync 窗口配置，但接线默认用 durable 路径。
- **崩溃恢复**：启动时逐帧 CRC/长度校验，尾部不完整帧截断丢弃，中间损坏报错拒绝启动。
- **dedup**：内存 LRU + 尾部扫描回放原 (msg_id, seq)。
- **edit/recall**：追加 overlay 帧，读取时叠加，不原地改写日志。

## 已知限制（v3.0 接线边界）

1. **管理面消息搜索看不到 appendlog 里的新消息**：`messages/search` 仍走 SQLite
   raw SQL；appendlog 消息不写 SQLite `msg_messages_hot`。管理面消息检索需要
   后续改造为读取 appendlog（V3-1 待做）。历史消息（归档前）查询同理。
2. **归档/清理**：archiver 仍走 SQLite 的 `ScanOlder/DeleteHotBefore`，appendlog
   里的消息暂不参与冷热分层（V3-2 待做）。
3. **切换期数据不互通**：从 appendlog 切回纯 SQLite（清空配置）后，旧 appendlog
   消息不再可见。迁移/回退工具待做（V3-3）。
4. **因此本版标记为实验性**：单实例新部署可用；有管理面消息检索需求或需要归档
   的生产部署建议暂保持纯 SQLite（v2 路径）。

## 验证

- 装配 e2e：`TestAppendlogWiringBootsAndServes`（配置 appendlog_dir 后真实发消息收 MSG_NEW）。
- 引擎单测：`server/internal/data/store/appendlog/`（1000 条读写、seq 单调、崩溃恢复、
  dedup、overlay、多分片）。
- 压测：CI stress workflow 用相同 feature_flags + `hw_profile` 对比纯 SQLite 基线
  （v2.0 ACK 1197/s）。
