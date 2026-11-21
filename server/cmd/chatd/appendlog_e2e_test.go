// appendlog_e2e_test.go — V3 wiring proof: with appendlog_dir configured,
// the assembly boots and a real MSG_SEND round-trip succeeds through the
// hybrid store (messages in append-only log, other domains in SQLite).
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yingo-server/OpenDiscord/server/internal/pkg/log"
	"github.com/yingo-server/OpenDiscord/server/internal/proto/kinds"
)

// startAppendlogAssembly is startAssembly with appendlog wired in.
func startAppendlogAssembly(t *testing.T) (*assembly, context.CancelFunc) {
	t.Helper()
	log.SetLevel(log.ErrorLevel)
	cfg := testConfig(t)
	cfg.AppendlogDir = filepath.Join(filepath.Dir(cfg.HotDB), "appendlog")
	cfg.AppendlogShards = 2
	if err := os.MkdirAll(cfg.AppendlogDir, 0o700); err != nil {
		t.Fatal(err)
	}
	a, err := newAssembly(cfg, log.For("chatd-aplog-test"))
	if err != nil {
		t.Fatalf("newAssembly: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if addr := a.user.Addr(); addr != "" && !strings.HasSuffix(addr, ":0") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.HasSuffix(a.user.Addr(), ":0") {
		cancel()
		t.Fatalf("usergw did not start (addr=%q)", a.user.Addr())
	}
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Errorf("assembly did not stop")
		}
		a.close()
	})
	return a, cancel
}

func TestAppendlogWiringBootsAndServes(t *testing.T) {
	a, _ := startAppendlogAssembly(t)
	base := "http://" + a.user.Addr()
	alice := loginGuest(t, base, "alice-aplog")
	bob := loginGuest(t, base, "bob-aplog")
	ca := dialToken(t, a.user.Addr(), alice.AccessToken)
	cb := dialToken(t, a.user.Addr(), bob.AccessToken)
	defer ca.Close()
	defer cb.Close()
	cid := createDirect(t, ca, alice, bob)

	_, _ = sendMsg(t, ca, cid, &alice, "aplog-1", "hello appendlog")
	// Bob receives MSG_NEW via fan-out (the message was committed to the
	// append-only log; this proves the hybrid store is on the live path).
	env, _ := readKind(t, cb, kinds.KindMsgNew, 8*time.Second)
	var msg struct {
		Content struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(env.Payload, &msg); err != nil {
		t.Fatalf("decode MSG_NEW: %v", err)
	}
	if msg.Content.Text != "hello appendlog" {
		t.Fatalf("text = %q, want hello appendlog", msg.Content.Text)
	}
}
