package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/basicallysource/asset-service/internal/assets"
	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/derive"
	"github.com/basicallysource/asset-service/internal/httpx"
)

// Working the queue from somewhere else.
//
// Deriving is the expensive half of this service and the only half that wants
// a machine with cores to spare -- and the machine this runs on is usually
// chosen for being cheap and shared, which is the opposite. These routes let
// the two be different machines: claim a job, send back what was made, say it
// is done. The service still owns the queue, the bytes, and every naming
// decision; the worker owns nothing but the CPU time.
//
// It is the same code either way. A worker in this process calls
// assets.Service directly; a worker elsewhere reaches the same functions
// through here.

// staleAfter is how long a claim may go quiet before the job is offered again.
// A worker in this process has its claims released when it restarts, which a
// worker on another machine cannot rely on: nothing here notices it dying.
const staleAfter = 30 * time.Minute

// maxRenditionBytes bounds one derived form sent back by a worker. Renditions
// are smaller than their original by construction, so this is a sanity limit
// rather than a real one.
const maxRenditionBytes = 512 << 20

// claimedJob is one unit of work, and everything needed to do it without
// holding storage credentials.
type claimedJob struct {
	Key         string `json:"key"`
	Namespace   string `json:"namespace"`
	Filename    string `json:"filename"`
	ContentType string `json:"content_type"`
	Attempts    int    `json:"attempts"`
	// SourceURL is where to read the original. It may expire.
	SourceURL string `json:"source_url"`
}

// admin reports whether the caller may work jobs in a namespace, answering the
// request itself when it may not. Work is gated on admin because a worker is
// as trusted as this service: it decides what an asset's smaller copies look
// like, which is not something a write scope should buy.
func (s *Server) admin(w http.ResponseWriter, r *http.Request, namespace string) *auth.Principal {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return nil
	}
	if !principal.Can(auth.ActionAdmin, namespace) {
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, "working the queue requires admin")
		return nil
	}
	return principal
}

// claimJob hands out the oldest job that is due, or 204 when there is nothing
// to do. A claim is not a lease the worker has to renew; it times out.
// maxAttempts is how many times one asset may be tried before the queue gives
// up on it. Shared by the reported-failure path and by the stale-claim
// release, so a job that crashes its worker is bounded by the same number as
// one that fails politely.
func (s *Server) maxAttempts() int {
	if s.RenditionAttempts > 0 {
		return s.RenditionAttempts
	}
	return catalog.DefaultJobAttempts
}

func (s *Server) claimJob(w http.ResponseWriter, r *http.Request) {
	// A claim may be for any namespace, so the caller has to administer all of
	// them. Asking after the job is drawn would mean drawing jobs the caller
	// cannot do and putting them back.
	if s.admin(w, r, "*") == nil {
		return
	}

	ctx := r.Context()
	if released, err := s.Catalog.ReleaseStaleJobs(ctx, time.Now().UTC().Add(-staleAfter), s.maxAttempts()); err != nil {
		s.writeAssetError(w, r, err)
		return
	} else if released > 0 {
		s.Logger.Info("jobs: reoffered work a worker stopped reporting on", "jobs", released)
	}

	job, err := s.Catalog.ClaimJob(ctx, time.Now().UTC())
	if errors.Is(err, catalog.ErrNotFound) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	asset, err := s.Catalog.AssetByKey(ctx, job.AssetKey)
	if errors.Is(err, catalog.ErrNotFound) {
		// The asset is gone. So is the reason to do this.
		if err := s.Catalog.CompleteJob(ctx, job.AssetKey); err != nil {
			s.writeAssetError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	source, _, err := s.Assets.KeyURL(asset.Key, asset.Visibility)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	httpx.JSON(w, http.StatusOK, claimedJob{
		Key:         asset.Key,
		Namespace:   asset.Namespace,
		Filename:    asset.Filename,
		ContentType: asset.ContentType,
		Attempts:    job.Attempts,
		SourceURL:   source,
	})
}

// putRendition stores one derived form a worker produced. The body is the raw
// bytes; everything about how they are named and stored is decided here, so a
// worker cannot invent a key any more than an uploader can.
func (s *Server) putRendition(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	key := query.Get("key")
	if key == "" {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "key is required")
		return
	}
	if s.admin(w, r, assets.Namespace(key)) == nil {
		return
	}

	name := query.Get("name")
	if name == "" {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "name is required")
		return
	}
	width, _ := strconv.Atoi(query.Get("width"))
	height, _ := strconv.Atoi(query.Get("height"))
	extension := query.Get("ext")

	asset, err := s.Catalog.AssetByKey(r.Context(), key)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxRenditionBytes+1))
	if err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "could not read the body")
		return
	}
	if len(body) == 0 {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "body is required")
		return
	}
	if int64(len(body)) > maxRenditionBytes {
		httpx.Error(w, http.StatusRequestEntityTooLarge, httpx.CodeTooLarge, "rendition is too large")
		return
	}

	if err := s.Assets.PutRendition(r.Context(), asset, derive.Rendition{
		Name:        name,
		Width:       width,
		Height:      height,
		ContentType: r.Header.Get("Content-Type"),
		Extension:   extension,
		Bytes:       body,
	}); err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// finishJob closes a claim. An empty error means it worked; anything else is
// recorded and retried until the job has failed too many times.
func (s *Server) finishJob(w http.ResponseWriter, r *http.Request) {
	key := r.URL.Query().Get("key")
	if key == "" {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "key is required")
		return
	}
	if s.admin(w, r, assets.Namespace(key)) == nil {
		return
	}

	var report struct {
		Error string `json:"error"`
		// Permanent means the bytes will never derive, so retrying is pointless.
		Permanent bool `json:"permanent"`
	}
	if r.ContentLength > 0 {
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&report); err != nil {
			httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "body must be JSON")
			return
		}
	}

	ctx := r.Context()
	if report.Error != "" && !report.Permanent {
		if err := s.Catalog.FailJob(ctx, key, report.Error,
			time.Now().UTC().Add(retryAfterFailure), s.maxAttempts()); err != nil {
			s.writeAssetError(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if err := s.Catalog.CompleteJob(ctx, key); err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// retryAfterFailure is when a job a worker could not finish is offered again.
// The worker itself does not choose: a client should not be able to ask to be
// retried immediately, forever.
const retryAfterFailure = 5 * time.Minute
