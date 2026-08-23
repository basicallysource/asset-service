package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// A camera writes where it stood into the file it produces, so what is
// published is a copy without it and the original is served the way a private
// asset is: only to someone who may read it, over a URL that expires.
func TestACameraOriginalIsNotWhatGetsPublished(t *testing.T) {
	h := newHarness(t)

	created := h.upload(t, h.writer, "/v1/assets?namespace=docs&filename=photo.png", "image/png", testPNG(t))
	if created.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", created.Code, created.Body)
	}
	key := decode(t, created).Key

	// Storage itself must not serve it. Handing out a different URL would not
	// be enough: what makes an object readable by anyone is its ACL.
	if public, exists := h.store.Public(key); !exists || public {
		t.Errorf("the original is stored public=%v (exists=%v); a camera original must not be", public, exists)
	}

	// Nothing publishable exists yet, and the original is not the fallback.
	anonymous := decode(t, h.do(t, http.MethodGet, "/v1/assets/"+key, "", ""))
	if anonymous.URL != "" {
		t.Errorf("url before the ladder is %q, want empty", anonymous.URL)
	}
	for _, r := range anonymous.Renditions {
		if r.Name == original {
			t.Errorf("an anonymous reader was handed the original at %q", r.URL)
		}
	}
	if res := h.do(t, http.MethodGet, "/a/"+key, "", ""); res.Code != http.StatusNotFound {
		t.Errorf("delivery before the ladder = %d, want 404 rather than a redirect to the original", res.Code)
	}

	// The worker makes the copy that may be published.
	_, job := claim(t, h, h.worker)
	if job == nil {
		t.Fatal("nothing to claim, but an image was just uploaded")
	}
	send := h.upload(t, h.worker,
		"/v1/jobs/renditions?key="+key+"&name=full&width=40&height=30&ext=.png",
		"image/png", testPNG(t))
	if send.Code != http.StatusNoContent {
		t.Fatalf("storing the copy: %d %s", send.Code, send.Body)
	}
	if done := h.do(t, http.MethodPost, "/v1/jobs/finish?key="+key, h.worker, `{}`); done.Code != http.StatusNoContent {
		t.Fatalf("finishing: %d %s", done.Code, done.Body)
	}

	// Now there is something to publish, and it is not the original.
	anonymous = decode(t, h.do(t, http.MethodGet, "/v1/assets/"+key, "", ""))
	var full string
	for _, r := range anonymous.Renditions {
		if r.Name == original {
			t.Errorf("an anonymous reader was handed the original at %q", r.URL)
		}
		if r.Name == "full" {
			full = r.URL
		}
	}
	if full == "" {
		t.Fatal("the published copy is not in the ladder")
	}
	if anonymous.URL != full || anonymous.URLExpires {
		t.Errorf("url = %q (expires %v), want the published copy at %q with a stable URL",
			anonymous.URL, anonymous.URLExpires, full)
	}

	res := h.do(t, http.MethodGet, "/a/"+key, "", "")
	if res.Code != http.StatusFound || res.Header().Get("Location") != full {
		t.Errorf("delivery = %d to %q, want 302 to the published copy", res.Code, res.Header().Get("Location"))
	}

	// A reader that may read the namespace can still get the bytes as they
	// were uploaded, over a URL that expires and must not be published.
	reader := decode(t, h.do(t, http.MethodGet, "/v1/assets/"+key, h.reader, ""))
	var found bool
	for _, r := range reader.Renditions {
		if r.Name != original {
			continue
		}
		found = true
		if !r.URLExpires {
			t.Errorf("the original was offered at %q without saying it expires", r.URL)
		}
	}
	if !found {
		t.Error("a reader of the namespace was not offered the original at all")
	}
}

func TestAnAssetThatIsItsOwnDeliverableIsUnchanged(t *testing.T) {
	h := newHarness(t)

	// A model, a text file, an archive: nothing a camera wrote, and the bytes
	// as uploaded are what a reader wants.
	created := h.upload(t, h.writer, "/v1/assets?namespace=docs&filename=notes.txt", "text/plain", "nothing to hide")
	if created.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", created.Code, created.Body)
	}
	key := decode(t, created).Key

	if public, exists := h.store.Public(key); !exists || !public {
		t.Errorf("a text file is stored public=%v, want public", public)
	}

	var got map[string]any
	res := h.do(t, http.MethodGet, "/v1/assets/"+key, "", "")
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["url"] == "" || got["url_expires"] == true {
		t.Errorf("url = %v, url_expires = %v; want a stable public URL", got["url"], got["url_expires"])
	}
	if res := h.do(t, http.MethodGet, "/a/"+key, "", ""); res.Code != http.StatusFound {
		t.Errorf("delivery = %d, want a redirect", res.Code)
	}
}
