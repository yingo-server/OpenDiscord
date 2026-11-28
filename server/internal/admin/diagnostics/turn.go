// Package diagnostics holds admin-facing connectivity self-checks that need
// no new dependencies (standard library net only) — OPS-2, lts-1.4-plan §5
// row 7: "通话需自备 TURN + 管理界面一键测试 TURN 连通性".
package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultTimeout is the whole-probe budget (docs/lts-1.4-remaining.md §3.3).
const DefaultTimeout = 3 * time.Second

// Result is what the admin surface renders. It never contains credentials.
type Result struct {
	OK        bool      `json:"ok"`
	Transport string    `json:"transport"` // udp | tcp
	Addr      string    `json:"addr"`      // host:port actually probed
	Detail    string    `json:"detail"`    // human-readable outcome
	Duration  int64     `json:"duration_ms"`
	CheckedAt time.Time `json:"checked_at"`
	err       error     // not serialized
}

// Err reports the failure (nil when OK). Kept out of the JSON payload.
func (r Result) Err() error { return r.err }

// ParseTURN extracts transport and host:port from a turn/turns URI
// (RFC 7064/7065): turn:host:port?transport=udp|tcp, turns:host:port.
// An omitted port defaults to 3478 (5349 for turns).
func ParseTURN(raw string) (transport, addr string, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", errors.New("turn url empty")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse turn url: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "turn" && scheme != "turns" {
		return "", "", fmt.Errorf("unsupported scheme %q (want turn/turns)", u.Scheme)
	}
	host := u.Host
	if host == "" {
		// "turn:host:port" is accepted by url.Parse as Opaque sometimes.
		host = u.Opaque
	}
	h, port, perr := net.SplitHostPort(host)
	if perr != nil {
		h = host
		if scheme == "turns" {
			port = "5349"
		} else {
			port = "3478"
		}
	}
	if h == "" {
		return "", "", errors.New("turn url missing host")
	}
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", fmt.Errorf("turn url bad port %q", port)
	}
	transport = u.Query().Get("transport")
	if transport == "" {
		if scheme == "turns" {
			transport = "tcp"
		} else {
			transport = "udp" // RFC 7065 default
		}
	}
	transport = strings.ToLower(transport)
	if transport != "udp" && transport != "tcp" {
		return "", "", fmt.Errorf("unsupported transport %q", transport)
	}
	return transport, net.JoinHostPort(h, port), nil
}

// ProbeTURN probes the configured TURN URI once. TCP: a successful dial
// proves reachability. UDP: dial alone proves nothing (connectionless), so a
// minimal STUN binding request (RFC 5389 §6, magic cookie) is sent and any
// reply — even an error response — proves a live TURN service. The 3s budget
// covers the whole probe.
func ProbeTURN(ctx context.Context, raw string) Result {
	return probeTURNTimeout(ctx, raw, DefaultTimeout)
}

func probeTURNTimeout(ctx context.Context, raw string, budget time.Duration) Result {
	start := time.Now()
	res := Result{CheckedAt: start.UTC()}
	fail := func(err error) Result {
		res.Duration = time.Since(start).Milliseconds()
		res.Detail = err.Error()
		res.err = err
		return res
	}

	transport, addr, err := ParseTURN(raw)
	if err != nil {
		res.Transport = "unknown"
		return fail(err)
	}
	res.Transport = transport
	res.Addr = addr

	dialer := &net.Dialer{Timeout: budget}
	cctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	conn, err := dialer.DialContext(cctx, transport, addr)
	if err != nil {
		return fail(fmt.Errorf("dial %s %s: %w", transport, addr, err))
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(budget))

	if transport == "tcp" {
		// Reachable: TCP connect succeeded on the TURN port.
		res.OK = true
		res.Detail = "tcp connect ok"
		res.Duration = time.Since(start).Milliseconds()
		return res
	}

	// UDP: STUN Binding Request, magic cookie, zero transaction id bytes is
	// fine for a liveness check (we only need ANY valid reply).
	stun := []byte{
		0x00, 0x01, 0x00, 0x00,
		0x21, 0x12, 0xA4, 0x42,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}
	if _, err := conn.Write(stun); err != nil {
		return fail(fmt.Errorf("udp write %s: %w", addr, err))
	}
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil {
		return fail(fmt.Errorf("udp probe %s: no reply within budget (%w)", addr, err))
	}
	if n < 20 || buf[0] != 0x01 && buf[0] != 0x11 { // Binding Success/Error Response
		return fail(fmt.Errorf("udp probe %s: non-STUN reply", addr))
	}
	res.OK = true
	res.Detail = "udp stun reply ok"
	res.Duration = time.Since(start).Milliseconds()
	return res
}
