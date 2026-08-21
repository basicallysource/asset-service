package objstore

import (
	"net/http"
	"testing"
	"time"
)

// The expected values below were produced by botocore, an independent
// implementation of SigV4, against the same inputs. They are what makes these
// tests worth having: agreeing with itself would prove nothing about a signer.
//
// Regenerate with a frozen clock if the signed header set ever changes. The
// presigned case must come from botocore's S3 signer specifically: S3 signs a
// query-authenticated GET over UNSIGNED-PAYLOAD, where the generic signer uses
// the hash of an empty body and produces a different, wrong signature.
const (
	testAccessKey = "AKIDEXAMPLE"
	testSecretKey = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
	testRegion    = "nyc3"
	testURL       = "https://example-assets.nyc3.example.com/docs/note-9f86d081884c.txt"
	testDigest    = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

	wantAuthorization = "AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260821/nyc3/s3/aws4_request, " +
		"SignedHeaders=content-type;host;x-amz-acl;x-amz-content-sha256;x-amz-date;x-amz-meta-digest, " +
		"Signature=b807f5b81605e55475540e4904909ebc556225409e8c324f72f2447fee4ca4c6"

	wantPresigned = testURL +
		"?X-Amz-Algorithm=AWS4-HMAC-SHA256" +
		"&X-Amz-Credential=AKIDEXAMPLE%2F20260821%2Fnyc3%2Fs3%2Faws4_request" +
		"&X-Amz-Date=20260821T123456Z" +
		"&X-Amz-Expires=900" +
		"&X-Amz-SignedHeaders=host" +
		"&X-Amz-Signature=cf49d388bd87060b5c9d87f85abb000305d37495eecd4dfe7b7b70f58499c3eb"
)

func testTime() time.Time { return time.Date(2026, 8, 21, 12, 34, 56, 0, time.UTC) }

func testCredentials() credentials {
	return credentials{AccessKey: testAccessKey, SecretKey: testSecretKey}
}

func TestSignRequestMatchesReferenceImplementation(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, testURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("X-Amz-Acl", "public-read")
	req.Header.Set(digestMeta, testDigest)

	signRequest(req, testCredentials(), testRegion, service, testDigest, testTime())

	if got := req.Header.Get("X-Amz-Date"); got != "20260821T123456Z" {
		t.Errorf("X-Amz-Date = %q", got)
	}
	if got := req.Header.Get("Authorization"); got != wantAuthorization {
		t.Errorf("Authorization mismatch\n got: %s\nwant: %s", got, wantAuthorization)
	}
}

func TestPresignMatchesReferenceImplementation(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, testURL, nil)
	if err != nil {
		t.Fatal(err)
	}

	got := presign(http.MethodGet, req.URL, testCredentials(), testRegion, service, 15*time.Minute, testTime())
	if got != wantPresigned {
		t.Errorf("presigned URL mismatch\n got: %s\nwant: %s", got, wantPresigned)
	}
}

func TestURIEncode(t *testing.T) {
	cases := map[string]struct {
		in          string
		encodeSlash bool
		want        string
	}{
		"unreserved survive":            {"a-z0.9_~", true, "a-z0.9_~"},
		"slash kept in paths":           {"docs/note.txt", false, "docs/note.txt"},
		"slash encoded in query values": {"a/b", true, "a%2Fb"},
		"space and plus":                {"a b+c", true, "a%20b%2Bc"},
		"utf8 is bytes":                 {"é", true, "%C3%A9"},
	}
	for name, c := range cases {
		if got := uriEncode(c.in, c.encodeSlash); got != c.want {
			t.Errorf("%s: uriEncode(%q) = %q, want %q", name, c.in, got, c.want)
		}
	}
}

func TestSignedHeadersCoverAmzAndContentType(t *testing.T) {
	req, err := http.NewRequest(http.MethodPut, testURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Cache-Control", "public, max-age=31536000, immutable")
	req.Header.Set("X-Amz-Acl", "private")
	req.Host = req.URL.Host

	signed, _ := canonicalHeaders(req)
	if want := "content-type;host;x-amz-acl"; signed != want {
		// Cache-Control is deliberately unsigned: storage honours it either
		// way, and signing every header breaks behind any proxy that adds one.
		t.Errorf("signed headers = %q, want %q", signed, want)
	}
}
