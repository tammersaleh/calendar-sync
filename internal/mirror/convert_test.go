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
