package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// stubKeys is a KeyStore holding one credential.
type stubKeys struct {
	key StoredKey
	err error
}

func (s stubKeys) APIKeyByID(context.Context, string) (StoredKey, error) {
	if s.err != nil {
		return StoredKey{}, s.err
	}
	return s.key, nil
}

func TestTokenRoundTrip(t *testing.T) {
	token, id, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, TokenPrefix+"_") {
		t.Errorf("token %q lacks the %q prefix", token, TokenPrefix)
	}

	gotID, secret, ok := ParseToken(token)
	if !ok {
		t.Fatalf("ParseToken(%q) failed", token)
	}
	if gotID != id {
		t.Errorf("id = %q, want %q", gotID, id)
	}
	if HashSecret(secret) != hash {
		t.Error("the secret does not hash to what would be stored")
	}
	if strings.Contains(token, hash) {
		t.Error("the stored hash appears inside the token")
	}
}

func TestTokensAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		token, _, _, err := NewToken()
		if err != nil {
			t.Fatal(err)
		}
		if seen[token] {
			t.Fatal("NewToken repeated itself")
		}
		seen[token] = true
	}
}

func TestParseTokenRejectsMalformed(t *testing.T) {
	for _, token := range []string{
		"", "asset", "asset_", "asset_abc", "other_abc_def", "asset__secret", "asset_abc_",
	} {
		if _, _, ok := ParseToken(token); ok {
			t.Errorf("ParseToken(%q) accepted a malformed token", token)
		}
	}
}

func TestParseTokenKeepsSecretsContainingUnderscores(t *testing.T) {
	// Secrets are base64url, whose alphabet includes '_'. Splitting on every
	// underscore instead of the first two would truncate them.
	id, secret, ok := ParseToken("asset_0011223344556677_ab_cd-ef_gh")
	if !ok {
		t.Fatal("well-formed token rejected")
	}
	if id != "0011223344556677" || secret != "ab_cd-ef_gh" {
		t.Errorf("id = %q, secret = %q", id, secret)
	}
}

func TestCan(t *testing.T) {
	principal := &Principal{Scopes: []string{"write:docs", "read:*"}}

	cases := map[string]struct {
		action, namespace string
		want              bool
	}{
		"granted write":            {ActionWrite, "docs", true},
		"write elsewhere":          {ActionWrite, "parts", false},
		"read anywhere":            {ActionRead, "parts", true},
		"read the written one too": {ActionRead, "docs", true},
		"unknown action":           {"delete", "docs", false},
	}
	for name, c := range cases {
		if got := principal.Can(c.action, c.namespace); got != c.want {
			t.Errorf("%s: Can(%q, %q) = %v", name, c.action, c.namespace, got)
		}
	}

	var anonymous *Principal
	if anonymous.Can(ActionRead, "docs") {
		t.Error("an anonymous principal was granted something")
	}
}

func TestCanIgnoresMalformedScopes(t *testing.T) {
	principal := &Principal{Scopes: []string{"docs", "write", ":docs", "write:"}}
	if principal.Can(ActionWrite, "docs") || principal.Can(ActionRead, "docs") {
		t.Error("a malformed scope granted access")
	}
}

func TestValidScope(t *testing.T) {
	for _, scope := range []string{"read:docs", "write:docs", "read:*", "write:*"} {
		if !ValidScope(scope) {
			t.Errorf("ValidScope(%q) = false", scope)
		}
	}
	for _, scope := range []string{"", "docs", "delete:docs", "read:", "read:a b", "read:a:b"} {
		if ValidScope(scope) {
			t.Errorf("ValidScope(%q) = true", scope)
		}
	}
}

func newRequest(t *testing.T, authorization string) *http.Request {
	t.Helper()
	r, err := http.NewRequest(http.MethodGet, "/v1/assets/docs/a.png", nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		r.Header.Set("Authorization", authorization)
	}
	return r
}

func TestAuthenticate(t *testing.T) {
	token, id, hash, err := NewToken()
	if err != nil {
		t.Fatal(err)
	}
	stored := StoredKey{ID: id, Name: "ci", SecretHash: hash, Scopes: []string{"write:docs"}}
	authenticator := &APIKeys{Keys: stubKeys{key: stored}}
	ctx := context.Background()

	t.Run("valid token", func(t *testing.T) {
		principal, err := authenticator.Authenticate(ctx, newRequest(t, "Bearer "+token))
		if err != nil {
			t.Fatalf("Authenticate: %v", err)
		}
		if principal == nil || principal.Name != "ci" || !principal.Can(ActionWrite, "docs") {
			t.Errorf("principal = %+v", principal)
		}
	})

	t.Run("no credentials is anonymous, not an error", func(t *testing.T) {
		principal, err := authenticator.Authenticate(ctx, newRequest(t, ""))
		if err != nil || principal != nil {
			t.Errorf("principal = %+v, err = %v", principal, err)
		}
	})

	t.Run("wrong secret", func(t *testing.T) {
		wrong := "asset_" + id + "_notthesecret"
		if _, err := authenticator.Authenticate(ctx, newRequest(t, "Bearer "+wrong)); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("err = %v, want ErrInvalidCredentials", err)
		}
	})

	t.Run("unknown key", func(t *testing.T) {
		missing := &APIKeys{Keys: stubKeys{err: errors.New("not found")}}
		if _, err := missing.Authenticate(ctx, newRequest(t, "Bearer "+token)); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("err = %v, want ErrInvalidCredentials", err)
		}
	})

	t.Run("revoked key", func(t *testing.T) {
		revoked := stored
		revoked.Revoked = true
		gone := &APIKeys{Keys: stubKeys{key: revoked}}
		if _, err := gone.Authenticate(ctx, newRequest(t, "Bearer "+token)); !errors.Is(err, ErrRevoked) {
			t.Errorf("err = %v, want ErrRevoked", err)
		}
	})

	t.Run("not a bearer scheme", func(t *testing.T) {
		if _, err := authenticator.Authenticate(ctx, newRequest(t, "Basic abc")); !errors.Is(err, ErrInvalidCredentials) {
			t.Errorf("err = %v, want ErrInvalidCredentials", err)
		}
	})
}
