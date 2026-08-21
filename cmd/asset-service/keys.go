package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
)

// keysCommand manages API keys. It runs against the database directly rather
// than over HTTP: minting a credential is an operator action on the host, not
// something the service should expose a route for.
func keysCommand(args []string) error {
	if len(args) == 0 {
		return errors.New("keys: expected add, list or revoke")
	}

	// On the host, work on the database directly: that is how the first
	// credential exists at all, before there is a service to ask.
	path := strings.TrimSpace(os.Getenv("ASSET_DB_PATH"))
	if path == "" {
		return remoteKeys(args)
	}

	db, err := catalog.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	switch args[0] {
	case "add":
		return addKey(ctx, db, args[1:])
	case "list":
		return listKeys(ctx, db)
	case "revoke":
		return revokeKey(ctx, db, args[1:])
	default:
		return fmt.Errorf("keys: unknown subcommand %q", args[0])
	}
}

func addKey(ctx context.Context, db *catalog.DB, args []string) error {
	if len(args) < 2 {
		return errors.New("keys add: expected a name and at least one scope, e.g. keys add ci-docs write:docs")
	}
	name, scopes := args[0], args[1:]

	for _, scope := range scopes {
		if !auth.ValidScope(scope) {
			return fmt.Errorf("keys add: %q is not a scope; use read:<namespace> or write:<namespace>", scope)
		}
	}

	token, id, secretHash, err := auth.NewToken()
	if err != nil {
		return err
	}
	if err := db.InsertAPIKey(ctx, catalog.APIKey{
		ID:         id,
		Name:       name,
		SecretHash: secretHash,
		Scopes:     scopes,
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		return err
	}

	// The only time this token exists in readable form. Nothing stores it.
	fmt.Println(token)
	return nil
}

func listKeys(ctx context.Context, db *catalog.DB) error {
	keys, err := db.ListAPIKeys(ctx)
	if err != nil {
		return err
	}
	for _, key := range keys {
		state := "active"
		if key.Revoked {
			state = "revoked"
		}
		fmt.Printf("%-24s %-8s %s  %s\n", key.Name, state,
			key.CreatedAt.Format(time.RFC3339), strings.Join(key.Scopes, " "))
	}
	return nil
}

func revokeKey(ctx context.Context, db *catalog.DB, args []string) error {
	if len(args) != 1 {
		return errors.New("keys revoke: expected one name")
	}
	revoked, err := db.RevokeAPIKey(ctx, args[0])
	if err != nil {
		return err
	}
	if !revoked {
		return fmt.Errorf("keys revoke: no active key named %q", args[0])
	}
	fmt.Printf("revoked %s\n", args[0])
	return nil
}
