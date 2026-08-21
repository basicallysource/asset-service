package assets

import (
	"errors"
	"strings"
	"testing"
)

const testDigest = "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"

func TestBuildKey(t *testing.T) {
	cases := []struct {
		name      string
		namespace string
		filename  string
		want      string
	}{
		{"plain", "docs", "diagram.png", "docs/diagram-9f86d081884c.png"},
		{"spaces and case", "docs", "My Wire Harness.PNG", "docs/my-wire-harness-9f86d081884c.png"},
		{"path is stripped", "docs", "/tmp/build/out/render.jpg", "docs/render-9f86d081884c.jpg"},
		{"windows path", "docs", `C:\build\render.jpg`, "docs/render-9f86d081884c.jpg"},
		{"no extension", "docs", "README", "docs/readme-9f86d081884c"},
		{"punctuation collapses", "docs", "a..b__c--d.txt", "docs/a-b-c-d-9f86d081884c.txt"},
		{"unicode drops out", "docs", "café.png", "docs/caf-9f86d081884c.png"},
		{"odd extension dropped", "docs", "archive.tar.gz~", "docs/archive-tar-9f86d081884c"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := BuildKey(c.namespace, c.filename, testDigest)
			if err != nil {
				t.Fatalf("BuildKey: %v", err)
			}
			if got != c.want {
				t.Errorf("BuildKey(%q, %q) = %q, want %q", c.namespace, c.filename, got, c.want)
			}
		})
	}
}

func TestBuildKeyRejectsBadInput(t *testing.T) {
	cases := map[string][2]string{
		"empty namespace":      {"", "a.png"},
		"uppercase namespace":  {"Docs", "a.png"},
		"namespace with slash": {"docs/sub", "a.png"},
		"namespace with dot":   {"docs.v2", "a.png"},
		"nameless file":        {"docs", "***"},
		"no filename":          {"docs", ""},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := BuildKey(c[0], c[1], testDigest); !errors.Is(err, ErrBadRequest) {
				t.Errorf("BuildKey(%q, %q) error = %v, want ErrBadRequest", c[0], c[1], err)
			}
		})
	}
}

func TestBuildKeyRequiresAFullDigest(t *testing.T) {
	for _, digest := range []string{"", "abc", strings.Repeat("z", 64), strings.ToUpper(testDigest)} {
		if _, err := BuildKey("docs", "a.png", digest); !errors.Is(err, ErrBadRequest) {
			t.Errorf("digest %q was accepted", digest)
		}
	}
}

func TestKeysAreLongNamesButNotEndlessOnes(t *testing.T) {
	key, err := BuildKey("docs", strings.Repeat("name", 40)+".png", testDigest)
	if err != nil {
		t.Fatal(err)
	}
	if name := strings.TrimPrefix(key, "docs/"); len(name) > 64+1+HashChars+4 {
		t.Errorf("key name is %d characters: %s", len(name), key)
	}
}

func TestNamespace(t *testing.T) {
	if got := Namespace("docs/diagram-9f86d081884c.png"); got != "docs" {
		t.Errorf("Namespace = %q, want docs", got)
	}
	if got := Namespace("nokey"); got != "" {
		t.Errorf("Namespace of a key with no namespace = %q, want empty", got)
	}
}

func TestTheServicesOwnRouteNamesAreNotNamespaces(t *testing.T) {
	// A namespace sharing a name with a top-level route makes a URL ambiguous
	// the moment anything serves assets and the API from one hostname.
	for _, reserved := range ReservedNamespaces {
		if ValidNamespace(reserved) {
			t.Errorf("%q is a route of this service and must not be a namespace", reserved)
		}
	}
	for _, fine := range []string{"web", "docs", "v2", "assets", "a-team"} {
		if !ValidNamespace(fine) {
			t.Errorf("%q should be a usable namespace", fine)
		}
	}
}
