// Package renditions produces the smaller copies of an uploaded image, in the
// background, one at a time.
//
// It is deliberately not part of the upload. Re-encoding a large photograph
// takes seconds, and an upload that waited for it would be an upload that
// times out on a slow connection for a reason that has nothing to do with the
// upload. The manifest says whether a ladder is still being built, so a caller
// that cares can wait and one that does not can use the original.
package renditions

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/basicallysource/asset-service/internal/assets"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/imaging"
	"github.com/basicallysource/asset-service/internal/objstore"
)

// Defaults for the worker's own behaviour.
const (
	DefaultPoll        = 15 * time.Second
	DefaultMaxAttempts = 4
	retryBase          = 30 * time.Second
	retryCap           = 30 * time.Minute
)

// Worker turns queued assets into ladders.
//
// One at a time, on purpose: this usually runs beside other services on a
// small machine, and image resizing will take every core it is offered. A
// queue that drains slightly slower is a much better neighbour than one that
// makes everything else on the host stutter.
type Worker struct {
	Catalog *catalog.DB
	Store   objstore.Store
	Options imaging.Options
	Logger  *slog.Logger

	// MaxAttempts is how many times a failing asset is retried before it is
	// left alone. MaxBytes bounds what is read back out of storage.
	MaxAttempts int
	MaxBytes    int64
	// Poll is the idle interval. Work normally starts immediately, on the
	// wake-up an upload sends; this is what catches anything that missed one.
	Poll time.Duration

	Now func() time.Time

	defaults sync.Once
	wake     chan struct{}
}

// Wake asks the worker to look for work now rather than at the next poll.
// It never blocks: a wake-up that arrives while one is already pending is
// redundant, because the worker drains the queue either way.
func (w *Worker) Wake() {
	w.applyDefaults()
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// Run works the queue until ctx is done.
func (w *Worker) Run(ctx context.Context) {
	w.applyDefaults()

	// Anything left claimed belongs to a process that is gone.
	if released, err := w.Catalog.ReleaseClaimedJobs(ctx); err != nil {
		w.Logger.Error("renditions: release claimed jobs", "error", err)
	} else if released > 0 {
		w.Logger.Info("renditions: requeued work from a previous run", "jobs", released)
	}

	timer := time.NewTimer(w.Poll)
	defer timer.Stop()

	for {
		// Drain rather than take one: a burst of uploads should not need a
		// wake-up each to get through.
		for {
			worked, err := w.processOne(ctx)
			if err != nil {
				w.Logger.Error("renditions: worker", "error", err)
			}
			if !worked || ctx.Err() != nil {
				break
			}
		}

		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(w.Poll)

		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		case <-timer.C:
		}
	}
}

// processOne handles at most one asset. It reports whether it found work.
func (w *Worker) processOne(ctx context.Context) (bool, error) {
	// Defaults are applied here as well as in Run, so that a caller driving
	// the worker one job at a time gets the same retry and size limits a
	// running worker would. Leaving that to Run made those limits zero, which
	// silently meant "give up after the first failure".
	w.applyDefaults()

	job, err := w.Catalog.ClaimJob(ctx, w.now())
	if errors.Is(err, catalog.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}

	if err := w.build(ctx, job.AssetKey); err != nil {
		delay := backoff(job.Attempts)
		w.Logger.Error("renditions: failed",
			"asset", job.AssetKey, "attempt", job.Attempts+1, "retry_in", delay, "error", err)
		if failErr := w.Catalog.FailJob(ctx, job.AssetKey, err.Error(), w.now().Add(delay), w.MaxAttempts); failErr != nil {
			return true, failErr
		}
		return true, nil
	}

	return true, w.Catalog.CompleteJob(ctx, job.AssetKey)
}

// build produces and stores every rendition of one asset.
func (w *Worker) build(ctx context.Context, key string) error {
	asset, err := w.Catalog.AssetByKey(ctx, key)
	if errors.Is(err, catalog.ErrNotFound) {
		// The asset is gone. So is the reason to do this.
		return nil
	}
	if err != nil {
		return err
	}
	if !imaging.Supported(asset.ContentType) {
		return nil
	}

	body, err := w.Store.Get(ctx, asset.Key)
	if err != nil {
		return err
	}
	original, err := io.ReadAll(io.LimitReader(body, w.MaxBytes))
	body.Close()
	if err != nil {
		return fmt.Errorf("read %s: %w", asset.Key, err)
	}

	started := w.now()
	ladder, err := imaging.Ladder(original, w.Options)
	if err != nil {
		// Bytes that claim to be an image and are not will never decode, so
		// there is nothing to come back for.
		if errors.Is(err, imaging.ErrUnsupported) {
			w.Logger.Warn("renditions: not a readable image", "asset", asset.Key, "error", err)
			return nil
		}
		return err
	}

	for _, rendition := range ladder {
		if err := w.store(ctx, asset, rendition); err != nil {
			return err
		}
	}

	w.Logger.Info("renditions: built",
		"asset", asset.Key, "count", len(ladder), "took", w.now().Sub(started).Round(time.Millisecond))
	return nil
}

func (w *Worker) store(ctx context.Context, asset catalog.Asset, r imaging.Rendition) error {
	sum := sha256.Sum256(r.Bytes)
	digest := hex.EncodeToString(sum[:])

	key, err := assets.RenditionKey(asset.Namespace, asset.Filename, r.Name, digest, imaging.OutputExtension)
	if err != nil {
		return err
	}

	head, err := w.Store.Head(ctx, key)
	if err != nil {
		return err
	}
	if !head.Exists {
		if err := w.Store.Put(ctx, objstore.PutRequest{
			Key:         key,
			Size:        int64(len(r.Bytes)),
			ContentType: imaging.OutputContentType,
			Digest:      digest,
			Public:      asset.Visibility == catalog.VisibilityPublic,
		}, bytes.NewReader(r.Bytes)); err != nil {
			return err
		}
	}

	return w.Catalog.InsertRendition(ctx, catalog.Rendition{
		AssetKey:    asset.Key,
		Name:        r.Name,
		Key:         key,
		ContentType: imaging.OutputContentType,
		Width:       r.Width,
		Height:      r.Height,
		Size:        int64(len(r.Bytes)),
		Digest:      digest,
		CreatedAt:   w.now(),
	})
}

func (w *Worker) applyDefaults() {
	w.defaults.Do(w.setDefaults)
}

func (w *Worker) setDefaults() {
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
	if w.Now == nil {
		w.Now = func() time.Time { return time.Now().UTC() }
	}
	if w.Poll <= 0 {
		w.Poll = DefaultPoll
	}
	if w.MaxAttempts <= 0 {
		w.MaxAttempts = DefaultMaxAttempts
	}
	if w.MaxBytes <= 0 {
		w.MaxBytes = 256 << 20
	}
	w.wake = make(chan struct{}, 1)
}

func (w *Worker) now() time.Time {
	if w.Now == nil {
		return time.Now().UTC()
	}
	return w.Now()
}

// backoff spaces out retries: storage being briefly unreachable should not
// become a hot loop against it.
func backoff(attempts int) time.Duration {
	delay := retryBase << attempts
	if delay > retryCap || delay <= 0 {
		return retryCap
	}
	return delay
}
