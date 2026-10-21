package catalog

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A thumbnail that arrives after a pile of slow work is still taken first; work
// of the same cost keeps its order of arrival.
func TestClaimJobTakesCheapestFirst(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()
	start := time.Now().UTC()

	for i, job := range []struct {
		key  string
		cost int
	}{
		{"n/slice-1.stl", 2},
		{"n/slice-2.stl", 2},
		{"n/clip.mp4", 1},
		{"n/photo-1.png", 0},
		{"n/photo-2.png", 0},
	} {
		contentType := map[int]string{0: "image/png", 1: "video/mp4", 2: "model/stl"}[job.cost]
		storeAsset(t, db, job.key, contentType)
		if err := db.Enqueue(ctx, job.key, job.cost, start.Add(time.Duration(i)*time.Second)); err != nil {
			t.Fatal(err)
		}
	}

	now := start.Add(time.Minute)
	for _, want := range []string{"n/photo-1.png", "n/photo-2.png", "n/clip.mp4", "n/slice-1.stl", "n/slice-2.stl"} {
		job, err := db.ClaimJob(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if job.AssetKey != want {
			t.Fatalf("claimed %s, want %s", job.AssetKey, want)
		}
	}
	if _, err := db.ClaimJob(ctx, now); !errors.Is(err, ErrNotFound) {
		t.Fatalf("an empty queue should say so, got %v", err)
	}
}
