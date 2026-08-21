package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/config"
	"github.com/basicallysource/asset-service/internal/derive"
	"github.com/basicallysource/asset-service/internal/objstore"
)

// measureBatch is how many assets one run measures. A bounded pass that can be
// repeated is better than an unbounded one that has to be interrupted.
const measureBatch = 500

// measureCommand records the pixel size of assets stored before the service
// measured them. It runs on the host, against the same configuration the
// service uses, because it has to read the bytes back out of storage.
//
// It is safe to run at any time and safe to run twice: an asset that already
// has dimensions is not looked at again.
func measureCommand(args []string) error {
	if len(args) > 0 {
		return errors.New("measure: takes no arguments")
	}

	path := strings.TrimSpace(os.Getenv("ASSET_DB_PATH"))
	if path == "" {
		return errors.New("measure: ASSET_DB_PATH is required; this runs on the host")
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
	pending, err := db.AssetsMissingDimensions(ctx, measureBatch)
	if err != nil {
		return err
	}
	if len(pending) == 0 {
		fmt.Println("nothing to measure")
		return nil
	}

	var measured, skipped int
	for _, asset := range pending {
		width, height, err := measureAsset(ctx, store, cfg.SpoolDir, asset)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", asset.Key, err)
			continue
		}
		if width == 0 || height == 0 {
			skipped++
			continue
		}
		if err := db.SetDimensions(ctx, asset.Key, width, height); err != nil {
			return err
		}
		measured++
		fmt.Printf("%s  %dx%d\n", asset.Key, width, height)
	}

	fmt.Printf("\nmeasured %d, no dimensions to read on %d\n", measured, skipped)
	if len(pending) == measureBatch {
		fmt.Println("there may be more; run it again")
	}
	return nil
}

// measureAsset stages one asset's bytes and reads their size.
func measureAsset(ctx context.Context, store objstore.Store, spoolDir string, asset catalog.Asset) (int, int, error) {
	if !derive.Supported(asset.ContentType) {
		return 0, 0, nil
	}

	body, err := store.Get(ctx, asset.Key)
	if err != nil {
		return 0, 0, err
	}
	defer body.Close()

	file, err := os.CreateTemp(spoolDir, "measure-")
	if err != nil {
		return 0, 0, err
	}
	defer os.Remove(file.Name())

	_, err = io.Copy(file, body)
	file.Close()
	if err != nil {
		return 0, 0, err
	}

	return derive.Dimensions(ctx, file.Name(), asset.ContentType)
}
