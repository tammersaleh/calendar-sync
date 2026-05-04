//go:build e2e

package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// reasonSourceCancelled is the SPEC-defined wire string for the
// source-deleted cell. The sync package owns the typed constant
// (sync.ReasonSourceCancelled); we pin the wire string directly so the
// e2e package doesn't depend on a sync-layer implementation detail.
const reasonSourceCancelled = "source_cancelled"

// TestE2E_SourceDeleted_DeleteMirror pins SPEC's source_cancelled cell
// (action=delete, reason=source_cancelled). When the source is deleted
// out from under a mirror, the next reconciliation must remove the
// mirror.
//
// Verifying the post-delete mirror state is fiddly: Calendar API's
// events.delete returns 204 but a follow-up events.get against the
// deleted ID often returns 410 Gone instead of a tombstone. We tolerate
// 404/410 by falling back to events.list with showDeleted=true (the
// same trick the wipeCalendar helper uses) and asserting status from
// whichever surfaces.
func TestE2E_SourceDeleted_DeleteMirror(t *testing.T) {
	h := Setup(t, SetupOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary: h.Title("source-deleted"),
		Start:   futureDateTime(0),
		End:     futureDateTime(1 * time.Hour),
	})

	res := h.Run(ctx)
	res.AssertSuccess(t)
	insertOut := res.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionInsert),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: source.ID,
	})
	mirrorID := insertOut.TargetEvent
	if mirrorID == "" {
		t.Fatal("insert outcome has empty target_event")
	}

	if err := h.GWS.EventsDelete(ctx, h.SourceCalID, source.ID); err != nil {
		t.Fatalf("delete source %s: %v", source.ID, err)
	}

	res2 := h.Run(ctx)
	res2.AssertSuccess(t)
	// reason="source_cancelled" is owned by the sync package (as
	// sync.ReasonSourceCancelled). Pinning the literal SPEC string here
	// keeps the e2e package free of implementation-layer imports - the
	// wire shape is the contract.
	deleteOut := res2.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionDelete),
		Reason:      reasonSourceCancelled,
		SourceEvent: source.ID,
		TargetEvent: mirrorID,
	})
	if deleteOut.TargetEvent != mirrorID {
		t.Errorf("delete outcome target_event = %q, want %q", deleteOut.TargetEvent, mirrorID)
	}

	// Confirm the mirror is now cancelled. events.get on a cancelled
	// event commonly 410s; fall back to a showDeleted-true list and
	// pluck the tombstone if so.
	mir, err := h.GWS.EventsGet(ctx, h.TargetCalID, mirrorID)
	if err != nil {
		if !errors.Is(err, gws.ErrAPINotFound) && !errors.Is(err, gws.ErrAPIGone) {
			t.Fatalf("get mirror %s post-delete: %v", mirrorID, err)
		}
		mir = findTombstone(t, h, ctx, mirrorID)
	}
	if mir.Status != gws.EventStatusCancelled {
		t.Errorf("post-delete mirror.status = %q, want %q", mir.Status, gws.EventStatusCancelled)
	}
}

// findTombstone locates a deleted event by ID via events.list with
// showDeleted=true. Used as a fallback when events.get returns 404/410
// for a cancelled event - the tombstone still exists in delta-listing
// land for the syncToken window, just not as a directly-gettable
// resource.
func findTombstone(t *testing.T, h *Harness, ctx context.Context, eventID string) *gws.Event {
	t.Helper()
	events, _, err := h.GWS.EventsList(ctx, gws.EventsListParams{
		CalendarID:  h.TargetCalID,
		ShowDeleted: true,
		MaxResults:  2500,
	})
	if err != nil {
		t.Fatalf("list target with showDeleted=true: %v", err)
	}
	for i := range events {
		if events[i].ID == eventID {
			return &events[i]
		}
	}
	t.Fatalf("could not locate mirror %s on target via showDeleted-list (events listed: %d)", eventID, len(events))
	return nil
}
