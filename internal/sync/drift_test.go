package sync

import (
	"context"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// TestDoPropagate_EmptyFieldsDegradesToStaleBookkeeping pins the empty-
// drifted-fields guard in doPropagate. MirrorDrifted=true (stored
// checksum doesn't match a fresh hash of the mirror's managed fields)
// can co-exist with DriftedFieldNames()=[] when the live managed fields
// actually match desired-from-source - the stored checksum is corrupt
// but the data on the mirror itself is fine.
//
// Pre-fix doPropagate would issue an EventsPatch against the SOURCE with
// an empty *PatchEvent body (all pointer fields nil, marshaling to `{}`).
// Either Calendar API rejects empty merge patches (perpetual pdir
// failure) or accepts them as a no-op (wasted RPC every tick to fix
// mirror-side bookkeeping that should be repaired locally).
//
// Post-fix the empty-fields branch routes through
// degradePropagateToStaleBookkeeping: no source-side write, one mirror-
// side patch + checksum follow-up to refresh the stored bookkeeping,
// outcome is action=patch / reason=stale_bookkeeping (matching SPEC's
// action/reason table on line 571 - the user-visible signal is "we
// repaired bookkeeping," not "we propagated edits to source").
func TestDoPropagate_EmptyFieldsDegradesToStaleBookkeeping(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	// Build a mirror whose live managed fields match desired-from-source
	// (start, end, summary, description, transparency, visibility all
	// equal). makeCleanCurrentMirror normally pins the stored checksum
	// to the live managed fields - we then deliberately corrupt it so
	// MirrorDrifted=true but DriftedFieldNames()=[] is the production
	// shape for this branch.
	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		source.Updated, "2026-04-30T10:00:00Z", // mirror Updated newer; routes to !source_changed && mirror_drifted
		source.Summary, source.Start, source.End)
	mirrorEv.ExtendedProperties.Private[mirror.ExtKeyChecksum] = "sha256:deadbeef"
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
	inv.Set(tuple, mirrorEv)

	// Sanity: the corruption produces the exact signals that drive the bug.
	desired := mirror.BuildPayload("src-cal", source)
	signal := mirror.ComputeDriftSignal(source, mirrorEv, desired)
	if !signal.MirrorDrifted {
		t.Fatalf("test setup wrong: MirrorDrifted should be true (corrupt stored checksum); got %+v", signal)
	}
	if signal.SourceChanged {
		t.Fatalf("test setup wrong: SourceChanged should be false; got %+v", signal)
	}
	if got := mirror.DriftedFieldNames(mirrorEv, desired); len(got) != 0 {
		t.Fatalf("test setup wrong: DriftedFieldNames should be empty (live==desired); got %v", got)
	}

	// Two mirror-side EventsPatch calls expected: main (rewrite from
	// source) + checksum follow-up. NO source-side patch.
	postMain := *mirrorEv
	postMain.Updated = "2026-04-30T11:00:00Z"
	api.queuePatch(&postMain)
	postChecksum := postMain
	api.queuePatch(&postChecksum)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}

	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonStaleBookkeeping {
		t.Errorf("outcome = %s/%s, want patch/stale_bookkeeping", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictNone {
		t.Errorf("Conflict = %q, want empty", got.Conflict)
	}
	if len(got.Fields) != 0 {
		t.Errorf("Fields = %v, want empty (no fields drifted)", got.Fields)
	}
	if got.SourceUpdated != "" || got.MirrorUpdated != "" {
		t.Errorf("conflict timestamps must be empty for non-conflict outcome; got src=%q mir=%q",
			got.SourceUpdated, got.MirrorUpdated)
	}

	// The critical assertion: NO source-side EventsPatch. An empty {}
	// patch on source is exactly the bug this test is pinning.
	for _, call := range api.callsByOp("EventsPatch") {
		if call.CalendarID == "src-cal" {
			t.Errorf("must not patch source on empty-fields propagate; got %+v", call)
		}
	}

	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 mirror-side EventsPatch (main + checksum); got %d", len(patches))
	}
	for i, p := range patches {
		if p.CalendarID != "tgt-cal" || p.EventID != "mi-1" {
			t.Errorf("patches[%d] should target the mirror (tgt-cal/mi-1); got %s/%s",
				i, p.CalendarID, p.EventID)
		}
	}

	// Inventory replaced with the post-write resource.
	updated, ok := inv.Lookup(tuple)
	if !ok {
		t.Fatal("inventory must still hold the mirror after stale-bookkeeping degrade")
	}
	if updated.Updated != postChecksum.Updated {
		t.Errorf("inventory mirror's Updated = %q, want %q (post-write resource)",
			updated.Updated, postChecksum.Updated)
	}
}
