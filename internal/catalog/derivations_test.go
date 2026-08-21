package catalog

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func storeAsset(t *testing.T, db *DB, key, contentType string) {
	t.Helper()
	if _, err := db.InsertAsset(context.Background(), Asset{
		Key: key, Namespace: "n", Filename: "f", ContentType: contentType,
		Size: 1, Digest: "sha256:" + key, Visibility: "public", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
}

// Work leaving the queue leaves a record: what it was, how it went, how long
// from claim to finish. The queue itself forgets on purpose; this is the
// half that remembers.
func TestFinishedWorkIsLogged(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	storeAsset(t, db, "n/part-abc.stl", "model/stl")
	storeAsset(t, db, "n/broken-def.png", "image/png")

	now := time.Now().UTC()
	if err := db.Enqueue(ctx, "n/part-abc.stl", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteJob(ctx, "n/part-abc.stl"); err != nil {
		t.Fatal(err)
	}

	if err := db.Enqueue(ctx, "n/broken-def.png", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := db.FailJob(ctx, "n/broken-def.png", "cannot decode", now.Add(time.Minute), 4); err != nil {
		t.Fatal(err)
	}

	recent, err := db.RecentDerivations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 2 {
		t.Fatalf("logged %d derivations, want 2", len(recent))
	}
	byKey := map[string]Derivation{}
	for _, d := range recent {
		byKey[d.AssetKey] = d
	}
	ok := byKey["n/part-abc.stl"]
	if ok.Outcome != "ok" || ok.ContentType != "model/stl" || ok.Seconds < 0 {
		t.Errorf("completed job logged as %+v", ok)
	}
	failed := byKey["n/broken-def.png"]
	if failed.Outcome != "failed" || failed.Error != "cannot decode" {
		t.Errorf("failed job logged as %+v", failed)
	}

	stats, err := db.DerivationStats(ctx, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("stats over %d content types, want 2", len(stats))
	}
}

// A job whose asset vanished times nothing and is not logged.
func TestOrphanedJobsAreNotLogged(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.Enqueue(ctx, "n/ghost.stl", now); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ClaimJob(ctx, now); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteJob(ctx, "n/ghost.stl"); err != nil {
		t.Fatal(err)
	}
	recent, err := db.RecentDerivations(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 0 {
		t.Fatalf("orphaned job was logged: %+v", recent)
	}
}
