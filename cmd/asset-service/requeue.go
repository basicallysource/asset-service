package main

import (
	"context"
	"errors"
	"flag"
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
//
// --rebuild is the other day: when what a rendition should look like changed,
// and the ones already made are the old answer. It throws their rows away and
// makes them again. The old objects stay in storage, unreferenced -- a key
// names its bytes, so nothing that already points at one breaks.
func requeueCommand(args []string) error {
	flags := flag.NewFlagSet("requeue", flag.ContinueOnError)
	rebuild := flags.Bool("rebuild", false,
		"also throw away derived forms that already exist and make them again")
	if err := flags.Parse(args); err != nil {
		return err
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
	var candidates []catalog.Asset
	if *rebuild {
		candidates, err = db.AssetsAwaitingNothing(ctx, requeueBatch)
	} else {
		candidates, err = db.AssetsWithoutRenditions(ctx, requeueBatch)
	}
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	var queued int
	for _, asset := range candidates {
		if !derive.Supported(asset.ContentType) {
			continue
		}
		if *rebuild {
			if err := db.DeleteRenditions(ctx, asset.Key); err != nil {
				return err
			}
		}
		if err := db.Enqueue(ctx, asset.Key, now); err != nil {
			return err
		}
		queued++
		fmt.Printf("%s  %s\n", asset.Key, asset.ContentType)
	}

	what := "with no derived forms"
	if *rebuild {
		what = "to be derived again"
	}
	fmt.Printf("\nqueued %d of %d assets %s\n", queued, len(candidates), what)
	if len(candidates) == requeueBatch {
		fmt.Println("there may be more; run it again once these are built")
	}
	return nil
}
