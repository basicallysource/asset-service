package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/basicallysource/asset-service/internal/assets"
	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/objstore"
)

type harness struct {
	handler http.Handler
	server  *Server
	store   *objstore.Memory
	writer  string // a token that may write to docs
	reader  string // a token that may only read docs
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	db, err := catalog.Open(filepath.Join(t.TempDir(), "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })

	// Real credentials in the real store, so these tests exercise the same
	// lookup path the service uses.
	mint := func(name string, scopes ...string) string {
		token, id, hash, err := auth.NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := db.InsertAPIKey(context.Background(), catalog.APIKey{
			ID: id, Name: name, SecretHash: hash, Scopes: scopes, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatal(err)
		}
		return token
	}

	store := objstore.NewMemory()
	server := &Server{
		Assets: &assets.Service{
			Store:        store,
			Catalog:      db,
			MaxBytes:     1 << 16,
			SpoolDir:     t.TempDir(),
			SignedURLTTL: time.Minute,
		},
		Auth:    &auth.APIKeys{Keys: CatalogKeys(db)},
		Catalog: db,
		Version: "test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	}

	return &harness{
		handler: server.Handler(),
		server:  server,
		store:   store,
		writer:  mint("ci", "write:docs", "read:docs"),
		reader:  mint("viewer", "read:docs"),
	}
}

func (h *harness) do(t *testing.T, method, target, token, body string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	r := httptest.NewRequest(method, target, reader)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		r.Header.Set("Content-Type", "text/plain")
	}
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

// upload sends a body of a particular type, which the plain do() helper always
// calls text.
func (h *harness) upload(t *testing.T, token, target, contentType, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", contentType)
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func decode(t *testing.T, w *httptest.ResponseRecorder) manifest {
	t.Helper()
	var m manifest
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	return m
}

func TestUploadRequiresAWriteScope(t *testing.T) {
	h := newHarness(t)

	cases := []struct {
		name, token string
		want        int
	}{
		{"anonymous", "", http.StatusUnauthorized},
		{"read-only key", h.reader, http.StatusForbidden},
		{"write key", h.writer, http.StatusCreated},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := h.do(t, http.MethodPost, "/v1/assets?namespace=docs&filename=note.txt", c.token, "hello")
			if w.Code != c.want {
				t.Errorf("status = %d, want %d (%s)", w.Code, c.want, w.Body.String())
			}
		})
	}
}

func TestUploadReturnsAManifestAndIsIdempotent(t *testing.T) {
	h := newHarness(t)

	first := h.do(t, http.MethodPost, "/v1/assets?namespace=docs&filename=note.txt", h.writer, "hello")
	if first.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", first.Code, first.Body.String())
	}
	created := decode(t, first)

	if want := "docs/note-2cf24dba5fb0.txt"; created.Key != want {
		t.Errorf("key = %q, want %q", created.Key, want)
	}
	if created.Digest != "sha256:2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" {
		t.Errorf("digest = %q", created.Digest)
	}
	if created.Size != 5 || created.Visibility != catalog.VisibilityPublic || created.URLExpires {
		t.Errorf("manifest = %+v", created)
	}
	if got := first.Header().Get("Location"); got != "/v1/assets/"+created.Key {
		t.Errorf("Location = %q", got)
	}
	if len(created.Renditions) != 1 || created.Renditions[0].Name != original {
		t.Errorf("renditions = %+v, want just the original", created.Renditions)
	}
	if created.RenditionsStatus != assets.LadderNone {
		t.Errorf("renditions_status = %q for a text file, want none", created.RenditionsStatus)
	}
	if created.URL != h.store.PublicURL(created.Key) {
		t.Errorf("url = %q, want the store's public URL", created.URL)
	}

	second := h.do(t, http.MethodPost, "/v1/assets?namespace=docs&filename=note.txt", h.writer, "hello")
	if second.Code != http.StatusOK {
		t.Errorf("re-upload status = %d, want 200", second.Code)
	}
	if decode(t, second).Key != created.Key {
		t.Error("re-upload produced a different key")
	}
	if h.store.Len() != 1 {
		t.Errorf("store holds %d objects, want 1", h.store.Len())
	}
}

func TestUploadRejectsBadRequests(t *testing.T) {
	h := newHarness(t)

	cases := map[string]struct {
		target, body string
		want         int
	}{
		"no namespace":      {"/v1/assets?filename=a.txt", "hi", http.StatusBadRequest},
		"bad namespace":     {"/v1/assets?namespace=Docs&filename=a.txt", "hi", http.StatusBadRequest},
		"no filename":       {"/v1/assets?namespace=docs", "hi", http.StatusBadRequest},
		"empty body":        {"/v1/assets?namespace=docs&filename=a.txt", "", http.StatusBadRequest},
		"bad visibility":    {"/v1/assets?namespace=docs&filename=a.txt&visibility=secret", "hi", http.StatusBadRequest},
		"body over the cap": {"/v1/assets?namespace=docs&filename=a.txt", strings.Repeat("x", (1<<16)+1), http.StatusRequestEntityTooLarge},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			w := h.do(t, http.MethodPost, c.target, h.writer, c.body)
			if w.Code != c.want {
				t.Errorf("status = %d, want %d (%s)", w.Code, c.want, w.Body.String())
			}
		})
	}
}

func TestPublicAssetsAreReadableByAnyone(t *testing.T) {
	h := newHarness(t)
	key := decode(t, h.do(t, http.MethodPost, "/v1/assets?namespace=docs&filename=note.txt", h.writer, "hello")).Key

	meta := h.do(t, http.MethodGet, "/v1/assets/"+key, "", "")
	if meta.Code != http.StatusOK {
		t.Fatalf("metadata status = %d (%s)", meta.Code, meta.Body.String())
	}

	delivery := h.do(t, http.MethodGet, "/a/"+key, "", "")
	if delivery.Code != http.StatusFound {
		t.Fatalf("delivery status = %d, want 302", delivery.Code)
	}
	if got := delivery.Header().Get("Location"); got != h.store.PublicURL(key) {
		t.Errorf("redirected to %q", got)
	}
	if got := delivery.Header().Get("Cache-Control"); !strings.HasPrefix(got, "public") {
		t.Errorf("Cache-Control = %q, want a public one", got)
	}
	if delivery.Body.Len() != 0 {
		t.Error("the service sent bytes; it must only send a redirect")
	}
}

func TestPrivateAssetsNeedAReadScope(t *testing.T) {
	h := newHarness(t)
	key := decode(t, h.do(t,
		http.MethodPost, "/v1/assets?namespace=docs&filename=secret.txt&visibility=private", h.writer, "classified")).Key

	if w := h.do(t, http.MethodGet, "/v1/assets/"+key, "", ""); w.Code != http.StatusUnauthorized {
		t.Errorf("anonymous metadata status = %d, want 401", w.Code)
	}

	meta := h.do(t, http.MethodGet, "/v1/assets/"+key, h.reader, "")
	if meta.Code != http.StatusOK {
		t.Fatalf("reader metadata status = %d (%s)", meta.Code, meta.Body.String())
	}
	if body := decode(t, meta); !body.URLExpires {
		t.Error("a private asset was handed a URL that does not expire")
	}

	delivery := h.do(t, http.MethodGet, "/a/"+key, h.reader, "")
	if delivery.Code != http.StatusFound {
		t.Fatalf("delivery status = %d", delivery.Code)
	}
	if got := delivery.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Errorf("Cache-Control = %q, want private, no-store", got)
	}
}

func TestAPrivateAssetInAnotherNamespaceIsIndistinguishableFromNothing(t *testing.T) {
	h := newHarness(t)

	// A key with no read scope at all.
	stranger := h.do(t, http.MethodPost, "/v1/assets?namespace=docs&filename=secret.txt&visibility=private", h.writer, "classified")
	key := decode(t, stranger).Key

	// The reader can see it; a caller with a valid but unrelated credential
	// gets the same answer a missing asset gets.
	outsider := newHarness(t)
	w := outsider.do(t, http.MethodGet, "/v1/assets/"+key, outsider.reader, "")
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestUnknownAssetIsNotFound(t *testing.T) {
	h := newHarness(t)
	for _, target := range []string{"/v1/assets/docs/missing-000000000000.txt", "/a/docs/missing-000000000000.txt"} {
		if w := h.do(t, http.MethodGet, target, h.writer, ""); w.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", target, w.Code)
		}
	}
}

func TestBadCredentialsAreRejectedAtTheEdge(t *testing.T) {
	h := newHarness(t)
	// Even on a route that would otherwise serve anonymous callers.
	w := h.do(t, http.MethodGet, "/healthz", "asset_0000000000000000_wrong", "")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}

func TestHealthAndReadiness(t *testing.T) {
	h := newHarness(t)
	for _, target := range []string{"/healthz", "/readyz"} {
		w := h.do(t, http.MethodGet, target, "", "")
		if w.Code != http.StatusOK {
			t.Errorf("GET %s = %d (%s)", target, w.Code, w.Body.String())
		}
	}
}

func TestEveryResponseCarriesARequestID(t *testing.T) {
	h := newHarness(t)
	w := h.do(t, http.MethodGet, "/healthz", "", "")
	if w.Header().Get("X-Request-Id") == "" {
		t.Error("no request id on the response")
	}
}

// An image is queued for a ladder at upload time; nothing in this harness runs
// the worker, so the manifest should say so rather than pretend to be done.
func TestAnImageUploadReportsItsLadderAsPending(t *testing.T) {
	h := newHarness(t)

	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 64)
	r := httptest.NewRequest(http.MethodPost,
		"/v1/assets?namespace=docs&filename=diagram.png", strings.NewReader(png))
	r.Header.Set("Authorization", "Bearer "+h.writer)
	r.Header.Set("Content-Type", "image/png")
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d (%s)", w.Code, w.Body.String())
	}
	body := decode(t, w)
	if body.RenditionsStatus != assets.LadderPending {
		t.Errorf("renditions_status = %q, want pending", body.RenditionsStatus)
	}
	if len(body.Renditions) != 1 || body.Renditions[0].Name != original {
		t.Errorf("renditions = %+v, want the original alone until the worker runs", body.Renditions)
	}
}

// A manifest URL ends in the asset's own extension, so anything in front of
// this service may take it for an image and cache it. It must say not to.
func TestManifestsAreNotCacheable(t *testing.T) {
	h := newHarness(t)
	key := decode(t, h.do(t, http.MethodPost, "/v1/assets?namespace=docs&filename=note.txt", h.writer, "hello")).Key

	for _, target := range []string{"/v1/assets/" + key, "/healthz"} {
		w := h.do(t, http.MethodGet, target, "", "")
		if got := w.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("GET %s Cache-Control = %q, want no-store", target, got)
		}
	}

	// The delivery redirect is the exception: it is allowed to be cached,
	// because the key it points at can only ever mean one file.
	delivery := h.do(t, http.MethodGet, "/a/"+key, "", "")
	if got := delivery.Header().Get("Cache-Control"); !strings.HasPrefix(got, "public") {
		t.Errorf("delivery Cache-Control = %q, want it cacheable", got)
	}
}
