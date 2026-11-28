// Package pushsvc implements the server half of mobile system push (Web Push
// / VAPID, RFC 8291 + RFC 8188).
//
// A browser client already speaks this
// contract:
//
//	POST /api/push/subscribe
//	  Cookie: access_token=...          (same auth contract as authsvc/media)
//	  body:   {"subscription": PushSubscriptionJSON}
//	  → 204  subscription registered
//	  → 401/400/500  fail-closed on the client (it only logs, never retries)
//
// The service keeps registered subscriptions in memory (map[uid][]Subscription
// behind a mutex) — no DB, no schema change: push state is best-effort and
// losing it on restart only means the browser re-subscribes.
//
// Fail-closed rules (server side):
//   - every handler is total: no panic escapes, malformed input is a 400;
//   - a push delivery failure never breaks the caller's flow: it is logged
//     and the dead subscription is dropped;
//   - without VAPID_PRIVATE_KEY / VAPID_SUBJECT the Sender degrades to a
//     no-op (subscriptions are still registered).
package pushsvc

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/yingo-server/UFOT/server/internal/pkg/log"
	"github.com/yingo-server/UFOT/server/internal/user/core/authn"
)

// Subscription is one registered push subscription (PushSubscriptionJSON).
type Subscription struct {
	// Endpoint is the browser push service URL (https only).
	Endpoint string
	// P256DH is the base64url encoded uncompressed P-256 public key (65 bytes).
	P256DH string
	// Auth is the base64url encoded auth secret (16 bytes).
	Auth string
}

// TokenVerifier is the access-token check surface (satisfied by
// *authn.Verifier; an interface so tests can fake it).
type TokenVerifier interface {
	Verify(token string) (*authn.Identity, error)
}

// Sender delivers a payload to one subscription. Injected so the service can
// be tested without touching the network.
type Sender interface {
	Send(sub Subscription, payload []byte) error
}

// Deps is the injected dependency set.
type Deps struct {
	Verifier TokenVerifier
	// Sender is optional: nil means "no VAPID configured" → no-op delivery.
	Sender Sender
	Logger *log.Logger
	// Now is overridable in tests.
	Now func() time.Time
}

// Service is the push HTTP handler + subscription store.
type Service struct {
	deps Deps

	mu   sync.Mutex
	subs map[int64][]Subscription
}

// New validates deps and returns the service.
func New(d Deps) (*Service, error) {
	if d.Verifier == nil {
		return nil, errors.New("pushsvc: verifier is required")
	}
	if d.Logger == nil {
		d.Logger = log.For("pushsvc")
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Sender == nil {
		d.Sender = NewNoopSender(d.Logger)
	}
	return &Service{deps: d, subs: map[int64][]Subscription{}}, nil
}

// Routes mounts on the shared user mux: `mux.Handle("/api/push/", svc.Routes())`.
func (s *Service) Routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /api/push/subscribe", s.handleSubscribe)
	m.HandleFunc("POST /api/push/test", s.handleTest)
	return m
}

// authenticate shares the mediasvc/authsvc contract: access_token HttpOnly
// cookie only (never the URL — security-spec §2.3).
func (s *Service) authenticate(r *http.Request) (*authn.Identity, bool) {
	c, err := r.Cookie("access_token")
	if err != nil || c.Value == "" {
		return nil, false
	}
	id, err := s.deps.Verifier.Verify(c.Value)
	if err != nil || id == nil || id.UserID <= 0 {
		return nil, false
	}
	return id, true
}

// subscribeBody is the wire shape the client posts.
type subscribeBody struct {
	Subscription struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
			P256DH string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	} `json:"subscription"`
}

// maxSubscribeBody caps the request body (a subscription is ~300 bytes).
const maxSubscribeBody = 8 << 10

// @authz: checked — the subscription is bound to the cookie uid.
func (s *Service) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	defer recoverHandler(w, s.deps.Logger)
	id, ok := s.authenticate(r)
	if !ok {
		s.unauthorized(w)
		return
	}
	var body subscribeBody
	dec := json.NewDecoder(io.LimitReader(r.Body, maxSubscribeBody))
	if err := dec.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid body", "code": "BAD_REQUEST"})
		return
	}
	sub, err := validateSubscription(body.Subscription.Endpoint, body.Subscription.Keys.P256DH, body.Subscription.Keys.Auth)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "code": "BAD_REQUEST"})
		return
	}
	if err := s.Add(id.UserID, sub); err != nil {
		s.deps.Logger.Error("push subscribe store failed", "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error", "code": "INTERNAL"})
		return
	}
	s.deps.Logger.Info("push subscription registered", "uid", id.UserID)
	w.WriteHeader(http.StatusNoContent)
}

// handleTest is the manual trigger entry point: it sends a test payload to
// the caller's own subscriptions. It is the operator/QA surface for the
// "subscribe works but nothing is delivered" case; it never touches other
// users and never affects the message path.
func (s *Service) handleTest(w http.ResponseWriter, r *http.Request) {
	defer recoverHandler(w, s.deps.Logger)
	id, ok := s.authenticate(r)
	if !ok {
		s.unauthorized(w)
		return
	}
	payload := []byte(`{"type":"test","title":"UFOT","body":"push test"}`)
	n := s.Notify(id.UserID, payload)
	writeJSON(w, http.StatusOK, map[string]any{"sent": n})
}

// Add registers (or refreshes) one subscription for a user. It is
// idempotent: re-subscribing with the same endpoint replaces the stored entry
// instead of duplicating it.
func (s *Service) Add(uid int64, sub Subscription) error {
	if uid <= 0 {
		return errors.New("pushsvc: invalid uid")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.subs[uid]
	for i := range list {
		if list[i].Endpoint == sub.Endpoint {
			list[i] = sub
			s.subs[uid] = list
			return nil
		}
	}
	s.subs[uid] = append(list, sub)
	return nil
}

// Clear drops every subscription of a user (logout / device revoke).
func (s *Service) Clear(uid int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subs, uid)
}

// Subscriptions returns a copy of a user's registered subscriptions.
func (s *Service) Subscriptions(uid int64) []Subscription {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Subscription, len(s.subs[uid]))
	copy(out, s.subs[uid])
	return out
}

// Notify delivers a payload to every subscription of a user. It never
// panics and never blocks the caller on a push service outage: failures are
// logged and dead (404/410) subscriptions are dropped.
//
// The returned count is the number of subscriptions the payload was handed
// to; the error is informational only (callers should ignore it — push is
// best-effort by design).
func (s *Service) Notify(uid int64, payload []byte) int {
	subs := s.Subscriptions(uid)
	if len(subs) == 0 {
		return 0
	}
	sent := 0
	for _, sub := range subs {
		err := s.deps.Sender.Send(sub, payload)
		if err == nil {
			sent++
			continue
		}
		s.deps.Logger.Warn("push send failed", "uid", uid, "err", err.Error())
		var se *StatusError
		if errors.As(err, &se) && isGone(se.Status) {
			s.remove(uid, sub.Endpoint)
		}
	}
	return sent
}

// remove drops one endpoint of a user.
func (s *Service) remove(uid int64, endpoint string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	list := s.subs[uid]
	out := list[:0]
	for _, sub := range list {
		if sub.Endpoint == endpoint {
			continue
		}
		out = append(out, sub)
	}
	if len(out) == 0 {
		delete(s.subs, uid)
		return
	}
	s.subs[uid] = out
}

// isGone reports whether a push service status means "subscription is dead".
func isGone(status int) bool {
	switch status {
	case http.StatusNotFound, http.StatusGone:
		return true
	default:
		return false
	}
}

func (s *Service) unauthorized(w http.ResponseWriter) {
	writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "code": "UNAUTHORIZED"})
}

// validateSubscription checks the browser-supplied subscription. Everything
// that is not usable is a 400 — a broken subscription must never be stored.
func validateSubscription(endpoint, p256dh, auth string) (Subscription, error) {
	endpoint = strings.TrimSpace(endpoint)
	p256dh = strings.TrimSpace(p256dh)
	auth = strings.TrimSpace(auth)
	if endpoint == "" {
		return Subscription{}, errors.New("missing endpoint")
	}
	if len(endpoint) > 2048 {
		return Subscription{}, errors.New("endpoint too long")
	}
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" {
		return Subscription{}, errors.New("invalid endpoint")
	}
	if u.Scheme != "https" {
		return Subscription{}, errors.New("endpoint must be https")
	}
	if p256dh == "" || auth == "" {
		return Subscription{}, errors.New("missing keys")
	}
	if len(p256dh) > 512 || len(auth) > 256 {
		return Subscription{}, errors.New("keys too long")
	}
	raw, err := decodeB64URL(p256dh)
	if err != nil {
		return Subscription{}, errors.New("invalid p256dh")
	}
	if len(raw) != 65 || raw[0] != 0x04 {
		return Subscription{}, errors.New("invalid p256dh")
	}
	sec, err := decodeB64URL(auth)
	if err != nil {
		return Subscription{}, errors.New("invalid auth")
	}
	if len(sec) != 16 {
		return Subscription{}, errors.New("invalid auth")
	}
	return Subscription{Endpoint: endpoint, P256DH: p256dh, Auth: auth}, nil
}

// decodeB64URL decodes base64url with or without padding.
func decodeB64URL(s string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	return base64.URLEncoding.DecodeString(s)
}

// recoverHandler turns a panic into a 500 instead of killing the request
// goroutine (fail-closed: a broken push must never take the server down).
func recoverHandler(w http.ResponseWriter, lg *log.Logger) {
	if rec := recover(); rec != nil {
		if lg != nil {
			lg.Error("push handler panic recovered", "err", fmt.Sprint(rec))
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error", "code": "INTERNAL"})
	}
}

// writeJSON mirrors the mediasvc response helper.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		body = []byte(`{"error":"internal error","code":"INTERNAL"}`)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
