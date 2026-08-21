package catalog

import (
	"context"
	"fmt"
	"time"
)

// Derivation is one finished attempt at building an asset's derived forms:
// what was worked on, how it went, and how long it took from claim to finish.
type Derivation struct {
	AssetKey    string
	ContentType string
	Outcome     string
	Error       string
	Renditions  int
	Attempts    int
	ClaimedAt   time.Time
	FinishedAt  time.Time
	Seconds     float64
}

// logDerivation appends the record of an attempt that just left the queue.
// Called by CompleteJob and FailJob with the job row they are about to
// consume, so nothing that finishes work can forget to be counted. Best
// effort by design: an asset that no longer exists, or a log insert that
// fails, must not stop the queue.
func (db *DB) logDerivation(ctx context.Context, assetKey, outcome, reason string, now time.Time) {
	var claimed string
	var attempts int
	if err := db.sql.QueryRowContext(ctx,
		`SELECT updated_at, attempts FROM jobs WHERE asset_key = ?`, assetKey).
		Scan(&claimed, &attempts); err != nil {
		return
	}
	var contentType string
	if err := db.sql.QueryRowContext(ctx,
		`SELECT content_type FROM assets WHERE key = ?`, assetKey).
		Scan(&contentType); err != nil {
		return // the asset is gone; there is nothing being timed
	}
	var renditions int
	_ = db.sql.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM renditions WHERE asset_key = ?`, assetKey).Scan(&renditions)

	claimedAt, err := time.Parse(time.RFC3339Nano, claimed)
	if err != nil {
		claimedAt = now
	}
	seconds := now.Sub(claimedAt).Seconds()
	if seconds < 0 {
		seconds = 0
	}
	if len(reason) > 200 {
		reason = reason[:200]
	}
	_, _ = db.sql.ExecContext(ctx, `
		INSERT INTO derivations (asset_key, content_type, outcome, error, renditions, attempts, claimed_at, finished_at, seconds)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		assetKey, contentType, outcome, reason, renditions, attempts+1,
		claimedAt.UTC().Format(time.RFC3339Nano), now.UTC().Format(time.RFC3339Nano), seconds)
}

// DerivationStat is one content type's totals over some window.
type DerivationStat struct {
	ContentType string
	Jobs        int
	Failed      int
	TotalSecs   float64
	MeanSecs    float64
	MaxSecs     float64
}

// DerivationStats sums the log per content type, newest-heavy first. A zero
// since means everything ever.
func (db *DB) DerivationStats(ctx context.Context, since time.Time) ([]DerivationStat, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT content_type,
		       COUNT(*),
		       SUM(CASE WHEN outcome = 'failed' THEN 1 ELSE 0 END),
		       SUM(seconds), AVG(seconds), MAX(seconds)
		FROM derivations
		WHERE finished_at >= ?
		GROUP BY content_type
		ORDER BY SUM(seconds) DESC`,
		since.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, fmt.Errorf("catalog: derivation stats: %w", err)
	}
	defer rows.Close()
	var out []DerivationStat
	for rows.Next() {
		var s DerivationStat
		if err := rows.Scan(&s.ContentType, &s.Jobs, &s.Failed, &s.TotalSecs, &s.MeanSecs, &s.MaxSecs); err != nil {
			return nil, fmt.Errorf("catalog: read derivation stat: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RecentDerivations returns the newest n log rows.
func (db *DB) RecentDerivations(ctx context.Context, n int) ([]Derivation, error) {
	rows, err := db.sql.QueryContext(ctx, `
		SELECT asset_key, content_type, outcome, error, renditions, attempts, claimed_at, finished_at, seconds
		FROM derivations ORDER BY finished_at DESC LIMIT ?`, n)
	if err != nil {
		return nil, fmt.Errorf("catalog: recent derivations: %w", err)
	}
	defer rows.Close()
	var out []Derivation
	for rows.Next() {
		var d Derivation
		var claimed, finished string
		if err := rows.Scan(&d.AssetKey, &d.ContentType, &d.Outcome, &d.Error, &d.Renditions,
			&d.Attempts, &claimed, &finished, &d.Seconds); err != nil {
			return nil, fmt.Errorf("catalog: read derivation: %w", err)
		}
		d.ClaimedAt, _ = time.Parse(time.RFC3339Nano, claimed)
		d.FinishedAt, _ = time.Parse(time.RFC3339Nano, finished)
		out = append(out, d)
	}
	return out, rows.Err()
}
