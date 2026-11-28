package diagnostics

// turn_test.go: OPS-2 TURN self-check tests — parse, TCP ok, UDP timeout,
// garbage URL (docs/lts-1.4-remaining.md §3.5).

import (
	"context"
	"net"
	"testing"
	"time"
)

func TestParseTURNUri(t *testing.T) {
	cases := []struct {
		in        string
		transport string
		addr      string
		wantErr   bool
	}{
		{"turn:turn.example.com:3478?transport=udp", "udp", "turn.example.com:3478", false},
		{"turn:turn.example.com:3478?transport=tcp", "tcp", "turn.example.com:3478", false},
		{"turn:turn.example.com", "udp", "turn.example.com:3478", false},
		{"turns:turn.example.com", "tcp", "turn.example.com:5349", false},
		{"turn:10.0.0.1:3478", "udp", "10.0.0.1:3478", false},
		{"", "", "", true},
		{"http://x", "", "", true},
		{"turn::3478", "", "", true},
		{"turn:h:notaport", "", "", true},
	}
	for _, c := range cases {
		tr, addr, err := ParseTURN(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("ParseTURN(%q): want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseTURN(%q): %v", c.in, err)
			continue
		}
		if tr != c.transport || addr != c.addr {
			t.Errorf("ParseTURN(%q) = %s %s, want %s %s", c.in, tr, addr, c.transport, c.addr)
		}
	}
}

func TestProbeTURNTCPOK(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listen: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	res := ProbeTURN(context.Background(), "turn:"+ln.Addr().String()+"?transport=tcp")
	if !res.OK {
		t.Fatalf("tcp probe should succeed: %+v", res)
	}
}

func TestProbeTURNTCPUnreachable(t *testing.T) {
	res := probeTURNTimeout(context.Background(), "turn:127.0.0.1:1?transport=tcp", time.Second)
	if res.OK {
		t.Fatal("tcp probe to closed port must fail")
	}
}

func TestProbeTURNTimeout(t *testing.T) {
	// UDP to a black hole: bound port that never replies.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no udp bind: %v", err)
	}
	addr := pc.LocalAddr().String()
	pc.Close()
	start := time.Now()
	res := probeTURNTimeout(context.Background(), "turn:"+addr, 300*time.Millisecond)
	if res.OK {
		t.Fatal("silent UDP endpoint must fail")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("budget blown: %v", d)
	}
}

func TestProbeTURNBadURL(t *testing.T) {
	res := ProbeTURN(context.Background(), "not-a-turn-uri")
	if res.OK || res.Err() == nil {
		t.Fatal("bad url must fail cleanly")
	}
}
