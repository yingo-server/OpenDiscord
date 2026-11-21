// Package hybrid composes the append-only log message store with the SQLite
// store for all non-message domains. It satisfies pipe.HotStores by embedding
// *sqlite.DB (all 7 interfaces) and overriding the 6 message methods so writes
// and reads go to the appendlog engine.
//
// Wired when featureflag appendlog_enabled is on; flag-off keeps the plain
// *sqlite.DB path byte-identical (docs/architecture-optimization-menu.md L3-1).
package hybrid

import (
	"context"
	"errors"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store/appendlog"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store/sqlite"
)

// Store is the v3 hybrid hot store: messages in append-only logs, everything
// else in SQLite. The embedded *sqlite.DB supplies Identity/Conv/Sys/Media/
// Report surfaces; the 6 message methods below override its promoted methods.
type Store struct {
	*sqlite.DB
	log *appendlog.DB
}

var (
	_ store.MsgWriter     = (*Store)(nil)
	_ store.MsgReader     = (*Store)(nil)
	_ store.IdentityStore = (*Store)(nil)
	_ store.ConvStore     = (*Store)(nil)
	_ store.SysStore      = (*Store)(nil)
	_ store.MediaStore    = (*Store)(nil)
	_ store.ReportStore   = (*Store)(nil)
)

// New builds a hybrid store over an already-open SQLite DB (which the
// assembly keeps using for admin/jobs/identity). Only message ops are
// redirected to the appendlog; the SQLite handle is shared, not closed here.
func New(sqliteDB *sqlite.DB, logDir string, shards int, syncWindow time.Duration) (*Store, error) {
	if sqliteDB == nil {
		return nil, errors.New("hybrid: nil sqlite DB")
	}
	logDB, err := appendlog.Open(appendlog.Config{Dir: logDir, Shards: shards, SyncWindow: syncWindow})
	if err != nil {
		return nil, err
	}
	return &Store{DB: sqliteDB, log: logDB}, nil
}

// Close closes only the appendlog; the shared SQLite DB is closed by the
// assembly's existing lifecycle.
func (s *Store) Close() error { return s.log.Close() }

// ---- MsgWriter overrides (promoted sqlite methods shadowed) ----

func (s *Store) AppendMessages(ctx context.Context, batch []store.PendingMsg) error {
	return s.log.AppendMessages(ctx, batch)
}
func (s *Store) AllocateSeq(ctx context.Context, cid int64, n int) (int64, error) {
	return s.log.AllocateSeq(ctx, cid, n)
}
func (s *Store) MarkEdited(ctx context.Context, cid, msgID, senderID int64, content []byte, at int64) (*store.HotMsg, error) {
	return s.log.MarkEdited(ctx, cid, msgID, senderID, content, at)
}
func (s *Store) MarkRecalled(ctx context.Context, cid, msgID, senderID, at int64) (*store.HotMsg, error) {
	return s.log.MarkRecalled(ctx, cid, msgID, senderID, at)
}

// ---- MsgReader overrides ----

func (s *Store) ReadHot(ctx context.Context, cid int64, beforeSeq int64, limit int, asc bool) ([]store.HotMsg, error) {
	return s.log.ReadHot(ctx, cid, beforeSeq, limit, asc)
}
func (s *Store) FirstSeqAfter(ctx context.Context, cid int64, since int64) (int64, error) {
	return s.log.FirstSeqAfter(ctx, cid, since)
}
