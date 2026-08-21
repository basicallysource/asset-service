// Package config turns the environment into one validated struct, once, at
// startup. Nothing else in the service reads an environment variable.
package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/imaging"
	"github.com/basicallysource/asset-service/internal/renditions"
)

// Prefix every variable this service reads.
const prefix = "ASSET_"

// Config is everything the service needs to run.
type Config struct {
	Addr     string
	LogLevel string

	DBPath   string
	SpoolDir string

	MaxUploadBytes int64
	UploadTimeout  time.Duration
	SignedURLTTL   time.Duration

	// PublicBaseURL is where readers fetch public objects -- a CDN or other
	// edge in front of the same bucket.
	PublicBaseURL string

	// Renditions controls whether derived forms are produced at all, and what
	// they look like when they are.
	Renditions        bool
	RenditionWidths   []int
	RenditionQuality  int
	RenditionPoll     time.Duration
	RenditionAttempts int

	S3Endpoint  string
	S3Region    string
	S3Bucket    string
	S3AccessKey string
	S3SecretKey string
	S3PathStyle bool
}

// Load reads and validates the environment. It reports every problem at once:
// a deploy that is missing three variables should say so on the first attempt.
func Load() (Config, error) {
	var problems []string

	cfg := Config{
		Addr:          optional("ADDR", ":8080"),
		LogLevel:      optional("LOG_LEVEL", "info"),
		DBPath:        required("DB_PATH", &problems),
		SpoolDir:      optional("SPOOL_DIR", ""),
		PublicBaseURL: strings.TrimRight(required("PUBLIC_BASE_URL", &problems), "/"),
		S3Endpoint:    required("S3_ENDPOINT", &problems),
		S3Region:      required("S3_REGION", &problems),
		S3Bucket:      required("S3_BUCKET", &problems),
		S3AccessKey:   required("S3_ACCESS_KEY", &problems),
		S3SecretKey:   required("S3_SECRET_KEY", &problems),
	}

	cfg.MaxUploadBytes = bytesValue("MAX_UPLOAD_BYTES", 256<<20, &problems)
	cfg.UploadTimeout = duration("UPLOAD_TIMEOUT", 30*time.Minute, &problems)
	cfg.SignedURLTTL = duration("SIGNED_URL_TTL", 15*time.Minute, &problems)
	cfg.S3PathStyle = boolean("S3_PATH_STYLE", false, &problems)
	cfg.Renditions = boolean("RENDITIONS", true, &problems)
	cfg.RenditionWidths = widths("RENDITION_WIDTHS", imaging.DefaultWidths, &problems)
	cfg.RenditionQuality = number("RENDITION_QUALITY", imaging.DefaultQuality, 1, 100, &problems)
	cfg.RenditionPoll = duration("RENDITION_POLL", renditions.DefaultPoll, &problems)
	cfg.RenditionAttempts = number("RENDITION_ATTEMPTS", renditions.DefaultMaxAttempts, 1, 100, &problems)

	if len(problems) > 0 {
		return Config{}, fmt.Errorf("config: %s", strings.Join(problems, "; "))
	}
	return cfg, nil
}

func lookup(name string) string { return strings.TrimSpace(os.Getenv(prefix + name)) }

func optional(name, fallback string) string {
	if v := lookup(name); v != "" {
		return v
	}
	return fallback
}

func required(name string, problems *[]string) string {
	v := lookup(name)
	if v == "" {
		*problems = append(*problems, prefix+name+" is required")
	}
	return v
}

func duration(name string, fallback time.Duration, problems *[]string) time.Duration {
	raw := lookup(name)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		*problems = append(*problems, prefix+name+" must be a positive duration like 15m")
		return fallback
	}
	return d
}

// bytesValue accepts a plain byte count or a size like 256MiB, because a
// deploy that means a quarter of a gigabyte should not have to count zeroes.
func bytesValue(name string, fallback int64, problems *[]string) int64 {
	raw := lookup(name)
	if raw == "" {
		return fallback
	}

	multiplier := int64(1)
	for suffix, factor := range map[string]int64{
		"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30,
	} {
		if trimmed, ok := strings.CutSuffix(raw, suffix); ok {
			raw, multiplier = strings.TrimSpace(trimmed), factor
			break
		}
	}

	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		*problems = append(*problems, prefix+name+" must be a positive size like 268435456 or 256MiB")
		return fallback
	}
	return n * multiplier
}

// widths parses a comma-separated list of image widths.
func widths(name string, fallback []int, problems *[]string) []int {
	raw := lookup(name)
	if raw == "" {
		return fallback
	}

	var parsed []int
	for _, field := range strings.Split(raw, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < 1 || n > 20000 {
			*problems = append(*problems, prefix+name+" must be a comma-separated list of pixel widths")
			return fallback
		}
		parsed = append(parsed, n)
	}
	sort.Ints(parsed)
	return parsed
}

func number(name string, fallback, low, high int, problems *[]string) int {
	raw := lookup(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < low || n > high {
		*problems = append(*problems, fmt.Sprintf("%s%s must be a number between %d and %d", prefix, name, low, high))
		return fallback
	}
	return n
}

func boolean(name string, fallback bool, problems *[]string) bool {
	raw := lookup(name)
	if raw == "" {
		return fallback
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		*problems = append(*problems, prefix+name+" must be true or false")
		return fallback
	}
	return v
}
