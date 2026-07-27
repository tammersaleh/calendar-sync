package mirror

import (
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// B37: the patch builders read live.Transparency / live.Visibility raw while
// the drift comparison reads them through ManagedFieldsFromEvent, which
// normalizes Google's omitted-means-default into the explicit form. On a
// resource where Google omitted the field, the builders therefore emitted
// `"transparency": ""` - not a member of the enum - and Calendar API answered
// HTTP 400.
//
// Observed in production the first tick after B28/B29 made the reverse-write
// path reachable: every tick retried the same events.patch, got 400, and
// pinned the target token. No data loss (the pin is the designed behaviour)
// but the edit could never land.
//
// The empty value must become the API default, NOT be omitted. Omitting it
// would leave a genuinely drifted source field unchanged, so the next tick
// would see the same drift and loop forever.

func eventWithTransparency(t, v string) *gws.Event {
	return &gws.Event{
		ID:           "evt",
		Summary:      "Standup",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Transparency: t,
		Visibility:   v,
	}
}

func TestBuildSourceOverridePatchBody_NeverEmitsEmptyEnums(t *testing.T) {
	body := BuildSourceOverridePatchBody(eventWithTransparency("", ""))

	if body.Transparency == nil {
		t.Fatal("Transparency must be sent, not omitted: an omitted field leaves real drift unresolved and the next tick loops")
	}
	if *body.Transparency != gws.TransparencyOpaque {
		t.Errorf("Transparency = %q, want %q", *body.Transparency, gws.TransparencyOpaque)
	}
	if body.Visibility == nil {
		t.Fatal("Visibility must be sent, not omitted")
	}
	if *body.Visibility != gws.VisibilityDefault {
		t.Errorf("Visibility = %q, want %q", *body.Visibility, gws.VisibilityDefault)
	}
}

func TestBuildSourceOverridePatchBody_PreservesExplicitEnums(t *testing.T) {
	body := BuildSourceOverridePatchBody(
		eventWithTransparency(gws.TransparencyTransparent, gws.VisibilityPrivate))

	if body.Transparency == nil || *body.Transparency != gws.TransparencyTransparent {
		t.Errorf("Transparency = %v, want transparent", body.Transparency)
	}
	if body.Visibility == nil || *body.Visibility != gws.VisibilityPrivate {
		t.Errorf("Visibility = %v, want private", body.Visibility)
	}
}

func TestBuildPropagatePatchBody_NeverEmitsEmptyEnums(t *testing.T) {
	// The propagate path only includes fields the drift check flagged. A
	// mirror whose transparency Google omitted, against a source that says
	// "transparent", is exactly that shape: drift is real, and the value we
	// must push is the normalized default.
	body := BuildPropagatePatchBody(
		eventWithTransparency("", ""),
		[]string{"transparency", "visibility"})

	if body.Transparency == nil {
		t.Fatal("Transparency was in the drifted set; omitting it would never resolve the drift")
	}
	if *body.Transparency != gws.TransparencyOpaque {
		t.Errorf("Transparency = %q, want %q", *body.Transparency, gws.TransparencyOpaque)
	}
	if body.Visibility == nil {
		t.Fatal("Visibility was in the drifted set; omitting it would never resolve the drift")
	}
	if *body.Visibility != gws.VisibilityDefault {
		t.Errorf("Visibility = %q, want %q", *body.Visibility, gws.VisibilityDefault)
	}
}

// The two builders must agree with the comparison that decides whether to
// call them. If DriftedFieldNames says two events match, a patch built from
// the live one must not then push a different value.
func TestPatchBuilders_AgreeWithDriftComparison(t *testing.T) {
	omitted := eventWithTransparency("", "")
	explicit := eventWithTransparency(gws.TransparencyOpaque, gws.VisibilityDefault)

	if got := DriftedFieldNames(omitted, explicit); len(got) != 0 {
		t.Fatalf("omitted and explicit forms must compare equal; drifted = %v", got)
	}

	fromOmitted := BuildSourceOverridePatchBody(omitted)
	fromExplicit := BuildSourceOverridePatchBody(explicit)

	if *fromOmitted.Transparency != *fromExplicit.Transparency {
		t.Errorf("patch bodies disagree on transparency: %q vs %q",
			*fromOmitted.Transparency, *fromExplicit.Transparency)
	}
	if *fromOmitted.Visibility != *fromExplicit.Visibility {
		t.Errorf("patch bodies disagree on visibility: %q vs %q",
			*fromOmitted.Visibility, *fromExplicit.Visibility)
	}
}
