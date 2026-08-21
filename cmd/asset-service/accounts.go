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

// accountsCommand adjusts how much an account is trusted. It runs against the
// database on the host: deciding that somebody is trustworthy is not something
// the service should offer a route for.
func accountsCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("accounts: expected list, trust, admin, block or reset")
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
		"trust": catalog.TierTrusted,
		"admin": catalog.TierAdmin,
		"block": catalog.TierBlocked,
		"reset": catalog.TierUnknown,
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
	keys, err := db.ListAPIKeys(ctx)
	if err != nil {
		return err
	}

	// Accounts are discovered through their keys: an account with none has
	// signed in and never used it, which is not worth a row of its own.
	seen := map[string]bool{}
	now := time.Now().UTC()
	for _, key := range keys {
		if key.AccountID == "" || seen[key.AccountID] {
			continue
		}
		seen[key.AccountID] = true

		account, err := db.AccountByID(ctx, key.AccountID)
		if err != nil {
			continue
		}
		live, err := db.LiveKeysFor(ctx, account.ID, now)
		if err != nil {
			return err
		}
		usage, err := db.UsageSince(ctx, account.ID, now.Add(-24*time.Hour))
		if err != nil {
			return err
		}
		limits := policy.For(account.Tier)
		fmt.Printf("%-24s %-9s %-24s %d keys, %d uploads today (%s)\n",
			account.Handle, account.Tier, account.ID, live, usage.Uploads,
			describeLimits(limits))
	}
	return nil
}

func describeLimits(l policy.Limits) string {
	if l.UploadsPerHour == 0 && l.BytesPerDay == 0 {
		return "no limits"
	}
	return fmt.Sprintf("%d/hour", l.UploadsPerHour)
}
