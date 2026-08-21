// Package catalog is the service's metadata store: what assets exist and which
// API keys may act on them.
//
// It is deliberately a rebuildable index, not the source of truth. Every fact
// about an asset -- its bytes, its digest, its content type -- also lives on
// the object in storage. Losing this database costs the key list and a
// listing pass over the bucket; it cannot lose an asset.
package catalog

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Visibility values. Public assets are readable by anyone who has the URL;
// private assets are only reachable through a signed, expiring URL.
const (
	VisibilityPublic  = "public"
	VisibilityPrivate = "private"
)

// DB is the metadata store.
type DB struct {
	sql *sql.DB
}

// Open opens (creating if needed) the database at path and applies migrations.
func Open(path string) (*DB, error) {
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"

	handle, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("catalog: open %s: %w", path, err)
	}

	// One connection. Writes to SQLite serialize anyway, every query here is a
	// point lookup, and no request holds a transaction open across a network
	// call -- so a pool would buy contention bugs and nothing else.
	handle.SetMaxOpenConns(1)

	db := &DB{sql: handle}
	if err := db.migrate(); err != nil {
		handle.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) Close() error { return db.sql.Close() }

// Ping checks the database is reachable.
func (db *DB) Ping(ctx context.Context) error { return db.sql.PingContext(ctx) }

// migrate applies any migration files numbered above the schema version the
// database records, in order, each in its own transaction.
func (db *DB) migrate() error {
	entries, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(entries)

	var version int
	if err := db.sql.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("catalog: read schema version: %w", err)
	}

	for _, name := range entries {
		n, err := strconv.Atoi(strings.SplitN(strings.TrimPrefix(name, "migrations/"), "_", 2)[0])
		if err != nil {
			return fmt.Errorf("catalog: migration %s is not numbered", name)
		}
		if n <= version {
			continue
		}

		body, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := db.sql.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("catalog: apply %s: %w", name, err)
		}
		// PRAGMA takes no parameters, hence the formatted literal. n comes
		// from a filename in the binary, not from input.
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", n)); err != nil {
			tx.Rollback()
			return fmt.Errorf("catalog: stamp %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("catalog: commit %s: %w", name, err)
		}
		version = n
	}
	return nil
}

// Asset is one stored asset.
type Asset struct {
	Key         string
	Namespace   string
	Digest      string
	Size        int64
	ContentType string
	Filename    string
	Visibility  string
	CreatedAt   time.Time
	CreatedBy   string
	// AccountID is the account whose limits this upload counted against.
	AccountID string
}

// ErrNotFound is returned when a lookup finds nothing.
var ErrNotFound = errors.New("catalog: not found")

// AssetByKey returns the asset stored at key, or ErrNotFound.
func (db *DB) AssetByKey(ctx context.Context, key string) (Asset, error) {
	row := db.sql.QueryRowContext(ctx, `
		SELECT key, namespace, digest, size, content_type, filename, visibility, created_at, created_by, account_id
		FROM assets WHERE key = ?`, key)

	var a Asset
	var created string
	err := row.Scan(&a.Key, &a.Namespace, &a.Digest, &a.Size, &a.ContentType, &a.Filename, &a.Visibility, &created, &a.CreatedBy, &a.AccountID)
	if errors.Is(err, sql.ErrNoRows) {
		return Asset{}, ErrNotFound
	}
	if err != nil {
		return Asset{}, fmt.Errorf("catalog: asset %s: %w", key, err)
	}
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	return a, nil
}

// InsertAsset records an asset. It reports whether this call was the one that
// created the row: a repeat upload of identical bytes lands on the same key
// and is not an error, it is the same asset.
func (db *DB) InsertAsset(ctx context.Context, a Asset) (bool, error) {
	res, err := db.sql.ExecContext(ctx, `
		INSERT INTO assets (key, namespace, digest, size, content_type, filename, visibility, created_at, created_by, account_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (key) DO NOTHING`,
		a.Key, a.Namespace, a.Digest, a.Size, a.ContentType, a.Filename, a.Visibility,
		a.CreatedAt.UTC().Format(time.RFC3339Nano), a.CreatedBy, a.AccountID)
	if err != nil {
		return false, fmt.Errorf("catalog: insert %s: %w", a.Key, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// APIKey is a credential that may act on namespaces.
type APIKey struct {
	ID         string
	Name       string
	SecretHash string
	Scopes     []string
	// AccountID is who this belongs to. Empty means an operator minted it by
	// hand on the host, which is the one kind of credential no limit applies
	// to.
	AccountID string
	CreatedAt time.Time
	// ExpiresAt is zero for a credential that does not expire.
	ExpiresAt time.Time
	Revoked   bool
}

// APIKeyByID looks a key up by its public id, or returns ErrNotFound.
func (db *DB) APIKeyByID(ctx context.Context, id string) (APIKey, error) {
	row := db.sql.QueryRowContext(ctx, apiKeyColumns+` WHERE id = ?`, id)
	return scanAPIKey(row.Scan)
}

// InsertAPIKey stores a new key.
func (db *DB) InsertAPIKey(ctx context.Context, k APIKey) error {
	var expires any
	if !k.ExpiresAt.IsZero() {
		expires = k.ExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO api_keys (id, name, secret_hash, scopes, account_id, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.Name, k.SecretHash, strings.Join(k.Scopes, " "), k.AccountID, expires,
		k.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("catalog: insert key %s: %w", k.Name, err)
	}
	return nil
}

// ListAPIKeys returns every key, newest first. Secret hashes come with them;
// callers that print keys must not print those.
func (db *DB) ListAPIKeys(ctx context.Context) ([]APIKey, error) {
	rows, err := db.sql.QueryContext(ctx, apiKeyColumns+` ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("catalog: list keys: %w", err)
	}
	defer rows.Close()

	var keys []APIKey
	for rows.Next() {
		k, err := scanAPIKey(rows.Scan)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// RevokeAPIKey marks a key unusable. It reports whether a live key was found.
func (db *DB) RevokeAPIKey(ctx context.Context, name string) (bool, error) {
	res, err := db.sql.ExecContext(ctx, `
		UPDATE api_keys SET revoked_at = ? WHERE name = ? AND revoked_at IS NULL`,
		time.Now().UTC().Format(time.RFC3339Nano), name)
	if err != nil {
		return false, fmt.Errorf("catalog: revoke %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

const apiKeyColumns = `
	SELECT id, name, secret_hash, scopes, account_id, expires_at, created_at, revoked_at IS NOT NULL
	FROM api_keys`

func scanAPIKey(scan func(...any) error) (APIKey, error) {
	var k APIKey
	var scopes, created string
	var expires sql.NullString
	err := scan(&k.ID, &k.Name, &k.SecretHash, &scopes, &k.AccountID, &expires, &created, &k.Revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKey{}, ErrNotFound
	}
	if err != nil {
		return APIKey{}, fmt.Errorf("catalog: read key: %w", err)
	}
	k.Scopes = strings.Fields(scopes)
	k.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if expires.Valid {
		k.ExpiresAt, _ = time.Parse(time.RFC3339Nano, expires.String)
	}
	return k, nil
}
