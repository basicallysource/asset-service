package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/httpx"
	"github.com/basicallysource/asset-service/internal/policy"
)

// Credentials can be managed over the API as well as on the host, so that
// running this service does not require a shell on the machine it runs on.
//
// The rule that makes that safe: a principal can only mint what it already
// administers. admin:web can create a key for web and cannot create one for
// parts, so no chain of key creation ever widens access. A key belongs to the
// account that minted it, so whatever that account may not do, the keys it
// mints may not do either.

// maxKeyLifetime bounds how long an issued credential can last. A key with no
// end date is a key nobody will ever get around to rotating.
const maxKeyLifetime = 365 * 24 * time.Hour

type keyBody struct {
	Name      string     `json:"name"`
	Scopes    []string   `json:"scopes"`
	Account   string     `json:"account,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Revoked   bool       `json:"revoked"`
}

func (s *Server) mintKey(w http.ResponseWriter, r *http.Request) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	var body struct {
		Name         string   `json:"name"`
		Scopes       []string `json:"scopes"`
		ExpiresInDay int      `json:"expires_in_days"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, signInBodyLimit)).Decode(&body); err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest,
			`send {"name": "...", "scopes": ["write:docs"], "expires_in_days": 90}`)
		return
	}
	if body.Name == "" || len(body.Scopes) == 0 {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "a name and at least one scope are required")
		return
	}
	for _, scope := range body.Scopes {
		if !auth.ValidScope(scope) {
			httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest,
				"scope "+scope+" is not <read|write|admin>:<namespace>")
			return
		}
	}
	if !principal.CanGrant(body.Scopes) {
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden,
			"a key can only be given scopes its creator already administers")
		return
	}

	lifetime := time.Duration(body.ExpiresInDay) * 24 * time.Hour
	if lifetime <= 0 || lifetime > maxKeyLifetime {
		lifetime = maxKeyLifetime
	}

	token, id, secretHash, err := auth.NewToken()
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	now := time.Now().UTC()
	expires := now.Add(lifetime)
	record := catalog.APIKey{
		ID:         id,
		Name:       body.Name,
		SecretHash: secretHash,
		Scopes:     body.Scopes,
		// The new key belongs to whoever asked for it, so its usage counts
		// against them rather than becoming untracked capacity.
		AccountID: principal.Account,
		CreatedAt: now,
		ExpiresAt: expires,
	}
	if err := s.Catalog.InsertAPIKey(r.Context(), record); err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	s.Logger.Info("minted a key",
		"name", body.Name, "scopes", body.Scopes, "by", principal.Name, "account", principal.Account)

	httpx.JSON(w, http.StatusCreated, struct {
		Token string `json:"token"`
		keyBody
	}{
		Token: token,
		keyBody: keyBody{
			Name: record.Name, Scopes: record.Scopes, Account: record.AccountID,
			CreatedAt: now, ExpiresAt: &expires,
		},
	})
}

func (s *Server) listKeys(w http.ResponseWriter, r *http.Request) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	keys, err := s.Catalog.ListAPIKeys(r.Context())
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	manages, err := s.keysManagedBy(r.Context(), principal)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	// Show only what this principal may revoke. Someone who administers one
	// namespace has no business enumerating everyone else's credentials.
	listed := make([]keyBody, 0, len(keys))
	for _, key := range keys {
		if !manages(key) {
			continue
		}
		body := keyBody{
			Name: key.Name, Scopes: key.Scopes, Account: key.AccountID,
			CreatedAt: key.CreatedAt, Revoked: key.Revoked,
		}
		if !key.ExpiresAt.IsZero() {
			expires := key.ExpiresAt
			body.ExpiresAt = &expires
		}
		listed = append(listed, body)
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"keys": listed})
}

func (s *Server) revokeKey(w http.ResponseWriter, r *http.Request) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	name := r.PathValue("name")
	revoked, err := s.revoke(r.Context(), principal, name)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	if !revoked {
		// A key this principal may not revoke is a key it may not know about
		// either.
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, "no such key")
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]string{"revoked": name})
}

// keysManagedBy is the one rule for which keys a principal may see and revoke:
// keys whose every scope it administers, and -- unless its account may revoke
// anybody's -- only those of its own account.
func (s *Server) keysManagedBy(ctx context.Context, principal *auth.Principal) (func(catalog.APIKey) bool, error) {
	tier, err := s.tierOf(ctx, principal)
	if err != nil {
		return nil, err
	}
	anyKey := policy.RevokesAnyKey(tier)
	return func(key catalog.APIKey) bool {
		if !principal.CanGrant(key.Scopes) {
			return false
		}
		return anyKey || key.AccountID == principal.Account
	}, nil
}

// revoke makes one key stop working, if this principal may revoke it. It
// reports false for a key that does not exist, is already revoked, or is not
// this principal's to revoke, without saying which. Both the API and the page
// come through here.
func (s *Server) revoke(ctx context.Context, principal *auth.Principal, name string) (bool, error) {
	keys, err := s.Catalog.ListAPIKeys(ctx)
	if err != nil {
		return false, err
	}
	manages, err := s.keysManagedBy(ctx, principal)
	if err != nil {
		return false, err
	}

	for _, key := range keys {
		if key.Name != name {
			continue
		}
		if !manages(key) {
			return false, nil
		}
		revoked, err := s.Catalog.RevokeAPIKey(ctx, name)
		if err != nil || !revoked {
			return false, err
		}
		s.Logger.Info("revoked a key", "name", name, "by", principal.Name)
		return true, nil
	}
	return false, nil
}
