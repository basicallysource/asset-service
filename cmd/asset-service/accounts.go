package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/identity"
	"github.com/basicallysource/asset-service/internal/policy"
)

// accountsCommand adjusts an account's standing. It runs against the database
// on the host; an admin can do the same over the API or on the login page.
func accountsCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("accounts: expected list, promote, admin, block or reset")
	}

	path := strings.TrimSpace(os.Getenv("ASSET_DB_PATH"))
	if path == "" {
		return errors.New("accounts: ASSET_DB_PATH is required; this runs on the host")
	}
	db, err := catalog.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	if args[0] == "list" {
		return listAccounts(ctx, db)
	}

	tiers := map[string]string{
		"promote": catalog.TierContributor,
		"admin":   catalog.TierAdmin,
		"block":   catalog.TierBlocked,
		"reset":   catalog.TierUnknown,
	}
	tier, ok := tiers[args[0]]
	if !ok {
		return fmt.Errorf("accounts: unknown subcommand %q", args[0])
	}
	if len(args) != 2 {
		return fmt.Errorf("accounts %s: expected one handle", args[0])
	}

	handle := args[1]
	accounts, err := db.AccountsByHandle(ctx, identity.Provider, handle)
	if err != nil {
		return err
	}
	if len(accounts) == 0 {
		return fmt.Errorf("accounts: nobody named %q has signed in", handle)
	}

	for _, account := range accounts {
		if err := db.SetTier(ctx, account.ID, tier); err != nil {
			return err
		}
		fmt.Printf("%s (%s) is now %s\n", account.Handle, account.ID, tier)

		// A blocked account keeps no working credentials.
		if tier == catalog.TierBlocked {
			revoked, err := db.RevokeAccountKeys(ctx, account.ID)
			if err != nil {
				return err
			}
			fmt.Printf("revoked %d of its keys\n", revoked)
		}
	}
	return nil
}

func listAccounts(ctx context.Context, db *catalog.DB) error {
	accounts, err := db.Accounts(ctx)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	for _, account := range accounts {
		live, err := db.LiveKeysFor(ctx, account.ID, now)
		if err != nil {
			return err
		}
		usage, err := db.UsageSince(ctx, account.ID, now.Add(-24*time.Hour))
		if err != nil {
			return err
		}
		limits := policy.For(account.Tier)
		fmt.Printf("%-24s %-11s %-24s %d keys, %d uploads today (%s)\n",
			account.Handle, account.Tier, account.ID, live, usage.Uploads,
			describeLimits(limits))
	}
	return nil
}

func describeLimits(l policy.Limits) string {
	if l.UploadsPerDay == 0 && l.BytesPerDay == 0 {
		return "no limits"
	}
	return fmt.Sprintf("%d/day, %d/week", l.UploadsPerDay, l.UploadsPerWeek)
}
