package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/basicallysource/asset-service/internal/httpx"
)

type contextKey int

const principalKey contextKey = iota

// Middleware resolves credentials once, at the edge, and puts the principal in
// the request context.
//
// A request with no credentials passes through as anonymous rather than being
// rejected: public assets are readable by anyone, and it is the handler that
// knows whether this particular route needs an identity. A request carrying
// credentials that do not work is rejected here -- that is never a request
// worth serving anonymously, and failing at the edge keeps a bad token from
// looking like a permissions bug further in.
func Middleware(a Authenticator) httpx.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := a.Authenticate(r.Context(), r)
			switch {
			case errors.Is(err, ErrRevoked):
				httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "credential has been revoked")
				return
			case err != nil:
				httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "invalid credentials")
				return
			}
			if principal != nil {
				r = r.WithContext(context.WithValue(r.Context(), principalKey, principal))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// From returns the principal behind a request, or nil if it is anonymous.
func From(ctx context.Context) *Principal {
	p, _ := ctx.Value(principalKey).(*Principal)
	return p
}
