package middleware

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/yingo-server/UFOT/server/internal/admin/identity"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxIdentity
)

// WithRequestID stores a request id in the context.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxRequestID, id)
}

// RequestID returns the request id, or "" when absent.
func RequestID(ctx context.Context) string {
	if v, ok := ctx.Value(ctxRequestID).(string); ok {
		return v
	}
	return ""
}

// NewRequestID returns a 128-bit hex request id.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req-unknown"
	}
	return hex.EncodeToString(b[:])
}

// WithIdentity stores the authenticated admin principal.
func WithIdentity(ctx context.Context, a *identity.Admin) context.Context {
	return context.WithValue(ctx, ctxIdentity, a)
}

// IdentityFrom returns the authenticated admin, or nil.
func IdentityFrom(ctx context.Context) *identity.Admin {
	if v, ok := ctx.Value(ctxIdentity).(*identity.Admin); ok {
		return v
	}
	return nil
}

// ErrorBody is the single admin-plane error shape: {code, reason, request_id}.
type ErrorBody struct {
	Code      int    `json:"code"`
	Reason    string `json:"reason"`
	RequestID string `json:"request_id"`
}

// WriteError renders the unified error body. HTTP status and internal frame
// error code share the same number (docs §8: one source of truth).
func WriteError(w http.ResponseWriter, r *http.Request, status int, reason string) {
	body := ErrorBody{Code: status, Reason: reason, RequestID: RequestID(r.Context())}
	if body.RequestID == "" {
		body.RequestID = NewRequestID()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Request-ID", body.RequestID)
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// WriteJSON renders a JSON response and sets the request id header.
func WriteJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	if id := RequestID(r.Context()); id != "" {
		w.Header().Set("X-Request-ID", id)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// TokenFromRequest extracts an admin session token from Authorization: Bearer
// or the admin session cookie. The user-plane cookie/JWT is never consulted.
func TokenFromRequest(r *http.Request, cookieName string) string {
	if h := r.Header.Get("Authorization"); h != "" {
		const prefix = "Bearer "
		if len(h) > len(prefix) && strings.EqualFold(h[:len(prefix)], prefix) {
			return strings.TrimSpace(h[len(prefix):])
		}
	}
	if c, err := r.Cookie(cookieName); err == nil {
		return c.Value
	}
	return ""
}

// HasBearer reports whether the request authenticates with an Authorization
// header rather than a cookie (CSRF is a cookie-only attack vector).
func HasBearer(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	return len(h) > 7 && strings.EqualFold(h[:7], "Bearer ")
}
