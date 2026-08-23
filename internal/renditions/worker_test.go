package renditions

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basicallysource/asset-service/internal/assets"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/derive"
	"github.com/basicallysource/asset-service/internal/imaging"
	"github.com/basicallysource/asset-service/internal/objstore"
)

func photo(t *testing.T, width, height int) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := range height {
		for x := range width {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 90, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type fixture struct {
	service *assets.Service
	worker  *Worker
	store   *objstore.Memory
	db      *catalog.DB
}

func newFixture(t *testing.T) *fixture {
	t.Helper()

	db, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store := objstore.NewMemory()
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))

	service := &assets.Service{
		Store: store, Catalog: db, MaxBytes: 8 << 20,
		SpoolDir: t.TempDir(), SignedURLTTL: time.Minute, Logger: quiet,
	}

	return &fixture{
		db:      db,
		store:   store,
		service: service,
		worker: &Worker{
			Catalog: db, Store: store, Assets: service, Logger: quiet,
			Options:  derive.Options{Image: imaging.Options{Widths: []int{320, 640}}},
			MaxBytes: 8 << 20,
		},
	}
}

func (f *fixture) upload(t *testing.T, filename, contentType string, body []byte) catalog.Asset {
	t.Helper()
	result, err := f.service.Put(context.Background(), assets.PutRequest{
		Namespace: "web", Filename: filename, ContentType: contentType,
		By: "test", Body: bytes.NewReader(body),
	})
	if err != nil {
		t.Fatalf("upload %s: %v", filename, err)
	}
	return result.Asset
}

func TestUploadingAnImageQueuesItAndTheWorkerBuildsTheLadder(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	asset := f.upload(t, "river.png", "image/png", photo(t, 1200, 900))

	if _, status, err := f.service.Ladder(ctx, asset); err != nil || status != assets.LadderPending {
		t.Fatalf("status before the worker ran = %q, err = %v; want pending", status, err)
	}

	worked, err := f.worker.processOne(ctx)
	if err != nil || !worked {
		t.Fatalf("processOne = %v, %v", worked, err)
	}

	renditions, status, err := f.service.Ladder(ctx, asset)
	if err != nil {
		t.Fatal(err)
	}
	if status != assets.LadderReady {
		t.Errorf("status = %q, want ready", status)
	}
	// Two rungs and the full-resolution copy that is what gets published.
	if len(renditions) != 3 {
		t.Fatalf("got %d renditions, want 2 rungs and a full copy", len(renditions))
	}
	if full := renditions[2]; full.Name != imaging.FullName || full.Width != 1200 || full.Height != 900 {
		t.Errorf("the last rendition is %q at %dx%d, want %q at the original's own size",
			full.Name, full.Width, full.Height, imaging.FullName)
	}

	for i, want := range []int{320, 640} {
		r := renditions[i]
		if r.Width != want || r.Height != want*3/4 {
			t.Errorf("rendition %d is %dx%d", i, r.Width, r.Height)
		}
		if r.ContentType != imaging.JPEGContentType {
			t.Errorf("rendition %d is %s", i, r.ContentType)
		}
		if r.Size <= 0 {
			t.Errorf("rendition %d is empty", i)
		}
		// The key names the original, the rung, and its own bytes.
		if !strings.HasPrefix(r.Key, "web/river-w"+itoa(want)+"-") || !strings.HasSuffix(r.Key, imaging.JPEGExtension) {
			t.Errorf("rendition %d has key %q", i, r.Key)
		}
		stored, ok := f.store.Bytes(r.Key)
		if !ok || int64(len(stored)) != r.Size {
			t.Errorf("rendition %d is not in storage at the size recorded", i)
		}
	}

	// Nothing left to do, so nothing is claimed on the next pass.
	if worked, err := f.worker.processOne(ctx); worked || err != nil {
		t.Errorf("second pass did work: %v, %v", worked, err)
	}
}

func TestRebuildingIsIdempotent(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	asset := f.upload(t, "river.png", "image/png", photo(t, 1200, 900))
	if _, err := f.worker.processOne(ctx); err != nil {
		t.Fatal(err)
	}
	before := f.store.Len()

	// Ask again, as a retry or a restart would.
	if err := f.db.Enqueue(ctx, asset.Key, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.worker.processOne(ctx); err != nil {
		t.Fatal(err)
	}

	if f.store.Len() != before {
		t.Errorf("store went from %d to %d objects; the same renditions were stored twice", before, f.store.Len())
	}
	renditions, _, err := f.service.Ladder(ctx, asset)
	if err != nil {
		t.Fatal(err)
	}
	if len(renditions) != 3 {
		t.Errorf("got %d renditions after a second run, want 3", len(renditions))
	}
}

func TestUnderivableTypesAreNeverQueued(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	asset := f.upload(t, "notes.txt", "text/plain", []byte("nothing to derive here"))

	renditions, status, err := f.service.Ladder(ctx, asset)
	if err != nil {
		t.Fatal(err)
	}
	if status != assets.LadderNone || len(renditions) != 0 {
		t.Errorf("status = %q with %d renditions, want none", status, len(renditions))
	}
	if worked, err := f.worker.processOne(ctx); worked || err != nil {
		t.Errorf("the worker found work for an underivable type: %v, %v", worked, err)
	}
}

// A model is queued on the strength of its content type alone. Whether THIS
// machine can slice it is a different question, answered by whichever worker
// claims the job -- see internal/model.
func TestModelsAreQueued(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	asset := f.upload(t, "bracket.stl", "model/stl", []byte("solid bracket\nendsolid\n"))

	_, status, err := f.service.Ladder(ctx, asset)
	if err != nil {
		t.Fatal(err)
	}
	if status != assets.LadderPending {
		t.Errorf("status = %q, want %q", status, assets.LadderPending)
	}
}

// Bytes that claim to be an image and are not will never decode, so the job is
// finished rather than retried until it is marked failed.
func TestUndecodableBytesAreGivenUpOnImmediately(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	asset := f.upload(t, "broken.png", "image/png", []byte("PNG? no."))

	if _, err := f.worker.processOne(ctx); err != nil {
		t.Fatal(err)
	}

	renditions, status, err := f.service.Ladder(ctx, asset)
	if err != nil {
		t.Fatal(err)
	}
	if status != assets.LadderReady || len(renditions) != 0 {
		t.Errorf("status = %q with %d renditions", status, len(renditions))
	}
	if _, err := f.db.JobFor(ctx, asset.Key); err == nil {
		t.Error("the job is still queued")
	}
}

func TestAFailedAttemptIsQueuedAgain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	asset := f.upload(t, "river.png", "image/png", photo(t, 1200, 900))

	// Storage loses the object between upload and processing.
	f.store.Forget(asset.Key)

	if worked, err := f.worker.processOne(ctx); !worked || err != nil {
		t.Fatalf("processOne = %v, %v", worked, err)
	}

	job, err := f.db.JobFor(ctx, asset.Key)
	if err != nil {
		t.Fatalf("a failed job was dropped instead of retried: %v", err)
	}
	if job.State != catalog.JobPending || job.Attempts != 1 {
		t.Errorf("job = %+v, want one attempt and still pending", job)
	}
	if job.LastError == "" {
		t.Error("nothing was recorded about why it failed")
	}

	// Not due yet, so the worker leaves it alone rather than spinning on it.
	if worked, err := f.worker.processOne(ctx); worked || err != nil {
		t.Errorf("the worker retried immediately: %v, %v", worked, err)
	}

	if _, status, _ := f.service.Ladder(ctx, asset); status != assets.LadderPending {
		t.Errorf("ladder status = %q while a retry is outstanding, want pending", status)
	}
}

// A job that cannot succeed should stop consuming the worker and become
// visible instead.
func TestAttemptsAreCappedAndTheJobEndsUpFailed(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	asset := f.upload(t, "river.png", "image/png", photo(t, 1200, 900))
	due := time.Now().UTC().Add(-time.Second)

	if err := f.db.FailJob(ctx, asset.Key, "first", due, 2); err != nil {
		t.Fatal(err)
	}
	if job, _ := f.db.JobFor(ctx, asset.Key); job.State != catalog.JobPending {
		t.Fatalf("state after one failure = %q, want pending", job.State)
	}

	if err := f.db.FailJob(ctx, asset.Key, "second", due, 2); err != nil {
		t.Fatal(err)
	}
	job, err := f.db.JobFor(ctx, asset.Key)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != catalog.JobFailed || job.Attempts != 2 {
		t.Errorf("job = %+v, want failed after two attempts", job)
	}

	if _, status, _ := f.service.Ladder(ctx, asset); status != assets.LadderFailed {
		t.Errorf("ladder status = %q, want failed", status)
	}
	// A failed job is not picked up again.
	if worked, err := f.worker.processOne(ctx); worked || err != nil {
		t.Errorf("the worker claimed a failed job: %v, %v", worked, err)
	}
}

// A job that kills its worker every time must run out of attempts. Before
// this, a release left attempts at zero, so the restarted worker was handed
// the same poison pill for ever and every other namespace queued behind it.
func TestAJobThatKeepsKillingTheWorkerIsGivenUpOn(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	f.upload(t, "river.png", "image/png", photo(t, 1200, 900))

	const limit = 3
	for i := 0; i < limit; i++ {
		if _, err := f.db.ClaimJob(ctx, time.Now().UTC()); err != nil {
			t.Fatalf("claim %d: %v", i, err)
		}
		// The worker dies here, so it never reports: only the release runs.
		if _, err := f.db.ReleaseClaimedJobs(ctx, limit); err != nil {
			t.Fatalf("release %d: %v", i, err)
		}
	}

	if _, err := f.db.ClaimJob(ctx, time.Now().UTC()); !errors.Is(err, catalog.ErrNotFound) {
		t.Fatalf("a job past its attempt limit was claimed again: err = %v", err)
	}
}

func TestWorkRequeuedAfterACrashIsPickedUpAgain(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	asset := f.upload(t, "river.png", "image/png", photo(t, 1200, 900))

	// Claim it and then vanish, as a killed process would.
	if _, err := f.db.ClaimJob(ctx, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	if worked, _ := f.worker.processOne(ctx); worked {
		t.Fatal("a job already claimed was claimed again")
	}

	released, err := f.db.ReleaseClaimedJobs(ctx, DefaultMaxAttempts)
	if err != nil || released != 1 {
		t.Fatalf("released %d jobs, err = %v", released, err)
	}
	if worked, err := f.worker.processOne(ctx); !worked || err != nil {
		t.Fatalf("processOne = %v, %v", worked, err)
	}
	if _, status, _ := f.service.Ladder(ctx, asset); status != assets.LadderReady {
		t.Errorf("status = %q, want ready", status)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
