package middleware

import (
	"net/http"
	"strconv"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/log"
)

// RequestRecorder is the metrics seam for reqlog (implemented by
// service.Metrics).
type RequestRecorder interface {
	ObserveRequest(method string, status int)
}

// statusWriter captures the response status without altering behavior.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	n, err := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, err
}

// Unwrap exposes the underlying writer to http.ResponseController.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// RequestLog assigns/propagates a request id and emits one structured access
// log line per request. It deliberately logs neither query strings (search
// terms are user content) nor headers/bodies (tokens/passwords/PII red line).
func RequestLog(logger *log.Logger, rec RequestRecorder) func(http.Handler) http.Handler {
	if logger == nil {
		logger = log.For("admin.reqlog")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			id := r.Header.Get("X-Request-ID")
			if id == "" || len(id) > 128 {
				id = NewRequestID()
			}
			ctx := WithRequestID(r.Context(), id)
			r = r.WithContext(ctx)
			sw := &statusWriter{ResponseWriter: w}
			start := time.Now()
			next.ServeHTTP(sw, r)
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			dur := time.Since(start)
			adminID := int64(0)
			if a := IdentityFrom(ctx); a != nil {
				adminID = a.AdminID
			}
			logger.Info("admin request",
				"request_id", id,
				"method", r.Method,
				"path", r.URL.Path,
				"status", sw.status,
				"bytes", sw.bytes,
				"duration_ms", dur.Milliseconds(),
				"admin_id", adminID,
			)
			if rec != nil {
				rec.ObserveRequest(r.Method, sw.status)
			}
		})
	}
}

// Recover converts a panic into a 500 error body and logs it. No stack detail
// is sent to the client.
func Recover(logger *log.Logger) func(http.Handler) http.Handler {
	if logger == nil {
		logger = log.For("admin.reqlog")
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if v := recover(); v != nil {
					logger.Error("panic recovered", "request_id", RequestID(r.Context()), "err", strconv.Quote(toStr(v)))
					WriteError(w, r, http.StatusInternalServerError, "internal_error")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func toStr(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if e, ok := v.(error); ok {
		return e.Error()
	}
	return "panic"
}
