package objstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

// TestClientAgainstLiveStorage exercises the client against a real
// S3-compatible provider. It is skipped unless the environment names one, so
// an ordinary `go test ./...` needs no credentials -- but when a provider is
// added or a key is rotated, this is the check that says the client works
// where it counts.
//
//	ASSET_S3_ENDPOINT=https://nyc3.digitaloceanspaces.com \
//	ASSET_S3_REGION=nyc3 ASSET_S3_BUCKET=... \
//	ASSET_S3_ACCESS_KEY=... ASSET_S3_SECRET_KEY=... \
//	ASSET_PUBLIC_BASE_URL=... go test ./internal/objstore -run Live -v
func TestClientAgainstLiveStorage(t *testing.T) {
	cfg := Config{
		Endpoint:      os.Getenv("ASSET_S3_ENDPOINT"),
		Region:        os.Getenv("ASSET_S3_REGION"),
		Bucket:        os.Getenv("ASSET_S3_BUCKET"),
		AccessKey:     os.Getenv("ASSET_S3_ACCESS_KEY"),
		SecretKey:     os.Getenv("ASSET_S3_SECRET_KEY"),
		PublicBaseURL: os.Getenv("ASSET_PUBLIC_BASE_URL"),
	}
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.AccessKey == "" {
		t.Skip("no live storage configured")
	}

	client, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Deterministic content, so a rerun reuses one key instead of littering.
	body := []byte("asset-service live storage check\n")
	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])
	key := "_selftest/probe-" + digest[:12] + ".txt"

	if err := client.Put(ctx, PutRequest{
		Key:         key,
		Size:        int64(len(body)),
		ContentType: "text/plain; charset=utf-8",
		Digest:      digest,
	}, bytes.NewReader(body)); err != nil {
		t.Fatalf("put: %v", err)
	}

	head, err := client.Head(ctx, key)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if !head.Exists || head.Size != int64(len(body)) {
		t.Fatalf("head = %+v, want an object of %d bytes", head, len(body))
	}
	if head.Digest != digest {
		t.Errorf("stored digest = %q, want %q -- object metadata did not survive the round trip", head.Digest, digest)
	}

	missing, err := client.Head(ctx, "_selftest/definitely-not-here-000000000000.txt")
	if err != nil {
		t.Fatalf("head missing: %v", err)
	}
	if missing.Exists {
		t.Error("a key that was never written reports as existing")
	}

	signed, err := client.SignedURL(key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(signed)
	if err != nil {
		t.Fatalf("fetch signed URL: %v", err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("signed URL returned %d: %s", resp.StatusCode, got)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("signed URL served %q, want %q", got, body)
	}
}
