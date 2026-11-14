// pullcap_test.go —— 旧客户端能力协商的分流测试
// （lts-1.4-acceptance §B.2/B.4/B.5）。
//
// 场景（universal_pull_enabled=true）：
//   - 有能力的 uid → 水位线 kind 14；
//   - 没上报能力的老客户端 uid → 仍然 inline 102（绝不漏消息）；
//   - 同一 uid 不会同时收到两种帧（无双投递窗口）。
//
// 回滚（universal_pull_enabled=false）：能力探针根本不被调用，行为与
// v1.2.1 逐字节一致（S-7）。
package pipe

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/pkg/featureflags"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// TestPullCapUniversalSplit 普遍拉取开启后的能力分流。
func TestPullCapUniversalSplit(t *testing.T) {
	const members = 6 // uid 1 = 发送者，uid 2..6 = 收件人

	t.Run("all capable", func(t *testing.T) {
		r := newPullRig(t, members)
		r.flags.Apply(map[string]any{featureflags.KeyUniversalPullEnabled: true})
		r.p.SetFeatureFlags(r.flags)
		r.p.SetPullCapability(func(int64) bool { return true })

		r.send(t, "cap-all")
		frames := collectFan(t, r.p, 120*time.Millisecond)
		if got := countKind(frames, uint32(kinds.KindMsgNew)); got != 0 {
			t.Fatalf("inline MSG_NEW = %d, want 0 (all recipients are pull-capable)", got)
		}
		if got := countKind(frames, uint32(kinds.KindWatermark)); got != members-1 {
			t.Fatalf("watermark = %d, want %d (one per recipient)", got, members-1)
		}
	})

	t.Run("no capable recipient", func(t *testing.T) {
		r := newPullRig(t, members)
		r.flags.Apply(map[string]any{featureflags.KeyUniversalPullEnabled: true})
		r.p.SetFeatureFlags(r.flags)
		r.p.SetPullCapability(func(int64) bool { return false })

		r.send(t, "cap-none")
		frames := collectFan(t, r.p, 120*time.Millisecond)
		if got := countKind(frames, uint32(kinds.KindMsgNew)); got != members-1 {
			t.Fatalf("inline MSG_NEW = %d, want %d (old clients must still get the message)",
				got, members-1)
		}
		if got := countKind(frames, uint32(kinds.KindWatermark)); got != 0 {
			t.Fatalf("watermark = %d, want 0 (no recipient can pull)", got)
		}
	})

	t.Run("mixed recipients", func(t *testing.T) {
		r := newPullRig(t, members)
		r.flags.Apply(map[string]any{featureflags.KeyUniversalPullEnabled: true})
		r.p.SetFeatureFlags(r.flags)
		r.p.SetPullCapability(func(uid int64) bool { return uid == 3 })

		r.send(t, "cap-mixed")
		frames := collectFan(t, r.p, 120*time.Millisecond)
		wm, inline := map[uint64]int{}, map[uint64]int{}
		for _, e := range frames {
			switch e.Kind {
			case uint32(kinds.KindWatermark):
				wm[e.UserID]++
			case uint32(kinds.KindMsgNew):
				inline[e.UserID]++
			}
		}
		if len(wm) != 1 || wm[3] != 1 {
			t.Fatalf("watermark recipients = %v, want only uid 3", wm)
		}
		if len(inline) != members-2 {
			t.Fatalf("inline recipients = %v, want %d old clients", inline, members-2)
		}
		if inline[3] != 0 {
			t.Fatalf("uid 3 received BOTH a watermark and an inline frame (double delivery)")
		}
		for uid := range inline {
			if wm[uid] != 0 {
				t.Fatalf("uid %d received both frames (double delivery)", uid)
			}
		}
	})
}

// TestPullCapFlagOffIsInert flag-off（S-7 回滚基线）：能力探针不被调用，
// 行为与 v1.2.1 逐字节一致（inline 102，无水位线）。
func TestPullCapFlagOffIsInert(t *testing.T) {
	const members = 4
	r := newPullRig(t, members)
	var probed atomic.Int64
	r.p.SetPullCapability(func(int64) bool {
		probed.Add(1)
		return true
	})

	r.send(t, "flag-off")
	frames := collectFan(t, r.p, 120*time.Millisecond)
	if got := countKind(frames, uint32(kinds.KindMsgNew)); got != members-1 {
		t.Fatalf("inline MSG_NEW = %d, want %d (flag-off must stay byte-identical)",
			got, members-1)
	}
	if got := countKind(frames, uint32(kinds.KindWatermark)); got != 0 {
		t.Fatalf("watermark = %d, want 0 (flag-off emits no watermark)", got)
	}
	if n := probed.Load(); n != 0 {
		t.Fatalf("capability probe called %d times with the flag off, want 0", n)
	}
}
