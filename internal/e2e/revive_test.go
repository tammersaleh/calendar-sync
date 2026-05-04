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

// TestE2E_Revive_CancelledMirror pins SPEC §"Drift detection model" / B20:
// when a mirror is cancelled out of band but the source remains syncable,
// the next run must revive it rather than leaving it cancelled forever.
// BuildInventory skips cancelled tombstones (inventory.go pass 2), so the
// source-tuple looks like an inventory miss; doInsert then 409s on
// Google's still-reserved deterministic ID, fetches the cancelled
// existing event, and routes to reviveCancelledMirror. Outcome shape:
// action=insert, reason=source_updated, target_event=<original mirror ID>.
func TestE2E_Revive_CancelledMirror(t *testing.T) {
	h := Setup(t, SetupOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sourceTitle := h.Title("revive-source")
	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary: sourceTitle,
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
		t.Fatal("initial insert outcome has empty target_event")
	}

	// Bypass-cancel the mirror directly via Calendar API. This is the
	// out-of-band path - calendar-sync did not emit this cancellation,
	// so its bookkeeping won't match the post-cancel state.
	if _, err := h.GWS.EventsPatch(ctx, h.TargetCalID, mirrorID, &gws.Event{
		Status: gws.EventStatusCancelled,
	}); err != nil {
		t.Fatalf("bypass-cancel mirror %s: %v", mirrorID, err)
	}

	res2 := h.Run(ctx)
	res2.AssertSuccess(t)
	reviveOut := res2.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionInsert),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: source.ID,
		TargetEvent: mirrorID,
	})
	if reviveOut.TargetEvent != mirrorID {
		t.Errorf("revive outcome target_event = %q, want %q (revive reuses the same id)", reviveOut.TargetEvent, mirrorID)
	}

	mir, err := h.GWS.EventsGet(ctx, h.TargetCalID, mirrorID)
	if err != nil {
		// A 410 here would mean Google didn't accept the revive's
		// status=confirmed and the event is still tombstoned. Fall back
		// to the same showDeleted-list trick the delete test uses so we
		// can fail with the actual post-state instead of a bare error.
		if !errors.Is(err, gws.ErrAPINotFound) && !errors.Is(err, gws.ErrAPIGone) {
			t.Fatalf("get revived mirror %s: %v", mirrorID, err)
		}
		mir = findTombstone(t, h, ctx, mirrorID)
	}
	if mir.Status != gws.EventStatusConfirmed {
		t.Errorf("revived mirror.status = %q, want %q", mir.Status, gws.EventStatusConfirmed)
	}
	if mir.Summary != sourceTitle {
		t.Errorf("revived mirror.summary = %q, want %q", mir.Summary, sourceTitle)
	}
	if mir.ExtendedProperties == nil || mir.ExtendedProperties.Private == nil {
		t.Fatalf("revived mirror missing extendedProperties.private; got %+v", mir.ExtendedProperties)
	}
	priv := mir.ExtendedProperties.Private
	if got := priv[mirror.ExtKeyVersion]; got != mirror.SchemaVersion {
		t.Errorf("revived mirror %s = %q, want %q", mirror.ExtKeyVersion, got, mirror.SchemaVersion)
	}
	if got := priv[mirror.ExtKeyChecksum]; got == "" {
		t.Errorf("revived mirror %s is empty (revive runs the standard checksum follow-up)", mirror.ExtKeyChecksum)
	}
	if got := priv[mirror.ExtKeySource]; got == "" {
		t.Errorf("revived mirror %s is empty", mirror.ExtKeySource)
	}
}
