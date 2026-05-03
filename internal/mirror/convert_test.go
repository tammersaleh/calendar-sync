package mirror

import (
	"reflect"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

func TestManagedFieldsFromEvent_ExtractsAllManagedFields(t *testing.T) {
	e := &gws.Event{
		Summary:      "x",
		Description:  "y",
		Location:     "Conference room A",
		Start:        &gws.EventDateTime{DateTime: "2026-04-30T12:00:00Z", TimeZone: "UTC"},
		End:          &gws.EventDateTime{Date: "2026-05-01"},
		Recurrence:   []string{"RRULE:FREQ=DAILY"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
		// Fields NOT in the managed set:
		ID:       "should-be-ignored",
		Updated:  "2026-04-29T23:00:00Z",
		HTMLLink: "ignored",
	}
	got := ManagedFieldsFromEvent(e)
	want := ManagedFields{
		Summary:      "x",
		Description:  "y",
		Location:     "Conference room A",
		Start:        EventDateTime{DateTime: "2026-04-30T12:00:00Z", TimeZone: "UTC"},
		End:          EventDateTime{Date: "2026-05-01"},
		Recurrence:   []string{"RRULE:FREQ=DAILY"},
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %#v\nwant %#v", got, want)
	}
}

func TestManagedFieldsFromEvent_NilStartEndProducesZero(t *testing.T) {
	// In practice every Event has start/end, but defensive nil-handling
	// avoids panics when a partial response sneaks through.
	e := &gws.Event{Summary: "x"}
	got := ManagedFieldsFromEvent(e)
	if got.Start != (EventDateTime{}) {
		t.Errorf("Start = %#v, want zero", got.Start)
	}
	if got.End != (EventDateTime{}) {
		t.Errorf("End = %#v, want zero", got.End)
	}
}

func TestManagedFieldsFromEvent_NormalizesTransparencyAndVisibility(t *testing.T) {
	// Google echoes back explicit transparency/visibility on patch responses
	// (BuildPayload sets them) but omits both on events.list responses when
	// they equal the default. Normalizing at the extraction boundary means
	// Checksum and DriftedFieldNames see the same values regardless of which
	// API path the Event came from. Without it, a stored checksum computed
	// from a patch response (Transparency="opaque") disagrees with a live
	// recompute from a list response (Transparency="") and fires
	// MirrorDrifted forever - the bug this normalization fixes.
	t.Run("transparency: empty becomes opaque", func(t *testing.T) {
		got := ManagedFieldsFromEvent(&gws.Event{Transparency: ""})
		if got.Transparency != gws.TransparencyOpaque {
			t.Errorf("Transparency = %q, want %q", got.Transparency, gws.TransparencyOpaque)
		}
	})
	t.Run("transparency: explicit opaque preserved", func(t *testing.T) {
		got := ManagedFieldsFromEvent(&gws.Event{Transparency: gws.TransparencyOpaque})
		if got.Transparency != gws.TransparencyOpaque {
			t.Errorf("Transparency = %q, want %q", got.Transparency, gws.TransparencyOpaque)
		}
	})
	t.Run("transparency: non-default preserved", func(t *testing.T) {
		got := ManagedFieldsFromEvent(&gws.Event{Transparency: gws.TransparencyTransparent})
		if got.Transparency != gws.TransparencyTransparent {
			t.Errorf("Transparency = %q, want %q", got.Transparency, gws.TransparencyTransparent)
		}
	})
	t.Run("visibility: empty becomes default", func(t *testing.T) {
		got := ManagedFieldsFromEvent(&gws.Event{Visibility: ""})
		if got.Visibility != gws.VisibilityDefault {
			t.Errorf("Visibility = %q, want %q", got.Visibility, gws.VisibilityDefault)
		}
	})
	t.Run("visibility: explicit default preserved", func(t *testing.T) {
		got := ManagedFieldsFromEvent(&gws.Event{Visibility: gws.VisibilityDefault})
		if got.Visibility != gws.VisibilityDefault {
			t.Errorf("Visibility = %q, want %q", got.Visibility, gws.VisibilityDefault)
		}
	})
	t.Run("visibility: non-default preserved", func(t *testing.T) {
		got := ManagedFieldsFromEvent(&gws.Event{Visibility: gws.VisibilityPrivate})
		if got.Visibility != gws.VisibilityPrivate {
			t.Errorf("Visibility = %q, want %q", got.Visibility, gws.VisibilityPrivate)
		}
	})
}

func TestManagedFieldsFromEvent_ChecksumStableAcrossRoundTrip(t *testing.T) {
	// The whole point of this conversion: extracting from a gws.Event
	// produces a hash equal to constructing the equivalent ManagedFields
	// directly. Pin that contract, including Location since v3.
	e := &gws.Event{
		Summary:      "Lunch",
		Description:  "with bob",
		Location:     "Office cafe",
		Start:        &gws.EventDateTime{DateTime: "2026-04-30T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-04-30T13:00:00Z"},
		Transparency: "opaque",
		Visibility:   "private",
	}
	fromGWS := Checksum(ManagedFieldsFromEvent(e))
	fromManual := Checksum(ManagedFields{
		Summary:      "Lunch",
		Description:  "with bob",
		Location:     "Office cafe",
		Start:        EventDateTime{DateTime: "2026-04-30T12:00:00Z"},
		End:          EventDateTime{DateTime: "2026-04-30T13:00:00Z"},
		Transparency: "opaque",
		Visibility:   "private",
	})
	if fromGWS != fromManual {
		t.Errorf("checksum drift between extraction and manual construction:\n  fromGWS    = %s\n  fromManual = %s",
			fromGWS, fromManual)
	}
}
