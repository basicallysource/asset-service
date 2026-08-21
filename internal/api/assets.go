package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/basicallysource/asset-service/internal/assets"
	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/httpx"
)

// deliveryMaxAge is how long a redirect may be reused. A key names its own
// bytes, so where they live cannot change under a reader; this is short only
// because the redirect target for a private asset must not be.
const deliveryMaxAge = "86400"

// manifest is what callers get back for an asset.
//
// Renditions is the ladder: every form of this asset that can be fetched. It
// holds one entry today, the bytes as uploaded. Derived forms -- an image at
// several widths, a poster frame, a compressed variant -- are appended here as
// they are produced, so a caller written against this shape today keeps
// working when they arrive.
type manifest struct {
	Key         string      `json:"key"`
	Namespace   string      `json:"namespace"`
	Digest      string      `json:"digest"`
	Size        int64       `json:"size"`
	ContentType string      `json:"content_type"`
	Filename    string      `json:"filename"`
	Visibility  string      `json:"visibility"`
	CreatedAt   time.Time   `json:"created_at"`
	URL         string      `json:"url"`
	URLExpires  bool        `json:"url_expires"`
	Renditions  []rendition `json:"renditions"`
}

type rendition struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
}

// original is the rendition name for the bytes exactly as uploaded.
const original = "original"

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	query := r.URL.Query()
	namespace := query.Get("namespace")
	if !assets.ValidNamespace(namespace) {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest,
			"namespace must be lowercase letters, digits and dashes")
		return
	}

	visibility := query.Get("visibility")
	if visibility == "" {
		visibility = catalog.VisibilityPublic
	}
	if visibility != catalog.VisibilityPublic && visibility != catalog.VisibilityPrivate {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "visibility must be public or private")
		return
	}

	if !principal.Can(auth.ActionWrite, namespace) {
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, "no write access to namespace "+namespace)
		return
	}

	result, err := s.Assets.Put(r.Context(), assets.PutRequest{
		Namespace:   namespace,
		Filename:    query.Get("filename"),
		ContentType: r.Header.Get("Content-Type"),
		Private:     visibility == catalog.VisibilityPrivate,
		By:          principal.Name,
		Body:        r.Body,
	})
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	body, err := s.manifest(result.Asset)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	status := http.StatusOK
	if result.Created {
		status = http.StatusCreated
	}
	w.Header().Set("Location", "/v1/assets/"+result.Asset.Key)
	httpx.JSON(w, status, body)
}

func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.readable(w, r)
	if !ok {
		return
	}
	body, err := s.manifest(asset)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, body)
}

// deliver sends a reader to the bytes rather than carrying them: the service
// never proxies content, so one download is one transfer out of storage.
func (s *Server) deliver(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.readable(w, r)
	if !ok {
		return
	}

	url, expires, err := s.Assets.URL(asset)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	if expires {
		// The target is a credential of sorts. Nothing may keep it.
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Vary", "Authorization")
	} else {
		w.Header().Set("Cache-Control", "public, max-age="+deliveryMaxAge)
	}

	// The redirect is written by hand rather than with http.Redirect, which
	// appends a courtesy HTML body. This endpoint answers machines and image
	// tags at whatever volume the sites in front of it have; the body would be
	// pure waste on every one of them.
	w.Header().Set("Location", url)
	w.WriteHeader(http.StatusFound)
}

// readable resolves the key in the request and enforces read access, writing
// the failure itself when there is one.
func (s *Server) readable(w http.ResponseWriter, r *http.Request) (catalog.Asset, bool) {
	key := r.PathValue("key")
	asset, err := s.Assets.Get(r.Context(), key)
	if err != nil {
		s.writeAssetError(w, r, err)
		return catalog.Asset{}, false
	}
	if asset.Visibility == catalog.VisibilityPublic {
		return asset, true
	}

	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return catalog.Asset{}, false
	}
	if !principal.Can(auth.ActionRead, asset.Namespace) {
		// Deliberately the same answer a missing asset gets: whether a private
		// asset exists is itself private.
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, "no such asset")
		return catalog.Asset{}, false
	}
	return asset, true
}

func (s *Server) manifest(a catalog.Asset) (manifest, error) {
	url, expires, err := s.Assets.URL(a)
	if err != nil {
		return manifest{}, err
	}
	return manifest{
		Key:         a.Key,
		Namespace:   a.Namespace,
		Digest:      "sha256:" + a.Digest,
		Size:        a.Size,
		ContentType: a.ContentType,
		Filename:    a.Filename,
		Visibility:  a.Visibility,
		CreatedAt:   a.CreatedAt,
		URL:         url,
		URLExpires:  expires,
		Renditions: []rendition{{
			Name:        original,
			ContentType: a.ContentType,
			Size:        a.Size,
			URL:         url,
		}},
	}, nil
}

// writeAssetError maps a service error onto a status. Anything unrecognised is
// a fault of ours: it is logged in full and reported as nothing more.
func (s *Server) writeAssetError(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, "no such asset")
	case errors.Is(err, assets.ErrBadRequest):
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
	case errors.Is(err, assets.ErrTooLarge):
		httpx.Error(w, http.StatusRequestEntityTooLarge, httpx.CodeTooLarge, err.Error())
	case errors.Is(err, assets.ErrDigestCollision):
		httpx.Error(w, http.StatusConflict, httpx.CodeConflict, err.Error())
	default:
		s.Logger.Error("request failed",
			"error", err,
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", httpx.RequestIDFrom(r.Context()))
		httpx.Error(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
	}
}
