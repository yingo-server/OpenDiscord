package authsvc

import (
	"net/http"
	"sync"
	"time"
)

// The login-method surface switches and the public discovery endpoint.
//
// Contract (client-visible):
//
//	GET /api/auth/methods -> 200 {"methods":{"phone":b,"email":b,"password":b,"guest":b}}
//
// The endpoint is UNAUTHENTICATED (a client must be able to ask before it has
// a session) but it is NOT unlimited: it is rate limited per source IP
// exactly like the login entry points, because an unlimited public probe is
// a free way to fingerprint the deployment.
//
// A disabled login method answers 403 {"error":"...","code":"METHOD_DISABLED"}.

// methodState is the resolved switch set.
func (s *Service) methodState() MethodFlags {
	if s.cfg.Methods == nil {
		return AllMethodsEnabled()
	}
	return s.cfg.Methods()
}

// errMethodDisabled is the rejection for a switched-off login method.
func errMethodDisabled(method string) *apiError {
	return &apiError{
		Status:  http.StatusForbidden,
		Code:    "METHOD_DISABLED",
		Message: method + " login is disabled",
	}
}

// methodAllowed reports whether the named login method is currently enabled.
// The name is one of "phone", "email", "password", "guest".
func (s *Service) methodAllowed(method string) bool {
	m := s.methodState()
	switch method {
	case "phone":
		return m.Phone
	case "email":
		return m.Email
	case "password":
		return m.Password
	case "guest":
		return m.Guest
	default:
		return false
	}
}

// methodsResponse is the discovery payload.
type methodsResponse struct {
	Methods MethodFlagsJSON `json:"methods"`
}

// MethodFlagsJSON is the JSON shape of the switch set.
type MethodFlagsJSON struct {
	Phone    bool `json:"phone"`
	Email    bool `json:"email"`
	Password bool `json:"password"`
	Guest    bool `json:"guest"`
}

func toJSONFlags(m MethodFlags) MethodFlagsJSON {
	return MethodFlagsJSON{
		Phone:    m.Phone,
		Email:    m.Email,
		Password: m.Password,
		Guest:    m.Guest,
	}
}

// handleMethods serves the public login-method discovery endpoint.
//
// GET /api/auth/methods
//
//	200 {"methods":{"phone":false,"email":false,"password":true,"guest":true}}
//	429 {"error":"too many requests","code":"RATE_LIMITED"} (+ retry_after_ms)
func (s *Service) handleMethods(w http.ResponseWriter, r *http.Request) *apiError {
	if !s.methodsLimiter.Allow(s.clientIP(r), s.cfg.Now(), s.probeBudget()) {
		return errRateLimited("RATE_LIMITED", methodsRateWindow)
	}
	s.writeJSON(w, http.StatusOK, methodsResponse{Methods: toJSONFlags(s.methodState())})
	return nil
}

// methodsRateWindow is the per-IP rate limit window for the discovery
// endpoint: 60 requests per minute per source IP.
const methodsRateWindow = time.Minute

// methodsMaxPerWindow is the built-in per-IP request budget inside the
// window. The DEPLOYMENT value comes from policy.json
// (auth.methods_probe_per_ip_per_min) and overrides this default.
const methodsMaxPerWindow = 60

// probeBudget resolves the effective per-IP budget for the discovery
// endpoint (policy wins; the built-in default is the fallback).
func (s *Service) probeBudget() int {
	if s.cfg.MethodsProbeBudget == nil {
		return methodsMaxPerWindow
	}
	if n := s.cfg.MethodsProbeBudget(); n > 0 {
		return n
	}
	return methodsMaxPerWindow
}

// ipLimiter is a small fixed-window per-IP counter. It is deliberately local
// to the discovery endpoint: it must not share state with the OTP/login
// budgets (which are keyed by HMAC'd identifiers in the store).
type ipLimiter struct {
	mu     sync.Mutex
	hits   map[string]ipWindow
	window time.Duration
}

func newIPLimiter(window time.Duration) *ipLimiter {
	return &ipLimiter{hits: make(map[string]ipWindow), window: window}
}

// Allow reports whether the key may make one more request in the current
// window. Expired windows are reset lazily on the next hit for that key.
//
// limit is passed per call (not fixed at construction) so a policy reload can
// change the budget without a restart.
func (l *ipLimiter) Allow(key string, now time.Time, limit int) bool {
	if key == "" {
		key = "unknown"
	}
	if limit <= 0 {
		limit = methodsMaxPerWindow
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry, ok := l.hits[key]
	if !ok || now.Sub(entry.start) >= l.window {
		l.hits[key] = ipWindow{count: 1, start: now}
		l.pruneLocked(now)
		return true
	}
	if entry.count >= limit {
		return false
	}
	entry.count++
	l.hits[key] = entry
	return true
}

// pruneLocked drops expired windows so the map cannot grow without bound.
// Called under the lock.
func (l *ipLimiter) pruneLocked(now time.Time) {
	if len(l.hits) < 4096 {
		return
	}
	for k, v := range l.hits {
		if now.Sub(v.start) >= l.window {
			delete(l.hits, k)
		}
	}
}

// ipWindow is one fixed window's state.
type ipWindow struct {
	count int
	start time.Time
}
