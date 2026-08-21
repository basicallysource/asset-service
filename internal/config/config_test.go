package config

import (
	"strings"
	"testing"
	"time"
)

func setRequired(t *testing.T) {
	t.Helper()
	for name, value := range map[string]string{
		"ASSET_DB_PATH":         "/var/lib/asset-service/catalog.db",
		"ASSET_PUBLIC_BASE_URL": "https://cdn.example/",
		"ASSET_S3_ENDPOINT":     "https://nyc3.example.com",
		"ASSET_S3_REGION":       "nyc3",
		"ASSET_S3_BUCKET":       "bucket",
		"ASSET_S3_ACCESS_KEY":   "key",
		"ASSET_S3_SECRET_KEY":   "secret",
	} {
		t.Setenv(name, value)
	}
}

func TestLoadDefaults(t *testing.T) {
	setRequired(t)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Addr != ":8080" || cfg.LogLevel != "info" {
		t.Errorf("addr = %q, log level = %q", cfg.Addr, cfg.LogLevel)
	}
	if cfg.MaxUploadBytes != 256<<20 || cfg.SignedURLTTL != 15*time.Minute {
		t.Errorf("max = %d, ttl = %s", cfg.MaxUploadBytes, cfg.SignedURLTTL)
	}
	if cfg.PublicBaseURL != "https://cdn.example" {
		t.Errorf("public base URL = %q, want the trailing slash gone", cfg.PublicBaseURL)
	}
}

func TestLoadReportsEveryMissingVariableAtOnce(t *testing.T) {
	t.Setenv("ASSET_DB_PATH", "")
	t.Setenv("ASSET_S3_BUCKET", "")

	_, err := Load()
	if err == nil {
		t.Fatal("Load succeeded with nothing configured")
	}
	for _, want := range []string{"ASSET_DB_PATH", "ASSET_S3_BUCKET", "ASSET_S3_ENDPOINT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestSizesAcceptUnits(t *testing.T) {
	setRequired(t)

	cases := map[string]int64{"1024": 1024, "256MiB": 256 << 20, "1GiB": 1 << 30, "64KiB": 64 << 10}
	for raw, want := range cases {
		t.Setenv("ASSET_MAX_UPLOAD_BYTES", raw)
		cfg, err := Load()
		if err != nil {
			t.Fatalf("%s: %v", raw, err)
		}
		if cfg.MaxUploadBytes != want {
			t.Errorf("%s parsed to %d, want %d", raw, cfg.MaxUploadBytes, want)
		}
	}
}

func TestBadValuesAreRejectedRatherThanIgnored(t *testing.T) {
	setRequired(t)

	for name, value := range map[string]string{
		"ASSET_MAX_UPLOAD_BYTES": "lots",
		"ASSET_SIGNED_URL_TTL":   "soon",
		"ASSET_S3_PATH_STYLE":    "maybe",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, value)
			if _, err := Load(); err == nil {
				t.Errorf("%s=%q was accepted", name, value)
			}
		})
	}
}

func TestNonPositiveSizesAndDurationsAreRejected(t *testing.T) {
	setRequired(t)

	t.Setenv("ASSET_MAX_UPLOAD_BYTES", "0")
	if _, err := Load(); err == nil {
		t.Error("a zero upload limit was accepted")
	}
	t.Setenv("ASSET_MAX_UPLOAD_BYTES", "")
	t.Setenv("ASSET_SIGNED_URL_TTL", "-5m")
	if _, err := Load(); err == nil {
		t.Error("a negative signed URL lifetime was accepted")
	}
}
