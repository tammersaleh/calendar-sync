package mirror

import (
	"fmt"
	"reflect"
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

// explicitFormOf rebuilds an Event from its OWN normalized managed fields,
// so `omitted` and `explicitFormOf(omitted)` are by definition the same event
// as far as the comparison layer is concerned.
//
// Driving this from ManagedFieldsFromEvent rather than hand-writing the
// expected values is what makes the test below generic: add a normalized
// field to ManagedFields and it is covered here automatically.
func explicitFormOf(e *gws.Event) *gws.Event {
	mf := ManagedFieldsFromEvent(e)
	toGWS := func(d EventDateTime) *gws.EventDateTime {
		if d == (EventDateTime{}) {
			return nil
		}
		return &gws.EventDateTime{Date: d.Date, DateTime: d.DateTime, TimeZone: d.TimeZone}
	}
	return &gws.Event{
		ID:           e.ID,
		Summary:      mf.Summary,
		Description:  mf.Description,
		Location:     mf.Location,
		Start:        toGWS(mf.Start),
		End:          toGWS(mf.End),
		Recurrence:   mf.Recurrence,
		Transparency: mf.Transparency,
		Visibility:   mf.Visibility,
	}
}

// The write layer must agree with the comparison layer that decides whether
// to call it. B37 happened because they disagreed: DriftedFieldNames read
// transparency through normalization and the builders read it raw, so a
// patch pushed a value the comparison considered identical - and in that
// case not even a legal one.
//
// This is deliberately a property over the whole PatchEvent rather than an
// assertion about the two enums that happen to be normalized today. If a
// third normalized field is added and a builder reads it raw, this fails
// without anyone remembering to extend the test. That generality is the
// point: B37 was a class of bug, not a one-off.
func TestPatchBuilders_AgreeWithDriftComparison(t *testing.T) {
	// Every field Google is known to omit left empty.
	omitted := &gws.Event{
		ID:      "evt",
		Summary: "Standup",
		Start:   &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:     &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
	}
	explicit := explicitFormOf(omitted)

	// Precondition: the comparison layer sees no difference at all.
	if got := DriftedFieldNames(omitted, explicit); len(got) != 0 {
		t.Fatalf("omitted and explicit forms must compare equal; drifted = %v", got)
	}

	if diff := patchBodyDiff(BuildSourceOverridePatchBody(omitted),
		BuildSourceOverridePatchBody(explicit)); diff != "" {
		t.Errorf("BuildSourceOverridePatchBody disagrees with DriftedFieldNames: %s", diff)
	}

	// Every managed field named, so the propagate builder is held to the
	// same standard as the full-state one.
	all := []string{"summary", "description", "location", "start", "end",
		"transparency", "visibility", "recurrence"}
	if diff := patchBodyDiff(BuildPropagatePatchBody(omitted, all),
		BuildPropagatePatchBody(explicit, all)); diff != "" {
		t.Errorf("BuildPropagatePatchBody disagrees with DriftedFieldNames: %s", diff)
	}
}

// patchBodyDiff reports the first field on which two patch bodies differ,
// or "" when they are identical. reflect.DeepEqual over the whole struct is
// what makes this generic across fields added later.
func patchBodyDiff(a, b *gws.PatchEvent) string {
	if reflect.DeepEqual(a, b) {
		return ""
	}
	av, bv := reflect.ValueOf(a).Elem(), reflect.ValueOf(b).Elem()
	for i := range av.NumField() {
		if !reflect.DeepEqual(av.Field(i).Interface(), bv.Field(i).Interface()) {
			return fmt.Sprintf("field %s: %s vs %s",
				av.Type().Field(i).Name,
				formatPatchField(av.Field(i)),
				formatPatchField(bv.Field(i)))
		}
	}
	return "bodies differ but no field mismatch found"
}

func formatPatchField(v reflect.Value) string {
	if v.Kind() == reflect.Ptr && v.IsNil() {
		return "<omitted>"
	}
	if v.Kind() == reflect.Ptr {
		return fmt.Sprintf("%q", v.Elem().Interface())
	}
	return fmt.Sprintf("%v", v.Interface())
}
