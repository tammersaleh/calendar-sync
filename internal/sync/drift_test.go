package sync

import (
	"context"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// TestDoPropagate_RecurringParent_DoesNotMoveSourceAnchor is the B38
// regression pin. A recurring source PARENT (Recurrence set, no
// RecurringEventID) whose mirror parent shows start/end drift with the
// source unchanged used to reach doPropagate and patch the SOURCE parent's
// start/end via BuildPropagatePatchBody. Patching a recurring parent's start
// on the parent endpoint moves the ENTIRE series anchor, shifting every
// occurrence - exactly the live damage recorded in doc/bugs.md B38 (the whole
// weekly series jumped 15 minutes off a single-occurrence interaction).
//
// The load-bearing invariant: no matter what caused the mirror parent to
// appear drifted on start/end, the daemon must never reverse-write those
// timing fields to the source parent. A false-positive drift there destroys
// the series irrecoverably; the asymmetry against the rare convenience of
// dragging a whole series from the mirror side makes refusal the safe choice.
func TestDoPropagate_RecurringParent_DoesNotMoveSourceAnchor(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	// Source recurring parent at 07:25-08:10, weekly.
	start0725 := &gws.EventDateTime{DateTime: "2026-08-10T14:25:00Z"}
	end0810 := &gws.EventDateTime{DateTime: "2026-08-10T15:10:00Z"}
	source := &gws.Event{
		ID:           "src-parent",
		Status:       gws.EventStatusConfirmed,
		Summary:      "Breakfast",
		Description:  "Breakfast",
		Start:        start0725,
		End:          end0810,
		Recurrence:   []string{"RRULE:FREQ=WEEKLY;BYDAY=MO,TU,WE"},
		Updated:      "2026-08-01T00:00:00Z",
		Transparency: gws.TransparencyOpaque,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
	}

	// Mirror parent built clean against the source (07:25), recurrence set,
	// checksum recomputed to reflect that clean state. Then drift the LIVE
	// start/end to 07:40-08:25 WITHOUT touching the stored checksum - the
	// production shape of a parent that appears edited on the mirror side
	// (MirrorDrifted=true) while the source is unchanged (!SourceChanged).
	mirrorEv := makeCleanCurrentMirror("mp-1", "src-cal:src-parent",
		source.Updated, "2026-08-02T00:00:00Z",
		"Breakfast", start0725, end0810)
	mirrorEv.Recurrence = source.Recurrence
	mirrorEv.ExtendedProperties.Private[mirror.ExtKeyChecksum] =
		mirror.Checksum(mirror.ManagedFieldsFromEvent(mirrorEv))
	mirrorEv.Start = &gws.EventDateTime{DateTime: "2026-08-10T14:40:00Z"}
	mirrorEv.End = &gws.EventDateTime{DateTime: "2026-08-10T15:25:00Z"}
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent"}
	inv.Set(tuple, mirrorEv)

	// Sanity: this is the propagate cell (MirrorDrifted && !SourceChanged)
	// with start/end in the drifted set - the exact input that produced B38.
	desired := mirror.BuildPayload("src-cal", source)
	signal := mirror.ComputeDriftSignal(source, mirrorEv, desired)
	if !signal.MirrorDrifted || signal.SourceChanged {
		t.Fatalf("test setup wrong: want MirrorDrifted && !SourceChanged; got %+v", signal)
	}
	if got := mirror.DriftedFieldNames(mirrorEv, desired); !contains(got, "start") {
		t.Fatalf("test setup wrong: want start in drifted fields; got %v", got)
	}

	// Queue enough mirror-side patches for the safe repair path (main +
	// checksum). Extra queued responses are harmless; the assertion is on
	// which calendar gets written, not the count.
	post := *mirrorEv
	post.Start = start0725
	post.End = end0810
	api.queuePatch(&post)
	api.queuePatch(&post)
	api.queuePatch(&post)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}

	// THE critical assertion: the source parent must not be patched at all.
	// A start/end patch on the recurring parent endpoint moves the series.
	patches := api.callsByOp("EventsPatch")
	for _, call := range patches {
		if call.CalendarID == "src-cal" {
			t.Fatalf("recurring parent must never be reverse-patched on the source; got patch %+v", call.PatchBody)
		}
	}
	// Exactly the revert shape: mirror main + checksum, both on the mirror.
	if len(patches) != 2 {
		t.Fatalf("expected exactly 2 mirror-side patches (revert main + checksum); got %d", len(patches))
	}
	for i, p := range patches {
		if p.CalendarID != "tgt-cal" || p.EventID != "mp-1" {
			t.Errorf("patches[%d] should target the mirror (tgt-cal/mp-1); got %s/%s", i, p.CalendarID, p.EventID)
		}
	}

	// Reported as a revert of the target edit, not a propagate.
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionRevert || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("anchor-only drift should revert/target_edited; got %s/%s", got.Action, got.Reason)
	}
	if !contains(got.Fields, "start") || !contains(got.Fields, "end") {
		t.Errorf("revert Fields should list the reverted anchor fields; got %v", got.Fields)
	}
}

// TestDoPropagate_RecurringParent_PropagatesSafeFieldsButNotAnchor pins the
// B38 mixed case: a recurring-parent mirror drifted on BOTH a safe field
// (summary) and an anchor field (start). The safe field still propagates to
// the source parent; the anchor field is stripped from the source patch, and
// the mirror rewrite from the post-patch source (which keeps the original
// start) snaps the anchor back. The source patch must NOT carry start.
func TestDoPropagate_RecurringParent_PropagatesSafeFieldsButNotAnchor(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	start0725 := &gws.EventDateTime{DateTime: "2026-08-10T14:25:00Z"}
	end0810 := &gws.EventDateTime{DateTime: "2026-08-10T15:10:00Z"}
	source := &gws.Event{
		ID:           "src-parent",
		Status:       gws.EventStatusConfirmed,
		Summary:      "Breakfast",
		Description:  "Breakfast",
		Start:        start0725,
		End:          end0810,
		Recurrence:   []string{"RRULE:FREQ=WEEKLY;BYDAY=MO,TU,WE"},
		Updated:      "2026-08-01T00:00:00Z",
		Transparency: gws.TransparencyOpaque,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
	}

	mirrorEv := makeCleanCurrentMirror("mp-1", "src-cal:src-parent",
		source.Updated, "2026-08-02T00:00:00Z",
		"Breakfast", start0725, end0810)
	mirrorEv.Recurrence = source.Recurrence
	mirrorEv.ExtendedProperties.Private[mirror.ExtKeyChecksum] =
		mirror.Checksum(mirror.ManagedFieldsFromEvent(mirrorEv))
	// Drift both a safe field (summary) and an anchor field (start).
	mirrorEv.Summary = "Breakfast (moved)"
	mirrorEv.Description = "Breakfast (moved)\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC"
	mirrorEv.Start = &gws.EventDateTime{DateTime: "2026-08-10T14:40:00Z"}
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent"}, mirrorEv)

	// Source patch (safe fields only) -> mirror main -> mirror checksum.
	postSrc := *source
	postSrc.Summary = "Breakfast (moved)"
	postSrc.Updated = "2026-08-03T00:00:00Z"
	api.queuePatch(&postSrc)
	post := *mirrorEv
	post.Start = start0725
	api.queuePatch(&post)
	api.queuePatch(&post)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}

	srcPatches := 0
	for _, call := range api.callsByOp("EventsPatch") {
		if call.CalendarID != "src-cal" {
			continue
		}
		srcPatches++
		if call.PatchBody.Start != nil || call.PatchBody.End != nil {
			t.Fatalf("source patch must not carry start/end for a recurring parent; got %+v", call.PatchBody)
		}
		if call.PatchBody.Summary == nil || *call.PatchBody.Summary != "Breakfast (moved)" {
			t.Errorf("source patch should carry the drifted summary; got %+v", call.PatchBody)
		}
	}
	if srcPatches != 1 {
		t.Fatalf("expected exactly one source patch (safe fields); got %d", srcPatches)
	}

	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPropagate || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("got %s/%s, want propagate/target_edited", got.Action, got.Reason)
	}
	if contains(got.Fields, "start") {
		t.Errorf("propagated Fields must not include start; got %v", got.Fields)
	}
	if !contains(got.Fields, "summary") {
		t.Errorf("propagated Fields should include summary; got %v", got.Fields)
	}
}

// TestDoPropagate_RecurringParent_RecurrenceClearRefused covers the B38 edge
// where the user clears the RRULE on the mirror parent (recurrence becomes
// the sole drift). Clearing recurrence on the source parent would collapse
// the whole series to a single event, so it must be refused too: the source
// stays authoritative and the mirror's recurrence is restored. No source write.
func TestDoPropagate_RecurringParent_RecurrenceClearRefused(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	start := &gws.EventDateTime{DateTime: "2026-08-10T14:25:00Z"}
	end := &gws.EventDateTime{DateTime: "2026-08-10T15:10:00Z"}
	source := &gws.Event{
		ID:           "src-parent",
		Status:       gws.EventStatusConfirmed,
		Summary:      "Breakfast",
		Description:  "Breakfast",
		Start:        start,
		End:          end,
		Recurrence:   []string{"RRULE:FREQ=WEEKLY;BYDAY=MO,TU,WE"},
		Updated:      "2026-08-01T00:00:00Z",
		Transparency: gws.TransparencyOpaque,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
	}

	mirrorEv := makeCleanCurrentMirror("mp-1", "src-cal:src-parent",
		source.Updated, "2026-08-02T00:00:00Z",
		"Breakfast", start, end)
	mirrorEv.Recurrence = source.Recurrence
	mirrorEv.ExtendedProperties.Private[mirror.ExtKeyChecksum] =
		mirror.Checksum(mirror.ManagedFieldsFromEvent(mirrorEv))
	// User cleared the RRULE on the mirror parent.
	mirrorEv.Recurrence = nil
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent"}, mirrorEv)

	desired := mirror.BuildPayload("src-cal", source)
	if got := mirror.DriftedFieldNames(mirrorEv, desired); !contains(got, "recurrence") || contains(got, "start") {
		t.Fatalf("test setup wrong: want only recurrence drifted; got %v", got)
	}

	post := *mirrorEv
	post.Recurrence = source.Recurrence
	api.queuePatch(&post)
	api.queuePatch(&post)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}

	for _, call := range api.callsByOp("EventsPatch") {
		if call.CalendarID == "src-cal" {
			t.Fatalf("recurrence clear must not be reverse-patched to source; got %+v", call.PatchBody)
		}
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionRevert || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("recurrence-clear drift should revert/target_edited; got %s/%s", got.Action, got.Reason)
	}
	if !contains(got.Fields, "recurrence") {
		t.Errorf("revert Fields should list recurrence; got %v", got.Fields)
	}
}

// TestDoPropagate_RecurringParent_SourceClearedMirrorStillRecurring covers the
// Codex-flagged transition: the source parent dropped its RRULE (now a one-off)
// while the mirror still carries the old recurrence. The guard must still fire
// off the MIRROR's recurrence and refuse to re-propagate the stale RRULE, which
// would otherwise resurrect the series on the source.
func TestDoPropagate_RecurringParent_SourceClearedMirrorStillRecurring(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	start := &gws.EventDateTime{DateTime: "2026-08-10T14:25:00Z"}
	end := &gws.EventDateTime{DateTime: "2026-08-10T15:10:00Z"}
	// Source is now a plain one-off (no Recurrence).
	source := &gws.Event{
		ID:           "src-parent",
		Status:       gws.EventStatusConfirmed,
		Summary:      "Breakfast",
		Description:  "Breakfast",
		Start:        start,
		End:          end,
		Updated:      "2026-08-01T00:00:00Z",
		Transparency: gws.TransparencyOpaque,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
	}

	// Mirror still carries the old RRULE - the sole drift is recurrence.
	mirrorEv := makeCleanCurrentMirror("mp-1", "src-cal:src-parent",
		source.Updated, "2026-08-02T00:00:00Z",
		"Breakfast", start, end)
	mirrorEv.ExtendedProperties.Private[mirror.ExtKeyChecksum] =
		mirror.Checksum(mirror.ManagedFieldsFromEvent(mirrorEv))
	mirrorEv.Recurrence = []string{"RRULE:FREQ=WEEKLY;BYDAY=MO,TU,WE"}
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent"}, mirrorEv)

	post := *mirrorEv
	post.Recurrence = nil
	api.queuePatch(&post)
	api.queuePatch(&post)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}

	for _, call := range api.callsByOp("EventsPatch") {
		if call.CalendarID == "src-cal" {
			t.Fatalf("stale mirror RRULE must not be re-propagated to the source one-off; got %+v", call.PatchBody)
		}
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionRevert || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("source-cleared transition should revert/target_edited; got %s/%s", got.Action, got.Reason)
	}
}

// TestDoPropagate_RecurringParent_AnchorOnlyConflictPreservesMetadata pins
// Codex finding #3: the anchor-only refusal delegates to doRevert so the
// conflict label and its timestamps from the four-way matrix survive (both
// sides changed, mirror newer -> conflict_target_won). Reporting a bare revert
// with no conflict would hide that both sides had diverged.
func TestDoPropagate_RecurringParent_AnchorOnlyConflictPreservesMetadata(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	start0725 := &gws.EventDateTime{DateTime: "2026-08-10T14:25:00Z"}
	end0810 := &gws.EventDateTime{DateTime: "2026-08-10T15:10:00Z"}
	// Source CHANGED (its stored source_updated on the mirror is older).
	source := &gws.Event{
		ID:           "src-parent",
		Status:       gws.EventStatusConfirmed,
		Summary:      "Breakfast",
		Description:  "Breakfast",
		Start:        start0725,
		End:          end0810,
		Recurrence:   []string{"RRULE:FREQ=WEEKLY"},
		Updated:      "2026-08-05T00:00:00Z", // newer than stored source_updated below
		Transparency: gws.TransparencyOpaque,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
	}

	// Mirror: stored source_updated older than source.Updated (SourceChanged),
	// live start drifted (MirrorDrifted), and mirror's own Updated NEWER than
	// source's -> newer-wins picks the mirror -> conflict_target_won ->
	// ActionPropagate -> doPropagate. Anchor-only drift then reverts.
	mirrorEv := makeCleanCurrentMirror("mp-1", "src-cal:src-parent",
		"2026-08-01T00:00:00Z", "2026-08-06T00:00:00Z",
		"Breakfast", start0725, end0810)
	mirrorEv.Recurrence = source.Recurrence
	mirrorEv.ExtendedProperties.Private[mirror.ExtKeyChecksum] =
		mirror.Checksum(mirror.ManagedFieldsFromEvent(mirrorEv))
	mirrorEv.Start = &gws.EventDateTime{DateTime: "2026-08-10T14:40:00Z"}
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent"}, mirrorEv)

	signal := mirror.ComputeDriftSignal(source, mirrorEv, mirror.BuildPayload("src-cal", source))
	if !signal.MirrorDrifted || !signal.SourceChanged {
		t.Fatalf("test setup wrong: want both MirrorDrifted && SourceChanged; got %+v", signal)
	}

	post := *mirrorEv
	post.Start = start0725
	api.queuePatch(&post)
	api.queuePatch(&post)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}

	for _, call := range api.callsByOp("EventsPatch") {
		if call.CalendarID == "src-cal" {
			t.Fatalf("conflict-target-won anchor drift must not patch the source parent; got %+v", call.PatchBody)
		}
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionRevert || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("want revert/target_edited; got %s/%s", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictTargetWon {
		t.Errorf("conflict label must survive the anchor-only revert; got %q", got.Conflict)
	}
	if got.SourceUpdated != source.Updated || got.MirrorUpdated != mirrorEv.Updated {
		t.Errorf("conflict timestamps must survive; got src=%q mir=%q", got.SourceUpdated, got.MirrorUpdated)
	}
}

// TestDoPropagate_RecurringParent_AllDayAnchorRefused confirms the anchor
// guard is agnostic to all-day (Date) vs timed (DateTime) parents: an all-day
// recurring parent whose mirror start Date drifted must still never move the
// source series.
func TestDoPropagate_RecurringParent_AllDayAnchorRefused(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	start := &gws.EventDateTime{Date: "2026-08-10"}
	end := &gws.EventDateTime{Date: "2026-08-11"}
	source := &gws.Event{
		ID:           "src-parent",
		Status:       gws.EventStatusConfirmed,
		Summary:      "All-day series",
		Description:  "All-day series",
		Start:        start,
		End:          end,
		Recurrence:   []string{"RRULE:FREQ=WEEKLY"},
		Updated:      "2026-08-01T00:00:00Z",
		Transparency: gws.TransparencyOpaque,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
	}

	mirrorEv := makeCleanCurrentMirror("mp-1", "src-cal:src-parent",
		source.Updated, "2026-08-02T00:00:00Z",
		"All-day series", start, end)
	mirrorEv.Recurrence = source.Recurrence
	mirrorEv.ExtendedProperties.Private[mirror.ExtKeyChecksum] =
		mirror.Checksum(mirror.ManagedFieldsFromEvent(mirrorEv))
	// User dragged the all-day series one day later on the mirror side.
	mirrorEv.Start = &gws.EventDateTime{Date: "2026-08-11"}
	mirrorEv.End = &gws.EventDateTime{Date: "2026-08-12"}
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent"}, mirrorEv)

	post := *mirrorEv
	post.Start = start
	post.End = end
	api.queuePatch(&post)
	api.queuePatch(&post)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}

	for _, call := range api.callsByOp("EventsPatch") {
		if call.CalendarID == "src-cal" {
			t.Fatalf("all-day recurring parent must never be reverse-patched on source; got %+v", call.PatchBody)
		}
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionRevert || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("all-day anchor drift should revert/target_edited; got %s/%s", got.Action, got.Reason)
	}
}

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
