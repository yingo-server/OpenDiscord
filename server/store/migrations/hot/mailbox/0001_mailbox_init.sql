-- mailbox 分域（hot, SQLite）。0001: v3-bus-design §2 骨架（P0，只建表不接线）。
--
-- 设计要点（docs/v3-bus-design.md §2）：
--   * mailbox[uid] = 单写者有序日志，mailbox_seq 由总线分配、单调递增、重启不回退
--     → mailbox_seq 是 per-uid 计数器，用 AUTOINCREMENT 保证 sqlite_sequence 持久化。
--   * crash-only：整张表即真相源，重启即恢复（S-5/T-SV-4）。
--   * compaction（T-SV-6）：acked 24h / unacked 7d 边界删除，由 P2 的清理 job 执行；
--     delivered_at 为 NULL = 尚未 ack（unacked），非 NULL 为客户端 kind 15/105 推进时间。
--   * envelope 存 blob（proto 编码），解包在内存进行——SQLite 不解释业务字段。

CREATE TABLE IF NOT EXISTS mailbox_entries (
    mailbox_seq  INTEGER PRIMARY KEY AUTOINCREMENT,  -- per-DB 单调；uid 维度单调由 P2 hub 保证
    user_id      INTEGER NOT NULL,
    envelope     BLOB    NOT NULL,                   -- 原始 Envelope（kind 102/103/104/105/14/20x）
    state        INTEGER NOT NULL DEFAULT 0,         -- 0=pending 1=inflight 2=acked
    created_at   INTEGER NOT NULL,                   -- unix ms，unacked 7d 边界依据
    delivered_at INTEGER,                            -- unix ms，acked 24h 边界依据；NULL=未 ack
    dedup_key    TEXT,                               -- fanout 幂等键（P2 去重 LRU 落盘兜底）
    reserved1    INTEGER,                            -- 预留（协议惯例：字段 4-9 保留）
    reserved2    INTEGER
);

-- 热路径查询：按 uid 取 cursor 之后的条目（DeliveryPlane 续传/消费）。
CREATE INDEX IF NOT EXISTS idx_mailbox_user_seq
    ON mailbox_entries (user_id, mailbox_seq);

-- compaction 扫描：按 (state, delivered_at) 找已 ack 过期条目。
CREATE INDEX IF NOT EXISTS idx_mailbox_state_delivered
    ON mailbox_entries (state, delivered_at);

-- per-uid 游标快照：max(105 累计推导, 15.cursor) 的生效 cursor，
-- 重启后 hub 用它初始化 mailbox_seq 下界（不回退，v3-bus-design §2.1）。
CREATE TABLE IF NOT EXISTS mailbox_cursors (
    user_id   INTEGER PRIMARY KEY,
    cursor    INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL
);
