package feedimport

import (
	"context"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/ical"
)

// transpItem is allDayItem/timedItem plus an explicit TRANSP value.
func transpItem(it ical.Item, transp string) ical.Item {
	it.Transparency = transp
	return it
}

// TestBuildEvent_ForceAllDayBusy pins the force_all_day_busy override:
// when set, an all-day item is forced opaque (busy) regardless of its TRANSP,
// while timed items always keep their own TRANSP. The discriminator is the
// item's all-day START only.
func TestBuildEvent_ForceAllDayBusy(t *testing.T) {
	day := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	hour := time.Date(2026, 7, 13, 14, 0, 0, 0, time.UTC)

	transpAllDay := transpItem(allDayItem("span-1", "Trip", day, end), "TRANSPARENT")
	opaqueAllDay := transpItem(allDayItem("span-2", "Trip", day, end), "OPAQUE")
	transpTimed := transpItem(timedItem("flight-1", "Flight", day, hour), "TRANSPARENT")

	tests := []struct {
		name  string
		force bool
		item  ical.Item
		want  string
	}{
		{"flag off, transparent all-day stays transparent", false, transpAllDay, gws.TransparencyTransparent},
		{"flag on, transparent all-day forced opaque", true, transpAllDay, gws.TransparencyOpaque},
		{"flag on, opaque all-day stays opaque", true, opaqueAllDay, gws.TransparencyOpaque},
		{"flag on, transparent timed keeps transparent", true, transpTimed, gws.TransparencyTransparent},
		{"flag off, transparent timed stays transparent", false, transpTimed, gws.TransparencyTransparent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			im := &Importer{Target: "t@group.calendar.google.com", FeedID: testFeedID, ForceAllDayBusy: tt.force}
			ev := im.buildEvent(tt.item)
			if ev.Transparency != tt.want {
				t.Errorf("transparency = %q, want %q", ev.Transparency, tt.want)
			}
		})
	}
}

// TestReconcile_ForceAllDayBusyRepatches proves the checksum interaction: an
// all-day event previously imported as transparent (flag off) is repatched to
// opaque when the feed later runs with force_all_day_busy on. The forced value
// enters the checksum, so the flip is detected as drift and patched - no code
// path special-cases the flag outside the desired-event build.
func TestReconcile_ForceAllDayBusyRepatches(t *testing.T) {
	day := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	item := transpItem(allDayItem("span-1", "Trip", day, end), "TRANSPARENT")

	target := "t@group.calendar.google.com"
	s := newStub()
	imOff := &Importer{API: s, Target: target, FeedID: testFeedID, ForceAllDayBusy: false}
	imOn := &Importer{API: s, Target: target, FeedID: testFeedID, ForceAllDayBusy: true}

	// Seed the stub with the genuine flag-off event (transparent + its checksum).
	stored := imOff.buildEvent(item)
	stored.ID = DeterministicID(testFeedID, item.UID)
	s.events[stored.ID] = stored
	if stored.Transparency != gws.TransparencyTransparent {
		t.Fatalf("seed transparency = %q, want transparent", stored.Transparency)
	}

	if _, err := imOn.Reconcile(context.Background(), []ical.Item{item}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(s.patches) != 1 {
		t.Fatalf("got %d patches, want 1 (checksum changed by the flip)", len(s.patches))
	}
	tp := s.patches[0].body.Transparency
	if tp == nil || *tp != gws.TransparencyOpaque {
		t.Fatalf("patched transparency = %v, want opaque", tp)
	}
}

// TestReconcile_ForceAllDayBusyStableNoChurn proves the flip does not churn
// once applied: an all-day event already stored as opaque under the flag sees
// no checksum change on the next poll, so it is left unchanged (no patch).
func TestReconcile_ForceAllDayBusyStableNoChurn(t *testing.T) {
	day := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	end := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	item := transpItem(allDayItem("span-1", "Trip", day, end), "TRANSPARENT")

	target := "t@group.calendar.google.com"
	s := newStub()
	im := &Importer{API: s, Target: target, FeedID: testFeedID, ForceAllDayBusy: true}

	stored := im.buildEvent(item) // opaque + flag-on checksum
	stored.ID = DeterministicID(testFeedID, item.UID)
	s.events[stored.ID] = stored

	res, err := im.Reconcile(context.Background(), []ical.Item{item})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if len(s.patches) != 0 {
		t.Fatalf("got %d patches, want 0 (stable, no churn)", len(s.patches))
	}
	if res.Unchanged != 1 {
		t.Errorf("Unchanged = %d, want 1", res.Unchanged)
	}
}
