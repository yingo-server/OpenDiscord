package sqlite

// Write-path tuning benchmarks (perf-spec module D). Each benchmark isolates
// one variable against the production baseline so the numbers can be compared
// directly:
//
//	BenchmarkPragmaConfigs   — PRAGMA variants (mmap / wal_autocheckpoint / cache_size)
//	BenchmarkBatchSizes      — 100 vs 1000 vs 5000 rows per group commit
//	BenchmarkWriteWithReaders— 1 writer + 4 concurrent readers (WAL read-lock pressure)
//	BenchmarkGroupCommitMatrix — batch_size x flush_interval (ns/op + p95 ack latency)
//
// Raw numbers and the adopt/reject decisions live in docs/perf-sqlite-write-path.md.

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/data/store"
)

// benchDBDSN opens a raw pool with a custom pragma DSN (mattn DSN params apply
// per-connection, unlike Exec'd PRAGMAs). The schema is migrated through a
// wrapper so the benchmark writes the real multi-value INSERT path.
func benchDBDSN(b *testing.B, extraDSN string) *DB {
	b.Helper()
	dsn := fmt.Sprintf("file:%s/bench.db?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=1"+
		"&_auto_vacuum=2&_cache_size=-8000&_txlock=immediate%s", b.TempDir(), extraDSN)
	sqlDB, err := sql.Open("sqlite3", dsn)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { sqlDB.Close() })
	sqlDB.SetMaxOpenConns(4)
	sqlDB.SetMaxIdleConns(4)
	d := &DB{DB: sqlDB, msgw: &msgWriter{db: sqlDB}, msgr: &msgReader{db: sqlDB}}
	if err := d.Migrate(context.Background()); err != nil {
		b.Fatalf("migrate: %v", err)
	}
	return d
}

// benchAppendN writes `n` rows in one AppendMessages call, `b.N` batches.
func benchAppendN(b *testing.B, db *DB, n int) {
	b.Helper()
	w := db.msgw
	ctx := context.Background()
	var batch []store.PendingMsg
	b.ReportMetric(float64(n), "rows/op")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch = benchBatch(1, int64(i*n+1), n)
		if err := w.AppendMessages(ctx, batch); err != nil {
			b.Fatalf("AppendMessages: %v", err)
		}
	}
	b.StopTimer()
}

// BenchmarkPragmaConfigs compares PRAGMA variants at a fixed 1000-row batch.
// baseline mirrors production Open(): WAL, synchronous=NORMAL,
// wal_autocheckpoint=2000, cache_size=-8000.
func BenchmarkPragmaConfigs(b *testing.B) {
	cases := []struct {
		name string
		dsn  string
	}{
		{"baseline", ""},
		{"mmap_256MB", "&_mmap_size=268435456"},
		{"cache_16MB", "&_cache_size=-16000"},
		{"mmap+cache16", "&_mmap_size=268435456&_cache_size=-16000"},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			benchAppendN(b, benchDBDSN(b, tc.dsn), 1000)
		})
	}
}

// BenchmarkWalAutoCheckpoint sweeps the passive-checkpoint threshold
// (pages). Production runs 2000; 1000 is SQLite's default, 4000 tests whether
// deferring checkpoints further still helps.
func BenchmarkWalAutoCheckpoint(b *testing.B) {
	for _, pages := range []int{1000, 2000, 4000} {
		b.Run(fmt.Sprintf("pages_%d", pages), func(b *testing.B) {
			db := benchDBDSN(b, "")
			if _, err := db.Exec(fmt.Sprintf("PRAGMA wal_autocheckpoint=%d", pages)); err != nil {
				b.Fatalf("wal_autocheckpoint: %v", err)
			}
			benchAppendN(b, db, 1000)
		})
	}
}

// BenchmarkBatchSizes sweeps rows per group commit to find the throughput
// sweet spot (task D-2: 100 / 1000 / 5000).
func BenchmarkBatchSizes(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		b.Run(fmt.Sprintf("rows_%d", n), func(b *testing.B) {
			benchAppendN(b, benchDBDSN(b, ""), n)
		})
	}
}

// BenchmarkWriteWithReaders measures write throughput with 4 concurrent
// readers hammering ReadHot — the WAL read-lock / checkpoint pressure case.
// The writer keeps its own message space so the readers see a growing but
// never colliding seq range.
func BenchmarkWriteWithReaders(b *testing.B) {
	db := benchDBDSN(b, "")
	ctx := context.Background()
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = db.ReadHot(ctx, 1, 0, 200, true)
			}
		}()
	}
	var batch []store.PendingMsg
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		batch = benchBatch(1, int64(i*1000+1), 1000)
		if err := db.AppendMessages(ctx, batch); err != nil {
			b.Fatalf("AppendMessages: %v", err)
		}
	}
	b.StopTimer()
	close(stop)
	wg.Wait()
	b.ReportMetric(1000, "rows/op")
}

// BenchmarkGroupCommitMatrix simulates the flusher's group commit against the
// real store: messages arrive uniformly inside the flush window, the batch
// commits via AppendMessages when batchSize or flushInterval is reached, and
// each message's ACK latency is enqueue->commit. Reports ns/op (per message),
// msg_per_flush and p95_us (ack latency p95) — the matrix is judged on p95
// FIRST, throughput second (a bigger batch that doubles p95 is a regression
// for an ack-holding pipeline).
func BenchmarkGroupCommitMatrix(b *testing.B) {
	for _, batchSize := range []int{500, 1000, 2000, 4000} {
		for _, intervalMS := range []int{25, 50, 100} {
			b.Run(fmt.Sprintf("batch_%d_interval_%dms", batchSize, intervalMS), func(b *testing.B) {
				db := benchDBDSN(b, "")
				ctx := context.Background()
				const concurrency = 8
				lat := make([]time.Duration, 0, b.N*batchSize)
				var mu sync.Mutex
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					base := int64(i*batchSize + 1)
					ready := make(chan store.PendingMsg, batchSize)
					var commitWG sync.WaitGroup
					commitWG.Add(1)
					go func() {
						defer commitWG.Done()
						batch := make([]store.PendingMsg, 0, batchSize)
						deadline := time.After(time.Duration(intervalMS) * time.Millisecond)
						for len(batch) < batchSize {
							select {
							case m := <-ready:
								batch = append(batch, m)
							case <-deadline:
								goto commit
							}
						}
					commit:
						now := time.Now()
						if err := db.AppendMessages(ctx, batch); err != nil {
							b.Error("AppendMessages: ", err)
						}
						mu.Lock()
						for range batch {
							lat = append(lat, time.Since(now))
						}
						mu.Unlock()
						b.ReportMetric(float64(len(batch)), "msg_per_flush")
					}()
					// `concurrency` senders produce batchSize msgs uniformly
					// across the flush window (arrival process, not the
					// measurement subject).
					var sendWG sync.WaitGroup
					for c := 0; c < concurrency; c++ {
						sendWG.Add(1)
						go func(c int) {
							defer sendWG.Done()
							for k := c; k < batchSize; k += concurrency {
								ready <- store.PendingMsg{
									MsgID: base + int64(k), Cid: 1, Seq: base + int64(k),
									SenderID: 7, ClientMsgID: fmt.Sprintf("gc-%d-%d", base, k),
									Content: []byte(`{"text":"hello"}`), SentAt: 42,
								}
							}
						}(c)
					}
					sendWG.Wait()
					commitWG.Wait()
				}
				b.StopTimer()
				mu.Lock()
				sort.Slice(lat, func(i, j int) bool { return lat[i] < lat[j] })
				if len(lat) > 0 {
					b.ReportMetric(float64(lat[len(lat)*95/100].Microseconds()), "p95_us")
				}
				mu.Unlock()
			})
		}
	}
}
