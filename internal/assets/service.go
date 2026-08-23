// Package assets is what the service does: take bytes, name them after
// themselves, store them once, and hand out URLs to them.
package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"os"
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/derive"
	"github.com/basicallysource/asset-service/internal/objstore"
)

const defaultContentType = "application/octet-stream"

// putAttempts is how many times a store upload is tried before giving up. The
// body is on local disk by then, so a retry costs a re-read, not a re-upload
// from the client.
const putAttempts = 3

// Errors callers act on.
var (
	// ErrTooLarge means the body exceeded the configured limit.
	ErrTooLarge = errors.New("asset exceeds the maximum upload size")
	// ErrDigestCollision means this key already holds different bytes. It
	// cannot happen from ordinary use; it means two different files hashed to
	// the same prefix under the same name, and the service refuses rather than
	// letting a URL start lying about its content.
	ErrDigestCollision = errors.New("key already holds different bytes")
)

// Service stores and resolves assets.
type Service struct {
	Store   objstore.Store
	Catalog *catalog.DB

	// MaxBytes is the largest body accepted. SpoolDir is where a body is held
	// while it is hashed; empty means the system temp directory.
	MaxBytes int64
	SpoolDir string

	// SignedURLTTL is how long a private asset's URL stays valid.
	SignedURLTTL time.Duration

	// Now is the clock, swapped in tests.
	Now func() time.Time

	// Notify is called when an asset has been queued for processing, so the
	// worker can start without waiting for its next poll. Optional.
	Notify func()

	Logger *slog.Logger
}

// Whether an asset's derived forms exist yet.
const (
	// LadderNone means this kind of asset has no derived forms.
	LadderNone = "none"
	// LadderPending means they are queued or being produced.
	LadderPending = "pending"
	// LadderReady means they are done.
	LadderReady = "ready"
	// LadderFailed means producing them did not work and was given up on.
	LadderFailed = "failed"
)

// PutRequest is one upload.
type PutRequest struct {
	Namespace   string
	Filename    string
	ContentType string
	Private     bool
	// By names the principal doing the upload, for the audit trail, and
	// AccountID the account whose limits it counted against.
	By        string
	AccountID string
	// MaxBytes further limits this one upload, below the service's own limit.
	// Zero means the service's limit is the only one.
	MaxBytes int64

	Body io.Reader
}

// PutResult is the stored asset and whether this call is what created it.
type PutResult struct {
	Asset   catalog.Asset
	Created bool
}

// Put stores an asset.
//
// It is idempotent by construction rather than by checking: identical bytes
// with the same name produce the same key, so re-uploading is a no-op that
// returns the asset that is already there. Nothing is ever overwritten,
// because nothing can be addressed without knowing its content first.
func (s *Service) Put(ctx context.Context, req PutRequest) (PutResult, error) {
	if !ValidNamespace(req.Namespace) {
		return PutResult{}, fmt.Errorf("%w: namespace %q must be lowercase letters, digits and dashes", ErrBadRequest, req.Namespace)
	}
	if strings.TrimSpace(req.Filename) == "" {
		return PutResult{}, fmt.Errorf("%w: filename is required", ErrBadRequest)
	}

	body, err := s.spool(req.Body, req.MaxBytes)
	if err != nil {
		return PutResult{}, err
	}
	defer body.Close()

	key, err := BuildKey(req.Namespace, req.Filename, body.Digest)
	if err != nil {
		return PutResult{}, err
	}

	visibility := catalog.VisibilityPublic
	if req.Private {
		visibility = catalog.VisibilityPrivate
	}

	// Already known? Then the bytes are already stored under this exact name.
	existing, err := s.Catalog.AssetByKey(ctx, key)
	switch {
	case err == nil && existing.Digest == body.Digest:
		return PutResult{Asset: existing, Created: false}, nil
	case err == nil:
		return PutResult{}, fmt.Errorf("%w: %s", ErrDigestCollision, key)
	case !errors.Is(err, catalog.ErrNotFound):
		return PutResult{}, err
	}

	if err := s.store(ctx, key, body, req, visibility); err != nil {
		return PutResult{}, err
	}

	resolvedType := contentType(req.ContentType, key)
	width, height := s.measure(ctx, body.file.Name(), resolvedType)

	asset := catalog.Asset{
		Key:         key,
		Namespace:   req.Namespace,
		Digest:      body.Digest,
		Size:        body.Size,
		ContentType: resolvedType,
		Filename:    req.Filename,
		Visibility:  visibility,
		CreatedAt:   s.now(),
		CreatedBy:   req.By,
		AccountID:   req.AccountID,
		Width:       width,
		Height:      height,
	}

	created, err := s.Catalog.InsertAsset(ctx, asset)
	if err != nil {
		return PutResult{}, err
	}
	if !created {
		// Another request stored the same bytes first. Same key, same
		// content, so its row is as good as ours -- report theirs.
		asset, err = s.Catalog.AssetByKey(ctx, key)
		if err != nil {
			return PutResult{}, err
		}
		return PutResult{Asset: asset, Created: false}, nil
	}

	s.queue(ctx, asset)
	return PutResult{Asset: asset, Created: true}, nil
}

// PutRendition stores one derived form of an asset and records it.
//
// It lives here rather than in the worker because the work can happen
// anywhere -- in this process, or on a machine with cores to spare that hands
// the bytes back over the API -- and where the bytes came from must not change
// what is stored or how it is named.
func (s *Service) PutRendition(ctx context.Context, asset catalog.Asset, r derive.Rendition) error {
	sum := sha256.Sum256(r.Bytes)
	digest := hex.EncodeToString(sum[:])

	key, err := RenditionKey(asset.Namespace, asset.Filename, r.Name, digest, r.Extension)
	if err != nil {
		return err
	}

	head, err := s.Store.Head(ctx, key)
	if err != nil {
		return err
	}
	if !head.Exists {
		if err := s.Store.Put(ctx, objstore.PutRequest{
			Key:         key,
			Size:        int64(len(r.Bytes)),
			ContentType: r.ContentType,
			Digest:      digest,
			Public:      asset.Visibility == catalog.VisibilityPublic,
		}, bytes.NewReader(r.Bytes)); err != nil {
			return err
		}
	}

	return s.Catalog.InsertRendition(ctx, catalog.Rendition{
		AssetKey:    asset.Key,
		Name:        r.Name,
		Key:         key,
		ContentType: r.ContentType,
		Width:       r.Width,
		Height:      r.Height,
		Size:        int64(len(r.Bytes)),
		Digest:      digest,
		CreatedAt:   s.now(),
	})
}

// measure reads the asset's pixel size off the spooled body, so a manifest can
// say how tall an image is before a page has downloaded it. It never fails an
// upload: bytes that will not measure still store fine, and a zero here means
// nothing more than "not known".
func (s *Service) measure(ctx context.Context, path, contentType string) (int, int) {
	width, height, err := derive.Dimensions(ctx, path, contentType)
	if err != nil {
		s.logger().Warn("measure asset", "content_type", contentType, "error", err)
		return 0, 0
	}
	return width, height
}

// queue asks for an asset's derived forms. It never fails an upload: the bytes
// are stored and the manifest already answers for them, so a queue that is
// briefly unwritable is a delay, not a lost asset.
func (s *Service) queue(ctx context.Context, asset catalog.Asset) {
	if !derive.Supported(asset.ContentType) {
		return
	}
	if err := s.Catalog.Enqueue(ctx, asset.Key, s.now()); err != nil {
		s.logger().Error("queue renditions", "asset", asset.Key, "error", err)
		return
	}
	if s.Notify != nil {
		s.Notify()
	}
}

// Ladder returns an asset's derived forms and whether more are coming.
func (s *Service) Ladder(ctx context.Context, asset catalog.Asset) ([]catalog.Rendition, string, error) {
	if !derive.Supported(asset.ContentType) {
		return nil, LadderNone, nil
	}

	renditions, err := s.Catalog.RenditionsFor(ctx, asset.Key)
	if err != nil {
		return nil, "", err
	}

	job, err := s.Catalog.JobFor(ctx, asset.Key)
	switch {
	case errors.Is(err, catalog.ErrNotFound):
		// No outstanding work: whatever exists is all there will be.
		return renditions, LadderReady, nil
	case err != nil:
		return nil, "", err
	case job.State == catalog.JobFailed:
		return renditions, LadderFailed, nil
	default:
		return renditions, LadderPending, nil
	}
}

func (s *Service) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// store uploads the body unless storage already holds these exact bytes.
func (s *Service) store(ctx context.Context, key string, body *spooled, req PutRequest, visibility string) error {
	head, err := s.Store.Head(ctx, key)
	if err != nil {
		return err
	}
	if head.Exists {
		if head.Digest != "" && head.Digest != body.Digest {
			return fmt.Errorf("%w: %s", ErrDigestCollision, key)
		}
		if head.Digest != "" {
			return nil // the bytes are already there; the catalog row is what is missing
		}
	}

	resolvedType := contentType(req.ContentType, key)
	put := objstore.PutRequest{
		Key:         key,
		Size:        body.Size,
		ContentType: resolvedType,
		Digest:      body.Digest,
		// A withheld original is stored private even when the asset is
		// public. Handing out a different URL would not be enough on its own:
		// what makes an object readable by anyone is its ACL, not which
		// address this service prints.
		Public: visibility == catalog.VisibilityPublic && !derive.WithholdsOriginal(resolvedType),
	}

	var lastErr error
	for attempt := 1; attempt <= putAttempts; attempt++ {
		reader, err := body.Reader()
		if err != nil {
			return err
		}
		if lastErr = s.Store.Put(ctx, put, reader); lastErr == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return lastErr
}

// Get returns one asset's metadata.
func (s *Service) Get(ctx context.Context, key string) (catalog.Asset, error) {
	return s.Catalog.AssetByKey(ctx, key)
}

// URL is where the bytes exactly as uploaded can be fetched from. A public
// asset gets a stable URL; a private one, and one whose original is withheld,
// gets one that expires. Either way the reader fetches from storage directly
// -- this service hands out addresses, it does not carry payloads.
//
// This is the original itself, so it is not what a page should publish. That
// is PublicForm.
func (s *Service) URL(a catalog.Asset) (url string, expires bool, err error) {
	if derive.WithholdsOriginal(a.ContentType) {
		return s.KeyURL(a.Key, catalog.VisibilityPrivate)
	}
	return s.KeyURL(a.Key, a.Visibility)
}

// PublicForm is the URL of the form of this asset that may be published.
//
// For anything whose original is withheld that is the largest derived form --
// an image's stripped full-resolution copy, a video's widest encode -- and for
// everything else it is the original, as it always was. An empty URL means
// there is nothing publishable yet, which is the state between an upload and
// the end of its ladder; the original is not an answer to fall back on, since
// withholding it is the point.
func (s *Service) PublicForm(a catalog.Asset, ladder []catalog.Rendition) (url string, expires bool, err error) {
	if a.Visibility == catalog.VisibilityPrivate || !derive.WithholdsOriginal(a.ContentType) {
		return s.URL(a)
	}

	// The full copy where there is one: an image is published at the size it
	// was uploaded, minus what the camera wrote into it. Failing that the
	// widest rendition there is -- which is a video's largest encode, and,
	// for an image stored before the copy existed, its largest rung until
	// `asset-service withhold` has had one built.
	widest := -1
	for i, r := range ladder {
		if r.Name == derive.FullName {
			return s.KeyURL(r.Key, a.Visibility)
		}
		if widest < 0 || r.Width > ladder[widest].Width {
			widest = i
		}
	}
	if widest < 0 {
		return "", false, nil
	}
	return s.KeyURL(ladder[widest].Key, a.Visibility)
}

// KeyURL is URL for any stored object, including a rendition, which is visible
// exactly as much as the asset it was derived from.
func (s *Service) KeyURL(key, visibility string) (url string, expires bool, err error) {
	if visibility == catalog.VisibilityPrivate {
		signed, err := s.Store.SignedURL(key, s.SignedURLTTL)
		return signed, true, err
	}
	return s.Store.PublicURL(key), false, nil
}

func (s *Service) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

// spooled is an upload held on local disk while it is hashed. The key cannot
// be known until the last byte has been read, so the body has to land
// somewhere first; disk rather than memory is what keeps a large upload from
// being a memory spike.
type spooled struct {
	file   *os.File
	Digest string
	Size   int64
}

func (s *Service) spool(r io.Reader, max int64) (*spooled, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: body is required", ErrBadRequest)
	}
	if max <= 0 || max > s.MaxBytes {
		max = s.MaxBytes
	}

	file, err := os.CreateTemp(s.SpoolDir, "upload-*")
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	spool := &spooled{file: file}

	hasher := sha256.New()
	// One byte past the limit, so an oversized body is detected rather than
	// silently truncated into a valid-looking asset.
	size, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(r, max+1))
	if err != nil {
		spool.Close()
		return nil, fmt.Errorf("spool: %w", err)
	}
	if size > max {
		spool.Close()
		return nil, ErrTooLarge
	}
	if size == 0 {
		spool.Close()
		return nil, fmt.Errorf("%w: body is empty", ErrBadRequest)
	}

	spool.Size = size
	spool.Digest = hex.EncodeToString(hasher.Sum(nil))
	return spool, nil
}

// Reader rewinds the spool file for a fresh read.
func (s *spooled) Reader() (io.Reader, error) {
	if _, err := s.file.Seek(0, io.SeekStart); err != nil {
		return nil, fmt.Errorf("spool: rewind: %w", err)
	}
	return s.file, nil
}

// Close removes the spool file. The name is unlinked either way, so a crash
// mid-upload leaves nothing behind that a restart has to clean up.
func (s *spooled) Close() error {
	name := s.file.Name()
	err := s.file.Close()
	if rmErr := os.Remove(name); err == nil && !os.IsNotExist(rmErr) {
		err = rmErr
	}
	return err
}

// contentType picks what storage will serve the bytes as: what the caller
// declared, else what the extension implies, else the honest unknown.
func contentType(declared, key string) string {
	declared = strings.TrimSpace(declared)
	if declared != "" && declared != defaultContentType && isPrintable(declared) {
		return declared
	}
	if dot := strings.LastIndex(key, "."); dot >= 0 {
		if byExt := mime.TypeByExtension(key[dot:]); byExt != "" {
			return byExt
		}
	}
	return defaultContentType
}

// isPrintable rejects a header value that could inject another header or
// otherwise misbehave once it is echoed back by storage.
func isPrintable(s string) bool {
	if len(s) > 255 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}
