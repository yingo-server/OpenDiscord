package appendlog

import (
	"context"
	"fmt"
	"testing"

	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
)

// benchAppend runs one AppendMessages batch with fresh seq allocation, which
// is the production contract (flusher assigns seq before calling store).
func benchAppend(b *testing.B, cfg Config, cid int64, prefix string) {
	db, err := Open(cfg)
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	b.SetBytes(int64(100 * 32)) // approx per-row overhead
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start, err := db.AllocateSeq(ctx, cid, 100)
		if err != nil {
			b.Fatal(err)
		}
		batch := make([]store.PendingMsg, 100)
		for j := range batch {
			batch[j] = store.PendingMsg{
				Cid: cid, SenderID: 7, Seq: start + int64(j),
				ClientMsgID: fmt.Sprintf("%s-%d-%d", prefix, i, j),
				Content:     []byte(`{"text":"benchmark payload"}`), SentAt: 1,
			}
		}
		if err := db.AppendMessages(ctx, batch); err != nil {
			b.Fatal(err)
		}
	}
}

// Durable path: fsync per batch write.
func BenchmarkAppendMessages(b *testing.B) {
	benchAppend(b, Config{Dir: b.TempDir(), Shards: 4, SyncWindow: 0}, 1, "bench")
}

// Relaxed durability: batched fsync (SyncWindow) — quantifies the trade-off.
func BenchmarkAppendMessagesAsync(b *testing.B) {
	benchAppend(b, Config{Dir: b.TempDir(), Shards: 4, SyncWindow: 64}, 2, "abench")
}
