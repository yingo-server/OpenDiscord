package middleware

import (
	"context"
	"net/http"

	"github.com/yingo-server/UFOT/server/internal/admin/identity"
	"github.com/yingo-server/UFOT/server/internal/pkg/log"
)

// AdminIdentity is the principal injected into a request context.
type AdminIdentity = identity.Admin

// SessionResolver resolves an opaque admin token into a principal. The
// implementation is service.AuthService; middleware never touches cookies,
// JWTs or database rows directly.
type SessionResolver interface {
	Resolve(ctx context.Context, token string) (*identity.Admin, error)
}

// Authenticator is the admin-plane auth middleware. It is deliberately
// separate from the user plane's Ed25519/JWT verifier: different cookie name,
// different resolver, different failure semantics (docs/security-spec §3.3).
type Authenticator struct {
	Resolver   SessionResolver
	CookieName string
	Logger     *log.Logger
}

// NewAuthenticator builds the middleware.
func NewAuthenticator(resolver SessionResolver, cookieName string) *Authenticator {
	return &Authenticator{Resolver: resolver, CookieName: cookieName, Logger: log.For("admin.authn")}
}

// Require rejects unauthenticated requests with the unified 401 body and
// injects *identity.Admin into the request context.
func (a *Authenticator) Require(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Resolver == nil {
			WriteError(w, r, http.StatusUnauthorized, "unauthenticated")
			return
		}
		token := TokenFromRequest(r, a.CookieName)
		if token == "" {
			WriteError(w, r, http.StatusUnauthorized, "unauthenticated")
			return
		}
		admin, err := a.Resolver.Resolve(r.Context(), token)
		if err != nil || admin == nil {
			WriteError(w, r, http.StatusUnauthorized, "unauthenticated")
			return
		}
		next.ServeHTTP(w, r.WithContext(WithIdentity(r.Context(), admin)))
	})
}
