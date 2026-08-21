package api

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/httpx"
)

// Credentials can be managed over the API as well as on the host, so that
// running this service does not require a shell on the machine it runs on.
//
// The rule that makes that safe: a principal can only mint what it already
// administers. admin:web can create a key for web and cannot create one for
// parts, so no chain of key creation ever widens access.

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

	// Show only what this principal could have created itself. Someone who
	// administers one namespace has no business enumerating everyone else's
	// credentials.
	listed := make([]keyBody, 0, len(keys))
	for _, key := range keys {
		if !principal.CanGrant(key.Scopes) {
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
	keys, err := s.Catalog.ListAPIKeys(r.Context())
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}

	var target catalog.APIKey
	found := false
	for _, key := range keys {
		if key.Name == name {
			target, found = key, true
			break
		}
	}
	if !found || !principal.CanGrant(target.Scopes) {
		// A key this principal may not administer is a key it may not know
		// about either.
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, "no such key")
		return
	}

	revoked, err := s.Catalog.RevokeAPIKey(r.Context(), name)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	if !revoked {
		httpx.Error(w, http.StatusNotFound, httpx.CodeNotFound, "no such active key")
		return
	}

	s.Logger.Info("revoked a key", "name", name, "by", principal.Name)
	httpx.JSON(w, http.StatusOK, map[string]string{"revoked": name})
}
