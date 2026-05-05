//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// TestE2E_B17_TargetEditPropagatesNextTick pins B17's contract: target-side
// edits propagate to source within one tick (~15s) instead of waiting for
// the 24h FullSync interval. Pre-B17 this scenario sat idle until the next
// FullSync; post-B17 the target-delta phase catches it on the next tick.
func TestE2E_B17_TargetEditPropagatesNextTick(t *testing.T) {
	h := Setup(t, SetupOptions{PropagateTargetEdits: true})
	// 240s outer budget covers two ticks (insert + edit) plus EventsGet
	// round-trips plus SIGTERM teardown - same shape as the watch insert
	// scenario but with a target-side edit instead of source-side.
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()

	// Insert source pre-startup so the daemon's startup FullSync seeds the
	// mirror AND the new target syncToken. Without the seeded target
	// syncToken the target-delta phase has nothing to incrementally diff
	// against on subsequent ticks.
	sourceTitle := h.Title("b17-source-original")
	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary: sourceTitle,
		Start:   futureDateTime(0),
		End:     futureDateTime(1 * time.Hour),
	})

	w := startWatch(t, h)

	// Wait for the startup insert outcome - the mirror is now established
	// and the daemon has captured an initial target syncToken.
	insertOut := w.waitForOutcome(t, 60*time.Second, func(o Outcome) bool {
		return o.SourceEvent == source.ID && o.Action == string(mirror.ActionInsert)
	})
	mirrorID := insertOut.TargetEvent
	if mirrorID == "" {
		t.Fatal("watch insert outcome has empty target_event")
	}

	// Patch the MIRROR (target side). Pre-B17 the daemon would not see
	// this until the next FullSync (24h default). Post-B17 the
	// target-delta phase on the next tick catches it.
	editedTitle := h.Title("b17-mirror-edited")
	if _, err := h.GWS.EventsPatch(ctx, h.TargetCalID, mirrorID, &gws.PatchEvent{
		Summary: gws.PatchStr(editedTitle),
	}); err != nil {
		t.Fatalf("patch mirror %s: %v", mirrorID, err)
	}

	// Within ~one tick budget the daemon's target-delta phase should
	// emit a propagate outcome routing the mirror's edit back to source.
	propagateOut := w.waitForOutcome(t, 60*time.Second, func(o Outcome) bool {
		return o.SourceEvent == source.ID &&
			o.Action == string(mirror.ActionPropagate)
	})
	if propagateOut.Reason != string(mirror.ReasonTargetEdited) {
		t.Errorf("propagate reason = %q, want %q", propagateOut.Reason, mirror.ReasonTargetEdited)
	}
	if propagateOut.TargetEvent != mirrorID {
		t.Errorf("propagate target_event = %q, want %q", propagateOut.TargetEvent, mirrorID)
	}
	// Fields should include "summary" since that's the only field we
	// changed - pin it as a guard against an over-eager propagate that
	// claims more drift than actually occurred.
	foundSummary := false
	for _, f := range propagateOut.Fields {
		if f == "summary" {
			foundSummary = true
			break
		}
	}
	if !foundSummary {
		t.Errorf("propagate fields = %v, want to include summary", propagateOut.Fields)
	}

	// Verify the source actually got patched - the whole point of the
	// propagate path; an outcome row alone wouldn't catch a write that
	// silently failed downstream.
	patchedSource, err := h.GWS.EventsGet(ctx, h.SourceCalID, source.ID)
	if err != nil {
		t.Fatalf("get source after propagate: %v", err)
	}
	if patchedSource.Summary != editedTitle {
		t.Errorf("source.summary = %q, want %q (propagate didn't write back)",
			patchedSource.Summary, editedTitle)
	}

	w.stop(t)
}
