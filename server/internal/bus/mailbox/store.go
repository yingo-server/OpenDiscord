// Package mailbox —— v3 消息总线 P2：MailboxHub 核心（docs/v3-bus-design.md §2/§9）。
//
// 分层铁律：本包属于 bus 层，只依赖 database/sql 与标准库，
// 不 import internal/data/*（import-lint：bus 不依赖 data/pipe）。
// SQLite 句柄由调用方（组装层）注入——与 data 层 openDriver 打开的同一个
// 热库连接池；mailbox 分域的 DDL 见 store/migrations/hot/mailbox/0001（P0）。
package mailbox

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Entry 是 mailbox_log 的一行（§2.1：{mailbox_seq, envelope, state}）。
type Entry struct {
	MailboxSeq  int64 // 全局自增主键；per-uid 视角下单调（单写者保证）
	UserID      int64
	Envelope    []byte        // 原始 Envelope（proto 编码帧）
	State       int           // 0=pending 1=inflight 2=acked
	CreatedAt   int64         // unix ms
	DeliveredAt sql.NullInt64 // unix ms；NULL = 未 ack
}

// 状态常量（与 0001 迁移注释一致）。
const (
	StatePending  = 0
	StateInflight = 1
	StateAcked    = 2
)

// Store 是 mailbox 分域的持久化门面。复用注入的 *sql.DB（热库），
// 全部 SQL 参数化；本包不拼接任何 SQL 字符串。
type Store struct {
	db *sql.DB
}

// NewStore 基于既有 sqlite 句柄构造（不负责打开/迁移——DDL 属 P0 迁移树）。
func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// Append 追加一条 envelope，返回全局单调 mailbox_seq（AUTOINCREMENT）。
// per-uid 单调性由 hub 的单写者 mutex 保证（§2.2：序号分配与追加在同一把锁内）。
// 幂等性不在 SQL 层做（dedup_key 仅作落盘兜底，唯一约束交给 LRU + 单写者）。
func (s *Store) Append(ctx context.Context, uid int64, dedupKey string, envelope []byte) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO mailbox_entries (user_id, envelope, state, created_at, dedup_key)
		 VALUES (?, ?, ?, ?, ?)`,
		uid, envelope, StatePending, nowMillis(), dedupKey)
	if err != nil {
		return 0, fmt.Errorf("mailbox: append uid=%d: %w", uid, err)
	}
	seq, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("mailbox: append lastinsert uid=%d: %w", uid, err)
	}
	return seq, nil
}

// ListSince 返回 uid 在 cursor 之后（不含 cursor）的条目，按 mailbox_seq 升序，
// 至多 limit 条（limit<=0 视为 1）。DeliveryPlane 续传/消费的热路径（§2.3）。
func (s *Store) ListSince(ctx context.Context, uid, cursor int64, limit int) ([]Entry, error) {
	if limit <= 0 {
		limit = 1
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT mailbox_seq, user_id, envelope, state, created_at, delivered_at
		 FROM mailbox_entries
		 WHERE user_id = ? AND mailbox_seq > ?
		 ORDER BY mailbox_seq ASC
		 LIMIT ?`, uid, cursor, limit)
	if err != nil {
		return nil, fmt.Errorf("mailbox: list uid=%d cursor=%d: %w", uid, cursor, err)
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.MailboxSeq, &e.UserID, &e.Envelope, &e.State, &e.CreatedAt, &e.DeliveredAt); err != nil {
			return nil, fmt.Errorf("mailbox: list scan uid=%d: %w", uid, err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// MarkAcked 累积推进游标（kind 15 语义，§4.2）：把 uid 的 seq<=cursor 且未 ack
// 的条目标记 acked（state=2, delivered_at=now），并把生效 cursor 落
// mailbox_cursors（max(旧值, cursor)——105/15 双源取大，幂等）。
func (s *Store) MarkAcked(ctx context.Context, uid, cursor int64) error {
	now := nowMillis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mailbox: ack begin uid=%d: %w", uid, err)
	}
	defer tx.Rollback() // 提交后 Rollback 为 no-op
	if _, err := tx.ExecContext(ctx,
		`UPDATE mailbox_entries
		 SET state = ?, delivered_at = ?
		 WHERE user_id = ? AND mailbox_seq <= ? AND state < ?`,
		StateAcked, now, uid, cursor, StateAcked); err != nil {
		return fmt.Errorf("mailbox: ack update uid=%d: %w", uid, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO mailbox_cursors (user_id, cursor, updated_at)
		 VALUES (?, ?, ?)
		 ON CONFLICT(user_id) DO UPDATE SET
		   cursor = MAX(cursor, excluded.cursor),
		   updated_at = excluded.updated_at`,
		uid, cursor, now); err != nil {
		return fmt.Errorf("mailbox: ack cursor uid=%d: %w", uid, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mailbox: ack commit uid=%d: %w", uid, err)
	}
	return nil
}

// Cursor 返回 uid 的持久化生效游标（无记录时 0）。重启恢复用（§2.3）。
func (s *Store) Cursor(ctx context.Context, uid int64) (int64, error) {
	var c int64
	err := s.db.QueryRowContext(ctx,
		`SELECT cursor FROM mailbox_cursors WHERE user_id = ?`, uid).Scan(&c)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("mailbox: cursor uid=%d: %w", uid, err)
	}
	return c, nil
}

// Head 返回 uid 当前最大 mailbox_seq（无条目时 0）。hub 重启时重建 head 用。
func (s *Store) Head(ctx context.Context, uid int64) (int64, error) {
	var h sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MAX(mailbox_seq) FROM mailbox_entries WHERE user_id = ?`, uid).Scan(&h)
	if err != nil {
		return 0, fmt.Errorf("mailbox: head uid=%d: %w", uid, err)
	}
	return h.Int64, nil
}

// Compact 执行保留策略删除（T-SV-6）：
//   - 已 ack 且 delivered_at < ackedBefore（unix ms）→ 删；
//   - 未 ack（state<2，即 pending/inflight）且 created_at < unackedBefore → 删。
//
// 两个边界相互独立，任传 math.MaxInt64 即关闭对应分支；返回删除行数。
// 活跃条目（新于边界）一律不触碰——WHERE 只匹配时间早于边界的行。
func (s *Store) Compact(ctx context.Context, ackedBefore, unackedBefore int64) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM mailbox_entries
		 WHERE (state = ? AND delivered_at IS NOT NULL AND delivered_at < ?)
		    OR (state < ? AND created_at < ?)`,
		StateAcked, ackedBefore, StateAcked, unackedBefore)
	if err != nil {
		return 0, fmt.Errorf("mailbox: compact: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mailbox: compact rows: %w", err)
	}
	return n, nil
}

// nowMillis 由变量承载，测试可替换（不引入 data 层的 now.go 依赖）。
var nowMillis = func() int64 { return time.Now().UnixMilli() }
