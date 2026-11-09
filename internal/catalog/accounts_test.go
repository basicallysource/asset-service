package catalog

import (
	"context"
	"testing"
)

// The schema's CHECK constraint and Tiers name the same set. When one gains a
// tier and the other does not, this is what notices.
func TestEveryTierCanBeStored(t *testing.T) {
	db := openTest(t)
	ctx := context.Background()

	if err := db.UpsertAccount(ctx, Account{ID: "github:1", Provider: "github", Handle: "octocat"}); err != nil {
		t.Fatal(err)
	}
	for _, tier := range Tiers {
		if err := db.SetTier(ctx, "github:1", tier); err != nil {
			t.Errorf("set tier %s: %v", tier, err)
			continue
		}
		account, err := db.AccountByID(ctx, "github:1")
		if err != nil {
			t.Fatal(err)
		}
		if account.Tier != tier {
			t.Errorf("tier = %q, want %q", account.Tier, tier)
		}
	}
	if err := db.SetTier(ctx, "github:1", "emperor"); err == nil {
		t.Error("an invented tier was stored")
	}
}
