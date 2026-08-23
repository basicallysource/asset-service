package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/httpx"
	"github.com/basicallysource/asset-service/internal/identity"
	"github.com/basicallysource/asset-service/internal/policy"
)

// Signing in is how somebody gets a credential without anyone here doing
// anything. They prove they are a GitHub account, and that account gets a
// token confined to a namespace of its own with limits attached.
//
// The limits are the reason this can be open. They belong to the account, not
// the token, so a second token buys nothing; and they are set where no real
// use will meet them and any attempt to use this as free hosting will.

// signInBodyLimit is generous for a JSON object holding one device code.
const signInBodyLimit = 4 << 10

type startResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

type tokenResponse struct {
	Token     string     `json:"token"`
	Account   string     `json:"account"`
	Handle    string     `json:"handle"`
	Namespace string     `json:"namespace"`
	Scopes    []string   `json:"scopes"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Limits    limitsBody `json:"limits"`
}

type limitsBody struct {
	MaxFileBytes   int64    `json:"max_file_bytes,omitempty"`
	UploadsPerHour int      `json:"uploads_per_hour,omitempty"`
	UploadsPerDay  int      `json:"uploads_per_day,omitempty"`
	UploadsPerWeek int      `json:"uploads_per_week,omitempty"`
	BytesPerDay    int64    `json:"bytes_per_day,omitempty"`
	MaxLiveTokens  int      `json:"max_live_tokens,omitempty"`
	ContentTypes   []string `json:"content_types,omitempty"`
}

// startSignIn asks GitHub for a device code for the caller to approve.
func (s *Server) startSignIn(w http.ResponseWriter, r *http.Request) {
	if !s.Identity.Configured() {
		httpx.Error(w, http.StatusNotImplemented, httpx.CodeInternal, "sign-in is not configured on this service")
		return
	}
	if !s.SignInThrottle.Allow(httpx.ClientIP(r, s.ClientIPHeader), time.Now()) {
		w.Header().Set("Retry-After", "60")
		httpx.Error(w, http.StatusTooManyRequests, httpx.CodeRateLimited, "too many sign-in attempts; wait a minute")
		return
	}

	device, err := s.Identity.Start(r.Context())
	if err != nil {
		s.Logger.Error("sign-in: start", "error", err)
		httpx.Error(w, http.StatusBadGateway, httpx.CodeInternal, "could not reach GitHub")
		return
	}

	httpx.JSON(w, http.StatusOK, startResponse{
		DeviceCode:      device.DeviceCode,
		UserCode:        device.UserCode,
		VerificationURI: device.VerificationURI,
		ExpiresIn:       device.ExpiresIn,
		Interval:        device.Interval,
	})
}

// finishSignIn turns an approved device code into a token.
func (s *Server) finishSignIn(w http.ResponseWriter, r *http.Request) {
	if !s.Identity.Configured() {
		httpx.Error(w, http.StatusNotImplemented, httpx.CodeInternal, "sign-in is not configured on this service")
		return
	}

	var body struct {
		DeviceCode string `json:"device_code"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, signInBodyLimit)).Decode(&body); err != nil || body.DeviceCode == "" {
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "send {\"device_code\": \"...\"}")
		return
	}

	user, err := s.Identity.Redeem(r.Context(), body.DeviceCode)
	switch {
	case errors.Is(err, identity.ErrPending):
		// Not an error: the person has not finished at github.com yet.
		httpx.JSON(w, http.StatusAccepted, map[string]string{"status": "pending"})
		return
	case errors.Is(err, identity.ErrSlowDown):
		httpx.JSON(w, http.StatusAccepted, map[string]string{"status": "slow_down"})
		return
	case errors.Is(err, identity.ErrExpired):
		httpx.Error(w, http.StatusBadRequest, httpx.CodeBadRequest, "that code expired; start again")
		return
	case errors.Is(err, identity.ErrDenied):
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, "the request was declined at GitHub")
		return
	case err != nil:
		s.Logger.Error("sign-in: redeem", "error", err)
		httpx.Error(w, http.StatusBadGateway, httpx.CodeInternal, "could not reach GitHub")
		return
	}

	token, err := s.issue(r, user)
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	httpx.JSON(w, http.StatusCreated, token)
}

// issue records the account and mints its token.
func (s *Server) issue(r *http.Request, user identity.User) (tokenResponse, error) {
	ctx := r.Context()
	accountID := identity.Provider + ":" + itoa(user.ID)

	if err := s.Catalog.UpsertAccount(ctx, catalog.Account{
		ID: accountID, Provider: identity.Provider, Handle: user.Login,
	}); err != nil {
		return tokenResponse{}, err
	}

	// Whoever runs this service is named in its configuration, so a fresh
	// database does not need somebody with shell access to bless them again.
	if s.isAdmin(user.Login) {
		if err := s.Catalog.SetTier(ctx, accountID, catalog.TierAdmin); err != nil {
			return tokenResponse{}, err
		}
	}

	account, err := s.Catalog.AccountByID(ctx, accountID)
	if err != nil {
		return tokenResponse{}, err
	}
	if account.Tier == catalog.TierBlocked {
		return tokenResponse{}, errBlocked
	}

	limits := policy.For(account.Tier)
	now := time.Now().UTC()

	live, err := s.Catalog.LiveKeysFor(ctx, accountID, now)
	if err != nil {
		return tokenResponse{}, err
	}
	if limits.MaxLiveTokens > 0 && live >= limits.MaxLiveTokens {
		return tokenResponse{}, errTooManyTokens
	}

	namespace := policy.Namespace(user.Login)
	scopes := policy.Scopes(account.Tier, account.Handle)

	token, id, secretHash, err := auth.NewToken()
	if err != nil {
		return tokenResponse{}, err
	}

	var expiresAt time.Time
	if limits.TokenLifetime > 0 {
		expiresAt = now.Add(limits.TokenLifetime)
	}

	if err := s.Catalog.InsertAPIKey(ctx, catalog.APIKey{
		ID:         id,
		Name:       accountID + "/" + id[:8],
		SecretHash: secretHash,
		Scopes:     scopes,
		AccountID:  accountID,
		CreatedAt:  now,
		ExpiresAt:  expiresAt,
	}); err != nil {
		return tokenResponse{}, err
	}

	response := tokenResponse{
		Token:     token,
		Account:   accountID,
		Handle:    account.Handle,
		Namespace: namespace,
		Scopes:    scopes,
		Limits: limitsBody{
			MaxFileBytes:   limits.MaxFileBytes,
			UploadsPerHour: limits.UploadsPerHour,
			UploadsPerDay:  limits.UploadsPerDay,
			UploadsPerWeek: limits.UploadsPerWeek,
			BytesPerDay:    limits.BytesPerDay,
			MaxLiveTokens:  limits.MaxLiveTokens,
			ContentTypes:   limits.ContentTypes,
		},
	}
	if !expiresAt.IsZero() {
		response.ExpiresAt = &expiresAt
	}

	s.Logger.Info("sign-in: issued a token",
		"account", accountID, "handle", account.Handle, "tier", account.Tier, "namespace", namespace)
	return response, nil
}

// isAdmin reports whether a login is one this service is configured to trust
// completely.
func (s *Server) isAdmin(login string) bool {
	for _, admin := range s.AdminLogins {
		if strings.EqualFold(admin, login) {
			return true
		}
	}
	return false
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	if negative {
		out = append([]byte{'-'}, out...)
	}
	return string(out)
}
