package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/config"
	"github.com/basicallysource/asset-service/internal/derive"
	"github.com/basicallysource/asset-service/internal/imaging"
	"github.com/basicallysource/asset-service/internal/objstore"
)

// withholdBatch bounds one pass, for the same reason measure and requeue are
// bounded: a run that can be repeated beats one that has to be interrupted.
const withholdBatch = 500

// withholdCommand stops storage serving the originals of assets stored before
// the service withheld them. A camera original uploaded today is stored
// private and published as a copy without its metadata; one stored earlier is
// still an object anybody can fetch, which is the same exposure.
//
// One namespace at a time, named on the command line, because this changes
// what a published URL does and that is a decision to take deliberately rather
// than across a whole bucket at once.
//
// Each asset goes one of two ways. If there is already something to publish in
// its place -- an image's full copy, a video's encodes -- the object's ACL is
// rewritten and nothing else happens: no bytes are read or moved. If there is
// not, it is queued so the copy gets built, and a later run withholds it. So
// this is safe to run twice, and the usual course is to run it, wait for the
// queue, and run it again.
//
// Two things it does not do. It does not reach a cache: an object that has
// been public may sit in a CDN or a browser for as long as its headers said,
// and nothing here retracts that. And it does not rewrite pages: anything that
// published the original's URL will start getting a refusal, so the pages that
// point at these assets want rebuilding from the manifest afterwards.
func withholdCommand(args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return errors.New("withhold: takes one namespace, e.g. asset-service withhold docs")
	}
	namespace := args[0]

	path := strings.TrimSpace(os.Getenv("ASSET_DB_PATH"))
	if path == "" {
		return errors.New("withhold: ASSET_DB_PATH is required; this runs on the host")
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	store, err := objstore.New(objstore.Config{
		Endpoint:      cfg.S3Endpoint,
		Region:        cfg.S3Region,
		Bucket:        cfg.S3Bucket,
		AccessKey:     cfg.S3AccessKey,
		SecretKey:     cfg.S3SecretKey,
		PathStyle:     cfg.S3PathStyle,
		PublicBaseURL: cfg.PublicBaseURL,
	})
	if err != nil {
		return err
	}

	db, err := catalog.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()

	ctx := context.Background()
	assets, err := db.AssetsInNamespace(ctx, namespace, withholdBatch)
	if err != nil {
		return err
	}

	var withheld, queued int
	for _, asset := range assets {
		if asset.Visibility != catalog.VisibilityPublic || !derive.WithholdsOriginal(asset.ContentType) {
			continue
		}

		ladder, err := db.RenditionsFor(ctx, asset.Key)
		if err != nil {
			return err
		}
		if !publishable(asset, ladder) {
			if err := db.Enqueue(ctx, asset.Key, time.Now().UTC()); err != nil {
				return err
			}
			queued++
			fmt.Printf("%s  queued: nothing to publish in its place yet\n", asset.Key)
			continue
		}

		if err := store.SetPrivate(ctx, asset.Key); err != nil {
			return fmt.Errorf("withhold %s: %w", asset.Key, err)
		}
		withheld++
		fmt.Printf("%s  withheld\n", asset.Key)
	}

	fmt.Printf("\nwithheld %d, queued %d of %d assets in %s\n", withheld, queued, len(assets), namespace)
	if queued > 0 {
		fmt.Println("run it again once the queue has drained to withhold the ones just queued")
	}
	if len(assets) == withholdBatch {
		fmt.Println("there may be more; run it again")
	}
	return nil
}

// publishable reports whether an asset already has the form that is served in
// the original's place: for an image the full-resolution copy, for a video its
// encodes. It is the same question Service.PublicForm answers, asked before
// making the original unreadable rather than after.
func publishable(asset catalog.Asset, ladder []catalog.Rendition) bool {
	if imaging.Publishable(asset.ContentType) {
		for _, r := range ladder {
			if r.Name == derive.FullName {
				return true
			}
		}
		return false
	}
	return len(ladder) > 0
}
