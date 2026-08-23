package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Tiers an account can be in. Everyone starts unknown.
const (
	// TierUnknown is a self-served account: real enough to be attributable,
	// unproven enough to be kept on a short leash.
	TierUnknown = "unknown"
	// TierContributor is an account an operator has vouched for: higher
	// limits, and more than just images.
	TierContributor = "contributor"
	// TierAdmin can mint credentials for any namespace. It is how the people
	// who run this service use it without shell access to the host.
	TierAdmin = "admin"
	// TierBlocked can no longer upload anything.
	TierBlocked = "blocked"
)

// Account is whoever is behind a token.
type Account struct {
	// ID is provider-qualified and immutable, e.g. "github:583231". It is the
	// numeric id rather than the login on purpose: a login can be renamed or
	// released and picked up by somebody else.
	ID       string
	Provider string
	// Handle is the human-readable name at the time of the last proof.
	Handle    string
	Tier      string
	CreatedAt time.Time
}

// UpsertAccount records an account, or refreshes the handle of one already
// known. It never changes a tier: that is an operator's decision, and proving
// an identity again should not undo it.
func (db *DB) UpsertAccount(ctx context.Context, a Account) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO accounts (id, provider, handle, tier, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (id) DO UPDATE SET handle = excluded.handle, updated_at = excluded.updated_at`,
		a.ID, a.Provider, a.Handle, TierUnknown, now, now)
	if err != nil {
		return fmt.Errorf("catalog: upsert account %s: %w", a.ID, err)
	}
	return nil
}

// AccountByID returns one account, or ErrNotFound.
func (db *DB) AccountByID(ctx context.Context, id string) (Account, error) {
	row := db.sql.QueryRowContext(ctx,
		`SELECT id, provider, handle, tier, created_at FROM accounts WHERE id = ?`, id)
	return scanAccount(row.Scan)
}

// SetTier changes how much an account is trusted.
func (db *DB) SetTier(ctx context.Context, id, tier string) error {
	switch tier {
	case TierUnknown, TierContributor, TierAdmin, TierBlocked:
	default:
		return fmt.Errorf("catalog: unknown tier %q", tier)
	}

	res, err := db.sql.ExecContext(ctx,
		`UPDATE accounts SET tier = ?, updated_at = ? WHERE id = ?`,
		tier, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return fmt.Errorf("catalog: set tier of %s: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Accounts lists everyone who has ever signed in, newest first.
func (db *DB) Accounts(ctx context.Context) ([]Account, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT id, provider, handle, tier, created_at FROM accounts ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("catalog: list accounts: %w", err)
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		a, err := scanAccount(rows.Scan)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// AccountsByHandle finds accounts by their human-readable name, which is what
// an operator will have to hand.
func (db *DB) AccountsByHandle(ctx context.Context, provider, handle string) ([]Account, error) {
	rows, err := db.sql.QueryContext(ctx,
		`SELECT id, provider, handle, tier, created_at FROM accounts
		 WHERE provider = ? AND handle = ? COLLATE NOCASE`, provider, handle)
	if err != nil {
		return nil, fmt.Errorf("catalog: accounts named %s: %w", handle, err)
	}
	defer rows.Close()

	var accounts []Account
	for rows.Next() {
		a, err := scanAccount(rows.Scan)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// Usage is what an account has uploaded within some window.
type Usage struct {
	Uploads int
	Bytes   int64
}

// UsageSince counts what an account stored since a moment. It reads the assets
// themselves rather than a counter, so it cannot drift out of step with what
// is actually there.
func (db *DB) UsageSince(ctx context.Context, accountID string, since time.Time) (Usage, error) {
	var usage Usage
	err := db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size), 0) FROM assets
		WHERE account_id = ? AND created_at > ?`,
		accountID, since.UTC().Format(time.RFC3339Nano)).Scan(&usage.Uploads, &usage.Bytes)
	if err != nil {
		return Usage{}, fmt.Errorf("catalog: usage for %s: %w", accountID, err)
	}
	return usage, nil
}

// LiveKeysFor counts an account's usable tokens, so that "make another one"
// cannot go on forever.
func (db *DB) LiveKeysFor(ctx context.Context, accountID string, now time.Time) (int, error) {
	var count int
	err := db.sql.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM api_keys
		WHERE account_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`,
		accountID, now.UTC().Format(time.RFC3339Nano)).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("catalog: live keys for %s: %w", accountID, err)
	}
	return count, nil
}

// RevokeAccountKeys makes every one of an account's tokens stop working.
func (db *DB) RevokeAccountKeys(ctx context.Context, accountID string) (int64, error) {
	res, err := db.sql.ExecContext(ctx, `
		UPDATE api_keys SET revoked_at = ? WHERE account_id = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), accountID)
	if err != nil {
		return 0, fmt.Errorf("catalog: revoke keys of %s: %w", accountID, err)
	}
	return res.RowsAffected()
}

func scanAccount(scan func(...any) error) (Account, error) {
	var a Account
	var created string
	err := scan(&a.ID, &a.Provider, &a.Handle, &a.Tier, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return Account{}, ErrNotFound
	}
	if err != nil {
		return Account{}, fmt.Errorf("catalog: read account: %w", err)
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return a, nil
}
