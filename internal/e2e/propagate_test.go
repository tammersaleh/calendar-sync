//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// TestE2E_TargetEdit_Propagates pins SPEC §"Drift detection model" /
// `target_edited` propagate cell: when the mirror's managed fields drift
// from source AND source is writable AND propagate_target_edits=true, the
// drift flows back to the source instead of being reverted. The fixture
// calendars are owned (accessRole=owner), so the only knob the test
// touches is the [settings].propagate_target_edits gate.
//
// Sequence: insert source → run (insert mirror) → patch the MIRROR's
// summary directly → run again. Expected outcome:
// action=propagate, reason=target_edited, target_event=<mirror id>,
// fields=["summary"]. Then the source's own summary should match the
// edited value.
func TestE2E_TargetEdit_Propagates(t *testing.T) {
	h := Setup(t, SetupOptions{PropagateTargetEdits: true})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	originalTitle := h.Title("propagate-original")
	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary: originalTitle,
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

	// Patch the MIRROR. This bumps mirror.Updated; source.Updated stays
	// pinned to the original-write timestamp - so on the next run the
	// drift signals are (source_changed=false, mirror_drifted=true), the
	// `target_edited` cell.
	editedTitle := h.Title("propagate-edited")
	if _, err := h.GWS.EventsPatch(ctx, h.TargetCalID, mirrorID, &gws.Event{
		Summary: editedTitle,
	}); err != nil {
		t.Fatalf("patch mirror %s: %v", mirrorID, err)
	}

	res2 := h.Run(ctx)
	res2.AssertSuccess(t)
	propOut := res2.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionPropagate),
		Reason:      string(mirror.ReasonTargetEdited),
		SourceEvent: source.ID,
		TargetEvent: mirrorID,
	})

	// SPEC §"Field-level propagate": the outcome's `fields` slice carries
	// the names of the managed fields that drifted. Only summary did.
	if len(propOut.Fields) != 1 || propOut.Fields[0] != "summary" {
		t.Errorf("propagate outcome fields = %v, want [summary]", propOut.Fields)
	}

	// Source must now carry the edited summary - the whole point of the
	// propagate path.
	postSource, err := h.GWS.EventsGet(ctx, h.SourceCalID, source.ID)
	if err != nil {
		t.Fatalf("get source %s post-propagate: %v", source.ID, err)
	}
	if postSource.Summary != editedTitle {
		t.Errorf("post-propagate source.summary = %q, want %q", postSource.Summary, editedTitle)
	}
}
