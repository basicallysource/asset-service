package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/derive"
)

// requeueBatch bounds one pass, so a service that has just learned to derive
// something new does not queue its entire history in one command.
const requeueBatch = 500

// requeueCommand asks for derived forms of assets that have none.
//
// It is for the day the service learns something it did not know: video, say,
// stored fine before anything could transcode it, and those assets have no
// ladder and no job to build one. Anything that already has renditions, or is
// already queued, is left alone -- so this is safe to run at any time and
// safe to run twice.
func requeueCommand(args []string) error {
	if len(args) > 0 {
		return errors.New("requeue: takes no arguments")
	}

	path := strings.TrimSpace(os.Getenv("ASSET_DB_PATH"))
	if path == "" {
		return errors.New("requeue: ASSET_DB_PATH is required; this runs on the host")
	}
	db, err := catalog.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	candidates, err := db.AssetsWithoutRenditions(ctx, requeueBatch)
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	var queued int
	for _, asset := range candidates {
		if !derive.Supported(asset.ContentType) {
			continue
		}
		if err := db.Enqueue(ctx, asset.Key, now); err != nil {
			return err
		}
		queued++
		fmt.Printf("%s  %s\n", asset.Key, asset.ContentType)
	}

	fmt.Printf("\nqueued %d of %d assets with no derived forms\n", queued, len(candidates))
	if len(candidates) == requeueBatch {
		fmt.Println("there may be more; run it again once these are built")
	}
	return nil
}
