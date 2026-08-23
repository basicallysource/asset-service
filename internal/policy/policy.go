// Package policy decides what an account may do.
//
// The numbers here are set so that nobody working normally will ever meet one
// and anybody abusing the service will meet one almost immediately. That is
// the only useful place to put a limit: high enough to be invisible, low
// enough that the answer to "can I host a hundred gigabytes of someone else's
// files here" is no.
//
// Limits belong to an account, never to a token. Minting another token must
// not buy more capacity, or every number below is decoration.
package policy

import (
	"strings"
	"time"

	"github.com/basicallysource/asset-service/internal/catalog"
)

// Limits are what one account may do.
type Limits struct {
	// MaxFileBytes is the largest single upload. Zero means the service's own
	// limit is the only one.
	MaxFileBytes int64
	// UploadsPerHour, UploadsPerDay and UploadsPerWeek are rolling windows
	// over how many files an account has actually stored; BytesPerDay is the
	// same kind of window over their size. Zero means unlimited.
	UploadsPerHour int
	UploadsPerDay  int
	UploadsPerWeek int
	BytesPerDay    int64
	// MaxLiveTokens caps how many usable credentials an account can hold.
	MaxLiveTokens int
	// TokenLifetime is how long a self-served token lasts.
	TokenLifetime time.Duration
	// ContentTypes is what may be uploaded. Empty means anything.
	ContentTypes []string
}

// Unlimited is what an operator-minted key gets: a credential made by hand on
// the host is already as trusted as the host.
var Unlimited = Limits{}

// For returns the limits of a tier.
func For(tier string) Limits {
	switch tier {
	case catalog.TierAdmin:
		// Whoever runs this service: no ceiling on what they may do, because
		// the alternative is a shell on the host. A time limit all the same --
		// a credential handed out by a web page should not outlive anybody's
		// memory of asking for it.
		return Limits{TokenLifetime: 365 * 24 * time.Hour}

	case catalog.TierContributor:
		// Someone an operator has promoted: everything the open door gets,
		// five times over, files big enough for video, and any content type.
		// Still bounded, because a compromised laptop should not be able to
		// fill a bucket overnight.
		return Limits{
			MaxFileBytes:   160 << 20,
			UploadsPerHour: 1000,
			UploadsPerDay:  1000,
			UploadsPerWeek: 2000,
			BytesPerDay:    10 << 30,
			MaxLiveTokens:  20,
			TokenLifetime:  365 * 24 * time.Hour,
		}

	case catalog.TierBlocked:
		return Limits{MaxFileBytes: -1}

	default:
		// Anyone who signed in. Uploading the images for a documentation page
		// is a handful of files and a few tens of megabytes; two hundred in a
		// day covers several pages in one sitting. Anybody sustaining that
		// pace across a week is either a contributor -- promote them -- or
		// abusing the service.
		return Limits{
			MaxFileBytes:   32 << 20,
			UploadsPerHour: 200,
			UploadsPerDay:  200,
			UploadsPerWeek: 400,
			BytesPerDay:    2 << 30,
			MaxLiveTokens:  5,
			TokenLifetime:  90 * 24 * time.Hour,
			// Images only. This is what contributors actually need, and it is
			// what keeps the service from becoming a way to hand out
			// executables from a domain that looks like ours.
			ContentTypes: []string{"image/jpeg", "image/png", "image/webp", "image/gif"},
		}
	}
}

// Scopes are what a self-served token gets. An account is confined to a
// namespace named after it -- anything wider is a decision somebody with admin
// makes deliberately, one key at a time.
func Scopes(tier, handle string) []string {
	namespace := Namespace(handle)
	scopes := []string{"write:" + namespace, "read:" + namespace}
	if tier == catalog.TierAdmin {
		// Read and write everywhere, and the right to hand out credentials.
		scopes = []string{"write:*", "read:*", "admin:*"}
	}
	return scopes
}

// Namespace is where an account may write when it is confined to one.
func Namespace(handle string) string {
	slug := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, handle)
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "anon"
	}
	return "u-" + slug
}

// Upload is what an account is asking to do.
type Upload struct {
	ContentType string
	// Size is the declared body size, or -1 when the caller did not say.
	Size int64
}

// Reasons an upload can be refused. They map onto the API's error codes.
const (
	CodeForbidden       = "forbidden"
	CodeUnsupportedType = "unsupported_type"
	CodeTooLarge        = "too_large"
	CodeRateLimited     = "rate_limited"
)

// Decision is the answer, and why.
type Decision struct {
	Allowed bool
	// Code is one of httpx's error codes.
	Code    string
	Message string
	// RetryAfter is set when the answer would be different later.
	RetryAfter time.Duration
}

var allowed = Decision{Allowed: true}

// Evaluate answers whether an upload may proceed. It is a pure function of the
// limits, the request and what the account has already used, so the rules can
// be read and tested in one place rather than inferred from handlers.
func Evaluate(limits Limits, up Upload, lastHour, lastDay, lastWeek catalog.Usage) Decision {
	if limits.MaxFileBytes < 0 {
		return Decision{Code: CodeForbidden, Message: "this account may not upload"}
	}

	if len(limits.ContentTypes) > 0 && !permits(limits.ContentTypes, up.ContentType) {
		return Decision{
			Code:    CodeUnsupportedType,
			Message: "this account may upload " + strings.Join(limits.ContentTypes, ", "),
		}
	}

	if limits.MaxFileBytes > 0 && up.Size > limits.MaxFileBytes {
		return Decision{
			Code:    CodeTooLarge,
			Message: "this account may upload files up to " + humanBytes(limits.MaxFileBytes),
		}
	}

	if limits.UploadsPerHour > 0 && lastHour.Uploads >= limits.UploadsPerHour {
		return Decision{
			Code:       CodeRateLimited,
			Message:    "this account has reached its hourly upload limit",
			RetryAfter: time.Hour,
		}
	}

	if limits.UploadsPerDay > 0 && lastDay.Uploads >= limits.UploadsPerDay {
		return Decision{
			Code:       CodeRateLimited,
			Message:    "this account has reached its daily upload limit",
			RetryAfter: 24 * time.Hour,
		}
	}

	if limits.UploadsPerWeek > 0 && lastWeek.Uploads >= limits.UploadsPerWeek {
		return Decision{
			Code:       CodeRateLimited,
			Message:    "this account has reached its weekly upload limit",
			RetryAfter: 7 * 24 * time.Hour,
		}
	}

	if limits.BytesPerDay > 0 && lastDay.Bytes >= limits.BytesPerDay {
		return Decision{
			Code:       CodeRateLimited,
			Message:    "this account has reached its daily upload limit of " + humanBytes(limits.BytesPerDay),
			RetryAfter: 24 * time.Hour,
		}
	}

	return allowed
}

// permits matches a content type ignoring any parameters after it, so
// "image/png" covers "image/png; charset=binary".
func permits(allowedTypes []string, contentType string) bool {
	base := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.Index(base, ";"); i >= 0 {
		base = strings.TrimSpace(base[:i])
	}
	for _, candidate := range allowedTypes {
		if candidate == base {
			return true
		}
	}
	return false
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return itoa(n/(1<<30)) + " GiB"
	case n >= 1<<20:
		return itoa(n/(1<<20)) + " MiB"
	case n >= 1<<10:
		return itoa(n/(1<<10)) + " KiB"
	default:
		return itoa(n) + " bytes"
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var out []byte
	for n > 0 {
		out = append([]byte{byte('0' + n%10)}, out...)
		n /= 10
	}
	return string(out)
}
