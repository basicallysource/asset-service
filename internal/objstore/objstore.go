// Package objstore is this service's view of S3-compatible object storage:
// put an object, ask whether one is already there, and hand a reader a URL it
// can fetch the bytes from itself.
//
// Nothing here serves a reader. A reader is given a URL -- a public one for
// public assets, a signed one for private assets -- and fetches from storage
// directly, which is what keeps one download from being billed as two
// transfers. Get exists for work the service does to an asset, like producing
// smaller copies of an image; it must never end up on the path of a request
// that could have been a redirect.
package objstore

import (
	"context"
	"io"
	"time"
)

// Object is what storage knows about a key.
type Object struct {
	Exists      bool
	Size        int64
	ContentType string
	// Digest is the full hex SHA-256 of the bytes, recorded as object
	// metadata at upload time. Storing it on the object rather than only in
	// the catalog is what makes the catalog rebuildable from the bucket.
	Digest string
}

// PutRequest describes one object to store.
type PutRequest struct {
	Key         string
	Size        int64
	ContentType string
	// Digest is the hex SHA-256 of the body. It names the object, is recorded
	// as metadata, and doubles as the SigV4 payload hash.
	Digest string
	// Public controls the storage ACL. A private object can only be read
	// through a signed URL.
	Public bool
}

// Store is the object storage this service needs. Client talks to real
// storage; Memory is the test double.
type Store interface {
	Head(ctx context.Context, key string) (Object, error)
	Put(ctx context.Context, req PutRequest, body io.Reader) error
	// Get reads an object back, for processing it. Not for serving it.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// PublicURL is where anyone can fetch a public object.
	PublicURL(key string) string
	// SignedURL is a time-limited URL for a private object.
	SignedURL(key string, ttl time.Duration) (string, error)
	// SetPrivate stops storage serving an object to anyone who asks. It is
	// for an object stored public before the service knew better; a new one
	// is stored with the ACL it should have.
	SetPrivate(ctx context.Context, key string) error
}
