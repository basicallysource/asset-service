package objstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Memory is an in-process Store for tests. It enforces the same invariant real
// storage does by construction -- an object's recorded digest is the hash of
// the bytes it was given -- so a test that passes against it is testing the
// same rules production runs under.
type Memory struct {
	// BaseURL is the prefix returned by PublicURL.
	BaseURL string

	mu      sync.Mutex
	objects map[string]memoryObject
}

type memoryObject struct {
	body        []byte
	digest      string
	contentType string
	public      bool
}

var _ Store = (*Memory)(nil)

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{BaseURL: "https://assets.example/store", objects: map[string]memoryObject{}}
}

func (m *Memory) Head(_ context.Context, key string) (Object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	o, ok := m.objects[key]
	if !ok {
		return Object{}, nil
	}
	return Object{Exists: true, Size: int64(len(o.body)), ContentType: o.contentType, Digest: o.digest}, nil
}

func (m *Memory) Put(_ context.Context, req PutRequest, body io.Reader) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	if got := hex.EncodeToString(sum[:]); got != req.Digest {
		return fmt.Errorf("objstore: body hashes to %s, request says %s", got, req.Digest)
	}
	if int64(len(b)) != req.Size {
		return fmt.Errorf("objstore: body is %d bytes, request says %d", len(b), req.Size)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[req.Key] = memoryObject{body: b, digest: req.Digest, contentType: req.ContentType, public: req.Public}
	return nil
}

func (m *Memory) Get(_ context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	o, ok := m.objects[key]
	if !ok {
		return nil, fmt.Errorf("objstore: get %s: no such object", key)
	}
	return io.NopCloser(bytes.NewReader(o.body)), nil
}

func (m *Memory) PublicURL(key string) string {
	return strings.TrimRight(m.BaseURL, "/") + "/" + strings.TrimLeft(key, "/")
}

func (m *Memory) SignedURL(key string, ttl time.Duration) (string, error) {
	return fmt.Sprintf("%s?expires=%d", m.PublicURL(key), int(ttl.Seconds())), nil
}

// Bytes returns what was stored at key, for assertions.
func (m *Memory) Bytes(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	return o.body, ok
}

// SetPrivate stops the double reporting an object as public, the way real
// storage stops serving it.
func (m *Memory) SetPrivate(_ context.Context, key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	o, ok := m.objects[key]
	if !ok {
		return fmt.Errorf("objstore: set private %s: no such object", key)
	}
	o.public = false
	m.objects[key] = o
	return nil
}

// Public reports whether an object is stored readable by anyone, and whether
// it is there at all. It is what an ACL decision can be asserted against.
func (m *Memory) Public(key string) (public, exists bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objects[key]
	return o.public, ok
}

// Forget removes an object, for tests that need storage to lose one.
func (m *Memory) Forget(key string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objects, key)
}

// Len is how many objects are stored.
func (m *Memory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.objects)
}
