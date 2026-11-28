// Package cors implements the user-facing CORS policy.
//
// The deployment is BACKEND-ONLY: the user interface is a separate program
// that points at this service through its own configuration, so a browser
// client routinely runs on a different origin than the API.
//
// The policy lives in ONE place and is applied at the ASSEMBLY layer, so
// every user-facing HTTP surface (auth, media, push, the /api fallback)
// answers with the same headers. A per-service CORS implementation is how
// the cross-origin surface silently rots: two of the three endpoints forgot
// it and the browser simply refused the request.
//
// Modes:
//
//	allow_any  — reflect any valid http(s) Origin (default)
//	allow_list — only PublicOrigin + the configured extra origins
//	off        — no CORS headers at all
package cors

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Mode constants.
const (
	ModeAllowAny  = "allow_any"
	ModeAllowList = "allow_list"
	ModeOff       = "off"
)

// Policy is the resolved CORS configuration.
type Policy struct {
	// Mode selects the policy (see the Mode* constants).
	Mode string
	// PublicOrigin is the canonical origin of the service (always part of
	// the allow_list).
	PublicOrigin string
	// Origins are extra origins allowed in allow_list mode.
	Origins []string
}

// ErrUnknownMode is returned by New when the configured mode is not one of
// the known values. A security switch must not silently fall back to the
// most permissive mode on a typo: an operator who writes "of" instead of
// "off" must see the mistake at startup, not discover it via a breach.
var ErrUnknownMode = fmt.Errorf("cors: unknown mode (known: %q, %q, %q; empty = %q)",
	ModeAllowAny, ModeAllowList, ModeOff, ModeAllowAny)

// New resolves a policy from the raw configuration values. An empty mode
// means allow_any (the standalone-client default). An UNKNOWN mode is an
// error, not a fallback.
func New(mode, publicOrigin string, origins []string) (Policy, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "":
		mode = ModeAllowAny
	case ModeAllowAny:
		mode = ModeAllowAny
	case ModeAllowList:
		mode = ModeAllowList
	case ModeOff:
		mode = ModeOff
	default:
		return Policy{}, fmt.Errorf("%w: got %q", ErrUnknownMode, mode)
	}
	return Policy{Mode: mode, PublicOrigin: publicOrigin, Origins: origins}, nil
}

// Allows reports whether the given Origin may receive CORS headers.
func (p Policy) Allows(origin string) bool {
	if origin == "" {
		return false
	}
	switch p.Mode {
	case ModeOff:
		return false
	case ModeAllowList:
		if p.PublicOrigin != "" && strings.EqualFold(origin, p.PublicOrigin) {
			return true
		}
		for _, o := range p.Origins {
			if o != "" && strings.EqualFold(origin, o) {
				return true
			}
		}
		return false
	default:
		return validHTTPOrigin(origin)
	}
}

// Apply writes the CORS headers for an allowed origin. It reports whether
// the origin was allowed (and therefore answered).
func (p Policy) Apply(w http.ResponseWriter, origin string) bool {
	if !p.Allows(origin) {
		return false
	}
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
	h.Set("Access-Control-Max-Age", "600")
	h.Set("Vary", "Origin")
	return true
}

// Middleware applies the policy to every response and answers CORS
// preflights for the API surface.
//
// Preflight handling: an OPTIONS request to an /api/ path is answered here
// with 204 and the policy headers. Without it the method-scoped routers
// (media/push) answer 405, and the browser reports the request as failed
// without ever reaching the real handler.
func (p Policy) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		allowed := p.Apply(w, origin)
		if r.Method == http.MethodOptions && strings.HasPrefix(r.URL.Path, "/api/") {
			if !allowed {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validHTTPOrigin reports whether s is a syntactically valid http(s) origin.
func validHTTPOrigin(s string) bool {
	u, err := url.Parse(s)
	if err != nil || u.Host == "" {
		return false
	}
	return u.Scheme == "http" || u.Scheme == "https"
}
