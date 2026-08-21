// Package api is the HTTP surface: five routes, and the rules about who may
// call them.
package api

import (
	"log/slog"
	"net/http"

	"github.com/basicallysource/asset-service/internal/assets"
	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/httpx"
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
}

// Handler builds the router and wraps it in the middleware every route shares.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
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
