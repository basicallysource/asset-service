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
	"github.com/basicallysource/asset-service/internal/objstore"
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
	case "keys":
		return keysCommand(args)
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
  asset-service serve                       run the HTTP service (default)
  asset-service keys add <name> <scope>...  mint an API key, printed once
  asset-service keys list                   list keys
  asset-service keys revoke <name>          make a key stop working
  asset-service version                     print the build version

A scope is <action>:<namespace>, where action is read or write and the
namespace may be *, for example write:docs or read:*.

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

	server := &api.Server{
		Assets: &assets.Service{
			Store:        store,
			Catalog:      db,
			MaxBytes:     cfg.MaxUploadBytes,
			SpoolDir:     cfg.SpoolDir,
			SignedURLTTL: cfg.SignedURLTTL,
		},
		Auth:    &auth.APIKeys{Keys: catalogKeys{db}},
		Catalog: db,
		Version: version,
		Logger:  logger,
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
		"max_upload_bytes", cfg.MaxUploadBytes)

	return listen(httpServer, logger)
}

// listen serves until a stop signal, then drains.
func listen(server *http.Server, logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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

// catalogKeys adapts the catalog to what auth needs, which is one lookup.
type catalogKeys struct{ db *catalog.DB }

func (c catalogKeys) APIKeyByID(ctx context.Context, id string) (auth.StoredKey, error) {
	key, err := c.db.APIKeyByID(ctx, id)
	if err != nil {
		return auth.StoredKey{}, err
	}
	return auth.StoredKey{
		ID:         key.ID,
		Name:       key.Name,
		SecretHash: key.SecretHash,
		Scopes:     key.Scopes,
		Revoked:    key.Revoked,
	}, nil
}
