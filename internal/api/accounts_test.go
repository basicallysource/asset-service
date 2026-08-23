package api

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
)

func TestOnlyAnAdminSeesAccountsOrMovesTiers(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.server.Catalog.UpsertAccount(ctx, catalog.Account{
		ID: "github:1", Provider: "github", Handle: "octocat",
	}); err != nil {
		t.Fatal(err)
	}

	if w := h.do(t, "GET", "/v1/accounts", h.writer, ""); w.Code != http.StatusForbidden {
		t.Errorf("a writer listing accounts: status = %d", w.Code)
	}
	if w := h.do(t, "POST", "/v1/accounts/github:1/tier", h.writer, `{"tier": "admin"}`); w.Code != http.StatusForbidden {
		t.Errorf("a writer promoting itself: status = %d", w.Code)
	}

	w := h.do(t, "GET", "/v1/accounts", h.worker, "")
	if w.Code != http.StatusOK {
		t.Fatalf("listing accounts: status = %d (%s)", w.Code, w.Body.String())
	}

	if w := h.do(t, "POST", "/v1/accounts/github:1/tier", h.worker, `{"tier": "contributor"}`); w.Code != http.StatusOK {
		t.Fatalf("promoting: status = %d (%s)", w.Code, w.Body.String())
	}
	account, err := h.server.Catalog.AccountByID(ctx, "github:1")
	if err != nil {
		t.Fatal(err)
	}
	if account.Tier != catalog.TierContributor {
		t.Errorf("tier = %q, want %q", account.Tier, catalog.TierContributor)
	}

	if w := h.do(t, "POST", "/v1/accounts/github:1/tier", h.worker, `{"tier": "emperor"}`); w.Code != http.StatusBadRequest {
		t.Errorf("an invented tier: status = %d", w.Code)
	}
}

func TestBlockingAnAccountOverTheAPIRevokesItsKeys(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	if err := h.server.Catalog.UpsertAccount(ctx, catalog.Account{
		ID: "github:2", Provider: "github", Handle: "mallory",
	}); err != nil {
		t.Fatal(err)
	}
	_, id, hash, err := auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.server.Catalog.InsertAPIKey(ctx, catalog.APIKey{
		ID: id, Name: "mallory-token", SecretHash: hash,
		Scopes: []string{"write:u-mallory"}, AccountID: "github:2",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	if w := h.do(t, "POST", "/v1/accounts/github:2/tier", h.worker, `{"tier": "blocked"}`); w.Code != http.StatusOK {
		t.Fatalf("blocking: status = %d (%s)", w.Code, w.Body.String())
	}

	live, err := h.server.Catalog.LiveKeysFor(ctx, "github:2", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if live != 0 {
		t.Errorf("a blocked account still holds %d live keys", live)
	}
}
