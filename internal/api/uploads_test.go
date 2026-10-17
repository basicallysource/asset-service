package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func (h *harness) open(t *testing.T, token string, length int) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/v1/uploads?namespace=docs&filename=note.txt", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Set("Upload-Length", strconv.Itoa(length))
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func (h *harness) part(t *testing.T, token, id string, offset int, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPatch, "/v1/uploads/"+id, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Upload-Offset", strconv.Itoa(offset))
	w := httptest.NewRecorder()
	h.handler.ServeHTTP(w, r)
	return w
}

func openedID(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	if w.Code != http.StatusCreated {
		t.Fatalf("open: %d %s", w.Code, w.Body.String())
	}
	var s uploadState
	if err := json.Unmarshal(w.Body.Bytes(), &s); err != nil || s.ID == "" {
		t.Fatalf("open answered %s", w.Body.String())
	}
	return s.ID
}

func TestAnUploadInPartsStoresWhatOneBodyWould(t *testing.T) {
	h := newHarness(t)
	id := openedID(t, h.open(t, h.writer, 11))

	for _, p := range []struct {
		offset int
		body   string
	}{{0, "hello "}, {6, "world"}} {
		w := h.part(t, h.writer, id, p.offset, p.body)
		if w.Code != http.StatusNoContent || w.Header().Get("Upload-Offset") != strconv.Itoa(p.offset+len(p.body)) {
			t.Fatalf("part at %d: %d, offset %q", p.offset, w.Code, w.Header().Get("Upload-Offset"))
		}
	}
	w := h.do(t, http.MethodPost, "/v1/uploads/"+id+"/finish", h.writer, "")
	if w.Code != http.StatusCreated {
		t.Fatalf("finish: %d %s", w.Code, w.Body.String())
	}
	parts := decode(t, w)

	// The same bytes in one body are the same asset, already there.
	w = h.upload(t, h.writer, "/v1/assets?namespace=docs&filename=note.txt", "text/plain", "hello world")
	if w.Code != http.StatusOK {
		t.Fatalf("one body after the parts: %d, want 200", w.Code)
	}
	if one := decode(t, w); one.Key != parts.Key || one.Size != 11 {
		t.Errorf("parts made %s (%d bytes), one body %s", parts.Key, parts.Size, one.Key)
	}

	// A finished upload is gone.
	if w := h.part(t, h.writer, id, 11, "!"); w.Code != http.StatusNotFound {
		t.Errorf("a part after the finish: %d, want 404", w.Code)
	}
}

func TestAnUploadTakesItsBytesInOrderAndWhole(t *testing.T) {
	h := newHarness(t)
	id := openedID(t, h.open(t, h.writer, 5))

	if w := h.part(t, h.writer, id, 2, "llo"); w.Code != http.StatusConflict || w.Header().Get("Upload-Offset") != "0" {
		t.Errorf("a part ahead of the upload: %d, offset %q", w.Code, w.Header().Get("Upload-Offset"))
	}
	if w := h.part(t, h.writer, id, 0, "hello world"); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("a part past the length: %d, want 413", w.Code)
	}
	if w := h.do(t, http.MethodPost, "/v1/uploads/"+id+"/finish", h.writer, ""); w.Code != http.StatusConflict {
		t.Errorf("a finish with nothing here: %d, want 409", w.Code)
	}
	// What was refused left nothing behind: the whole part still fits.
	if w := h.part(t, h.writer, id, 0, "hello"); w.Code != http.StatusNoContent {
		t.Fatalf("the part again: %d %s", w.Code, w.Body.String())
	}
	if w := h.do(t, http.MethodPost, "/v1/uploads/"+id+"/finish", h.writer, ""); w.Code != http.StatusCreated || decode(t, w).Size != 5 {
		t.Errorf("finish: %d %s", w.Code, w.Body.String())
	}
}

func TestAnUploadIsOnlyItsOpeners(t *testing.T) {
	h := newHarness(t)
	if w := h.open(t, h.reader, 5); w.Code != http.StatusForbidden {
		t.Errorf("a read-only key opened an upload: %d", w.Code)
	}
	if w := h.open(t, h.writer, 1<<20); w.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("an upload larger than the service takes: %d, want 413", w.Code)
	}

	id := openedID(t, h.open(t, h.writer, 5))
	if w := h.part(t, h.reader, id, 0, "hello"); w.Code != http.StatusNotFound {
		t.Errorf("somebody else's part: %d, want 404", w.Code)
	}
	if w := h.do(t, http.MethodPost, "/v1/uploads/"+id+"/finish", h.reader, ""); w.Code != http.StatusNotFound {
		t.Errorf("somebody else's finish: %d, want 404", w.Code)
	}
	if w := h.part(t, "", id, 0, "hello"); w.Code != http.StatusUnauthorized {
		t.Errorf("an anonymous part: %d, want 401", w.Code)
	}
}
