package handler

import (
	"bytes"
	"net/http"
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/admin/middleware"
)

const (
	idempotencyTTL      = 5 * time.Minute
	idempotencyMaxKeys  = 10000
	idempotencyKeyLimit = 128
)

type idemEntry struct {
	done    chan struct{}
	created time.Time
	status  int
	header  http.Header
	body    []byte
}

// idempotencyCache implements Stripe-style Idempotency-Key semantics: the
// first response for (admin, method, path, key) is replayed byte-for-byte for
// 5 minutes. Concurrent duplicates wait for the first request to finish.
type idempotencyCache struct {
	mu      sync.Mutex
	entries map[string]*idemEntry
	now     func() time.Time
}

func newIdempotencyCache() *idempotencyCache {
	return &idempotencyCache{entries: map[string]*idemEntry{}, now: time.Now}
}

// SetClock is a test seam.
func (c *idempotencyCache) SetClock(now func() time.Time) {
	if now != nil {
		c.now = now
	}
}

func (c *idempotencyCache) Wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("Idempotency-Key")
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		if len(key) > idempotencyKeyLimit {
			middleware.WriteError(w, r, http.StatusBadRequest, "invalid_idempotency_key")
			return
		}
		adminID := int64(0)
		if a := middleware.IdentityFrom(r.Context()); a != nil {
			adminID = a.AdminID
		}
		full := itoa(adminID) + "|" + r.Method + "|" + r.URL.Path + "|" + key

		now := c.now()
		c.mu.Lock()
		c.gcLocked(now)
		if e, ok := c.entries[full]; ok {
			c.mu.Unlock()
			select {
			case <-e.done:
				copyHeader(w.Header(), e.header)
				if id := middleware.RequestID(r.Context()); id != "" {
					w.Header().Set("X-Request-ID", id)
				}
				w.WriteHeader(e.status)
				_, _ = w.Write(e.body)
			case <-r.Context().Done():
				middleware.WriteError(w, r, http.StatusRequestTimeout, "request_canceled")
			}
			return
		}
		e := &idemEntry{done: make(chan struct{}), created: now}
		c.entries[full] = e
		c.mu.Unlock()

		rec := &bufferedResponse{header: http.Header{}, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		e.status = rec.status
		e.header = rec.header
		e.body = rec.body.Bytes()
		close(e.done)

		// Do not cache server errors: a retry with the same key may succeed.
		if rec.status >= 500 {
			c.mu.Lock()
			delete(c.entries, full)
			c.mu.Unlock()
		}
		copyHeader(w.Header(), rec.header)
		if id := middleware.RequestID(r.Context()); id != "" {
			w.Header().Set("X-Request-ID", id)
		}
		w.WriteHeader(rec.status)
		_, _ = w.Write(rec.body.Bytes())
	})
}

func (c *idempotencyCache) gcLocked(now time.Time) {
	for k, e := range c.entries {
		select {
		case <-e.done:
			if now.Sub(e.created) > idempotencyTTL {
				delete(c.entries, k)
			}
		default:
			// in-flight: keep
		}
	}
	if len(c.entries) <= idempotencyMaxKeys {
		return
	}
	var oldestKey string
	var oldest time.Time
	for k, e := range c.entries {
		if oldestKey == "" || e.created.Before(oldest) {
			oldestKey, oldest = k, e.created
		}
	}
	if oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// bufferedResponse captures a handler response for replay.
type bufferedResponse struct {
	header http.Header
	status int
	wrote  bool
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }

func (b *bufferedResponse) WriteHeader(status int) {
	if b.wrote {
		return
	}
	b.status = status
	b.wrote = true
}

func (b *bufferedResponse) Write(p []byte) (int, error) {
	if !b.wrote {
		b.status = http.StatusOK
		b.wrote = true
	}
	return b.body.Write(p)
}

func copyHeader(dst, src http.Header) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var buf [20]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
