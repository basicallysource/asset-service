package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/basicallysource/asset-service/internal/identity"
)

// fakeGitHub stands in for github.com. approved flips when the person has
// entered the code, which is what the device flow is waiting for.
type fakeGitHub struct {
	approved bool
	login    string
	id       int64
}

func (f *fakeGitHub) server(t *testing.T) *identity.GitHub {
	t.Helper()

	mux := http.NewServeMux()
	mux.HandleFunc("/login/device/code", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"device_code": "device-abc", "user_code": "WDJB-MJHT",
			"verification_uri": "https://github.com/login/device",
			"expires_in":       900, "interval": 1,
		})
	})
	mux.HandleFunc("/login/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if !f.approved {
			json.NewEncoder(w).Encode(map[string]string{"error": "authorization_pending"})
			return
		}
		json.NewEncoder(w).Encode(map[string]string{"access_token": "gho_fake"})
	})
	mux.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer gho_fake" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": f.id, "login": f.login})
	})

	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	return &identity.GitHub{
		ClientID:  "test-client",
		DeviceURL: server.URL + "/login/device/code",
		TokenURL:  server.URL + "/login/oauth/access_token",
		UserURL:   server.URL + "/user",
	}
}

// signIn runs the whole device flow and returns the issued token response.
func (h *harness) signIn(t *testing.T, github *fakeGitHub) tokenResponse {
	t.Helper()

	start := h.do(t, http.MethodPost, "/v1/auth/github/start", "", "")
	if start.Code != http.StatusOK {
		t.Fatalf("start = %d (%s)", start.Code, start.Body.String())
	}
	var device startResponse
	if err := json.Unmarshal(start.Body.Bytes(), &device); err != nil {
		t.Fatal(err)
	}
	if device.UserCode == "" {
		t.Fatal("no user code to show anybody")
	}

	body := `{"device_code":"` + device.DeviceCode + `"}`
	if pending := h.do(t, http.MethodPost, "/v1/auth/github/token", "", body); pending.Code != http.StatusAccepted {
		t.Fatalf("before approval = %d, want 202 so the caller keeps polling", pending.Code)
	}

	github.approved = true
	issued := h.do(t, http.MethodPost, "/v1/auth/github/token", "", body)
	if issued.Code != http.StatusCreated {
		t.Fatalf("after approval = %d (%s)", issued.Code, issued.Body.String())
	}

	var token tokenResponse
	if err := json.Unmarshal(issued.Body.Bytes(), &token); err != nil {
		t.Fatal(err)
	}
	return token
}

func TestSigningInGivesAConfinedToken(t *testing.T) {
	h := newHarness(t)
	github := &fakeGitHub{login: "octocat", id: 583231}
	h.server.Identity = github.server(t)
	h.handler = h.server.Handler()

	issued := h.signIn(t, github)

	if issued.Handle != "octocat" || issued.Account != "github:583231" {
		t.Errorf("issued to %+v", issued)
	}
	if issued.Namespace != "u-octocat" {
		t.Errorf("namespace = %q, want u-octocat", issued.Namespace)
	}
	if strings.Join(issued.Scopes, " ") != "write:u-octocat read:u-octocat" {
		t.Errorf("scopes = %v, want only its own namespace", issued.Scopes)
	}
	if issued.ExpiresAt == nil {
		t.Error("a self-served token was issued with no expiry")
	}
	if len(issued.Limits.ContentTypes) == 0 || issued.Limits.UploadsPerHour == 0 {
		t.Errorf("limits = %+v, want the caller told what they are", issued.Limits)
	}

	// It works where it should.
	png := "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 32)
	w := h.upload(t, issued.Token, "/v1/assets?namespace=u-octocat&filename=a.png", "image/png", png)
	if w.Code != http.StatusCreated {
		t.Errorf("upload to its own namespace = %d (%s)", w.Code, w.Body.String())
	}

	// And nowhere else.
	w = h.upload(t, issued.Token, "/v1/assets?namespace=docs&filename=a.png", "image/png", png)
	if w.Code != http.StatusForbidden {
		t.Errorf("upload to somebody else's namespace = %d, want 403", w.Code)
	}
}

func TestAnUnknownAccountIsHeldToItsLimits(t *testing.T) {
	h := newHarness(t)
	github := &fakeGitHub{login: "octocat", id: 583231}
	h.server.Identity = github.server(t)
	h.handler = h.server.Handler()

	issued := h.signIn(t, github)

	// Content types it may not upload.
	w := h.upload(t, issued.Token, "/v1/assets?namespace=u-octocat&filename=run.sh", "application/x-sh", "#!/bin/sh\n")
	if w.Code != http.StatusUnsupportedMediaType {
		t.Errorf("shell script upload = %d, want 415 (%s)", w.Code, w.Body.String())
	}

	// Files larger than it may store. The body is declared, not sent, so this
	// is refused before anything is transferred.
	big := httptest.NewRequest(http.MethodPost,
		"/v1/assets?namespace=u-octocat&filename=huge.png", strings.NewReader("x"))
	big.Header.Set("Authorization", "Bearer "+issued.Token)
	big.Header.Set("Content-Type", "image/png")
	big.ContentLength = 64 << 20
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, big)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized upload = %d, want 413 (%s)", recorder.Code, recorder.Body.String())
	}
}

func TestAnAdminSignInCanMintKeysAndAnOrdinaryOneCannot(t *testing.T) {
	h := newHarness(t)
	github := &fakeGitHub{login: "octocat", id: 583231}
	h.server.Identity = github.server(t)

	t.Run("ordinary account", func(t *testing.T) {
		h.handler = h.server.Handler()
		issued := h.signIn(t, github)

		w := h.do(t, http.MethodPost, "/v1/keys", issued.Token,
			`{"name":"mine","scopes":["write:u-octocat"]}`)
		if w.Code != http.StatusForbidden {
			t.Errorf("minting = %d, want 403: no admin scope (%s)", w.Code, w.Body.String())
		}
	})

	t.Run("configured admin", func(t *testing.T) {
		h.server.AdminLogins = []string{"OctoCat"} // case-insensitive on purpose
		h.handler = h.server.Handler()
		github.approved = false
		issued := h.signIn(t, github)

		if strings.Join(issued.Scopes, " ") != "write:* read:* admin:*" {
			t.Fatalf("admin scopes = %v", issued.Scopes)
		}

		w := h.do(t, http.MethodPost, "/v1/keys", issued.Token,
			`{"name":"docs-ci","scopes":["write:docs","read:docs"],"expires_in_days":30}`)
		if w.Code != http.StatusCreated {
			t.Fatalf("minting = %d (%s)", w.Code, w.Body.String())
		}

		var minted struct {
			Token string `json:"token"`
		}
		json.Unmarshal(w.Body.Bytes(), &minted)

		// The minted key works where it was granted, and nowhere else.
		png := "\x89PNG\r\n\x1a\n" + strings.Repeat("x", 32)
		if up := h.upload(t, minted.Token, "/v1/assets?namespace=docs&filename=a.png", "image/png", png); up.Code != http.StatusCreated {
			t.Errorf("minted key upload = %d (%s)", up.Code, up.Body.String())
		}
		if up := h.upload(t, minted.Token, "/v1/assets?namespace=parts&filename=a.png", "image/png", png); up.Code != http.StatusForbidden {
			t.Errorf("minted key reached another namespace: %d", up.Code)
		}

		// A key cannot hand out more than its maker holds.
		narrow := h.do(t, http.MethodPost, "/v1/keys", minted.Token,
			`{"name":"wider","scopes":["write:parts"]}`)
		if narrow.Code != http.StatusForbidden {
			t.Errorf("a key with no admin scope minted another: %d", narrow.Code)
		}
	})
}

func TestSignInIsOffWhenUnconfigured(t *testing.T) {
	h := newHarness(t)
	// No Identity at all.
	if w := h.do(t, http.MethodPost, "/v1/auth/github/start", "", ""); w.Code != http.StatusNotImplemented {
		t.Errorf("start = %d, want 501 when sign-in is not configured", w.Code)
	}
}

func TestTheLoginPageIsSelfContained(t *testing.T) {
	h := newHarness(t)

	page := h.do(t, http.MethodGet, "/login", "", "")
	if page.Code != http.StatusOK {
		t.Fatalf("GET /login = %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, "/login/htmx.js") {
		t.Error("the page does not load its script from this service")
	}
	// Nothing may be fetched from anywhere else: an admin page that depends on
	// a CDN is an admin page a CDN can change.
	for _, host := range []string{"unpkg.com", "cdn.jsdelivr.net", "googleapis.com"} {
		if strings.Contains(body, host) {
			t.Errorf("the page references %s", host)
		}
	}

	if script := h.do(t, http.MethodGet, "/login/htmx.js", "", ""); script.Code != http.StatusOK || script.Body.Len() < 1000 {
		t.Errorf("GET /login/htmx.js = %d, %d bytes", script.Code, script.Body.Len())
	}
}
