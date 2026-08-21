package api

import (
	"net/http"

	"github.com/basicallysource/asset-service/internal/httpx"
)

// readyProbeKey is a key that is never written. Storage answering "not found"
// for it proves reachability and working credentials in one request, without
// creating anything.
const readyProbeKey = "_ready/probe"

// health says the process is up. It is deliberately cheap and dependency-free,
// so a supervisor restarting on its failure is restarting for a real reason.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.Version})
}

// ready says the process can actually serve: the catalog opens and storage
// answers with the credentials it was given. A deploy that fails this should
// be rolled back rather than left running.
func (s *Server) ready(w http.ResponseWriter, r *http.Request) {
	if err := s.Catalog.Ping(r.Context()); err != nil {
		s.Logger.Error("readiness: catalog", "error", err)
		httpx.Error(w, http.StatusServiceUnavailable, httpx.CodeInternal, "catalog unavailable")
		return
	}
	if _, err := s.Assets.Store.Head(r.Context(), readyProbeKey); err != nil {
		s.Logger.Error("readiness: storage", "error", err)
		httpx.Error(w, http.StatusServiceUnavailable, httpx.CodeInternal, "storage unavailable")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready", "version": s.Version})
}
