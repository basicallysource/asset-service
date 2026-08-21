// Package auth answers two questions: who is making this request, and may they
// do this to that namespace.
//
// The only credential today is an API key, which is what a build or a person
// with a script uses. Everything a handler sees is the Principal, so adding a
// second kind of credential later -- a signed-in user, a session from another
// service -- is a new Authenticator, not a change to any handler.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
)

// Actions a principal can hold on a namespace.
const (
	ActionRead  = "read"
	ActionWrite = "write"
)

// TokenPrefix marks this service's API keys wherever one turns up.
const TokenPrefix = "asset"

// Errors an Authenticator returns. Absent credentials are not an error: an
// anonymous request is legitimate, since public assets need no credential.
var (
	ErrInvalidCredentials = errors.New("auth: invalid credentials")
	ErrRevoked            = errors.New("auth: credential revoked")
)

// Principal is the identity behind a request.
type Principal struct {
	// ID is stable and safe to log. Name is human-chosen.
	ID     string
	Name   string
	Scopes []string
}

// Can reports whether the principal may perform action on namespace. A scope
// is "<action>:<namespace>", and the namespace may be "*".
func (p *Principal) Can(action, namespace string) bool {
	if p == nil {
		return false
	}
	for _, scope := range p.Scopes {
		want, ns, ok := strings.Cut(scope, ":")
		if !ok || want != action {
			continue
		}
		if ns == "*" || ns == namespace {
			return true
		}
	}
	return false
}

// ValidScope reports whether a scope string is well formed. The namespace
// half is opaque here -- what makes a legal namespace is a storage question,
// enforced where keys are built. A scope naming a namespace that cannot exist
// simply never matches anything.
func ValidScope(scope string) bool {
	action, ns, ok := strings.Cut(scope, ":")
	if !ok || (action != ActionRead && action != ActionWrite) {
		return false
	}
	return ns != "" && !strings.ContainsAny(ns, ": \t")
}

// Authenticator turns a request into a Principal. It returns (nil, nil) when
// the request carries no credentials at all.
type Authenticator interface {
	Authenticate(ctx context.Context, r *http.Request) (*Principal, error)
}

// KeyStore is the slice of the catalog this package needs.
type KeyStore interface {
	APIKeyByID(ctx context.Context, id string) (StoredKey, error)
}

// StoredKey is a credential as the store holds it.
type StoredKey struct {
	ID         string
	Name       string
	SecretHash string
	Scopes     []string
	Revoked    bool
}

// APIKeys authenticates bearer tokens against a KeyStore.
type APIKeys struct {
	Keys KeyStore
}

var _ Authenticator = (*APIKeys)(nil)

// Authenticate reads the Authorization header and resolves it to a Principal.
func (a *APIKeys) Authenticate(ctx context.Context, r *http.Request) (*Principal, error) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return nil, nil
	}
	token, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return nil, ErrInvalidCredentials
	}

	id, secret, ok := ParseToken(strings.TrimSpace(token))
	if !ok {
		return nil, ErrInvalidCredentials
	}

	stored, err := a.Keys.APIKeyByID(ctx, id)
	if err != nil {
		// A missing id and a wrong secret are the same answer on purpose.
		return nil, ErrInvalidCredentials
	}
	if subtle.ConstantTimeCompare([]byte(HashSecret(secret)), []byte(stored.SecretHash)) != 1 {
		return nil, ErrInvalidCredentials
	}
	if stored.Revoked {
		return nil, ErrRevoked
	}

	return &Principal{ID: stored.ID, Name: stored.Name, Scopes: stored.Scopes}, nil
}

// NewToken mints a credential. It returns the token to hand over once, the id
// to store in the clear, and the hash to store in place of the secret.
func NewToken() (token, id, secretHash string, err error) {
	idBytes := make([]byte, 8)
	secretBytes := make([]byte, 32)
	if _, err = rand.Read(idBytes); err != nil {
		return "", "", "", err
	}
	if _, err = rand.Read(secretBytes); err != nil {
		return "", "", "", err
	}

	id = hex.EncodeToString(idBytes)
	secret := base64.RawURLEncoding.EncodeToString(secretBytes)
	return TokenPrefix + "_" + id + "_" + secret, id, HashSecret(secret), nil
}

// ParseToken splits a token into its id and secret halves.
func ParseToken(token string) (id, secret string, ok bool) {
	parts := strings.SplitN(token, "_", 3)
	if len(parts) != 3 || parts[0] != TokenPrefix || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}

// HashSecret is how a token's secret half is stored. A plain SHA-256 is
// correct here and a password KDF would not be: the secret is 32 bytes from
// crypto/rand, so there is no dictionary to run against it.
func HashSecret(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
