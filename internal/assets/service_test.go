package assets

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/objstore"
)

func newService(t *testing.T) (*Service, *objstore.Memory) {
	t.Helper()

	db, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	store := objstore.NewMemory()
	return &Service{
		Store:        store,
		Catalog:      db,
		MaxBytes:     1 << 20,
		SpoolDir:     t.TempDir(),
		SignedURLTTL: time.Minute,
	}, store
}

func put(t *testing.T, s *Service, namespace, filename, body string) PutResult {
	t.Helper()
	result, err := s.Put(context.Background(), PutRequest{
		Namespace:   namespace,
		Filename:    filename,
		ContentType: "text/plain",
		By:          "test",
		Body:        strings.NewReader(body),
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	return result
}

func TestPutNamesBytesAfterThemselves(t *testing.T) {
	service, store := newService(t)

	result := put(t, service, "docs", "note.txt", "hello")
	if !result.Created {
		t.Error("first upload did not report itself as created")
	}
	if want := "docs/note-2cf24dba5fb0.txt"; result.Asset.Key != want {
		t.Errorf("key = %q, want %q", result.Asset.Key, want)
	}
	if result.Asset.Size != 5 || result.Asset.Visibility != catalog.VisibilityPublic {
		t.Errorf("asset = %+v", result.Asset)
	}

	stored, ok := store.Bytes(result.Asset.Key)
	if !ok || string(stored) != "hello" {
		t.Errorf("stored bytes = %q, ok = %v", stored, ok)
	}
}

func TestPutIsIdempotent(t *testing.T) {
	service, store := newService(t)

	first := put(t, service, "docs", "note.txt", "hello")
	second := put(t, service, "docs", "note.txt", "hello")

	if second.Created {
		t.Error("re-uploading identical bytes reported a new asset")
	}
	if first.Asset.Key != second.Asset.Key {
		t.Errorf("same bytes produced two keys: %q and %q", first.Asset.Key, second.Asset.Key)
	}
	if store.Len() != 1 {
		t.Errorf("store holds %d objects, want 1", store.Len())
	}
}

func TestDifferentBytesNeverShareAKey(t *testing.T) {
	service, store := newService(t)

	first := put(t, service, "docs", "note.txt", "hello")
	second := put(t, service, "docs", "note.txt", "goodbye")

	if first.Asset.Key == second.Asset.Key {
		t.Fatalf("different content landed on one key: %q", first.Asset.Key)
	}
	if store.Len() != 2 {
		t.Errorf("store holds %d objects, want 2", store.Len())
	}
}

// A key whose hash prefix collides with different bytes is the one case where
// content addressing could quietly start lying. It must fail loudly instead --
// whether the disagreement is spotted in the catalog or in storage.
func TestPutRefusesAKeyThatHoldsDifferentBytes(t *testing.T) {
	ctx := context.Background()
	const body = "hello"
	// sha256("hello") begins 2cf24dba5fb0, so this is the key "hello" claims.
	const key = "docs/note-2cf24dba5fb0.txt"

	t.Run("catalog disagrees", func(t *testing.T) {
		service, _ := newService(t)
		if _, err := service.Catalog.InsertAsset(ctx, catalog.Asset{
			Key: key, Namespace: "docs", Digest: strings.Repeat("a", 64), Size: 1,
			ContentType: "text/plain", Filename: "note.txt",
			Visibility: catalog.VisibilityPublic, CreatedAt: time.Now().UTC(), CreatedBy: "test",
		}); err != nil {
			t.Fatal(err)
		}

		_, err := service.Put(ctx, PutRequest{
			Namespace: "docs", Filename: "note.txt", Body: strings.NewReader(body), By: "test",
		})
		if !errors.Is(err, ErrDigestCollision) {
			t.Fatalf("err = %v, want ErrDigestCollision", err)
		}
	})

	t.Run("storage disagrees", func(t *testing.T) {
		service, store := newService(t)
		other := []byte("something else entirely")
		sum := sha256.Sum256(other)
		if err := store.Put(ctx, objstore.PutRequest{
			Key: key, Size: int64(len(other)), ContentType: "text/plain",
			Digest: hex.EncodeToString(sum[:]), Public: true,
		}, bytes.NewReader(other)); err != nil {
			t.Fatal(err)
		}

		_, err := service.Put(ctx, PutRequest{
			Namespace: "docs", Filename: "note.txt", Body: strings.NewReader(body), By: "test",
		})
		if !errors.Is(err, ErrDigestCollision) {
			t.Fatalf("err = %v, want ErrDigestCollision", err)
		}
		if got, _ := store.Bytes(key); string(got) != string(other) {
			t.Error("the stored object was overwritten")
		}
	})
}

func TestPutRecoversACatalogRowFromStorage(t *testing.T) {
	service, store := newService(t)

	first := put(t, service, "docs", "note.txt", "hello")

	// Simulate a catalog rebuilt from nothing: the bytes are still in storage.
	fresh, err := catalog.Open(filepath.Join(t.TempDir(), "empty.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { fresh.Close() })
	service.Catalog = fresh

	again := put(t, service, "docs", "note.txt", "hello")
	if !again.Created {
		t.Error("expected the missing row to be written")
	}
	if again.Asset.Key != first.Asset.Key {
		t.Errorf("key changed after catalog loss: %q then %q", first.Asset.Key, again.Asset.Key)
	}
	if store.Len() != 1 {
		t.Errorf("store holds %d objects, want the original 1", store.Len())
	}
}

func TestPutRejectsEmptyAndOversizedBodies(t *testing.T) {
	service, _ := newService(t)
	service.MaxBytes = 16
	ctx := context.Background()

	_, err := service.Put(ctx, PutRequest{Namespace: "docs", Filename: "a.txt", Body: strings.NewReader("")})
	if !errors.Is(err, ErrBadRequest) {
		t.Errorf("empty body error = %v, want ErrBadRequest", err)
	}

	_, err = service.Put(ctx, PutRequest{
		Namespace: "docs", Filename: "a.txt", Body: strings.NewReader(strings.Repeat("x", 17)),
	})
	if !errors.Is(err, ErrTooLarge) {
		t.Errorf("oversized body error = %v, want ErrTooLarge", err)
	}

	// Exactly at the limit is fine: the limit is a maximum, not a boundary to
	// be off by one on.
	if _, err := service.Put(ctx, PutRequest{
		Namespace: "docs", Filename: "a.txt", Body: strings.NewReader(strings.Repeat("x", 16)),
	}); err != nil {
		t.Errorf("body exactly at the limit was rejected: %v", err)
	}
}

func TestSpoolFilesAreCleanedUp(t *testing.T) {
	service, _ := newService(t)
	spoolDir := service.SpoolDir

	put(t, service, "docs", "note.txt", "hello")

	entries, err := filepath.Glob(filepath.Join(spoolDir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("spool directory still holds %v", entries)
	}
}

func TestContentTypeFallsBackToTheExtension(t *testing.T) {
	service, _ := newService(t)

	result, err := service.Put(context.Background(), PutRequest{
		Namespace: "docs", Filename: "diagram.png", By: "test",
		ContentType: "application/octet-stream",
		Body:        strings.NewReader("not really a png"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(result.Asset.ContentType, "image/png") {
		t.Errorf("content type = %q, want image/png", result.Asset.ContentType)
	}
}

func TestPrivateAssetsGetAnExpiringURL(t *testing.T) {
	service, _ := newService(t)

	result, err := service.Put(context.Background(), PutRequest{
		Namespace: "docs", Filename: "secret.txt", Private: true, By: "test",
		Body: strings.NewReader("classified"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Asset.Visibility != catalog.VisibilityPrivate {
		t.Fatalf("visibility = %q", result.Asset.Visibility)
	}

	url, expires, err := service.URL(result.Asset)
	if err != nil {
		t.Fatal(err)
	}
	if !expires || !strings.Contains(url, "expires=") {
		t.Errorf("private URL = %q, expires = %v", url, expires)
	}
}
