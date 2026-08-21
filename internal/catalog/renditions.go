package catalog

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Rendition is one derived form of an asset.
type Rendition struct {
	AssetKey    string
	Name        string
	Key         string
	ContentType string
	Width       int
	Height      int
	Size        int64
	Digest      string
	CreatedAt   time.Time
}

// Job states. A job that finishes is deleted, so there is no done state.
const (
	JobPending = "pending"
	JobRunning = "running"
	JobFailed  = "failed"
)

// Job is outstanding work for one asset.
type Job struct {
	AssetKey  string
	State     string
	Attempts  int
	LastError string
}

// InsertRendition records a derived form. Producing the same rendition twice
// is not an error: the bytes are identical, so the row is too.
func (db *DB) InsertRendition(ctx context.Context, r Rendition) error {
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO renditions (asset_key, name, key, content_type, width, height, size, digest, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (asset_key, name) DO NOTHING`,
		r.AssetKey, r.Name, r.Key, r.ContentType, r.Width, r.Height, r.Size, r.Digest,
		r.CreatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("catalog: insert rendition %s of %s: %w", r.Name, r.AssetKey, err)
	}
	return nil
}

// RenditionsFor returns an asset's derived forms, smallest first.
func (db *DB) RenditionsFor(ctx context.Context, assetKey string) ([]Rendition, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT asset_key, name, key, content_type, width, height, size, digest, created_at
		FROM renditions WHERE asset_key = ? ORDER BY width, name`, assetKey)
	if err != nil {
		return nil, fmt.Errorf("catalog: renditions of %s: %w", assetKey, err)
	}
	defer rows.Close()

	var renditions []Rendition
	for rows.Next() {
		var r Rendition
		var created string
		if err := rows.Scan(&r.AssetKey, &r.Name, &r.Key, &r.ContentType, &r.Width, &r.Height,
			&r.Size, &r.Digest, &created); err != nil {
			return nil, fmt.Errorf("catalog: read rendition: %w", err)
		}
		r.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
		renditions = append(renditions, r)
	}
	return renditions, rows.Err()
}

// Enqueue asks for an asset's renditions to be produced. Asking twice while
// the first is outstanding changes nothing.
func (db *DB) Enqueue(ctx context.Context, assetKey string, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339Nano)
	_, err := db.sql.ExecContext(ctx, `
		INSERT INTO jobs (asset_key, state, attempts, next_attempt_at, created_at, updated_at)
		VALUES (?, 'pending', 0, ?, ?, ?)
		ON CONFLICT (asset_key) DO NOTHING`,
		assetKey, stamp, stamp, stamp)
	if err != nil {
		return fmt.Errorf("catalog: enqueue %s: %w", assetKey, err)
	}
	return nil
}

// ClaimJob takes the oldest job that is due, or returns ErrNotFound when there
// is nothing to do.
func (db *DB) ClaimJob(ctx context.Context, now time.Time) (Job, error) {
	stamp := now.UTC().Format(time.RFC3339Nano)
	row := db.sql.QueryRowContext(ctx, `
		UPDATE jobs SET state = 'running', updated_at = ?
		WHERE asset_key = (
			SELECT asset_key FROM jobs
			WHERE state = 'pending' AND next_attempt_at <= ?
			ORDER BY next_attempt_at LIMIT 1
		)
		RETURNING asset_key, state, attempts, last_error`, stamp, stamp)

	var job Job
	err := row.Scan(&job.AssetKey, &job.State, &job.Attempts, &job.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("catalog: claim job: %w", err)
	}
	return job, nil
}

// CompleteJob removes finished work.
func (db *DB) CompleteJob(ctx context.Context, assetKey string) error {
	if _, err := db.sql.ExecContext(ctx, `DELETE FROM jobs WHERE asset_key = ?`, assetKey); err != nil {
		return fmt.Errorf("catalog: complete %s: %w", assetKey, err)
	}
	return nil
}

// FailJob records an attempt that did not work. It goes back in the queue
// until it has failed maxAttempts times, after which it stays failed and stops
// consuming the worker -- a job that cannot succeed should be visible, not
// retried forever.
func (db *DB) FailJob(ctx context.Context, assetKey, reason string, retryAt time.Time, maxAttempts int) error {
	_, err := db.sql.ExecContext(ctx, `
		UPDATE jobs
		SET attempts = attempts + 1,
		    state = CASE WHEN attempts + 1 >= ? THEN 'failed' ELSE 'pending' END,
		    next_attempt_at = ?,
		    last_error = ?,
		    updated_at = ?
		WHERE asset_key = ?`,
		maxAttempts, retryAt.UTC().Format(time.RFC3339Nano), reason,
		time.Now().UTC().Format(time.RFC3339Nano), assetKey)
	if err != nil {
		return fmt.Errorf("catalog: fail %s: %w", assetKey, err)
	}
	return nil
}

// ReleaseClaimedJobs puts work that was in flight back in the queue. A process
// that dies mid-job leaves its row saying "running"; without this, that asset
// would never get its renditions.
func (db *DB) ReleaseClaimedJobs(ctx context.Context) (int64, error) {
	res, err := db.sql.ExecContext(ctx, `
		UPDATE jobs SET state = 'pending', updated_at = ? WHERE state = 'running'`,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, fmt.Errorf("catalog: release claimed jobs: %w", err)
	}
	return res.RowsAffected()
}

// JobFor returns the outstanding job for an asset, or ErrNotFound when there
// is none -- which means either it finished or it was never asked for.
func (db *DB) JobFor(ctx context.Context, assetKey string) (Job, error) {
	row := db.sql.QueryRowContext(ctx,
		`SELECT asset_key, state, attempts, last_error FROM jobs WHERE asset_key = ?`, assetKey)

	var job Job
	err := row.Scan(&job.AssetKey, &job.State, &job.Attempts, &job.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, fmt.Errorf("catalog: job for %s: %w", assetKey, err)
	}
	return job, nil
}
