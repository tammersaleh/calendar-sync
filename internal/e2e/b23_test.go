//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// TestE2E_StaleBookkeeping pins SPEC §"`fields_disagree`: stale-bookkeeping
// fallback" / B23 (SPEC line 127-139). Stored source_updated and stored
// checksum can both report "no change since last write" while the source's
// CURRENT managed fields diverge from the mirror's CURRENT managed fields -
// e.g. after a source edit was followed by a no-op write that bumped
// stored bookkeeping ahead of the actual mirror state. The third signal
// (fields_disagree) catches that divergence by comparing source's live
// managed fields to mirror's live managed fields directly.
//
// We can't reproduce the no-op-write origin path against real APIs cleanly,
// so we synthesize the inconsistent state: edit the source's start time
// (advancing source.Updated and source's managed fields), then patch the
// mirror's calendar-sync:source_updated extended property so it equals
// source's NEW Updated stamp. The mirror's managed fields and stored
// checksum stay where they were (extendedProperties.private is not in the
// managed-field set), so the daemon sees source_changed=false +
// mirror_drifted=false + fields_disagree=true.
//
// Expected outcome: action=patch, reason=stale_bookkeeping, no conflict
// label (the daemon doesn't have evidence of a user-edit conflict, just
// bookkeeping divergence).
func TestE2E_StaleBookkeeping(t *testing.T) {
	h := Setup(t, SetupOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	originalStart := futureDateTime(0)
	originalEnd := futureDateTime(1 * time.Hour)
	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary: h.Title("b23-stale"),
		Start:   originalStart,
		End:     originalEnd,
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

	// Capture the stored checksum for the post-run sanity check that the
	// mirror's checksum advances when the stale_bookkeeping path rewrites it.
	preMirror, err := h.GWS.EventsGet(ctx, h.TargetCalID, mirrorID)
	if err != nil {
		t.Fatalf("get mirror %s: %v", mirrorID, err)
	}
	preChecksum := preMirror.ExtendedProperties.Private[mirror.ExtKeyChecksum]
	if preChecksum == "" {
		t.Fatalf("pre-edit mirror missing checksum (got %+v)", preMirror.ExtendedProperties.Private)
	}

	// Step 1: edit a managed field on the source. This bumps source.Updated
	// and produces a real divergence between source's and mirror's managed
	// fields once we sabotage stored bookkeeping below.
	movedStart := futureDateTime(2 * time.Hour)
	movedEnd := futureDateTime(3 * time.Hour)
	patchedSource, err := h.GWS.EventsPatch(ctx, h.SourceCalID, source.ID, &gws.PatchEvent{
		Start: movedStart,
		End:   movedEnd,
	})
	if err != nil {
		t.Fatalf("patch source start/end: %v", err)
	}
	if patchedSource.Updated == "" {
		t.Fatalf("post-patch source has empty Updated stamp: %+v", patchedSource)
	}
	if patchedSource.Updated == source.Updated {
		t.Fatalf("source.Updated did not advance after start patch: still %q", patchedSource.Updated)
	}

	// Step 2: poison stored bookkeeping. Set the mirror's
	// calendar-sync:source_updated to the NEW source.Updated. The daemon's
	// source_changed signal compares source.Updated to this stored value;
	// equal -> source_changed=false even though the source's managed fields
	// just changed. extendedProperties.private is NOT part of managed fields,
	// so the mirror's checksum (stored OR recomputed live) is unaffected;
	// mirror_drifted=false. Source's CURRENT managed fields differ from
	// mirror's CURRENT managed fields (start moved on source, mirror still
	// has the original start) -> fields_disagree=true.
	if _, err := h.GWS.EventsPatch(ctx, h.TargetCalID, mirrorID, &gws.PatchEvent{
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySourceUpdated: patchedSource.Updated,
			},
		},
	}); err != nil {
		t.Fatalf("poison mirror bookkeeping (calendar-sync:source_updated): %v", err)
	}

	// Step 3: run reconciliation. The daemon must see fields_disagree=true,
	// take the stale_bookkeeping cell, and rewrite the mirror from source.
	res2 := h.Run(ctx)
	res2.AssertSuccess(t)
	staleOut := res2.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionPatch),
		Reason:      string(mirror.ReasonStaleBookkeeping),
		SourceEvent: source.ID,
		TargetEvent: mirrorID,
	})
	// SPEC line 137: no Conflict label - the daemon doesn't have evidence
	// of a user-edit conflict, just bookkeeping divergence.
	if got := staleOut.Conflict; got != "" {
		t.Errorf("stale_bookkeeping outcome conflict = %q, want empty (SPEC line 137)", got)
	}

	// Post-run: the mirror's Start must now match the source's new Start
	// (the user-visible effect of the rewrite path), and the stored
	// checksum must advance to a fresh hash of the mirror's NEW managed
	// fields.
	postMirror, err := h.GWS.EventsGet(ctx, h.TargetCalID, mirrorID)
	if err != nil {
		t.Fatalf("get mirror %s post-run: %v", mirrorID, err)
	}
	if postMirror.Start == nil || patchedSource.Start == nil {
		t.Fatalf("nil start: mirror=%v source=%v", postMirror.Start, patchedSource.Start)
	}
	if postMirror.Start.DateTime != patchedSource.Start.DateTime {
		t.Errorf("post-run mirror.Start.DateTime = %q, want %q (matches patched source)",
			postMirror.Start.DateTime, patchedSource.Start.DateTime)
	}
	postChecksum := postMirror.ExtendedProperties.Private[mirror.ExtKeyChecksum]
	if postChecksum == "" {
		t.Fatal("post-run mirror has empty calendar-sync:checksum")
	}
	if postChecksum == preChecksum {
		t.Errorf("checksum did not change after stale_bookkeeping rewrite: still %q", postChecksum)
	}
	// Stored checksum must equal a fresh hash of the mirror's own current
	// managed fields - same contract as the happy-path assertion.
	live := mirror.ManagedFieldsFromEvent(postMirror)
	if expected := mirror.Checksum(live); postChecksum != expected {
		t.Errorf("stored checksum %q != recomputed %q (managed fields: %+v)", postChecksum, expected, live)
	}
}
