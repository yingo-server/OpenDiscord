package service

import (
	"fmt"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/bridgeproxy"
)

// Metrics is a hand-written Prometheus exposition source (no prometheus
// dependency by design — the admin surface only needs a handful of counters).
type Metrics struct {
	mu       sync.Mutex
	requests map[string]uint64 // method|status -> count
	started  time.Time

	bridge        func() bridgeproxy.Stats
	auditFailures func() uint64
	loginStats    func() (failures, lockouts uint64)
	opHits        func() uint64
}

// NewMetrics builds an empty registry.
func NewMetrics() *Metrics {
	return &Metrics{requests: map[string]uint64{}, started: time.Now()}
}

// SetBridgeStats wires the bridge proxy snapshot.
func (m *Metrics) SetBridgeStats(fn func() bridgeproxy.Stats) { m.bridge = fn }

// SetAuditFailures wires the audit shim failure counter.
func (m *Metrics) SetAuditFailures(fn func() uint64) { m.auditFailures = fn }

// SetLoginStats wires the login limiter counters.
func (m *Metrics) SetLoginStats(fn func() (uint64, uint64)) { m.loginStats = fn }

// SetOpLimiterHits wires the operation-limiter rejection counter.
func (m *Metrics) SetOpLimiterHits(fn func() uint64) { m.opHits = fn }

// ObserveRequest records one completed HTTP request (reqlog seam).
func (m *Metrics) ObserveRequest(method string, status int) {
	switch method {
	case "GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS":
	default:
		method = "OTHER"
	}
	key := method + "|" + itoa(int64(status))
	m.mu.Lock()
	m.requests[key]++
	m.mu.Unlock()
}

// Render produces Prometheus text exposition format 0.0.4.
func (m *Metrics) Render() string {
	var b strings.Builder
	b.WriteString("# HELP ufot_admin_build_info Admin API build information.\n")
	b.WriteString("# TYPE ufot_admin_build_info gauge\n")
	b.WriteString("ufot_admin_build_info{version=\"dev\"} 1\n")
	b.WriteString("# HELP ufot_admin_uptime_seconds Seconds since the admin server started.\n")
	b.WriteString("# TYPE ufot_admin_uptime_seconds gauge\n")
	fmt.Fprintf(&b, "ufot_admin_uptime_seconds %.0f\n", time.Since(m.started).Seconds())
	b.WriteString("# HELP go_goroutines Number of goroutines.\n")
	b.WriteString("# TYPE go_goroutines gauge\n")
	fmt.Fprintf(&b, "go_goroutines %d\n", runtime.NumGoroutine())

	b.WriteString("# HELP ufot_admin_http_requests_total Admin HTTP requests by method and status.\n")
	b.WriteString("# TYPE ufot_admin_http_requests_total counter\n")
	m.mu.Lock()
	keys := make([]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.SplitN(k, "|", 2)
		fmt.Fprintf(&b, "ufot_admin_http_requests_total{method=%q,status=%q} %d\n", parts[0], parts[1], m.requests[k])
	}
	m.mu.Unlock()

	if m.loginStats != nil {
		f, l := m.loginStats()
		b.WriteString("# HELP ufot_admin_login_failures_total Failed admin login attempts.\n")
		b.WriteString("# TYPE ufot_admin_login_failures_total counter\n")
		fmt.Fprintf(&b, "ufot_admin_login_failures_total %d\n", f)
		b.WriteString("# HELP ufot_admin_login_lockouts_total Login lockout events.\n")
		b.WriteString("# TYPE ufot_admin_login_lockouts_total counter\n")
		fmt.Fprintf(&b, "ufot_admin_login_lockouts_total %d\n", l)
	}
	if m.opHits != nil {
		b.WriteString("# HELP ufot_admin_ratelimit_hits_total Management operations rejected by the rate limiter.\n")
		b.WriteString("# TYPE ufot_admin_ratelimit_hits_total counter\n")
		fmt.Fprintf(&b, "ufot_admin_ratelimit_hits_total %d\n", m.opHits())
	}
	if m.auditFailures != nil {
		b.WriteString("# HELP ufot_admin_audit_append_failures_total Audit appends that failed after the primary operation succeeded.\n")
		b.WriteString("# TYPE ufot_admin_audit_append_failures_total counter\n")
		fmt.Fprintf(&b, "ufot_admin_audit_append_failures_total %d\n", m.auditFailures())
	}
	if m.bridge != nil {
		st := m.bridge()
		b.WriteString("# HELP ufot_admin_bridge_requests_total Logical injection requests.\n")
		b.WriteString("# TYPE ufot_admin_bridge_requests_total counter\n")
		fmt.Fprintf(&b, "ufot_admin_bridge_requests_total %d\n", st.Requests)
		b.WriteString("# HELP ufot_admin_bridge_attempts_total HTTP attempts including retries.\n")
		b.WriteString("# TYPE ufot_admin_bridge_attempts_total counter\n")
		fmt.Fprintf(&b, "ufot_admin_bridge_attempts_total %d\n", st.Attempts)
		b.WriteString("# HELP ufot_admin_bridge_failures_total Logical injection failures.\n")
		b.WriteString("# TYPE ufot_admin_bridge_failures_total counter\n")
		fmt.Fprintf(&b, "ufot_admin_bridge_failures_total %d\n", st.Failures)
		b.WriteString("# HELP ufot_admin_bridge_retries_total Retried HTTP attempts.\n")
		b.WriteString("# TYPE ufot_admin_bridge_retries_total counter\n")
		fmt.Fprintf(&b, "ufot_admin_bridge_retries_total %d\n", st.Retries)
		b.WriteString("# HELP ufot_admin_bridge_circuit_open Circuit breaker state (1=open).\n")
		b.WriteString("# TYPE ufot_admin_bridge_circuit_open gauge\n")
		open := 0
		if st.CircuitOpen {
			open = 1
		}
		fmt.Fprintf(&b, "ufot_admin_bridge_circuit_open %d\n", open)
		b.WriteString("# HELP ufot_admin_bridge_circuit_opened_total Times the breaker opened.\n")
		b.WriteString("# TYPE ufot_admin_bridge_circuit_opened_total counter\n")
		fmt.Fprintf(&b, "ufot_admin_bridge_circuit_opened_total %d\n", st.CircuitOpened)
	}
	return b.String()
}
