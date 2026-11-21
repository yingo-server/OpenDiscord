# appendlog 消息存储（V3 实验性，v3.0 接线）

状态：v3.0 接线完成，默认**关闭**；需显式配置 `appendlog_dir` 才启用。

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
