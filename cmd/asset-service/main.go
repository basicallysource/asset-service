// Command asset-service stores assets and hands out URLs to them.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/basicallysource/asset-service/internal/api"
	"github.com/basicallysource/asset-service/internal/assets"
	"github.com/basicallysource/asset-service/internal/auth"
	"github.com/basicallysource/asset-service/internal/catalog"
	"github.com/basicallysource/asset-service/internal/config"
	"github.com/basicallysource/asset-service/internal/derive"
	"github.com/basicallysource/asset-service/internal/identity"
	"github.com/basicallysource/asset-service/internal/imaging"
	"github.com/basicallysource/asset-service/internal/objstore"
	"github.com/basicallysource/asset-service/internal/renditions"
	"github.com/basicallysource/asset-service/internal/video"
)

// version is stamped at build time with -ldflags "-X main.version=...".
var version = "dev"

// shutdownGrace is how long in-flight requests get once a stop signal arrives.
// An upload already on disk finishes; a new one is refused.
const shutdownGrace = 30 * time.Second

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	command := "serve"
	if len(args) > 0 {
		command, args = args[0], args[1:]
	}

	switch command {
	case "serve":
		return serve()
	case "login":
		return loginCommand(args)
	case "upload":
		return uploadCommand(args)
	case "keys":
		return keysCommand(args)
	case "accounts":
		return accountsCommand(args)
	case "measure":
		return measureCommand(args)
	case "requeue":
		return requeueCommand(args)
	case "work":
		return workCommand(args)
	case "version":
		fmt.Println(version)
		return nil
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown command %q", command)
	}
}

func usage(w *os.File) {
	fmt.Fprint(w, `asset-service - store assets, hand out URLs to them

Usage:
  asset-service serve                        run the HTTP service (default)

  asset-service login --url <service>        sign in with GitHub, keep the token
  asset-service upload <file> [flags]        store a file, print its URL
      --namespace <ns>   where to put it (defaults to your own namespace)
      --name <filename>  filename to record (defaults to the file's own)
      --private          reachable only through a signed URL
      --quiet            print only the URL

  asset-service keys add <name> <scope>...   mint a key, printed once
  asset-service keys list                    list keys
  asset-service keys revoke <name>           make a key stop working

  asset-service accounts list                who has signed in
  asset-service accounts trust <handle>      raise an account's limits
  asset-service accounts admin <handle>      let an account manage keys
  asset-service accounts block <handle>      stop an account uploading

  asset-service measure                      record the pixel size of assets
                                             stored before it was measured
  asset-service requeue                      build derived forms for assets
                                             that have none

  asset-service work [flags]                 do the deriving for a service,
                                             on this machine
      --once             drain the queue and stop
      --poll <duration>  wait this long when there is nothing to do
      --preset <name>    libx264 speed against size, e.g. veryfast

  asset-service version                      print the build version

A scope is <action>:<namespace>. The action is read, write, or admin -- the
right to hand out credentials for that namespace -- and the namespace may be
*, for example write:docs, read:*, admin:docs.

keys and accounts run against the database when ASSET_DB_PATH is set, which is
how an operator works on the host itself. Everywhere else they run against the
service you signed in to.

Configuration is environment only; see agent-docs/operations.md.
`)
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	logger := newLogger(cfg.LogLevel)
	slog.SetDefault(logger)

	// A fresh host has neither directory. Creating them here means a first
	// start needs nothing prepared but the volume itself.
	if err := os.MkdirAll(filepath.Dir(cfg.DBPath), 0o700); err != nil {
		return fmt.Errorf("create database directory: %w", err)
	}
	if cfg.SpoolDir != "" {
		if err := os.MkdirAll(cfg.SpoolDir, 0o700); err != nil {
			return fmt.Errorf("create spool directory: %w", err)
		}
	}

	db, err := catalog.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer db.Close()

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

	service := &assets.Service{
		Store:        store,
		Catalog:      db,
		MaxBytes:     cfg.MaxUploadBytes,
		SpoolDir:     cfg.SpoolDir,
		SignedURLTTL: cfg.SignedURLTTL,
		Logger:       logger,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if cfg.Renditions {
		worker := &renditions.Worker{
			Catalog: db,
			Store:   store,
			Options: derive.Options{
				Image: imaging.Options{Widths: cfg.RenditionWidths, Quality: cfg.RenditionQuality},
				Video: video.Options{Widths: cfg.VideoWidths, CRF: cfg.VideoCRF, Preset: cfg.VideoPreset},
			},
			Logger:      logger,
			MaxAttempts: cfg.RenditionAttempts,
			MaxBytes:    cfg.MaxUploadBytes,
			Poll:        cfg.RenditionPoll,
			WorkDir:     cfg.SpoolDir,
		}
		// The upload path pokes the worker so a new image starts being
		// processed at once rather than at the next poll.
		service.Notify = worker.Wake
		go worker.Run(ctx)
	}

	server := &api.Server{
		Assets:         service,
		Auth:           &auth.APIKeys{Keys: api.CatalogKeys(db)},
		Catalog:        db,
		Version:        version,
		Logger:         logger,
		Identity:       &identity.GitHub{ClientID: cfg.GitHubClientID},
		ClientIPHeader: cfg.ClientIPHeader,
		AdminLogins:    cfg.AdminLogins,

		RenditionAttempts: cfg.RenditionAttempts,
	}

	httpServer := &http.Server{
		Addr:    cfg.Addr,
		Handler: server.Handler(),
		// Headers must arrive promptly; a body may take as long as a large
		// upload honestly needs, and no longer.
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       cfg.UploadTimeout,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	logger.Info("starting",
		"version", version,
		"addr", cfg.Addr,
		"bucket", cfg.S3Bucket,
		"public_base_url", cfg.PublicBaseURL,
		"max_upload_bytes", cfg.MaxUploadBytes,
		"renditions", cfg.Renditions,
		"sign_in", cfg.GitHubClientID != "",
		"admins", len(cfg.AdminLogins))

	return listen(ctx, httpServer, logger)
}

// listen serves until ctx is done, then drains.
func listen(ctx context.Context, server *http.Server, logger *slog.Logger) error {
	failed := make(chan error, 1)
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			failed <- err
		}
	}()

	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
		logger.Info("stopping")
	}

	shutdown, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	return server.Shutdown(shutdown)
}

func newLogger(level string) *slog.Logger {
	var parsed slog.Level
	if err := parsed.UnmarshalText([]byte(level)); err != nil {
		parsed = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: parsed}))
}
