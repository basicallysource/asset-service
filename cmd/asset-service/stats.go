package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/catalog"
)

// statsCommand answers "how often do we derive, and how long does it take"
// from the derivations log — the row CompleteJob and FailJob append as work
// leaves the queue. Runs on the host, against the database, like requeue.
func statsCommand(args []string) error {
	path := strings.TrimSpace(os.Getenv("ASSET_DB_PATH"))
	if path == "" {
		return errors.New("stats: ASSET_DB_PATH is required; this runs on the host")
	}
	db, err := catalog.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx := context.Background()

	for _, window := range []struct {
		name  string
		since time.Time
	}{
		{"last 7 days", time.Now().UTC().AddDate(0, 0, -7)},
		{"all time", time.Time{}},
	} {
		stats, err := db.DerivationStats(ctx, window.since)
		if err != nil {
			return err
		}
		fmt.Printf("%s\n", window.name)
		if len(stats) == 0 {
			fmt.Println("  nothing derived")
			continue
		}
		for _, s := range stats {
			fmt.Printf("  %-16s %4d job(s), %d failed, %7.1fs total, %6.1fs mean, %6.1fs max\n",
				s.ContentType, s.Jobs, s.Failed, s.TotalSecs, s.MeanSecs, s.MaxSecs)
		}
	}

	recent, err := db.RecentDerivations(ctx, 10)
	if err != nil {
		return err
	}
	if len(recent) > 0 {
		fmt.Println("most recent")
		for _, d := range recent {
			line := fmt.Sprintf("  %s  %-6s %6.1fs  %s", d.FinishedAt.Format("2006-01-02 15:04"), d.Outcome, d.Seconds, d.AssetKey)
			if d.Error != "" {
				line += "  (" + d.Error + ")"
			}
			fmt.Println(line)
		}
	}
	return nil
}
