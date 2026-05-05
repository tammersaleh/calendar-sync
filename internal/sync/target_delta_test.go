package sync

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// makeWritablePDir is a target-delta-friendly variant of makeTestPDir: the
// effective two-way-sync gate is open (SourceWritable && PropagateTargetEdits).
// uniqueWritableTargets only counts targets behind such pdirs.
func makeWritablePDir(pair, source, target string) config.PDir {
	pd := makeTestPDir(pair, source, target, true)
	pd.PropagateTargetEdits = true
	return pd
}

// makeMirrorWithUserEdit returns a mirror Event whose live summary differs
// from the stored checksum's view (mirror_drifted=true), simulating a user
// edit on the target side. The mirror's source-tuple stays the same so the
// owning-pdir lookup matches.
func makeMirrorWithUserEdit(id, sourceTuple, sourceUpdated, mirrorUpdated, originalSummary, editedSummary string) *gws.Event {
	start := &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"}
	end := &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"}
	// Build the clean baseline first to capture a valid checksum, then
	// mutate Summary so it disagrees with the recorded checksum.
	m := makeCleanCurrentMirror(id, sourceTuple, sourceUpdated, mirrorUpdated, originalSummary, start, end)
	m.Summary = editedSummary
	return m
}

// queueWritableTargetSeed enqueues the response for the
// seedTargetSyncTokens call against target T. Pairs with newStubAPI's
// listByLabel routing (label "list:T:full" matches any non-syncToken,
// non-PrivateExtendedProperty list against T).
func queueWritableTargetSeed(api *stubAPI, target, token string) {
	api.queueListFull(target, nil, token)
}

// queueTargetIncrDelta enqueues a target-delta response against target T.
// The labelling matches "list:T:incr" because the EventsListParams will
// carry a non-empty SyncToken.
func queueTargetIncrDelta(api *stubAPI, target string, events []gws.Event, nextToken string) {
	api.queueListIncr(target, events, nextToken)
}

// queueTargetIncrErr enqueues an error for the next target-delta list call.
func queueTargetIncrErr(api *stubAPI, target string, err error) {
	api.queueListIncrErr(target, err)
}

// ---------- 1. Non-recurring target edit propagates to source ----------

func TestTargetDeltaPhase_NonRecurringEdit(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	// Build a mirror representing the user's post-edit live state: summary
	// changed from "Standup" to "Standup-edited" but stored checksum still
	// reflects "Standup" (the pre-edit value).
	m := makeMirrorWithUserEdit("mirror-1", "src-A:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z",
		"Standup", "Standup-edited")

	// Pre-seed reconciler: target token + inventory.
	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	inv := NewInventory("tgt-A")
	tup := mirror.SourceTuple{CalendarID: "src-A", EventID: "src-evt"}
	// Initial inventory holds a clean copy (no edit yet); the target-delta
	// dispatch refreshes the inventory entry with the post-edit live mirror.
	inv.Set(tup, makeCleanCurrentMirror("mirror-1", "src-A:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T10:00:00Z",
		"Standup",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		&gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"}))
	r.inventories["tgt-A"] = inv

	// Target-delta returns the post-edit mirror. Empty source-delta.
	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*m}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new") // empty source-delta

	// Source events.get for the dispatch + propagate path:
	//   1. events.get on source to feed the Classifier.
	//   2. events.patch on source (propagate the edit).
	//   3. events.patch on target (rewrite mirror with fresh checksum).
	//   4. events.patch on target (checksum follow-up).
	srcEvent := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueGet("src-A", "src-evt", srcEvent)
	patchedSrc := *srcEvent
	patchedSrc.Summary = "Standup-edited"
	patchedSrc.Updated = "2026-04-30T12:00:00Z"
	api.queuePatch(&patchedSrc) // propagate to source
	postMain := *m
	postMain.Updated = "2026-04-30T12:00:01Z"
	api.queuePatch(&postMain) // rewrite mirror
	api.queuePatch(&postMain) // checksum followup

	r.syncTokens["src-A"] = "tok-src-old"

	sink, captured := captureOutputs()
	r.Output = sink

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Expect a propagate outcome (target-delta -> drift -> propagate).
	var propagated *Outcome
	for i := range *captured {
		o := (*captured)[i]
		if o.Action == mirror.ActionPropagate {
			propagated = &o
			break
		}
	}
	if propagated == nil {
		t.Fatalf("expected one propagate outcome from target-delta; got %+v", *captured)
	}
	if propagated.SourceEventID != "src-evt" || propagated.TargetEventID != "mirror-1" {
		t.Errorf("propagate IDs = src=%q tgt=%q, want src-evt + mirror-1",
			propagated.SourceEventID, propagated.TargetEventID)
	}
	if !reflect.DeepEqual(propagated.Fields, []string{"summary"}) {
		t.Errorf("Fields = %v, want [summary]", propagated.Fields)
	}

	// Target token advanced to tok-tgt-new; PerTarget reflects that.
	if got := r.targetSyncTokens["tgt-A"]; got != "tok-tgt-new" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-tgt-new", got)
	}
	if !res.PerTarget["tgt-A"].SyncTokenChanged {
		t.Error("PerTarget[tgt-A].SyncTokenChanged should be true")
	}
}

// ---------- 2. Recurring parent edit propagates ----------

func TestTargetDeltaPhase_RecurringParentEdit(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	// Mirror parent (RecurringEventID="", Recurrence set) with a user edit
	// to summary.
	parent := makeMirrorWithUserEdit("mirror-parent", "src-A:src-parent",
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z",
		"Series", "Series-edited")
	parent.Recurrence = []string{"RRULE:FREQ=WEEKLY"}

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	inv := NewInventory("tgt-A")
	tup := mirror.SourceTuple{CalendarID: "src-A", EventID: "src-parent"}
	preEdit := makeCleanCurrentMirror("mirror-parent", "src-A:src-parent",
		"2026-04-29T20:00:00Z", "2026-04-30T10:00:00Z",
		"Series",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		&gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"})
	preEdit.Recurrence = []string{"RRULE:FREQ=WEEKLY"}
	inv.Set(tup, preEdit)
	r.inventories["tgt-A"] = inv
	r.syncTokens["src-A"] = "tok-src-old"

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*parent}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new") // empty source-delta

	// Source parent fetch + propagate path. The Classifier will run step 7
	// (horizon) for a recurring parent - that calls events.instances.
	srcParent := &gws.Event{
		ID:           "src-parent",
		Status:       gws.EventStatusConfirmed,
		Summary:      "Series",
		Description:  "Series",
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Recurrence:   []string{"RRULE:FREQ=WEEKLY"},
		Updated:      "2026-04-29T20:00:00Z",
		Transparency: gws.TransparencyOpaque,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
	}
	api.queueGet("src-A", "src-parent", srcParent)
	api.queueInstances([]gws.Event{{ID: "any-instance"}}) // horizon check
	patched := *srcParent
	patched.Summary = "Series-edited"
	patched.Updated = "2026-04-30T12:00:00Z"
	api.queuePatch(&patched)        // propagate to source
	post := *parent                  // rewrite mirror
	api.queuePatch(&post)
	api.queuePatch(&post) // checksum

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var propagated *Outcome
	for i := range *captured {
		o := (*captured)[i]
		if o.Action == mirror.ActionPropagate {
			propagated = &o
			break
		}
	}
	if propagated == nil {
		t.Fatalf("expected propagate from recurring-parent edit; got %+v", *captured)
	}
	if propagated.SourceEventID != "src-parent" {
		t.Errorf("SourceEventID = %q, want src-parent", propagated.SourceEventID)
	}
}

// ---------- 3. Managed recurring instance edit propagates ----------

func TestTargetDeltaPhase_ManagedInstanceEdit(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	// Managed-form mirror instance: calendar-sync:source carries the source
	// instance ID with the `_<UTC>` suffix already attached.
	instanceID := "mirror-parent_20260520T160000Z"
	srcInstanceID := "src-parent_20260520T160000Z"
	tuple := mirror.SourceTuple{CalendarID: "src-A", EventID: srcInstanceID}

	m := makeMirrorWithUserEdit(instanceID, tuple.String(),
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z",
		"Lunch", "Lunch-edited")
	m.RecurringEventID = "mirror-parent"
	m.OriginalStartTime = &gws.EventDateTime{DateTime: "2026-05-20T16:00:00Z"}

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	inv := NewInventory("tgt-A")
	preEdit := makeCleanCurrentMirror(instanceID, tuple.String(),
		"2026-04-29T20:00:00Z", "2026-04-30T10:00:00Z",
		"Lunch",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		&gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"})
	preEdit.RecurringEventID = "mirror-parent"
	inv.Set(tuple, preEdit)
	r.inventories["tgt-A"] = inv
	r.syncTokens["src-A"] = "tok-src-old"

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*m}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	// events.get on the SOURCE INSTANCE. Source instance has matching
	// summary "Lunch"; the recurring handler then drives the propagate
	// shape.
	srcInstance := &gws.Event{
		ID:                srcInstanceID,
		RecurringEventID:  "src-parent",
		Status:            gws.EventStatusConfirmed,
		Summary:           "Lunch",
		Description:       "Lunch",
		Start:             &gws.EventDateTime{DateTime: "2026-05-20T16:00:00Z"},
		End:               &gws.EventDateTime{DateTime: "2026-05-20T17:00:00Z"},
		OriginalStartTime: &gws.EventDateTime{DateTime: "2026-05-20T16:00:00Z"},
		Updated:           "2026-04-29T20:00:00Z",
		Transparency:      gws.TransparencyOpaque,
		HTMLLink:          "https://www.google.com/calendar/event?eid=ABC",
	}
	api.queueGet("src-A", srcInstanceID, srcInstance)

	// The recurring handler step 1 looks up the mirror parent in inventory
	// (we don't have one) -> ReconcileParent fallback. We don't need that
	// path for this test; the simpler approach is to put the mirror parent
	// in inventory so step 1 finds it directly.
	mirrorParent := &gws.Event{
		ID:         "mirror-parent",
		Status:     gws.EventStatusConfirmed,
		Summary:    "Series",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"},
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:  "src-A:src-parent",
				mirror.ExtKeyVersion: mirror.SchemaVersion,
			},
		},
	}
	parentTuple := mirror.SourceTuple{CalendarID: "src-A", EventID: "src-parent"}
	inv.Set(parentTuple, mirrorParent)

	// events.instances on mirror parent (locate the mirror instance via
	// originalStart) -> returns m.
	api.queueInstances([]gws.Event{*m})
	// Drift matrix path: recurring handler runs the propagate shape.
	patchedSrc := *srcInstance
	patchedSrc.Summary = "Lunch-edited"
	patchedSrc.Updated = "2026-04-30T12:00:00Z"
	api.queuePatch(&patchedSrc) // propagate to source instance
	post := *m
	api.queuePatch(&post) // rewrite mirror
	api.queuePatch(&post) // checksum

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var propagated *Outcome
	for i := range *captured {
		o := (*captured)[i]
		if o.Action == mirror.ActionPropagate {
			propagated = &o
			break
		}
	}
	if propagated == nil {
		t.Fatalf("expected propagate from managed-instance edit; got %+v", *captured)
	}
	// The events.get must have used the FULL source-instance ID (with suffix).
	gets := api.callsByOp("EventsGet")
	found := false
	for _, c := range gets {
		if c.CalendarID == "src-A" && c.EventID == srcInstanceID {
			found = true
		}
	}
	if !found {
		t.Errorf("expected events.get on src-A/%s; got calls=%v", srcInstanceID, gets)
	}
}

// ---------- 4. Inherited recurring instance edit -> Phase 1 skip ----------

func TestTargetDeltaPhase_InheritedInstanceEdit_Phase1Skip(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	// Inherited-form mirror instance: calendar-sync:source carries the source
	// PARENT id (no `_<UTC>` suffix). The mirror instance's own ID has the
	// suffix appended by Calendar API.
	instanceID := "mirror-parent_20260520T160000Z"
	parentTupleStr := "src-A:src-parent" // inherited form

	m := makeMirrorWithUserEdit(instanceID, parentTupleStr,
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z",
		"Lunch", "Lunch-edited")
	m.RecurringEventID = "mirror-parent"
	m.OriginalStartTime = &gws.EventDateTime{DateTime: "2026-05-20T16:00:00Z"}

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*m}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	// events.get on the constructed source-instance ID returns 404
	// (mirror-only override territory).
	expectedSrcInstID := "src-parent_20260520T160000Z"
	api.queueGetErr("src-A", expectedSrcInstID,
		&gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Expect exactly one skip outcome with reason=mirror_only_override.
	var skip *Outcome
	for i := range *captured {
		o := (*captured)[i]
		if o.Action == mirror.ActionSkip && o.Reason == reasonMirrorOnlyOverride {
			skip = &o
			break
		}
	}
	if skip == nil {
		t.Fatalf("expected skip(mirror_only_override); got %+v", *captured)
	}
	// Token still advances - skip is success-shaped per the dispatch path.
	if got := r.targetSyncTokens["tgt-A"]; got != "tok-tgt-new" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-tgt-new (skip != error)", got)
	}
}

// ---------- 4b. Non-recurring source orphan -> skip(source_orphan) ----------

func TestTargetDeltaPhase_NonRecurringSourceOrphanEmitsSkip(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	// Non-recurring mirror whose source has been deleted. The user edit on
	// the mirror is irrelevant - the source 404 short-circuits the dispatch
	// before drift detection runs.
	m := makeMirrorWithUserEdit("mirror-1", "src-A:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z",
		"Standup", "Standup-edited")

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*m}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	// Source returns 404 - it was deleted between FullSync and this delta.
	api.queueGetErr("src-A", "src-evt",
		&gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Expect exactly one skip outcome with reason=source_orphan.
	var skip *Outcome
	for i := range *captured {
		o := (*captured)[i]
		if o.Action == mirror.ActionSkip && o.Reason == reasonSourceOrphan {
			skip = &o
			break
		}
	}
	if skip == nil {
		t.Fatalf("expected skip(source_orphan); got %+v", *captured)
	}
	if skip.SourceEventID != "src-evt" || skip.TargetEventID != "mirror-1" {
		t.Errorf("skip IDs = src=%q tgt=%q, want src-evt + mirror-1",
			skip.SourceEventID, skip.TargetEventID)
	}
	// Token advances - skip is success-shaped per the dispatch path.
	if got := r.targetSyncTokens["tgt-A"]; got != "tok-tgt-new" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-tgt-new (skip != error)", got)
	}
}

// ---------- 5. Self-write suppression for normal mirror ----------

func TestTargetDeltaPhase_SelfWriteSuppression(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	// The mirror is "fresh" - matches a clean current-schema mirror with
	// stored checksum exactly reflecting live managed fields. This is the
	// post-write delta shape: a tick sees the freshly-rewritten mirror in
	// the delta, but mirror_drifted=false and source_changed=false ->
	// skip(unchanged).
	tup := mirror.SourceTuple{CalendarID: "src-A", EventID: "src-evt"}
	clean := makeCleanCurrentMirror("mirror-1", tup.String(),
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z",
		"Standup",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		&gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"})

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	inv := NewInventory("tgt-A")
	inv.Set(tup, clean)
	r.inventories["tgt-A"] = inv
	r.syncTokens["src-A"] = "tok-src-old"

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*clean}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	src := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	src.Summary = "Standup"
	api.queueGet("src-A", "src-evt", src)
	// NO patches queued - self-write should be skip(unchanged).

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	for _, o := range *captured {
		if o.Action == mirror.ActionPatch || o.Action == mirror.ActionPropagate {
			t.Errorf("self-write should not trigger writes; got %+v", o)
		}
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 0 {
		t.Errorf("self-write should issue zero patches; got %d", len(patches))
	}
}

// ---------- 6. Inherited auto-materialized instance with no user edit ----------
//
// An inherited-form recurring instance that the user hasn't edited shows up
// in the target delta when the parent is first written (Google materializes
// instances and copies extended properties verbatim). The dispatch should
// route through the recurring handler's bootstrap path (inherited_upgrade)
// which rewrites the instance with per-instance metadata - NOT propagate to
// the source.
func TestTargetDeltaPhase_InheritedAutoMaterializedNoUserEdit(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	// Inherited-form mirror instance: live managed fields match
	// desired-from-source-instance; only the metadata is "wrong" (parent's
	// source-tuple instead of per-instance). The recurring handler's
	// inherited-upgrade path rewrites the instance with per-instance
	// metadata + fresh checksum.
	instanceID := "mirror-parent_20260520T160000Z"
	parentTupleStr := "src-A:src-parent"
	start := &gws.EventDateTime{DateTime: "2026-05-20T16:00:00Z"}
	end := &gws.EventDateTime{DateTime: "2026-05-20T17:00:00Z"}
	clean := makeCleanCurrentMirror(instanceID, parentTupleStr,
		"2026-04-29T20:00:00Z", "2026-04-30T10:00:00Z",
		"Lunch", start, end)
	clean.RecurringEventID = "mirror-parent"
	clean.OriginalStartTime = start

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	inv := NewInventory("tgt-A")
	r.inventories["tgt-A"] = inv
	r.syncTokens["src-A"] = "tok-src-old"

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*clean}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	// Source instance that exists at the same originalStartTime.
	srcInstance := &gws.Event{
		ID:                "src-parent_20260520T160000Z",
		RecurringEventID:  "src-parent",
		Status:            gws.EventStatusConfirmed,
		Summary:           "Lunch",
		Description:       "Lunch",
		Start:             start,
		End:               end,
		OriginalStartTime: start,
		Updated:           "2026-04-29T20:00:00Z",
		Transparency:      gws.TransparencyOpaque,
		HTMLLink:          "https://www.google.com/calendar/event?eid=ABC",
	}
	api.queueGet("src-A", "src-parent_20260520T160000Z", srcInstance)

	// Mirror parent in inventory (so resolveMirrorParent step 1 hits).
	mirrorParent := &gws.Event{
		ID:         "mirror-parent",
		Status:     gws.EventStatusConfirmed,
		Summary:    "Series",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"},
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:  parentTupleStr,
				mirror.ExtKeyVersion: mirror.SchemaVersion,
			},
		},
	}
	inv.Set(mirror.SourceTuple{CalendarID: "src-A", EventID: "src-parent"}, mirrorParent)

	// Recurring handler step 2: events.instances locates the mirror
	// instance.
	api.queueInstances([]gws.Event{*clean})
	// Inherited-upgrade rewrite + checksum follow-up.
	post := *clean
	api.queuePatch(&post)
	api.queuePatch(&post)

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Expect exactly one outcome and it should be the inherited_upgrade
	// rewrite, NOT a propagate.
	var found *Outcome
	for i := range *captured {
		o := (*captured)[i]
		if o.Reason == "inherited_upgrade" {
			found = &o
		}
		if o.Action == mirror.ActionPropagate {
			t.Errorf("inherited bootstrap must not propagate; got %+v", o)
		}
	}
	if found == nil {
		t.Errorf("expected inherited_upgrade outcome; got %+v", *captured)
	}

	// Crucially: NO source-side patch (target-edit propagation only fires
	// on real user edits, not on the bootstrap rewrite).
	for _, c := range api.callsByOp("EventsPatch") {
		if c.CalendarID == "src-A" {
			t.Errorf("inherited bootstrap must not patch source; got %+v", c)
		}
	}
}

// ---------- 7. Non-mirror events in target-delta are skipped silently ----------

func TestTargetDeltaPhase_NotMirror(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"

	// Target delta returns a user-created event with NO calendar-sync:source.
	userEvent := gws.Event{
		ID:      "user-evt",
		Status:  gws.EventStatusConfirmed,
		Summary: "Personal note",
	}
	queueTargetIncrDelta(api, "tgt-A", []gws.Event{userEvent}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(*captured) != 0 {
		t.Errorf("non-mirror should emit no outcome; got %+v", *captured)
	}
	// No EventsGet - we never tried to fetch a source.
	if calls := api.callsByOp("EventsGet"); len(calls) != 0 {
		t.Errorf("non-mirror should issue zero events.get; got %d", len(calls))
	}
}

// ---------- 8. No owning pdir -> skip silently ----------

func TestTargetDeltaPhase_NoOwningPdir(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"

	// Mirror with calendar-sync:source pointing at a SOURCE that no enabled
	// pdir mirrors here. (Stray mirror; previous pdir disabled.)
	stray := makeMirrorWithUserEdit("mirror-stray", "src-OTHER:other-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z",
		"X", "X-edited")

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*stray}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(*captured) != 0 {
		t.Errorf("stray mirror should emit no outcome; got %+v", *captured)
	}
	if calls := api.callsByOp("EventsGet"); len(calls) != 0 {
		t.Errorf("stray mirror should issue zero events.get; got %d", len(calls))
	}
}

// ---------- 9. Non-writable target -> never lists ----------

func TestTargetDeltaPhase_NonWritableTarget(t *testing.T) {
	api := newStubAPI()
	// Pdir has SourceWritable=false: the gate is closed, so target-delta
	// must NOT list this target.
	pd := makeTestPDir("p1", "src-A", "tgt-A", false)
	pd.PropagateTargetEdits = true
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-src-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	api.queueListIncr("src-A", nil, "tok-src-new") // only the source-delta fires

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Verify NO EventsList was issued against tgt-A.
	for _, c := range api.callsByOp("EventsList") {
		if c.CalendarID == "tgt-A" {
			t.Errorf("non-writable target must not be listed; got %+v", c)
		}
	}
}

// ---------- 10. 410 GONE on target-delta clears token + NeedsFullResync ----------

func TestTargetDeltaPhase_410Recovery(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-stale"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"

	queueTargetIncrErr(api, "tgt-A", &gws.Error{Code: gws.CodeAPIGone, ExitCode: 1})
	api.queueListIncr("src-A", nil, "tok-src-new")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if _, ok := r.targetSyncTokens["tgt-A"]; ok {
		t.Errorf("targetSyncTokens[tgt-A] should be cleared after 410")
	}
	if !res.PerTarget["tgt-A"].NeedsFullResync {
		t.Errorf("PerTarget[tgt-A].NeedsFullResync should be true after 410")
	}
}

// ---------- 11. Token advances on success ----------

func TestTargetDeltaPhase_TokenAdvancementOnSuccess(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"

	// Empty delta: no events but a fresh nextSyncToken.
	queueTargetIncrDelta(api, "tgt-A", nil, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := r.targetSyncTokens["tgt-A"]; got != "tok-tgt-new" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-tgt-new", got)
	}
	if !res.PerTarget["tgt-A"].SyncTokenChanged {
		t.Error("PerTarget[tgt-A].SyncTokenChanged should be true")
	}
}

// ---------- 12. Token does NOT advance when classify error fires ----------

func TestTargetDeltaPhase_TokenStaysOnError(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"

	m := makeMirrorWithUserEdit("mirror-1", "src-A:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z",
		"X", "X-edit")

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*m}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	// events.get returns a backend error - this is NOT 404 so the dispatch
	// path returns it as a fatal error.
	api.queueGetErr("src-A", "src-evt",
		&gws.Error{Code: gws.CodeBackendError, ExitCode: 1})

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := r.targetSyncTokens["tgt-A"]; got != "tok-tgt-old" {
		t.Errorf("token must NOT advance on error; got %q want tok-tgt-old", got)
	}
}

// ---------- 13. Seed runs BEFORE inventory rebuild ----------

func TestSeedTargetSyncTokens_RunsBeforeInventoryRebuild(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	api.queueListFull("src-A", nil, "tok-src-1")
	queueWritableTargetSeed(api, "tgt-A", "tok-tgt-1")
	queueEmptyInventory(api, "tgt-A")

	r := newTestReconciler(api, canonical)
	if _, err := r.FullSync(context.Background()); err != nil {
		t.Fatalf("FullSync: %v", err)
	}

	// Walk the recorded calls. The order must be:
	//   1. Source-list against src-A (no syncToken, no PrivateExtendedProperty).
	//   2. Target seed: events.list against tgt-A (no syncToken,
	//      no PrivateExtendedProperty).
	//   3. Inventory rebuild: events.list against tgt-A with
	//      PrivateExtendedProperty (one per schema version).
	//
	// Pin: index of target seed must be < index of any inventory rebuild call.
	var seedIdx, firstInvIdx int = -1, -1
	for i, c := range api.calls {
		if c.Op != "EventsList" {
			continue
		}
		p := c.ListParams
		if p.CalendarID != "tgt-A" {
			continue
		}
		if p.SyncToken == "" && len(p.PrivateExtendedProperty) == 0 {
			if seedIdx == -1 {
				seedIdx = i
			}
		}
		if len(p.PrivateExtendedProperty) > 0 {
			if firstInvIdx == -1 {
				firstInvIdx = i
			}
		}
	}
	if seedIdx == -1 {
		t.Fatal("seed call not recorded")
	}
	if firstInvIdx == -1 {
		t.Fatal("inventory rebuild call not recorded")
	}
	if seedIdx >= firstInvIdx {
		t.Errorf("seed must run before inventory rebuild; seedIdx=%d invIdx=%d",
			seedIdx, firstInvIdx)
	}
	if got := r.targetSyncTokens["tgt-A"]; got != "tok-tgt-1" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-tgt-1", got)
	}
}

// ---------- 14. Tick: target-delta runs BEFORE source-delta ----------

func TestTickPhaseOrdering_TargetBeforeSource(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"

	queueTargetIncrDelta(api, "tgt-A", nil, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// First incremental events.list (SyncToken set) against tgt-A must
	// appear before the first incremental events.list against src-A.
	var tgtIdx, srcIdx int = -1, -1
	for i, c := range api.calls {
		if c.Op != "EventsList" {
			continue
		}
		if c.ListParams.SyncToken == "" {
			continue
		}
		if c.CalendarID == "tgt-A" && tgtIdx == -1 {
			tgtIdx = i
		}
		if c.CalendarID == "src-A" && srcIdx == -1 {
			srcIdx = i
		}
	}
	if tgtIdx == -1 || srcIdx == -1 {
		t.Fatalf("expected both target+source incremental list calls; tgtIdx=%d srcIdx=%d",
			tgtIdx, srcIdx)
	}
	if tgtIdx >= srcIdx {
		t.Errorf("target-delta must run BEFORE source-delta; tgtIdx=%d srcIdx=%d",
			tgtIdx, srcIdx)
	}
}

// ---------- Defensive helpers around findOwningPDir + extractInstanceSuffix ----------

func TestFindOwningPDir_GateClosed_NoMatch(t *testing.T) {
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	pd.PropagateTargetEdits = false // gate closed
	canonical := makeCanonical(pd)
	r := New(nil, canonical)

	if _, ok := r.findOwningPDir("tgt-A", "src-A"); ok {
		t.Error("gate-closed pdir must not match findOwningPDir")
	}
}

func TestFindOwningPDir_MultiplePDirsSharingTarget_RouteByCalSrc(t *testing.T) {
	pd1 := makeWritablePDir("p1", "src-A", "tgt-shared")
	pd2 := makeWritablePDir("p2", "src-B", "tgt-shared")
	canonical := makeCanonical(pd1, pd2)
	r := New(nil, canonical)

	got, ok := r.findOwningPDir("tgt-shared", "src-A")
	if !ok || got.PairName != "p1" {
		t.Errorf("source=src-A must route to p1; got %+v ok=%v", got, ok)
	}
	got2, ok := r.findOwningPDir("tgt-shared", "src-B")
	if !ok || got2.PairName != "p2" {
		t.Errorf("source=src-B must route to p2; got %+v ok=%v", got2, ok)
	}
}

func TestExtractInstanceSuffix(t *testing.T) {
	tests := []struct {
		id      string
		want    string
		wantOk  bool
	}{
		{"mirror-parent_20260520T160000Z", "_20260520T160000Z", true},
		{"cs2abc123_20260601T000000Z", "_20260601T000000Z", true},
		{"no-suffix", "", false},
		{"", "", false},
	}
	for _, tt := range tests {
		got, ok := extractInstanceSuffix(tt.id)
		if ok != tt.wantOk || got != tt.want {
			t.Errorf("extractInstanceSuffix(%q) = (%q, %v), want (%q, %v)",
				tt.id, got, ok, tt.want, tt.wantOk)
		}
	}
}

// ---------- 15. Regression: target edit propagates within ONE tick ----------
//
// This is the user-observable scenario the whole feature exists for: the
// daemon completed a FullSync; the user edits a mirror on the target; the
// next Tick should detect the edit and propagate within that single tick
// (not 24h later).
func TestTick_PropagatesTargetEdit_OneTick(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	// Phase 1: FullSync seeds tokens + inventory.
	src := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	tup := mirror.SourceTuple{CalendarID: "src-A", EventID: "src-evt"}
	mirrorEv := makeCleanCurrentMirror("mirror-1", tup.String(),
		src.Updated, "2026-04-30T10:00:00Z",
		src.Summary, src.Start, src.End)

	api.queueListFull("src-A", []gws.Event{*src}, "tok-src-1")
	queueWritableTargetSeed(api, "tgt-A", "tok-tgt-1")
	api.queueListInventory("tgt-A", mirror.SchemaVersion, []gws.Event{*mirrorEv})
	api.queueListInventory("tgt-A", "2", nil)
	api.queueListInventory("tgt-A", "1", nil)

	r := newTestReconciler(api, canonical)
	if _, err := r.FullSync(context.Background()); err != nil {
		t.Fatalf("FullSync: %v", err)
	}
	if got := r.targetSyncTokens["tgt-A"]; got != "tok-tgt-1" {
		t.Fatalf("FullSync did not seed targetSyncTokens; got %q", got)
	}

	// Phase 2: user edits the mirror on the target.
	edited := *mirrorEv
	edited.Summary = "Standup-edited"

	// Phase 3: tick should see the edit and propagate.
	queueTargetIncrDelta(api, "tgt-A", []gws.Event{edited}, "tok-tgt-2")
	api.queueListIncr("src-A", nil, "tok-src-2")

	api.queueGet("src-A", "src-evt", src) // dispatch reads source
	patchedSrc := *src
	patchedSrc.Summary = "Standup-edited"
	patchedSrc.Updated = "2026-04-30T12:00:00Z"
	api.queuePatch(&patchedSrc) // propagate to source
	post := edited
	api.queuePatch(&post) // rewrite mirror
	api.queuePatch(&post) // checksum

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Within a single tick we expect a propagate outcome.
	var propagated *Outcome
	for i := range *captured {
		o := (*captured)[i]
		if o.Action == mirror.ActionPropagate {
			propagated = &o
			break
		}
	}
	if propagated == nil {
		t.Fatalf("user edit must propagate within one tick; got %+v", *captured)
	}
}

// TestSeedTargetSyncTokens_FailedSeedSurfaces ensures the seed loop
// records errors but doesn't abort the rest of the FullSync. The next tick
// will skip the un-seeded target silently; the next FullSync re-attempts.
func TestSeedTargetSyncTokens_FailedSeedSurfaces(t *testing.T) {
	api := newStubAPI()
	pdA := makeWritablePDir("pA", "src-A", "tgt-A")
	pdB := makeWritablePDir("pB", "src-B", "tgt-B")
	canonical := makeCanonical(pdA, pdB)

	api.queueListFull("src-A", nil, "tok-src-A")
	api.queueListFull("src-B", nil, "tok-src-B")
	queueWritableTargetSeed(api, "tgt-A", "tok-tgt-A")
	api.queueListFullErr("tgt-B", errors.New("seed boom"))
	queueEmptyInventory(api, "tgt-A")
	queueEmptyInventory(api, "tgt-B")

	r := newTestReconciler(api, canonical)
	if _, err := r.FullSync(context.Background()); err != nil {
		t.Fatalf("FullSync: %v", err)
	}

	if got := r.targetSyncTokens["tgt-A"]; got != "tok-tgt-A" {
		t.Errorf("tgt-A seed should succeed; got %q", got)
	}
	if _, ok := r.targetSyncTokens["tgt-B"]; ok {
		t.Errorf("tgt-B seed must NOT be present after error")
	}
}
