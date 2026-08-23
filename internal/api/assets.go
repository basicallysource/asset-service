package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/basicallysource/asset-service/internal/assets"
	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/derive"
	"github.com/basicallysource/asset-service/internal/httpx"
	"github.com/basicallysource/asset-service/internal/policy"
)

// deliveryMaxAge is how long a redirect may be reused. A key names its own
// bytes, so where they live cannot change under a reader; this is short only
// because the redirect target for a private asset must not be.
const deliveryMaxAge = "86400"

// manifest is what callers get back for an asset.
//
// Renditions is the ladder: every form of this asset that can be fetched,
// smallest first, with the bytes as uploaded last. Pick from it rather than
// assuming what is in it -- an image too small to shrink usefully has a ladder
// of one, and one still being processed grows.
//
// RenditionsStatus says whether the ladder is finished: "ready" when it is,
// "pending" while derived forms are still being produced, "failed" if that was
// given up on, and "none" for a kind of asset that has no derived forms.
//
// Width and Height are the original's own pixel size, present for the kinds of
// asset that have one. They are what lets a page reserve the right space
// before an image arrives, so use them rather than a rendition's: the ladder
// tops out below what a camera produces, and an asset too small to shrink has
// no ladder to read a shape from at all.
//
// URL is the form of the asset that may be published. For anything that comes
// off a camera that is the largest derived form -- an image's full-resolution
// copy without the camera's notes in it, a video's widest encode -- because
// the original itself carries where it was taken and what took it. It is
// empty until there is one, which is until RenditionsStatus is "ready"; the
// asset's own bytes are never the fallback. Everything else is served as it
// was uploaded, as before.
type manifest struct {
	Key              string      `json:"key"`
	Namespace        string      `json:"namespace"`
	Digest           string      `json:"digest"`
	Size             int64       `json:"size"`
	ContentType      string      `json:"content_type"`
	Width            int         `json:"width,omitempty"`
	Height           int         `json:"height,omitempty"`
	Filename         string      `json:"filename"`
	Visibility       string      `json:"visibility"`
	CreatedAt        time.Time   `json:"created_at"`
	URL              string      `json:"url"`
	URLExpires       bool        `json:"url_expires"`
	Renditions       []rendition `json:"renditions"`
	RenditionsStatus string      `json:"renditions_status"`
}

type rendition struct {
	Name        string `json:"name"`
	ContentType string `json:"content_type"`
	Width       int    `json:"width,omitempty"`
	Height      int    `json:"height,omitempty"`
	Size        int64  `json:"size"`
	URL         string `json:"url"`
	// URLExpires marks a URL that must not be published or kept: the bytes as
	// uploaded, handed to a caller that is allowed to read them.
	URLExpires bool `json:"url_expires,omitempty"`
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

	limits, ok := s.allowed(w, r, principal)
	if !ok {
		return
	}

	result, err := s.Assets.Put(r.Context(), assets.PutRequest{
		Namespace:   namespace,
		Filename:    query.Get("filename"),
		ContentType: r.Header.Get("Content-Type"),
		Private:     visibility == catalog.VisibilityPrivate,
		By:          principal.Name,
		AccountID:   principal.Account,
		MaxBytes:    limits.MaxFileBytes,
		Body:        r.Body,
	})
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	body, err := s.manifest(r, result.Asset)
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

// allowed applies the uploader's account limits. An operator-minted key has no
// account and no limits: it was made by hand on the host, which is already as
// trusted as the service itself.
func (s *Server) allowed(w http.ResponseWriter, r *http.Request, principal *auth.Principal) (policy.Limits, bool) {
	if principal.Account == "" {
		return policy.Unlimited, true
	}

	ctx := r.Context()
	account, err := s.Catalog.AccountByID(ctx, principal.Account)
	if err != nil {
		s.writeAssetError(w, r, err)
		return policy.Limits{}, false
	}

	limits := policy.For(account.Tier)
	now := time.Now().UTC()

	lastHour, err := s.Catalog.UsageSince(ctx, account.ID, now.Add(-time.Hour))
	if err != nil {
		s.writeAssetError(w, r, err)
		return policy.Limits{}, false
	}
	lastDay, err := s.Catalog.UsageSince(ctx, account.ID, now.Add(-24*time.Hour))
	if err != nil {
		s.writeAssetError(w, r, err)
		return policy.Limits{}, false
	}

	decision := policy.Evaluate(limits, policy.Upload{
		ContentType: r.Header.Get("Content-Type"),
		Size:        r.ContentLength,
	}, lastHour, lastDay)
	if decision.Allowed {
		return limits, true
	}

	status, code := http.StatusForbidden, httpx.CodeForbidden
	switch decision.Code {
	case policy.CodeUnsupportedType:
		status, code = http.StatusUnsupportedMediaType, httpx.CodeUnsupported
	case policy.CodeTooLarge:
		status, code = http.StatusRequestEntityTooLarge, httpx.CodeTooLarge
	case policy.CodeRateLimited:
		status, code = http.StatusTooManyRequests, httpx.CodeRateLimited
		w.Header().Set("Retry-After", itoa(int64(decision.RetryAfter.Seconds())))
	}
	s.Logger.Info("upload refused",
		"account", account.ID, "handle", account.Handle, "tier", account.Tier, "reason", decision.Code)
	httpx.Error(w, status, code, decision.Message)
	return policy.Limits{}, false
}

func (s *Server) metadata(w http.ResponseWriter, r *http.Request) {
	asset, ok := s.readable(w, r)
	if !ok {
		return
	}
	body, err := s.manifest(r, asset)
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

	derived, _, err := s.Assets.Ladder(r.Context(), asset)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	// The published form, for every caller alike. A reader that may have the
	// bytes as uploaded gets them from a manifest instead: a redirect that
	// depended on who asked would be one more way for an expiring URL to end
	// up published in a page.
	url, expires, err := s.Assets.PublicForm(asset, derived)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	if url == "" {
		// Withheld, and its published form is not made yet. Saying so beats a
		// redirect to bytes that were meant to stay off a page.
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound,
			"this asset has no published form yet")
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

// mayReadOriginal reports whether this caller may be told where the bytes as
// uploaded are. Anyone may have an original that is not withheld; a withheld
// one goes only to a principal that could read the asset if it were private,
// which is the same test that guards a private asset's own bytes.
func (s *Server) mayReadOriginal(r *http.Request, a catalog.Asset) bool {
	if a.Visibility != catalog.VisibilityPublic || !derive.WithholdsOriginal(a.ContentType) {
		return true
	}
	principal := auth.From(r.Context())
	return principal != nil && principal.Can(auth.ActionRead, a.Namespace)
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

func (s *Server) manifest(r *http.Request, a catalog.Asset) (manifest, error) {
	derived, status, err := s.Assets.Ladder(r.Context(), a)
	if err != nil {
		return manifest{}, err
	}

	url, expires, err := s.Assets.PublicForm(a, derived)
	if err != nil {
		return manifest{}, err
	}

	// Smallest first, original last: a caller walking the list for the first
	// rung wide enough always lands on the cheapest one that works.
	ladder := make([]rendition, 0, len(derived)+1)
	for _, d := range derived {
		derivedURL, _, err := s.Assets.KeyURL(d.Key, a.Visibility)
		if err != nil {
			return manifest{}, err
		}
		ladder = append(ladder, rendition{
			Name:        d.Name,
			ContentType: d.ContentType,
			Width:       d.Width,
			Height:      d.Height,
			Size:        d.Size,
			URL:         derivedURL,
		})
	}
	// The bytes as uploaded, last. A withheld original is only named to a
	// caller that could read it if the asset were private -- handing an
	// anonymous reader a signed URL to it would publish it just as surely as
	// leaving the object public did.
	if s.mayReadOriginal(r, a) {
		originalURL, originalExpires, err := s.Assets.URL(a)
		if err != nil {
			return manifest{}, err
		}
		ladder = append(ladder, rendition{
			Name:        original,
			ContentType: a.ContentType,
			Width:       a.Width,
			Height:      a.Height,
			Size:        a.Size,
			URL:         originalURL,
			URLExpires:  originalExpires,
		})
	}

	return manifest{
		Key:              a.Key,
		Namespace:        a.Namespace,
		Digest:           "sha256:" + a.Digest,
		Size:             a.Size,
		ContentType:      a.ContentType,
		Width:            a.Width,
		Height:           a.Height,
		Filename:         a.Filename,
		Visibility:       a.Visibility,
		CreatedAt:        a.CreatedAt,
		URL:              url,
		URLExpires:       expires,
		Renditions:       ladder,
		RenditionsStatus: status,
	}, nil
}

// Refusals that come from the account rather than the request.
var (
	errBlocked       = errors.New("this account may not upload")
	errTooManyTokens = errors.New("this account already holds as many tokens as it may; revoke one first")
)

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
	case errors.Is(err, errBlocked):
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, err.Error())
	case errors.Is(err, errTooManyTokens):
		httpx.Error(w, http.StatusTooManyRequests, httpx.CodeRateLimited, err.Error())
	default:
		s.Logger.Error("request failed",
			"error", err,
			"method", r.Method,
			"path", r.URL.Path,
			"request_id", httpx.RequestIDFrom(r.Context()))
		httpx.Error(w, http.StatusInternalServerError, httpx.CodeInternal, "internal error")
	}
}
