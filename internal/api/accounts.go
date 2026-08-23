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

// Accounts can be managed over the API as well as on the host, for the same
// reason keys can: running this service should not require a shell on the
// machine it runs on. Only an admin sees the list or moves anybody between
// tiers -- an account's standing is an operator's decision, never its own.

type accountBody struct {
	ID           string    `json:"id"`
	Provider     string    `json:"provider"`
	Handle       string    `json:"handle"`
	Tier         string    `json:"tier"`
	Namespace    string    `json:"namespace"`
	CreatedAt    time.Time `json:"created_at"`
	UploadsToday int       `json:"uploads_today"`
	LiveKeys     int       `json:"live_keys"`
}

func (s *Server) listAccounts(w http.ResponseWriter, r *http.Request) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	if !principal.Holds(auth.ActionAdmin) {
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, "only an administrator may list accounts")
		return
	}

	bodies, err := s.accountBodies(r.Context())
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, map[string]any{"accounts": bodies})
}

func (s *Server) setAccountTier(w http.ResponseWriter, r *http.Request) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	if !principal.Holds(auth.ActionAdmin) {
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, "only an administrator may change a tier")
		return
	}

	var body struct {
		Tier string `json:"tier"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, signInBodyLimit)).Decode(&body); err != nil {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, `send {"tier": "contributor"}`)
		return
	}

	account, err := s.applyTier(r.Context(), r.PathValue("id"), body.Tier, principal.Name)
	if err != nil {
		if err == errBadTier {
			httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
			return
		}
		s.writeAssetError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, s.accountBody(r.Context(), account))
}

var errBadTier = &tierError{}

type tierError struct{}

func (*tierError) Error() string {
	return "tier must be unknown, contributor, admin or blocked"
}

// applyTier moves one account and, when it is being blocked, takes its keys
// with it: a blocked account keeps no working credentials.
func (s *Server) applyTier(ctx context.Context, id, tier, by string) (catalog.Account, error) {
	switch tier {
	case catalog.TierUnknown, catalog.TierContributor, catalog.TierAdmin, catalog.TierBlocked:
	default:
		return catalog.Account{}, errBadTier
	}

	if err := s.Catalog.SetTier(ctx, id, tier); err != nil {
		return catalog.Account{}, err
	}
	if tier == catalog.TierBlocked {
		if _, err := s.Catalog.RevokeAccountKeys(ctx, id); err != nil {
			return catalog.Account{}, err
		}
	}

	account, err := s.Catalog.AccountByID(ctx, id)
	if err != nil {
		return catalog.Account{}, err
	}
	s.Logger.Info("changed a tier", "account", id, "handle", account.Handle, "tier", tier, "by", by)
	return account, nil
}

func (s *Server) accountBodies(ctx context.Context) ([]accountBody, error) {
	accounts, err := s.Catalog.Accounts(ctx)
	if err != nil {
		return nil, err
	}
	bodies := make([]accountBody, 0, len(accounts))
	for _, account := range accounts {
		bodies = append(bodies, s.accountBody(ctx, account))
	}
	return bodies, nil
}

func (s *Server) accountBody(ctx context.Context, account catalog.Account) accountBody {
	body := accountBody{
		ID:        account.ID,
		Provider:  account.Provider,
		Handle:    account.Handle,
		Tier:      account.Tier,
		Namespace: policy.Namespace(account.Handle),
		CreatedAt: account.CreatedAt,
	}
	now := time.Now().UTC()
	if usage, err := s.Catalog.UsageSince(ctx, account.ID, now.Add(-24*time.Hour)); err == nil {
		body.UploadsToday = usage.Uploads
	}
	if live, err := s.Catalog.LiveKeysFor(ctx, account.ID, now); err == nil {
		body.LiveKeys = live
	}
	return body
}
