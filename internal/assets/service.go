// Package assets is what the service does: take bytes, name them after
// themselves, store them once, and hand out URLs to them.
package assets

import (
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
	"github.com/basicallysource/asset-service/internal/imaging"
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
	// By names the principal doing the upload, for the audit trail.
	By   string
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

	body, err := s.spool(req.Body)
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

	asset := catalog.Asset{
		Key:         key,
		Namespace:   req.Namespace,
		Digest:      body.Digest,
		Size:        body.Size,
		ContentType: contentType(req.ContentType, key),
		Filename:    req.Filename,
		Visibility:  visibility,
		CreatedAt:   s.now(),
		CreatedBy:   req.By,
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

// queue asks for an asset's derived forms. It never fails an upload: the bytes
// are stored and the manifest already answers for them, so a queue that is
// briefly unwritable is a delay, not a lost asset.
func (s *Service) queue(ctx context.Context, asset catalog.Asset) {
	if !imaging.Supported(asset.ContentType) {
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
	if !imaging.Supported(asset.ContentType) {
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

	put := objstore.PutRequest{
		Key:         key,
		Size:        body.Size,
		ContentType: contentType(req.ContentType, key),
		Digest:      body.Digest,
		Public:      visibility == catalog.VisibilityPublic,
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

// URL is where the bytes of an asset can be fetched from. Public assets get a
// stable URL; private assets get one that expires. Either way the reader
// fetches from storage directly -- this service hands out addresses, it does
// not carry payloads.
func (s *Service) URL(a catalog.Asset) (url string, expires bool, err error) {
	return s.KeyURL(a.Key, a.Visibility)
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

func (s *Service) spool(r io.Reader) (*spooled, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: body is required", ErrBadRequest)
	}

	file, err := os.CreateTemp(s.SpoolDir, "upload-*")
	if err != nil {
		return nil, fmt.Errorf("spool: %w", err)
	}
	spool := &spooled{file: file}

	hasher := sha256.New()
	// One byte past the limit, so an oversized body is detected rather than
	// silently truncated into a valid-looking asset.
	size, err := io.Copy(io.MultiWriter(file, hasher), io.LimitReader(r, s.MaxBytes+1))
	if err != nil {
		spool.Close()
		return nil, fmt.Errorf("spool: %w", err)
	}
	if size > s.MaxBytes {
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
