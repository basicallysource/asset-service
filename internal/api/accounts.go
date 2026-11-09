package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/httpx"
	"github.com/basicallysource/asset-service/internal/policy"
)

// Accounts can be managed over the API as well as on the host, for the same
// reason keys can: running this service should not require a shell on the
// machine it runs on. Only an admin or a manager sees the list, and a manager
// moves accounts only between unknown and contributor -- an account's standing
// is somebody else's decision, never its own. The rules are in
// internal/policy; applyTier is the one place they are applied.

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
	sees, err := s.seesAccounts(r.Context(), principal)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	if !sees {
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

	account, err := s.applyTier(r.Context(), principal, r.PathValue("id"), body.Tier)
	switch {
	case errors.Is(err, errBadTier):
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, err.Error())
		return
	case errors.Is(err, errTierNotYours):
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, err.Error())
		return
	case err != nil:
		s.writeAssetError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusOK, s.accountBody(r.Context(), account))
}

var (
	errBadTier      = errors.New("tier must be " + strings.Join(catalog.Tiers, ", "))
	errTierNotYours = errors.New("only an administrator may make that change")
)

// tierOf is the tier a principal acts with: that of the account its credential
// belongs to, or policy.NoAccount for one minted on the host. An account that
// cannot be read is an error, never a fall back to "no account".
func (s *Server) tierOf(ctx context.Context, principal *auth.Principal) (string, error) {
	if principal.Account == "" {
		return policy.NoAccount, nil
	}
	account, err := s.Catalog.AccountByID(ctx, principal.Account)
	if err != nil {
		return "", err
	}
	return account.Tier, nil
}

// seesAccounts is whether a principal may see the account list at all.
func (s *Server) seesAccounts(ctx context.Context, principal *auth.Principal) (bool, error) {
	if !principal.Holds(auth.ActionAdmin) {
		return false, nil
	}
	tier, err := s.tierOf(ctx, principal)
	if err != nil {
		return false, err
	}
	return policy.SeesAccounts(tier), nil
}

// applyTier moves one account, if this principal may, and when it is being
// blocked takes its keys with it: a blocked account keeps no working
// credentials. Both the API and the page come through here.
func (s *Server) applyTier(ctx context.Context, principal *auth.Principal, id, tier string) (catalog.Account, error) {
	if !catalog.ValidTier(tier) {
		return catalog.Account{}, errBadTier
	}

	sees, err := s.seesAccounts(ctx, principal)
	if err != nil {
		return catalog.Account{}, err
	}
	if !sees {
		return catalog.Account{}, errTierNotYours
	}
	by, err := s.tierOf(ctx, principal)
	if err != nil {
		return catalog.Account{}, err
	}
	target, err := s.Catalog.AccountByID(ctx, id)
	if err != nil {
		return catalog.Account{}, err
	}
	if !policy.MayMoveAccount(by, target.Tier, tier) {
		return catalog.Account{}, errTierNotYours
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
	s.Logger.Info("changed a tier", "account", id, "handle", account.Handle, "tier", tier, "by", principal.Name)
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
