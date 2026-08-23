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
)

// A small page for the things people need a screen for: signing in, handing
// somebody else a key, and -- for an admin -- moving accounts between tiers.
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
	// who signed in is an administrator's view, nobody else's.
	Visible  bool
	Accounts []accountRow
	Tiers    []string
	Error    string
}

type accountRow struct {
	ID           string
	Handle       string
	Tier         string
	UploadsToday int
	LiveKeys     int
	Joined       string
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

	name := r.PathValue("name")
	keys, err := s.Catalog.ListAPIKeys(r.Context())
	if err != nil {
		s.renderKeys(w, r, nil, "Could not read the key list.")
		return
	}
	for _, key := range keys {
		if key.Name != name {
			continue
		}
		if !principal.CanGrant(key.Scopes) {
			s.renderKeys(w, r, nil, "That key is not yours to revoke.")
			return
		}
		if _, err := s.Catalog.RevokeAPIKey(r.Context(), name); err != nil {
			s.renderKeys(w, r, nil, "Could not revoke it.")
			return
		}
		s.Logger.Info("revoked a key", "name", name, "by", principal.Name)
		break
	}
	s.renderKeys(w, r, nil, "")
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
	if err != nil {
		view.Error = "Could not read the key list."
	}
	for _, key := range keys {
		if !principal.CanGrant(key.Scopes) {
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

// accountsFragment renders the account list for an administrator. For anybody
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

	if _, err := s.applyTier(r.Context(), r.PathValue("id"), r.PostFormValue("tier"), principal.Name); err != nil {
		s.renderAccounts(w, r, "Could not change that tier: "+err.Error())
		return
	}
	s.renderAccounts(w, r, "")
}

// renderAccounts draws every account and where it stands, for an admin.
func (s *Server) renderAccounts(w http.ResponseWriter, r *http.Request, message string) {
	principal := auth.From(r.Context())
	if principal == nil {
		httpx.Error(w, http.StatusUnauthorized, httpx.CodeUnauthorized, "authentication required")
		return
	}

	view := accountsView{
		Visible: principal.Holds(auth.ActionAdmin),
		Tiers:   []string{catalog.TierUnknown, catalog.TierContributor, catalog.TierAdmin, catalog.TierBlocked},
		Error:   message,
	}
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
