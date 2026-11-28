package limit

import (
	"testing"
	"time"

	"github.com/yingo-server/UFOT/server/internal/proto/kinds"
)

// TestTypingPayloadCapEnforced: the four "0 B / no payload" kinds (TYPING and
// PRESENCE_*) must reject an oversize payload. With the explicit PayloadCap
// (64B) a 1KB typing frame is dropped instead of slipping past the old
// "MaxPayload==0 means unlimited" bug.
func TestTypingPayloadCapEnforced(t *testing.T) {
	l := New(DefaultConfig())
	l.AddConn(1, 1)
	now := time.Unix(100, 0)

	if v := l.AllowAt(1, 1, uint32(kinds.KindTyping), 1024, now); v != Drop {
		t.Fatalf("1KB typing = %v, want Drop", v)
	}
	if v := l.AllowAt(1, 1, uint32(kinds.KindPresenceUpdate), 1024, now); v != Drop {
		t.Fatalf("1KB presence = %v, want Drop", v)
	}
	if v := l.AllowAt(1, 1, uint32(kinds.KindPresenceOnline), 1024, now); v != Drop {
		t.Fatalf("1KB presence-sub = %v, want Drop", v)
	}
	// A legitimate small payload ({"conv_id":N} ~ 20B) still passes.
	if v := l.AllowAt(1, 1, uint32(kinds.KindTyping), 20, now); v != OK {
		t.Fatalf("small typing = %v, want OK", v)
	}
}

// TestCallSignalingRateAllowsBurst: the 350-354 call-signaling rules (40/s,
// burst 60) must let a 60-frame burst through so WebRTC SDP+ICE setup is not
// throttled (the old DefaultRule 10/s burst 20 would NACK mid-setup).
func TestCallSignalingRateAllowsBurst(t *testing.T) {
	l := New(DefaultConfig())
	l.AddConn(1, 77)
	now := time.Unix(200, 0)
	kind := uint32(kinds.KindCallOffer)
	for i := 0; i < 60; i++ {
		if v := l.AllowAt(1, 77, kind, 2000, now); v != OK {
			t.Fatalf("call-signal frame %d = %v, want OK (burst 60)", i, v)
		}
	}
}

// TestL3PenaltyEscalates: three consecutive over-limit windows → user budget
// halved (security-spec §1.2.1); ten consecutive → Kill (disconnect all).
func TestL3PenaltyEscalates(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Rules = map[uint32]Rule{101: {Rate: 1, Burst: 1, Capacity: 1}}
	cfg.DefaultRule = cfg.Rules[101]
	cfg.KillMinFrames = 1 << 30 // disable connection-level L4, isolate L3
	cfg.KillWindow = 60 * time.Second
	cfg.UserHalfWindows = 3
	cfg.UserKillWindows = 10
	l := New(cfg)
	l.AddConn(1, 900)
	base := time.Unix(60_000, 0)

	// One over-limit window: first frame OK, second over.
	win := func(w int) {
		at := base.Add(time.Duration(w) * 60 * time.Second)
		l.AllowAt(1, 900, 101, 0, at)
		l.AllowAt(1, 900, 101, 0, at)
	}

	// Drive windows 0..3: entering window 3 finalises the 3rd over window.
	for w := 0; w <= 3; w++ {
		win(w)
	}
	u := l.users[900]
	if u == nil {
		t.Fatal("user state missing")
	}
	if u.overWindows < 3 {
		t.Fatalf("overWindows = %d, want >= 3", u.overWindows)
	}
	if u.penaltyUntil.IsZero() {
		t.Fatal("half-budget penalty not armed after 3 over windows")
	}
	// Budget halved: user multiplier must be 0.5 during the penalty.
	if m := l.userMultiplier(900, l.cfg.Load(), base.Add(4*60*time.Second)); !near(m, 0.5) {
		t.Fatalf("penalty multiplier = %v, want 0.5", m)
	}

	// Drive windows 4..10: entering window 10 finalises the 10th → Kill.
	var vKill Verdict
	for w := 4; w <= 10; w++ {
		at := base.Add(time.Duration(w) * 60 * time.Second)
		v := l.AllowAt(1, 900, 101, 0, at)
		l.AllowAt(1, 900, 101, 0, at)
		if w == 10 {
			vKill = v
		}
	}
	if vKill != Kill {
		t.Fatalf("verdict at window 10 = %v, want Kill", vKill)
	}
	u = l.users[900]
	if u.overWindows < 10 {
		t.Fatalf("overWindows = %d, want >= 10", u.overWindows)
	}
	if u.killUntil.IsZero() {
		t.Fatal("kill penalty not armed after 10 over windows")
	}
}
