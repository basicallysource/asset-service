package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
)

// accountKey puts an account in a tier and gives it a key, the way signing in
// or minting would, and returns the key's token.
func accountKey(t *testing.T, h *harness, account, tier, keyName string, scopes ...string) string {
	t.Helper()
	ctx := context.Background()
	if account != "" {
		if err := h.server.Catalog.UpsertAccount(ctx, catalog.Account{
			ID: account, Provider: "github", Handle: strings.TrimPrefix(account, "github:"),
		}); err != nil {
			t.Fatal(err)
		}
		if err := h.server.Catalog.SetTier(ctx, account, tier); err != nil {
			t.Fatal(err)
		}
	}
	token, id, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.server.Catalog.InsertAPIKey(ctx, catalog.APIKey{
		ID: id, Name: keyName, SecretHash: hash, Scopes: scopes, AccountID: account,
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}
	return token
}

func keyLive(t *testing.T, h *harness, name string) bool {
	t.Helper()
	keys, err := h.server.Catalog.ListAPIKeys(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if key.Name == name {
			return !key.Revoked
		}
	}
	t.Fatalf("no key named %s", name)
	return false
}

func listedKeys(t *testing.T, h *harness, token string) map[string]bool {
	t.Helper()
	w := h.do(t, "GET", "/v1/keys", token, "")
	if w.Code != http.StatusOK {
		t.Fatalf("listing keys: status = %d (%s)", w.Code, w.Body.String())
	}
	var body struct {
		Keys []keyBody `json:"keys"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, key := range body.Keys {
		names[key.Name] = true
	}
	return names
}

func tierOfAccount(t *testing.T, h *harness, id string) string {
	t.Helper()
	account, err := h.server.Catalog.AccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return account.Tier
}

var everything = []string{"write:*", "read:*", "admin:*"}

func TestAManagerRevokesOnlyItsOwnKeys(t *testing.T) {
	h := newHarness(t)

	manager := accountKey(t, h, "github:10", catalog.TierManager, "manager-self", everything...)
	accountKey(t, h, "github:11", catalog.TierAdmin, "admins-key", "write:docs")
	accountKey(t, h, "github:12", catalog.TierContributor, "someones-key", "write:docs")
	accountKey(t, h, "", "", "operator-key", "write:docs")

	// A manager mints for any namespace, like an admin.
	if w := h.do(t, "POST", "/v1/keys", manager, `{"name": "managers-ci", "scopes": ["write:docs", "read:docs"]}`); w.Code != http.StatusCreated {
		t.Fatalf("a manager minting: status = %d (%s)", w.Code, w.Body.String())
	}

	listed := listedKeys(t, h, manager)
	for _, name := range []string{"admins-key", "someones-key", "operator-key", "worker"} {
		if listed[name] {
			t.Errorf("a manager is shown %s", name)
		}
	}
	if !listed["managers-ci"] || !listed["manager-self"] {
		t.Errorf("a manager is not shown its own keys: %v", listed)
	}

	for _, name := range []string{"admins-key", "someones-key", "operator-key", "worker"} {
		if w := h.do(t, "POST", "/v1/keys/"+name+"/revoke", manager, ""); w.Code != http.StatusNotFound {
			t.Errorf("a manager revoking %s: status = %d", name, w.Code)
		}
		if w := h.upload(t, manager, "/login/keys/"+name+"/revoke", "application/x-www-form-urlencoded", ""); w.Code != http.StatusOK {
			t.Errorf("a manager revoking %s on the page: status = %d", name, w.Code)
		}
		if !keyLive(t, h, name) {
			t.Errorf("a manager revoked %s", name)
		}
	}

	if w := h.do(t, "POST", "/v1/keys/managers-ci/revoke", manager, ""); w.Code != http.StatusOK {
		t.Errorf("a manager revoking its own key: status = %d (%s)", w.Code, w.Body.String())
	}
	if keyLive(t, h, "managers-ci") {
		t.Error("a manager could not revoke its own key")
	}
}

func TestAdminsAndOperatorsRevokeAnyKeyTheyAdminister(t *testing.T) {
	h := newHarness(t)

	admin := accountKey(t, h, "github:11", catalog.TierAdmin, "admin-self", everything...)
	accountKey(t, h, "github:10", catalog.TierManager, "managers-key", "write:docs")
	accountKey(t, h, "", "", "operator-key", "write:docs")
	accountKey(t, h, "github:12", catalog.TierContributor, "someones-key", "write:docs")

	if w := h.do(t, "POST", "/v1/keys/managers-key/revoke", admin, ""); w.Code != http.StatusOK {
		t.Errorf("an admin revoking a manager's key: status = %d", w.Code)
	}
	if w := h.upload(t, admin, "/login/keys/operator-key/revoke", "application/x-www-form-urlencoded", ""); w.Code != http.StatusOK || keyLive(t, h, "operator-key") {
		t.Errorf("an admin revoking an operator key on the page: status = %d", w.Code)
	}
	if w := h.do(t, "POST", "/v1/keys/someones-key/revoke", h.worker, ""); w.Code != http.StatusOK {
		t.Errorf("an operator key revoking an account's key: status = %d", w.Code)
	}
}

// A key belongs to the account that minted it, so a manager cannot launder its
// way out of its rules by minting a key and using that instead.
func TestAKeyAManagerMintsIsHeldToTheManagersRules(t *testing.T) {
	h := newHarness(t)

	manager := accountKey(t, h, "github:10", catalog.TierManager, "manager-self", everything...)
	accountKey(t, h, "", "", "operator-key", "write:docs")
	accountKey(t, h, "github:12", catalog.TierUnknown, "someones-key", "write:u-12")

	w := h.do(t, "POST", "/v1/keys", manager, `{"name": "minted", "scopes": ["write:*", "read:*", "admin:*"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("minting: status = %d (%s)", w.Code, w.Body.String())
	}
	var minted struct {
		Token   string `json:"token"`
		Account string `json:"account"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &minted); err != nil {
		t.Fatal(err)
	}
	if minted.Account != "github:10" {
		t.Fatalf("a manager's key belongs to %q", minted.Account)
	}

	// And a key minted by that key still belongs to the manager.
	w = h.do(t, "POST", "/v1/keys", minted.Token, `{"name": "minted-again", "scopes": ["admin:*"]}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("minting from a minted key: status = %d (%s)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"account":"github:10"`) {
		t.Errorf("a key minted by a minted key: %s", w.Body.String())
	}

	for _, name := range []string{"operator-key", "someones-key", "worker"} {
		if w := h.do(t, "POST", "/v1/keys/"+name+"/revoke", minted.Token, ""); w.Code != http.StatusNotFound {
			t.Errorf("a manager's minted key revoking %s: status = %d", name, w.Code)
		}
	}
	if w := h.do(t, "POST", "/v1/accounts/github:12/tier", minted.Token, `{"tier": "admin"}`); w.Code != http.StatusForbidden {
		t.Errorf("a manager's minted key making an admin: status = %d", w.Code)
	}
	if w := h.do(t, "POST", "/v1/accounts/github:10/tier", minted.Token, `{"tier": "admin"}`); w.Code != http.StatusForbidden {
		t.Errorf("a manager's minted key promoting its own account: status = %d", w.Code)
	}

	// Its own account's keys it may revoke, the manager's self-served one included.
	if w := h.do(t, "POST", "/v1/keys/manager-self/revoke", minted.Token, ""); w.Code != http.StatusOK {
		t.Errorf("a minted key revoking its own account's key: status = %d", w.Code)
	}
}

func TestAManagerMovesAccountsOnlyBetweenUnknownAndContributor(t *testing.T) {
	h := newHarness(t)

	manager := accountKey(t, h, "github:10", catalog.TierManager, "manager-self", everything...)
	accountKey(t, h, "github:11", catalog.TierAdmin, "admins-key", "write:docs")
	accountKey(t, h, "github:13", catalog.TierManager, "other-managers-key", "write:docs")
	accountKey(t, h, "github:14", catalog.TierBlocked, "blocked-key", "write:docs")
	accountKey(t, h, "github:12", catalog.TierUnknown, "someones-key", "write:u-12")

	if w := h.do(t, "GET", "/v1/accounts", manager, ""); w.Code != http.StatusOK {
		t.Fatalf("a manager listing accounts: status = %d (%s)", w.Code, w.Body.String())
	}

	set := func(id, tier string) int {
		return h.do(t, "POST", "/v1/accounts/"+id+"/tier", manager, `{"tier": "`+tier+`"}`).Code
	}

	if code := set("github:12", catalog.TierContributor); code != http.StatusOK {
		t.Fatalf("a manager promoting: status = %d", code)
	}
	if tier := tierOfAccount(t, h, "github:12"); tier != catalog.TierContributor {
		t.Fatalf("tier = %q", tier)
	}
	if code := set("github:12", catalog.TierUnknown); code != http.StatusOK {
		t.Fatalf("a manager resetting: status = %d", code)
	}

	refused := []struct{ id, tier, why string }{
		{"github:12", catalog.TierAdmin, "making an admin"},
		{"github:12", catalog.TierManager, "making a manager"},
		{"github:12", catalog.TierBlocked, "blocking"},
		{"github:11", catalog.TierUnknown, "demoting an admin"},
		{"github:13", catalog.TierContributor, "demoting another manager"},
		{"github:10", catalog.TierContributor, "demoting itself"},
		{"github:10", catalog.TierAdmin, "promoting itself"},
		{"github:14", catalog.TierUnknown, "unblocking"},
	}
	for _, c := range refused {
		before := tierOfAccount(t, h, c.id)
		if code := set(c.id, c.tier); code != http.StatusForbidden {
			t.Errorf("a manager %s: status = %d", c.why, code)
		}
		if w := h.upload(t, manager, "/login/accounts/"+c.id+"/tier", "application/x-www-form-urlencoded", "tier="+c.tier); w.Code != http.StatusOK {
			t.Errorf("a manager %s on the page: status = %d", c.why, w.Code)
		}
		if after := tierOfAccount(t, h, c.id); after != before {
			t.Errorf("a manager %s: tier went from %s to %s", c.why, before, after)
		}
	}
	if !keyLive(t, h, "someones-key") {
		t.Error("a manager's refused block still revoked the account's keys")
	}

	// The page offers a manager only what it may do.
	w := h.do(t, "GET", "/login/accounts", manager, "")
	if w.Code != http.StatusOK {
		t.Fatalf("the accounts page: status = %d", w.Code)
	}
	if page := w.Body.String(); strings.Contains(page, `value="admin"`) || strings.Contains(page, `value="blocked"`) ||
		strings.Contains(page, "/login/accounts/github:11/tier") || !strings.Contains(page, "/login/accounts/github:12/tier") {
		t.Errorf("the accounts page offers a manager the wrong moves:\n%s", page)
	}

	// An admin and an operator key still move anybody anywhere.
	admin := accountKey(t, h, "github:15", catalog.TierAdmin, "admin-self", everything...)
	if code := h.do(t, "POST", "/v1/accounts/github:12/tier", admin, `{"tier": "manager"}`).Code; code != http.StatusOK {
		t.Errorf("an admin making a manager: status = %d", code)
	}
	if code := h.do(t, "POST", "/v1/accounts/github:13/tier", h.worker, `{"tier": "blocked"}`).Code; code != http.StatusOK {
		t.Errorf("an operator key blocking a manager: status = %d", code)
	}
}
