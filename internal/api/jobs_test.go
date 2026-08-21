package api

import (
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A worker on another machine sees exactly what one in this process sees: a
// job, a URL to the bytes, and somewhere to put what it made.

func testPNG(t *testing.T) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 40, 30))
	for y := range 30 {
		for x := range 40 {
			img.Set(x, y, color.RGBA{R: uint8(x * 6), G: uint8(y * 8), B: 30, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.String()
}

func claim(t *testing.T, h *harness, token string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	res := h.do(t, http.MethodPost, "/v1/jobs/claim", token, "")
	if res.Code != http.StatusOK {
		return res, nil
	}
	var job map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &job); err != nil {
		t.Fatalf("unreadable job: %v", err)
	}
	return res, job
}

func TestAWorkerClaimsUploadedWorkAndStoresWhatItMade(t *testing.T) {
	h := newHarness(t)

	upload := h.upload(t, h.writer, "/v1/assets?namespace=docs&filename=chart.png", "image/png", testPNG(t))
	if upload.Code != http.StatusCreated {
		t.Fatalf("upload: %d %s", upload.Code, upload.Body)
	}
	var stored map[string]any
	json.Unmarshal(upload.Body.Bytes(), &stored)
	key := stored["key"].(string)

	_, job := claim(t, h, h.worker)
	if job == nil {
		t.Fatal("nothing to claim, but an image was just uploaded")
	}
	if job["key"] != key {
		t.Errorf("claimed %v, want %s", job["key"], key)
	}
	if job["source_url"] == "" {
		t.Error("a worker cannot fetch the original without a URL to it")
	}

	// A second claim finds nothing: the first one is being worked.
	if res, _ := claim(t, h, h.worker); res.Code != http.StatusNoContent {
		t.Errorf("a claimed job was offered twice: %d", res.Code)
	}

	send := h.upload(t, h.worker,
		"/v1/jobs/renditions?key="+key+"&name=w320&width=320&height=240&ext=.webp",
		"image/webp", "pretend-these-are-webp-bytes")
	if send.Code != http.StatusNoContent {
		t.Fatalf("storing a rendition: %d %s", send.Code, send.Body)
	}

	if done := h.do(t, http.MethodPost, "/v1/jobs/finish?key="+key, h.worker, `{}`); done.Code != http.StatusNoContent {
		t.Fatalf("finishing: %d %s", done.Code, done.Body)
	}

	manifest := h.do(t, http.MethodGet, "/v1/assets/"+key, h.reader, "")
	var got map[string]any
	json.Unmarshal(manifest.Body.Bytes(), &got)
	if got["renditions_status"] != "ready" {
		t.Errorf("renditions_status is %v, want ready", got["renditions_status"])
	}
	renditions := got["renditions"].([]any)
	if len(renditions) != 2 {
		t.Fatalf("got %d renditions, want the one made plus the original", len(renditions))
	}
	first := renditions[0].(map[string]any)
	if first["name"] != "w320" || first["width"].(float64) != 320 {
		t.Errorf("first rendition is %v", first)
	}
}

func TestWorkingTheQueueTakesMoreThanAWriteScope(t *testing.T) {
	h := newHarness(t)

	if res := h.upload(t, h.writer, "/v1/assets?namespace=docs&filename=chart.png", "image/png", testPNG(t)); res.Code != http.StatusCreated {
		t.Fatalf("upload: %d", res.Code)
	}

	for _, token := range []string{h.writer, h.reader, ""} {
		res := h.do(t, http.MethodPost, "/v1/jobs/claim", token, "")
		if res.Code == http.StatusOK {
			t.Errorf("a token without admin claimed a job")
		}
	}
}

func TestAFailureIsReportedAndTheJobComesBack(t *testing.T) {
	h := newHarness(t)

	if res := h.upload(t, h.writer, "/v1/assets?namespace=docs&filename=chart.png", "image/png", testPNG(t)); res.Code != http.StatusCreated {
		t.Fatalf("upload: %d", res.Code)
	}
	_, job := claim(t, h, h.worker)
	key := job["key"].(string)

	fail := h.do(t, http.MethodPost, "/v1/jobs/finish?key="+key, h.worker,
		`{"error":"the encoder fell over"}`)
	if fail.Code != http.StatusNoContent {
		t.Fatalf("reporting a failure: %d %s", fail.Code, fail.Body)
	}

	manifest := h.do(t, http.MethodGet, "/v1/assets/"+key, h.reader, "")
	var got map[string]any
	json.Unmarshal(manifest.Body.Bytes(), &got)
	// Still pending, not failed: one bad attempt is worth another try.
	if got["renditions_status"] != "pending" {
		t.Errorf("renditions_status is %v, want pending after one failure", got["renditions_status"])
	}
}

func TestBytesThatWillNeverDeriveAreNotOfferedAgain(t *testing.T) {
	h := newHarness(t)

	if res := h.upload(t, h.writer, "/v1/assets?namespace=docs&filename=chart.png", "image/png", testPNG(t)); res.Code != http.StatusCreated {
		t.Fatalf("upload: %d", res.Code)
	}
	_, job := claim(t, h, h.worker)
	key := job["key"].(string)

	done := h.do(t, http.MethodPost, "/v1/jobs/finish?key="+key, h.worker,
		`{"error":"not an image","permanent":true}`)
	if done.Code != http.StatusNoContent {
		t.Fatalf("reporting: %d %s", done.Code, done.Body)
	}

	if res, _ := claim(t, h, h.worker); res.Code != http.StatusNoContent {
		t.Errorf("a permanently failed job was offered again: %d", res.Code)
	}
}
