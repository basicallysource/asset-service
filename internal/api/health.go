package api

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/basicallysource/asset-service/internal/httpx"
	"github.com/basicallysource/asset-service/internal/objstore"
)

// readyProbeKey is a key that is never written. Storage answering "not found"
// for it proves reachability and working credentials in one request, without
// creating anything.
const readyProbeKey = "_ready/probe"

// readyProbeFor is how long one storage check answers for every caller.
//
// This route has to be unauthenticated -- an external monitor is the whole
// point of it -- and it makes a request to storage. Without a window, anyone
// could turn a flood of cheap requests here into a flood of expensive ones
// there. A monitor checking once a minute never notices the difference.
const readyProbeFor = 5 * time.Second

// storageProbe remembers the last storage check so that load on this route
// does not become load on the object store.
type storageProbe struct {
	mu   sync.Mutex
	at   time.Time
	err  error
	done bool
}

// check returns the last answer if it is recent, and otherwise asks storage.
func (p *storageProbe) check(ctx context.Context, store objstore.Store, now time.Time) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.done && now.Sub(p.at) < readyProbeFor {
		return p.err
	}
	_, err := store.Head(ctx, readyProbeKey)
	p.at, p.err, p.done = now, err, true
	return err
}

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
	if err := s.readyProbe.check(r.Context(), s.Assets.Store, time.Now()); err != nil {
		s.Logger.Error("readiness: storage", "error", err)
		httpx.Error(w, http.StatusServiceUnavailable, httpx.CodeInternal, "storage unavailable")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"status": "ready", "version": s.Version})
}
