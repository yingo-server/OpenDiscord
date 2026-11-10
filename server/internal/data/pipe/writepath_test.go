package pipe

import (
	"context"
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/data/policy"
	"github.com/yingo-server/OpenDiscord/server/internal/data/store"
)

// TestFlusherApplyWritePolicy: the group-commit batch size and flush interval
// must come from the policy document (write_path section), not be hardcoded.
// pipe.New is the seam: it accepts a policy.Policy, policy.Snapshot or
// *policy.Store; nil/unknown types fall back to the built-in defaults.
func TestFlusherApplyWritePolicy(t *testing.T) {
	cases := []struct {
		name      string
		pol       any
		wantBatch int
		wantEvery time.Duration
	}{
		{"nil", nil, defaultFlushBatch, defaultFlushEvery},
		{"unknown type", "policy-as-string", defaultFlushBatch, defaultFlushEvery},
		{"empty policy", policy.Policy{}, defaultFlushBatch, defaultFlushEvery},
		{"default policy", policy.Default(), defaultFlushBatch, defaultFlushEvery},
		{
			"custom",
			policy.Policy{WritePath: policy.WritePathConfig{BatchSize: 2000, FlushIntervalMS: 25}},
			2000, 25 * time.Millisecond,
		},
		{
			"snapshot",
			policy.Snapshot{Policy: policy.Policy{WritePath: policy.WritePathConfig{BatchSize: 500, FlushIntervalMS: 100}}},
			500, 100 * time.Millisecond,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlusher(nil, nil, nil, nil, nil, nil)
			f.applyWritePolicy(tc.pol)
			if f.batchSize != tc.wantBatch {
				t.Fatalf("batchSize = %d, want %d", f.batchSize, tc.wantBatch)
			}
			if f.interval != tc.wantEvery {
				t.Fatalf("interval = %v, want %v", f.interval, tc.wantEvery)
			}
		})
	}
}

// TestFlusherApplyWritePolicyFromStoreNil: a nil *policy.Store must not panic
// and must keep the built-in defaults.
func TestFlusherApplyWritePolicyFromStoreNil(t *testing.T) {
	f := newFlusher(nil, nil, nil, nil, nil, nil)
	var ps *policy.Store
	f.applyWritePolicy(ps)
	if f.batchSize != defaultFlushBatch || f.interval != defaultFlushEvery {
		t.Fatalf("nil *policy.Store changed defaults: %d %v", f.batchSize, f.interval)
	}
}

// TestFlusherPolicyPartialOverride: a policy that sets only one of the two
// knobs overrides just that knob (the document is merged onto defaults).
func TestFlusherPolicyPartialOverride(t *testing.T) {
	f := newFlusher(nil, nil, nil, nil, nil, nil)
	f.applyWritePolicy(policy.Policy{WritePath: policy.WritePathConfig{BatchSize: 2000}})
	if f.batchSize != 2000 {
		t.Fatalf("batchSize = %d, want 2000", f.batchSize)
	}
	if f.interval != defaultFlushEvery {
		t.Fatalf("interval = %v, want default %v", f.interval, defaultFlushEvery)
	}
}

// TestFlusherBatchSizeDrivesCommitCadence: with a policy batch size of 2 the
// flusher commits every 2 messages (no interval flush within the window), so
// 4 messages produce exactly 2 commits of 2 rows each.
func TestFlusherBatchSizeDrivesCommitCadence(t *testing.T) {
	msgs := &countingWriter{}
	out := make(chan *env, 64)
	ready := make(chan ReadyMsg, 64)
	f := newFlusher(msgs, nil, out, ready, NewDedup(16), nil)
	f.putAck = func(e *env) { out <- e }
	f.applyWritePolicy(policy.Policy{WritePath: policy.WritePathConfig{BatchSize: 2, FlushIntervalMS: 1000}})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go f.run(ctx)

	for i := 0; i < 4; i++ {
		ready <- ReadyMsg{Msg: store.PendingMsg{MsgID: int64(i + 1), Cid: 1, Seq: int64(i + 1),
			SenderID: 7, ClientMsgID: string(rune('a' + i)), SentAt: 1}}
	}
	// 4 acks = 2 commits fully processed.
	for i := 0; i < 4; i++ {
		<-out
	}
	if msgs.batches != 2 {
		t.Fatalf("commits = %d, want 2 (batch_size=2, 4 messages)", msgs.batches)
	}
	if msgs.rows != 4 {
		t.Fatalf("rows = %d, want 4", msgs.rows)
	}
}

// countingWriter is a MsgWriter that records commits (batch count + row total).
type countingWriter struct {
	batches int
	rows    int
}

func (c *countingWriter) AppendMessages(ctx context.Context, batch []store.PendingMsg) error {
	c.batches++
	c.rows += len(batch)
	return nil
}
func (c *countingWriter) AllocateSeq(ctx context.Context, cid int64, n int) (int64, error) {
	return 1, nil
}
func (c *countingWriter) MarkEdited(ctx context.Context, cid, msgID, senderID int64, content []byte, at int64) (*store.HotMsg, error) {
	return nil, store.ErrNotFound
}
func (c *countingWriter) MarkRecalled(ctx context.Context, cid, msgID, senderID, at int64) (*store.HotMsg, error) {
	return nil, nil
}
