package policy

import (
	"strings"
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

// A manager is an admin for everything it does to its own work: the same
// limits and the same self-served scopes.
func TestAManagerHasAnAdminsLimitsAndScopes(t *testing.T) {
	if For(catalog.TierManager).MaxFileBytes != For(catalog.TierAdmin).MaxFileBytes ||
		For(catalog.TierManager).UploadsPerDay != For(catalog.TierAdmin).UploadsPerDay ||
		For(catalog.TierManager).TokenLifetime != For(catalog.TierAdmin).TokenLifetime {
		t.Errorf("manager limits %+v differ from admin %+v", For(catalog.TierManager), For(catalog.TierAdmin))
	}
	manager, admin := Scopes(catalog.TierManager, "m"), Scopes(catalog.TierAdmin, "a")
	if strings.Join(manager, " ") != strings.Join(admin, " ") {
		t.Errorf("manager scopes %v, admin scopes %v", manager, admin)
	}
}

func TestWhoMayMoveWhichAccounts(t *testing.T) {
	cases := []struct {
		by, from, to string
		want         bool
	}{
		{NoAccount, catalog.TierAdmin, catalog.TierBlocked, true},
		{catalog.TierAdmin, catalog.TierManager, catalog.TierUnknown, true},
		{catalog.TierAdmin, catalog.TierUnknown, catalog.TierManager, true},
		{catalog.TierManager, catalog.TierUnknown, catalog.TierContributor, true},
		{catalog.TierManager, catalog.TierContributor, catalog.TierUnknown, true},
		{catalog.TierManager, catalog.TierUnknown, catalog.TierManager, false},
		{catalog.TierManager, catalog.TierUnknown, catalog.TierAdmin, false},
		{catalog.TierManager, catalog.TierUnknown, catalog.TierBlocked, false},
		{catalog.TierManager, catalog.TierBlocked, catalog.TierUnknown, false},
		{catalog.TierManager, catalog.TierManager, catalog.TierContributor, false},
		{catalog.TierManager, catalog.TierAdmin, catalog.TierUnknown, false},
		{catalog.TierContributor, catalog.TierUnknown, catalog.TierContributor, false},
		{catalog.TierUnknown, catalog.TierUnknown, catalog.TierContributor, false},
	}
	for _, c := range cases {
		if got := MayMoveAccount(c.by, c.from, c.to); got != c.want {
			t.Errorf("%q moving %s to %s: %v, want %v", c.by, c.from, c.to, got, c.want)
		}
	}

	if RevokesAnyKey(catalog.TierManager) || !RevokesAnyKey(catalog.TierAdmin) || !RevokesAnyKey(NoAccount) {
		t.Error("only an admin or a key with no account revokes any key")
	}
}
