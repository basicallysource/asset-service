// Package api is the HTTP surface: five routes, and the rules about who may
// call them.
package api

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/basicallysource/asset-service/internal/assets"
	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/httpx"
	"github.com/basicallysource/asset-service/internal/identity"
)

// bodyHeadroom is how much slack the transport-level body cap gets over the
// service's own limit. The service is what should report an oversized upload,
// with a message that says which limit was hit; this is only a backstop
// against a body that is absurd rather than merely too big.
const bodyHeadroom = 1 << 20

// Server is the HTTP service.
type Server struct {
	Assets  *assets.Service
	Auth    auth.Authenticator
	Catalog *catalog.DB
	Version string
	Logger  *slog.Logger

	// Identity issues credentials to people who prove who they are. Nil, or
	// unconfigured, means this service does not offer sign-in.
	Identity *identity.GitHub
	// SignInThrottle limits how often one caller may start a sign-in, which is
	// the only unauthenticated thing here that costs anything.
	SignInThrottle *httpx.Throttle
	// ClientIPHeader names the header a proxy in front of this service sets to
	// the real client address. Empty means trust none.
	ClientIPHeader string
	// AdminLogins are the identity-provider handles that get full rights when
	// they sign in. This is how the people who run the service bootstrap
	// themselves without shell access to the host.
	AdminLogins []string

	// readyProbe rate-limits the storage check behind /readyz.
	readyProbe storageProbe
}

// CatalogKeys adapts the catalog to what auth needs, which is one lookup. It
// lives here so that the binary and the tests resolve credentials the same
// way; a test that authenticates differently from production is a test that
// can pass while production cannot log anybody in.
func CatalogKeys(db *catalog.DB) auth.KeyStore { return catalogKeys{db} }

type catalogKeys struct{ db *catalog.DB }

func (c catalogKeys) APIKeyByID(ctx context.Context, id string) (auth.StoredKey, error) {
	key, err := c.db.APIKeyByID(ctx, id)
	if err != nil {
		return auth.StoredKey{}, err
	}
	return auth.StoredKey{
		ID:         key.ID,
		Name:       key.Name,
		SecretHash: key.SecretHash,
		Scopes:     key.Scopes,
		Account:    key.AccountID,
		ExpiresAt:  key.ExpiresAt,
		Revoked:    key.Revoked,
	}, nil
}

// Handler builds the router and wraps it in the middleware every route shares.
func (s *Server) Handler() http.Handler {
	if s.SignInThrottle == nil {
		// A sign-in costs a round trip to GitHub, so it is worth a limit even
		// before any account exists to attach one to.
		s.SignInThrottle = &httpx.Throttle{Every: 20 * time.Second, Burst: 5}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("GET /login/htmx.js", s.htmxScript)
	mux.HandleFunc("GET /login/keys", s.keysFragment)
	mux.HandleFunc("POST /login/keys", s.mintKeyForm)
	mux.HandleFunc("POST /login/keys/{name}/revoke", s.revokeKeyForm)
	mux.HandleFunc("POST /v1/keys", s.mintKey)
	mux.HandleFunc("GET /v1/keys", s.listKeys)
	mux.HandleFunc("POST /v1/keys/{name}/revoke", s.revokeKey)
	mux.HandleFunc("POST /v1/auth/github/start", s.startSignIn)
	mux.HandleFunc("POST /v1/auth/github/token", s.finishSignIn)
	mux.HandleFunc("GET /healthz", s.health)
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /v1/assets", s.upload)
	mux.HandleFunc("GET /v1/assets/{key...}", s.metadata)
	mux.HandleFunc("GET /a/{key...}", s.deliver)

	return httpx.Chain(mux,
		httpx.RequestID,
		httpx.Recover(s.Logger),
		httpx.Log(s.Logger),
		httpx.MaxBytes(s.Assets.MaxBytes+bodyHeadroom),
		auth.Middleware(s.Auth),
	)
}
