package api

import (
	"embed"
	"html/template"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/httpx"
	"github.com/basicallysource/asset-service/internal/policy"
)

// A small page for the things people need a screen for: signing in, handing
// somebody else a key, and -- for an admin or a manager -- moving accounts
// between tiers.
//
// It holds its token in the tab rather than in a cookie, and attaches it by
// hand to every request. Nothing here is an ambient credential, so there is
// nothing for another site to make this page do on a visitor's behalf.

//go:embed web/login.html web/keys.html web/accounts.html web/htmx.min.js
var webFiles embed.FS

var keysTemplate = template.Must(template.ParseFS(webFiles, "web/keys.html"))

var accountsTemplate = template.Must(template.ParseFS(webFiles, "web/accounts.html"))

type keysView struct {
	Keys     []keyRow
	CanGrant bool
	// Manages is whether to show the key section at all. Somebody who can
	// neither mint nor see a key has no use for an empty table titled "keys
	// you can manage".
	Manages   bool
	Namespace string
	Minted    *mintedKey
	Error     string
}

type keyRow struct {
	// Index makes each row's copy button point at its own name.
	Index   int
	Name    string
	Scopes  string
	Expires string
	Revoked bool
}

type mintedKey struct {
	Name  string
	Token string
}

type accountsView struct {
	// Visible is whether this principal gets the section at all. The list of
	// who signed in is for administrators and managers, nobody else.
	Visible  bool
	Accounts []accountRow
	Error    string
}

type accountRow struct {
	ID     string
	Handle string
	Tier   string
	// Tiers are the ones this principal may move the account to, its current
	// one included. Empty means it is not this principal's to move.
	Tiers        []string
	UploadsToday int
	LiveKeys     int
	Joined       string
}

// tiersFor lists the tiers a principal of tier by may move an account in tier
// from to, the current one included so a form can show it selected. It is
// empty when the account is not this principal's to move.
func tiersFor(by, from string) []string {
	var tiers []string
	for _, tier := range catalog.Tiers {
		if tier == from || policy.MayMoveAccount(by, from, tier) {
			tiers = append(tiers, tier)
		}
	}
	if len(tiers) < 2 {
		return nil
	}
	return tiers
}

// firstNamespace is where this principal can write, for telling them so.
func firstNamespace(principal *auth.Principal) string {
	for _, scope := range principal.Scopes {
		if action, namespace, ok := strings.Cut(scope, ":"); ok && action == auth.ActionWrite {
			return namespace
		}
	}
	return ""
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	page, err := webFiles.ReadFile("web/login.html")
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(page)
}

func (s *Server) htmxScript(w http.ResponseWriter, r *http.Request) {
	script, err := webFiles.ReadFile("web/htmx.min.js")
	if err != nil {
		s.writeAssetError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	// Pinned in the binary, so it changes only when the service does.
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(script)
}

// keysFragment renders the key list for whoever is asking.
func (s *Server) keysFragment(w http.ResponseWriter, r *http.Request) {
	s.renderKeys(w, r, nil, "")
}

// mintKeyForm is the page's version of POST /v1/keys: same rules, HTML back.
func (s *Server) mintKeyForm(w http.ResponseWriter, r *http.Request) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderKeys(w, r, nil, "That form did not parse.")
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	scopes := strings.Fields(r.PostFormValue("scopes"))
	if name == "" || len(scopes) == 0 {
		s.renderKeys(w, r, nil, "A name and at least one scope are required.")
		return
	}
	for _, scope := range scopes {
		if !auth.ValidScope(scope) {
			s.renderKeys(w, r, nil, scope+" is not <read|write|admin>:<namespace>.")
			return
		}
	}
	if !principal.CanGrant(scopes) {
		s.renderKeys(w, r, nil, "You can only grant scopes you already administer.")
		return
	}

	days, _ := strconv.Atoi(r.PostFormValue("expires_in_days"))
	lifetime := time.Duration(days) * 24 * time.Hour
	if lifetime <= 0 || lifetime > maxKeyLifetime {
		lifetime = maxKeyLifetime
	}

	token, id, secretHash, err := auth.NewToken()
	if err != nil {
		s.renderKeys(w, r, nil, "Could not mint a key.")
		return
	}
	now := time.Now().UTC()
	if err := s.Catalog.InsertAPIKey(r.Context(), catalog.APIKey{
		ID: id, Name: name, SecretHash: secretHash, Scopes: scopes,
		AccountID: principal.Account, CreatedAt: now, ExpiresAt: now.Add(lifetime),
	}); err != nil {
		s.renderKeys(w, r, nil, "A key by that name already exists.")
		return
	}

	s.Logger.Info("minted a key", "name", name, "scopes", scopes, "by", principal.Name)
	s.renderKeys(w, r, &mintedKey{Name: name, Token: token}, "")
}

func (s *Server) revokeKeyForm(w http.ResponseWriter, r *http.Request) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	revoked, err := s.revoke(r.Context(), principal, r.PathValue("name"))
	switch {
	case err != nil:
		s.renderKeys(w, r, nil, "Could not revoke it.")
	case !revoked:
		s.renderKeys(w, r, nil, "There is no such key for you to revoke.")
	default:
		s.renderKeys(w, r, nil, "")
	}
}

// renderKeys draws the list of keys this principal may administer.
func (s *Server) renderKeys(w http.ResponseWriter, r *http.Request, minted *mintedKey, message string) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	view := keysView{
		Minted:    minted,
		Error:     message,
		CanGrant:  principal.Holds(auth.ActionAdmin),
		Namespace: firstNamespace(principal),
	}

	keys, err := s.Catalog.ListAPIKeys(r.Context())
	manages, merr := s.keysManagedBy(r.Context(), principal)
	if err != nil || merr != nil {
		view.Error = "Could not read the key list."
		keys = nil
	}
	for _, key := range keys {
		if !manages(key) {
			continue
		}
		row := keyRow{
			Index:   len(view.Keys),
			Name:    key.Name,
			Scopes:  strings.Join(key.Scopes, " "),
			Expires: "never",
			Revoked: key.Revoked,
		}
		if !key.ExpiresAt.IsZero() {
			row.Expires = key.ExpiresAt.Format("2 Jan 2006")
		}
		view.Keys = append(view.Keys, row)
	}

	view.Manages = view.CanGrant || len(view.Keys) > 0

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := keysTemplate.ExecuteTemplate(w, "keys", view); err != nil {
		s.Logger.Error("render keys", "error", err)
	}
}

// accountsFragment renders the account list for an admin or a manager. For anybody
// else it renders nothing: the section simply does not exist for them.
func (s *Server) accountsFragment(w http.ResponseWriter, r *http.Request) {
	s.renderAccounts(w, r, "")
}

// setTierForm is the page's version of POST /v1/accounts/{id}/tier: same
// rules, HTML back.
func (s *Server) setTierForm(w http.ResponseWriter, r *http.Request) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}
	if !principal.Holds(auth.ActionAdmin) {
		httpx.Error(w, http.StatusForbidden, httpx.CodeForbidden, "only an administrator may change a tier")
		return
	}
	if err := r.ParseForm(); err != nil {
		s.renderAccounts(w, r, "That form did not parse.")
		return
	}

	if _, err := s.applyTier(r.Context(), principal, r.PathValue("id"), r.PostFormValue("tier")); err != nil {
		s.renderAccounts(w, r, "Could not change that tier: "+err.Error())
		return
	}
	s.renderAccounts(w, r, "")
}

// renderAccounts draws every account and where it stands.
func (s *Server) renderAccounts(w http.ResponseWriter, r *http.Request, message string) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	view := accountsView{Error: message}
	visible, err := s.seesAccounts(r.Context(), principal)
	by, berr := s.tierOf(r.Context(), principal)
	if err != nil || berr != nil {
		view.Error = "Could not read the account list."
		visible = false
	}
	view.Visible = visible
	if view.Visible {
		bodies, err := s.accountBodies(r.Context())
		if err != nil {
			view.Error = "Could not read the account list."
		}
		for _, body := range bodies {
			view.Accounts = append(view.Accounts, accountRow{
				ID:           body.ID,
				Handle:       body.Handle,
				Tier:         body.Tier,
				Tiers:        tiersFor(by, body.Tier),
				UploadsToday: body.UploadsToday,
				LiveKeys:     body.LiveKeys,
				Joined:       body.CreatedAt.Format("2 Jan 2006"),
			})
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := accountsTemplate.ExecuteTemplate(w, "accounts", view); err != nil {
		s.Logger.Error("render accounts", "error", err)
	}
}
