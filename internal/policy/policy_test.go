package policy

import (
	"testing"
	"time"

	"github.com/basicallysource/asset-service/internal/catalog"
)

func TestAnUnknownAccountIsThrottledByTheDayAndTheWeek(t *testing.T) {
	limits := For(catalog.TierUnknown)
	upload := Upload{ContentType: "image/png", Size: 1024}

	if d := Evaluate(limits, upload, catalog.Usage{}, catalog.Usage{}, catalog.Usage{}); !d.Allowed {
		t.Fatalf("a first upload was refused: %s", d.Message)
	}

	day := catalog.Usage{Uploads: limits.UploadsPerDay}
	if d := Evaluate(limits, upload, catalog.Usage{}, day, day); d.Allowed || d.RetryAfter != 24*time.Hour {
		t.Errorf("at the daily cap: allowed=%v, retry after %s", d.Allowed, d.RetryAfter)
	}

	// A week of modest days still runs out: the weekly cap binds even when
	// no single day did.
	week := catalog.Usage{Uploads: limits.UploadsPerWeek}
	if d := Evaluate(limits, upload, catalog.Usage{}, catalog.Usage{}, week); d.Allowed || d.RetryAfter != 7*24*time.Hour {
		t.Errorf("at the weekly cap: allowed=%v, retry after %s", d.Allowed, d.RetryAfter)
	}
}

// The contributor tier is defined as five times the open door, with the
// content-type fence removed. If somebody changes one side, this is what
// reminds them the ratio is the deal.
func TestAContributorGetsFiveTimesTheOpenDoor(t *testing.T) {
	open, contributor := For(catalog.TierUnknown), For(catalog.TierContributor)

	if contributor.UploadsPerHour != 5*open.UploadsPerHour ||
		contributor.UploadsPerDay != 5*open.UploadsPerDay ||
		contributor.UploadsPerWeek != 5*open.UploadsPerWeek ||
		contributor.MaxFileBytes != 5*open.MaxFileBytes ||
		contributor.BytesPerDay != 5*open.BytesPerDay {
		t.Errorf("contributor limits are not five times the open door:\n%+v\nvs\n%+v", contributor, open)
	}
	if len(contributor.ContentTypes) != 0 {
		t.Errorf("a contributor should not be limited to %v", contributor.ContentTypes)
	}
}
