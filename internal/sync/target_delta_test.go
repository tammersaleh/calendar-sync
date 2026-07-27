package sync

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
	"github.com/tammersaleh/calendar-sync/internal/recurring"
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

	// Locate the mirror instance by its constructed ID (mirror parent +
	// occurrence key) -> returns m.
	api.queueGet("tgt-A", instanceID, m)
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

// ---------- 4. Inherited recurring instance edit -> B29 membership table ----------

// b29Fixture is the production shape behind B29, assembled once so the
// several tests that exercise the membership decision table differ only in
// the catalog state and the queued responses.
//
// The series is a weekly parent at 08:45 UTC+? (15:45Z here). The user
// dragged ONE occurrence on the MIRROR to 16:00Z. Google copies the parent's
// extendedProperties onto a user-created override, so the mirror's
// calendar-sync:source still names the source PARENT and
// IsInheritedRecurringInstance reports true.
type b29Fixture struct {
	sourceParentID   string
	mirrorParentID   string
	sourceInstanceID string
	mirrorInstanceID string
	nativeStart      *gws.EventDateTime
	nativeEnd        *gws.EventDateTime
	movedStart       *gws.EventDateTime
	movedEnd         *gws.EventDateTime
	mirrorParent     *gws.Event
	mirrorInstance   *gws.Event
}

func newB29Fixture() b29Fixture {
	const (
		key            = "20260520T154500Z"
		sourceParentID = "src-parent"
		mirrorParentID = "mirror-parent"
	)
	f := b29Fixture{
		sourceParentID:   sourceParentID,
		mirrorParentID:   mirrorParentID,
		sourceInstanceID: sourceParentID + "_" + key,
		mirrorInstanceID: mirrorParentID + "_" + key,
		nativeStart:      &gws.EventDateTime{DateTime: "2026-05-20T15:45:00Z"},
		nativeEnd:        &gws.EventDateTime{DateTime: "2026-05-20T16:30:00Z"},
		movedStart:       &gws.EventDateTime{DateTime: "2026-05-20T16:00:00Z"},
		movedEnd:         &gws.EventDateTime{DateTime: "2026-05-20T16:45:00Z"},
	}
	f.mirrorParent = &gws.Event{
		ID:         mirrorParentID,
		Status:     gws.EventStatusConfirmed,
		Summary:    "Exercise",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"},
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:  "src-A:" + sourceParentID,
				mirror.ExtKeyVersion: mirror.SchemaVersion,
			},
		},
	}
	// Built clean at the native slot, then moved: the stored checksum still
	// describes 15:45 while the live event sits at 16:00, exactly as a
	// user-dragged occurrence looks on the wire.
	f.mirrorInstance = makeCleanCurrentMirror(f.mirrorInstanceID, "src-A:"+sourceParentID,
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z", "Exercise",
		f.nativeStart, f.nativeEnd)
	f.mirrorInstance.RecurringEventID = mirrorParentID
	f.mirrorInstance.OriginalStartTime = f.nativeStart
	f.mirrorInstance.Start = f.movedStart
	f.mirrorInstance.End = f.movedEnd
	return f
}

// virtualSourceInstance is what events.get returns for the constructed
// source-instance ID when the series has NO exception there: HTTP 200 and a
// synthesized occurrence sitting on the RRULE's native slot. This 200 (not
// 404) is the defect at the heart of B29.
func (f b29Fixture) virtualSourceInstance() *gws.Event {
	return &gws.Event{
		ID:                f.sourceInstanceID,
		RecurringEventID:  f.sourceParentID,
		Status:            gws.EventStatusConfirmed,
		Summary:           "Exercise",
		Description:       "Exercise",
		Start:             f.nativeStart,
		End:               f.nativeEnd,
		OriginalStartTime: f.nativeStart,
		Updated:           "2026-04-29T20:00:00Z",
		Transparency:      gws.TransparencyOpaque,
		HTMLLink:          "https://www.google.com/calendar/event?eid=ABC",
	}
}

// newB29Reconciler wires a reconciler with the fixture's mirror parent in
// inventory and both syncTokens seeded, ready for a single Tick.
func newB29Reconciler(api *stubAPI, f b29Fixture) *Reconciler {
	r := newTestReconciler(api, makeCanonical(makeWritablePDir("p1", "src-A", "tgt-A")))
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.syncTokens["src-A"] = "tok-src-old"
	inv := NewInventory("tgt-A")
	inv.Set(mirror.SourceTuple{CalendarID: "src-A", EventID: f.sourceParentID}, f.mirrorParent)
	r.inventories["tgt-A"] = inv
	return r
}

// seedReadySourceCatalog installs a Ready, unbounded-coverage exception
// catalog. Tests that drive Tick directly (rather than through a FullSync)
// need it: the batch preflight defers any batch carrying an inherited-form
// recurring instance whose source calendar has no proven collection.
func seedReadySourceCatalog(r *Reconciler, source string, exceptions ...gws.Event) {
	r.rebuildSourceCatalog(source, exceptions, time.Time{}, time.Time{})
}

// srcPatchBody returns the body of the first EventsPatch issued against the
// source calendar, or nil when there was none.
func srcPatchBody(api *stubAPI, sourceCal string) *gws.PatchEvent {
	for _, c := range api.callsByOp("EventsPatch") {
		if c.CalendarID == sourceCal {
			return c.PatchBody
		}
	}
	return nil
}

// TestTargetDeltaPhase_VirtualOccurrence_MaterializesSourceOverride is the
// B29 regression test, shaped like production rather than like the old 404
// fixture.
//
// The source series has no exception at the edited occurrence, so events.get
// on the constructed source-instance ID answers 200 with a VIRTUAL occurrence
// at the native slot. The mirror carries the parent's extendedProperties, so
// the inherited-instance branch fires and - before this fix - source-won,
// patching the user's 16:00 back to 15:45 with conflict=inherited_source_won
// and never touching the source.
//
// With the exception catalog reporting Absent, the drift is recognized as the
// user's edit and materialized onto the source instead.
func TestTargetDeltaPhase_VirtualOccurrence_MaterializesSourceOverride(t *testing.T) {
	api := newStubAPI()
	f := newB29Fixture()
	r := newB29Reconciler(api, f)
	// Ready catalog that does NOT contain this occurrence: the series has
	// other exceptions, just not one here.
	seedReadySourceCatalog(r, "src-A",
		exception(f.sourceParentID+"_20260527T154500Z", f.sourceParentID,
			"2026-05-27T15:45:00Z", "2026-05-27T16:30:00Z", gws.EventStatusConfirmed))

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*f.mirrorInstance}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")
	api.queueGet("src-A", f.sourceInstanceID, f.virtualSourceInstance())
	api.queueGet("tgt-A", f.mirrorInstanceID, f.mirrorInstance)

	// 1. Source patch materializes the exception at the user's time.
	patchedSource := f.virtualSourceInstance()
	patchedSource.Start = f.movedStart
	patchedSource.End = f.movedEnd
	patchedSource.Updated = "2026-04-30T12:00:00Z"
	api.queuePatch(patchedSource)
	// 2. Mirror rewrite from the post-patch source + checksum follow-up.
	post := *f.mirrorInstance
	post.Updated = "2026-04-30T12:00:01Z"
	api.queuePatch(&post)
	api.queuePatch(&post)

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var propagated *Outcome
	for i := range *captured {
		o := (*captured)[i]
		if o.Action == mirror.ActionPropagate && o.Reason == recurring.ReasonMirrorOnlyOverride {
			propagated = &o
			break
		}
	}
	if propagated == nil {
		t.Fatalf("expected propagate(mirror_only_override); got %+v", *captured)
	}
	if propagated.SourceEventID != f.sourceInstanceID {
		t.Errorf("SourceEventID = %q, want %q", propagated.SourceEventID, f.sourceInstanceID)
	}
	if propagated.TargetEventID != f.mirrorInstanceID {
		t.Errorf("TargetEventID = %q, want %q", propagated.TargetEventID, f.mirrorInstanceID)
	}

	// The source now carries the user's time, and never a recurrence (B16).
	body := srcPatchBody(api, "src-A")
	if body == nil {
		t.Fatalf("expected an events.patch against src-A; got %+v", api.callsByOp("EventsPatch"))
	}
	if body.Start == nil || body.Start.DateTime != f.movedStart.DateTime {
		t.Errorf("source patch Start = %+v, want %q", body.Start, f.movedStart.DateTime)
	}
	if body.Recurrence != nil {
		t.Errorf("source patch Recurrence MUST be nil (B16 guardrail); got %v", body.Recurrence)
	}

	// The mirror must NOT be reverted to the native slot. The rewrite comes
	// from the post-patch source, so it carries 16:00.
	var mirrorRewrite *gws.PatchEvent
	for _, c := range api.callsByOp("EventsPatch") {
		if c.CalendarID == "tgt-A" && c.PatchBody != nil && c.PatchBody.Start != nil {
			mirrorRewrite = c.PatchBody
			break
		}
	}
	if mirrorRewrite == nil {
		t.Fatal("expected a mirror rewrite patch carrying a start")
	}
	if mirrorRewrite.Start.DateTime != f.movedStart.DateTime {
		t.Errorf("mirror rewrite Start = %q, want %q (the user's edit must survive)",
			mirrorRewrite.Start.DateTime, f.movedStart.DateTime)
	}

	// The mapping is upgraded to the suffixed (managed) source tuple, so the
	// next tick takes the standard matrix instead of the inherited branch.
	managed := mirror.SourceTuple{CalendarID: "src-A", EventID: f.sourceInstanceID}
	got, ok := r.inventories["tgt-A"].Lookup(managed)
	if !ok {
		t.Fatalf("inventory missing managed-form entry at %+v", managed)
	}
	if got.ID != post.ID {
		t.Errorf("inventory entry id = %q, want %q", got.ID, post.ID)
	}

	if tok := r.targetSyncTokens["tgt-A"]; tok != "tok-tgt-new" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-tgt-new", tok)
	}
}

// TestTargetDeltaPhase_PresentException_InheritedBootstrapSourceWins pins the
// B15 hazard the isInherited guard exists for, which the B29 fix must not
// regress. Here the source DOES hold a real exception at the occurrence
// (the catalog says Present) and has not changed since the mirror recorded
// it. The auto-materialized mirror still shows the parent's RRULE
// projection, and that projection must lose: propagating it would clobber
// the user's source-side reschedule.
func TestTargetDeltaPhase_PresentException_InheritedBootstrapSourceWins(t *testing.T) {
	api := newStubAPI()
	f := newB29Fixture()
	r := newB29Reconciler(api, f)

	// The source exception exists and sits at 16:00; the auto-materialized
	// mirror still shows the 15:45 projection.
	sourceException := f.virtualSourceInstance()
	sourceException.Start = f.movedStart
	sourceException.End = f.movedEnd
	inherited := makeCleanCurrentMirror(f.mirrorInstanceID, "src-A:"+f.sourceParentID,
		sourceException.Updated, "2026-04-30T15:00:00Z", "Exercise",
		f.nativeStart, f.nativeEnd)
	inherited.RecurringEventID = f.mirrorParentID
	inherited.OriginalStartTime = f.nativeStart

	seedReadySourceCatalog(r, "src-A",
		exception(f.sourceInstanceID, f.sourceParentID,
			f.movedStart.DateTime, f.movedEnd.DateTime, gws.EventStatusConfirmed))

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*inherited}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")
	api.queueGet("src-A", f.sourceInstanceID, sourceException)
	api.queueGet("tgt-A", f.mirrorInstanceID, inherited)

	post := *inherited
	api.queuePatch(&post) // source-wins rewrite of the mirror
	api.queuePatch(&post) // checksum follow-up

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var found *Outcome
	for i := range *captured {
		o := (*captured)[i]
		if o.Conflict == mirror.ConflictInheritedSourceWon {
			found = &o
		}
		if o.Action == mirror.ActionPropagate {
			t.Errorf("a Present source exception must never propagate; got %+v", o)
		}
	}
	if found == nil {
		t.Fatalf("expected an inherited_source_won outcome; got %+v", *captured)
	}
	if body := srcPatchBody(api, "src-A"); body != nil {
		t.Errorf("source must not be patched when the exception is Present; got %+v", body)
	}

	// The rewrite still upgrades the mirror to per-instance metadata.
	patches := api.callsByOp("EventsPatch")
	if len(patches) == 0 || patches[0].PatchBody == nil || patches[0].PatchBody.ExtendedProperties == nil {
		t.Fatal("expected a mirror rewrite carrying extended properties")
	}
	gotTuple := patches[0].PatchBody.ExtendedProperties.Private[mirror.ExtKeySource]
	wantTuple := "src-A:" + f.sourceInstanceID
	if gotTuple != wantTuple {
		t.Errorf("rewrite %s = %q, want %q (per-instance tuple)",
			mirror.ExtKeySource, gotTuple, wantTuple)
	}
	if tok := r.targetSyncTokens["tgt-A"]; tok != "tok-tgt-new" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-tgt-new", tok)
	}
}

// TestTargetDeltaPhase_RecurringInstance404_TerminalSkip pins the corrected
// meaning of a 404 on the constructed source-instance ID. Google answers 200
// for every slot the parent's RRULE does produce, so a 404 means the slot is
// not in the series at all. The pre-B29 code read that 404 as "no source
// override, therefore materialize", which would create an exception with no
// occurrence behind it.
func TestTargetDeltaPhase_RecurringInstance404_TerminalSkip(t *testing.T) {
	api := newStubAPI()
	f := newB29Fixture()
	r := newB29Reconciler(api, f)
	seedReadySourceCatalog(r, "src-A")

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*f.mirrorInstance}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")
	api.queueGetErr("src-A", f.sourceInstanceID,
		&gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var skip *Outcome
	for i := range *captured {
		o := (*captured)[i]
		if o.Action == mirror.ActionSkip && o.Reason == reasonInstanceNotInSeries {
			skip = &o
			break
		}
	}
	if skip == nil {
		t.Fatalf("expected skip(instance_not_in_series); got %+v", *captured)
	}
	if len(api.callsByOp("EventsPatch")) != 0 {
		t.Errorf("a 404 must produce zero writes; got %+v", api.callsByOp("EventsPatch"))
	}
	// Terminal, so the event is consumed rather than replayed forever.
	if tok := r.targetSyncTokens["tgt-A"]; tok != "tok-tgt-new" {
		t.Errorf("terminal skip must advance the token; got %q", tok)
	}
}

// TestTargetDeltaPhase_UnknownCatalog_DefersWholeBatch pins the preflight.
// With the source collection unproven, no membership question can be
// answered, so the batch performs zero writes and its token stays put. The
// target token itself is fine, so NeedsFullResync must NOT be set - reseeding
// would seed past the unconsumed edit and lose it.
func TestTargetDeltaPhase_UnknownCatalog_DefersWholeBatch(t *testing.T) {
	api := newStubAPI()
	f := newB29Fixture()
	r := newB29Reconciler(api, f)
	// No catalog for src-A at all: readiness is Unknown.

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*f.mirrorInstance}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	sink, captured := captureOutputs()
	r.Output = sink

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(*captured) != 0 {
		t.Errorf("a deferred batch must emit no outcomes; got %+v", *captured)
	}
	if len(api.callsByOp("EventsPatch")) != 0 {
		t.Errorf("a deferred batch must perform zero writes; got %+v", api.callsByOp("EventsPatch"))
	}
	// Not even the dispatch read fires: preflight bails before the loop.
	for _, c := range api.callsByOp("EventsGet") {
		t.Errorf("a deferred batch must issue no events.get; got %+v", c)
	}
	if tok := r.targetSyncTokens["tgt-A"]; tok != "tok-tgt-old" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-tgt-old (unadvanced)", tok)
	}
	if res.PerTarget["tgt-A"].NeedsFullResync {
		t.Error("a source-side membership gap must NOT request a target reseed")
	}
	if res.PerTarget["tgt-A"].SyncTokenChanged {
		t.Error("SyncTokenChanged must be false for a deferred batch")
	}
}

// TestTargetDeltaPhase_UnknownCatalog_BlocksSafePrefixToo pins that the
// preflight covers the WHOLE batch. A non-recurring edit early in the batch
// would succeed on its own, but writing it while a later event pins the token
// would rewrite that prefix on every tick for as long as the source read
// stays broken.
func TestTargetDeltaPhase_UnknownCatalog_BlocksSafePrefixToo(t *testing.T) {
	api := newStubAPI()
	f := newB29Fixture()
	r := newB29Reconciler(api, f)
	// No catalog for src-A.

	safe := makeMirrorWithUserEdit("mirror-1", "src-A:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z",
		"Standup", "Standup-edited")

	queueTargetIncrDelta(api, "tgt-A",
		[]gws.Event{*safe, *f.mirrorInstance}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	// Everything the safe prefix would need to propagate successfully is
	// queued, so a pass that skips the preflight really would write it.
	srcEvent := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueGet("src-A", "src-evt", srcEvent)
	patchedSrc := *srcEvent
	patchedSrc.Summary = "Standup-edited"
	api.queuePatch(&patchedSrc)
	safePost := *safe
	api.queuePatch(&safePost)
	api.queuePatch(&safePost)

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(*captured) != 0 {
		t.Errorf("the safe prefix must not be written while the batch is deferred; got %+v", *captured)
	}
	if len(api.callsByOp("EventsPatch")) != 0 {
		t.Errorf("zero writes expected; got %+v", api.callsByOp("EventsPatch"))
	}
	if tok := r.targetSyncTokens["tgt-A"]; tok != "tok-tgt-old" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-tgt-old", tok)
	}
}

// TestTargetDeltaPhase_UnknownCatalog_DoesNotBlockNonRecurringEdits pins the
// other side of the preflight's scope. Only inherited-form recurring
// instances consult the catalog, so an Unknown source must not stall an
// ordinary target edit - that would trade a correctness fix for an
// availability regression.
func TestTargetDeltaPhase_UnknownCatalog_DoesNotBlockNonRecurringEdits(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.syncTokens["src-A"] = "tok-src-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	// No catalog for src-A.

	m := makeMirrorWithUserEdit("mirror-1", "src-A:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z",
		"Standup", "Standup-edited")

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*m}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	srcEvent := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueGet("src-A", "src-evt", srcEvent)
	patchedSrc := *srcEvent
	patchedSrc.Summary = "Standup-edited"
	api.queuePatch(&patchedSrc)
	post := *m
	api.queuePatch(&post)
	api.queuePatch(&post)

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var propagated bool
	for _, o := range *captured {
		if o.Action == mirror.ActionPropagate {
			propagated = true
		}
	}
	if !propagated {
		t.Errorf("a non-recurring edit must not be gated on the exception catalog; got %+v", *captured)
	}
	if tok := r.targetSyncTokens["tgt-A"]; tok != "tok-tgt-new" {
		t.Errorf("targetSyncTokens[tgt-A] = %q, want tok-tgt-new", tok)
	}
}

// TestTargetDeltaPhase_MaterializeRetry_DoesNotDuplicate pins the immediate
// catalog insert. The source patch lands, then the mirror rewrite fails, so
// the token stays pinned and the next tick re-delivers the same edit. If the
// catalog still answered Absent, that retry would materialize a SECOND
// override.
func TestTargetDeltaPhase_MaterializeRetry_DoesNotDuplicate(t *testing.T) {
	api := newStubAPI()
	f := newB29Fixture()
	r := newB29Reconciler(api, f)
	seedReadySourceCatalog(r, "src-A")

	// --- tick 1: source patch succeeds, mirror rewrite fails.
	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*f.mirrorInstance}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-1")
	api.queueGet("src-A", f.sourceInstanceID, f.virtualSourceInstance())
	api.queueGet("tgt-A", f.mirrorInstanceID, f.mirrorInstance)

	patchedSource := f.virtualSourceInstance()
	patchedSource.Start = f.movedStart
	patchedSource.End = f.movedEnd
	patchedSource.Updated = "2026-04-30T12:00:00Z"
	api.queuePatch(patchedSource)
	// The stub drains the patch-error queue ahead of the response queue, so
	// a nil placeholder lets the source patch succeed before the mirror
	// rewrite fails.
	api.queuePatchErr(nil)
	api.queuePatchErr(&gws.Error{Op: "events.patch", Code: gws.CodeAPIAuthFailed, ExitCode: 2})

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("tick 1: %v", err)
	}
	if tok := r.targetSyncTokens["tgt-A"]; tok != "tok-tgt-old" {
		t.Fatalf("tick 1 must pin the token after a write failure; got %q", tok)
	}
	if got := r.lookupSourceException("src-A", patchedSource); got != recurring.MembershipPresent {
		t.Fatalf("the materialized exception must be indexed immediately; got %s", got)
	}

	// --- tick 2: the same edit is re-delivered. The exception now exists,
	// so the mirror (already at 16:00) simply gets its metadata upgraded.
	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*f.mirrorInstance}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-2")
	api.queueGet("src-A", f.sourceInstanceID, patchedSource)
	api.queueGet("tgt-A", f.mirrorInstanceID, f.mirrorInstance)
	post := *f.mirrorInstance
	api.queuePatch(&post)
	api.queuePatch(&post)

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("tick 2: %v", err)
	}

	var srcPatches int
	for _, c := range api.callsByOp("EventsPatch") {
		if c.CalendarID == "src-A" {
			srcPatches++
		}
	}
	if srcPatches != 1 {
		t.Errorf("the source must be patched exactly once across both ticks; got %d", srcPatches)
	}
}

// TestTargetDeltaPhase_OutOfScopeOccurrence_NoWrites pins the coverage guard.
// The occurrence sits beyond the window the source snapshot proved, so
// absence from the index means nothing: source-wins might destroy the user's
// edit and target-wins might clobber a source override the snapshot never
// reached. Write nothing, and consume the event.
func TestTargetDeltaPhase_OutOfScopeOccurrence_NoWrites(t *testing.T) {
	api := newStubAPI()
	f := newB29Fixture()
	r := newB29Reconciler(api, f)
	// Coverage stops well before the 2026-05-20 occurrence.
	r.rebuildSourceCatalog("src-A", nil,
		must2026(), must2026().Add(24*time.Hour))

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*f.mirrorInstance}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")
	api.queueGet("src-A", f.sourceInstanceID, f.virtualSourceInstance())
	api.queueGet("tgt-A", f.mirrorInstanceID, f.mirrorInstance)

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	var skip *Outcome
	for i := range *captured {
		o := (*captured)[i]
		if o.Reason == recurring.ReasonOutsideCatalogCoverage {
			skip = &o
		}
	}
	if skip == nil {
		t.Fatalf("expected skip(outside_catalog_coverage); got %+v", *captured)
	}
	if len(api.callsByOp("EventsPatch")) != 0 {
		t.Errorf("out-of-scope must write nothing; got %+v", api.callsByOp("EventsPatch"))
	}
	if tok := r.targetSyncTokens["tgt-A"]; tok != "tok-tgt-new" {
		t.Errorf("out-of-scope is terminal and must be consumed; token = %q", tok)
	}
}

// ---------- 4a. Target-cancellation quarantine ----------

// TestTargetDeltaPhase_CancelledTargetEventIsQuarantined pins B29 item 6.
// Target deltas list with ShowDeleted=true, so waking this phase makes the
// B20 revive cells reachable for events the user deliberately deleted. Until
// reverse cancellation ships, such an event is warned about, skipped, and
// CONSUMED - pinning it would head-of-line block every later target edit.
func TestTargetDeltaPhase_CancelledTargetEventIsQuarantined(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.syncTokens["src-A"] = "tok-src-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	logger := &captureLogger{}
	r.Log = logger

	deleted := makeCleanCurrentMirror("mirror-1", "src-A:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z", "Standup",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		&gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"})
	deleted.Status = gws.EventStatusCancelled

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*deleted}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(*captured) != 1 {
		t.Fatalf("expected exactly one outcome; got %+v", *captured)
	}
	got := (*captured)[0]
	if got.Action != mirror.ActionSkip || got.Reason != reasonTargetCancelled {
		t.Errorf("outcome = %s/%s, want skip/target_cancelled", got.Action, got.Reason)
	}
	// Never classified: no source read, so no revive cell is reachable.
	if calls := api.callsByOp("EventsGet"); len(calls) != 0 {
		t.Errorf("a quarantined deletion must issue no events.get; got %+v", calls)
	}
	if calls := api.callsByOp("EventsPatch"); len(calls) != 0 {
		t.Errorf("a quarantined deletion must issue no writes; got %+v", calls)
	}
	// Consumed, not pinned.
	if tok := r.targetSyncTokens["tgt-A"]; tok != "tok-tgt-new" {
		t.Errorf("a quarantined deletion must advance the token; got %q", tok)
	}
	var warned bool
	for _, entry := range logger.warns {
		msg, _ := entry["msg"].(string)
		if strings.Contains(msg, "target-side deletion") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("expected a structured warning about the quarantined deletion; got %v", logger.warns)
	}
}

// TestTargetDeltaPhase_CancelledRecurringInstanceIsQuarantined is the
// recurring variant. It also pins that the quarantine runs BEFORE the batch
// preflight can care: a cancelled event never consults the catalog, so an
// Unknown source must not turn a deletion into a deferred batch.
func TestTargetDeltaPhase_CancelledRecurringInstanceIsQuarantined(t *testing.T) {
	api := newStubAPI()
	f := newB29Fixture()
	r := newB29Reconciler(api, f)
	// Deliberately no catalog for src-A.

	deleted := *f.mirrorInstance
	deleted.Status = gws.EventStatusCancelled

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{deleted}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	sink, captured := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if len(*captured) != 1 || (*captured)[0].Reason != reasonTargetCancelled {
		t.Fatalf("expected one skip(target_cancelled); got %+v", *captured)
	}
	if calls := api.callsByOp("EventsPatch"); len(calls) != 0 {
		t.Errorf("a quarantined deletion must issue no writes; got %+v", calls)
	}
	if tok := r.targetSyncTokens["tgt-A"]; tok != "tok-tgt-new" {
		t.Errorf("a quarantined deletion must advance the token; got %q", tok)
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

	// Pin the outcome shape: exactly one outcome, action=skip,
	// reason=unchanged. A future drift-signal regression that emits
	// skip(stale_bookkeeping) (or any other reason) without firing patches
	// would still satisfy a "no writes" assertion - asserting on the
	// reason is what holds the contract.
	if len(*captured) != 1 {
		t.Fatalf("self-write should emit exactly one outcome; got %+v", *captured)
	}
	got := (*captured)[0]
	if got.Action != mirror.ActionSkip || got.Reason != mirror.ReasonUnchanged {
		t.Errorf("self-write outcome = %s/%s, want skip/unchanged",
			got.Action, got.Reason)
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
	// The batch carries an inherited-form instance, so the preflight needs a
	// proven source collection before any of it may be written (B29).
	seedReadySourceCatalog(r, "src-A",
		exception("src-parent_20260520T160000Z", "src-parent",
			"2026-05-20T16:00:00Z", "2026-05-20T17:00:00Z", gws.EventStatusConfirmed))

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

	// Recurring handler step 2: the constructed-ID locate get returns the
	// mirror instance.
	api.queueGet("tgt-A", instanceID, clean)
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

	// The locate get must be addressed to the mirror instance ID constructed
	// from the MIRROR PARENT's id (mirror-parent + occurrence key), not from
	// the child instance's id. If the inv.Set shadow ever returns, the
	// recurring handler's resolveMirrorParent step would pick up the child
	// (cached at the parent's tuple) and locate-instance would construct a
	// double-suffixed id that 404s in production.
	var locateGet *recordedCall
	for i := range api.callsByOp("EventsGet") {
		c := api.callsByOp("EventsGet")[i]
		if c.CalendarID == "tgt-A" {
			locateGet = &c
		}
	}
	if locateGet == nil {
		t.Fatal("expected a locate events.get against the target calendar")
	}
	if locateGet.EventID != instanceID {
		t.Errorf("locate get must address the mirror instance id %q; got %q", instanceID, locateGet.EventID)
	}

	// The parent inventory entry must still point at the parent (not the
	// child instance). Pinning this protects the recurring handler's parent
	// lookup against future regressions of the inv.Set shadow.
	parentTuple := mirror.SourceTuple{CalendarID: "src-A", EventID: "src-parent"}
	got, ok := inv.Lookup(parentTuple)
	if !ok {
		t.Fatalf("parent inventory entry missing after target-delta")
	}
	if got.ID != "mirror-parent" {
		t.Errorf("parent inventory entry shadowed; got id=%q want %q", got.ID, "mirror-parent")
	}
	if got.RecurringEventID != "" {
		t.Errorf("parent inventory entry shadowed (got a child instance with RecurringEventID=%q)",
			got.RecurringEventID)
	}
}

// TestTargetDeltaPhase_InheritedInstance_DoesNotShadowParentInInventory
// pins the Finding-1 fix: an inherited-form recurring instance must NOT
// overwrite the parent's inventory entry. Before the fix, inv.Set(tuple,
// mirrorEvent) used the source-tuple parsed from the inherited instance -
// which equals the parent's tuple - and silently shadowed the parent with
// the child instance. The recurring handler's resolveMirrorParent would
// then return the child, and locateMirrorInstance would query
// events.instances against the child's id (B16-class data corruption).
//
// This is the simpler shadow-pin variant of
// TestTargetDeltaPhase_InheritedAutoMaterializedNoUserEdit: it doesn't
// exercise the inherited_upgrade path, just the inventory-shape invariant.
func TestTargetDeltaPhase_InheritedInstance_DoesNotShadowParentInInventory(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	// Inherited-form mirror instance. parentTupleStr = "src-A:src-parent"
	// has no `_<UTC>` suffix, so the source-tuple parses as the parent's
	// tuple - exactly the value that would shadow the parent on inv.Set.
	instanceID := "mirror-parent_20260520T160000Z"
	parentTupleStr := "src-A:src-parent"
	start := &gws.EventDateTime{DateTime: "2026-05-20T16:00:00Z"}
	end := &gws.EventDateTime{DateTime: "2026-05-20T17:00:00Z"}
	clean := makeCleanCurrentMirror(instanceID, parentTupleStr,
		"2026-04-29T20:00:00Z", "2026-04-30T10:00:00Z",
		"Lunch", start, end)
	clean.RecurringEventID = "mirror-parent"
	clean.OriginalStartTime = start

	// Pre-seed: parent in inventory at the parent's tuple. This is the
	// canonical shape BuildInventory's pass-2 produces (parent kept,
	// inherited child filtered).
	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	inv := NewInventory("tgt-A")
	parentTuple := mirror.SourceTuple{CalendarID: "src-A", EventID: "src-parent"}
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
	inv.Set(parentTuple, mirrorParent)
	r.inventories["tgt-A"] = inv
	r.syncTokens["src-A"] = "tok-src-old"
	// Without a proven source collection the B29 preflight would defer the
	// whole batch and this test would pass vacuously.
	seedReadySourceCatalog(r, "src-A",
		exception("src-parent_20260520T160000Z", "src-parent",
			"2026-05-20T16:00:00Z", "2026-05-20T17:00:00Z", gws.EventStatusConfirmed))

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*clean}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	// Source instance exists. Drives the recurring handler down the
	// inherited-upgrade path; we don't assert on the outcome here, just on
	// the inventory shape afterward.
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
	post := *clean
	api.queuePatch(&post)
	api.queuePatch(&post)

	sink, _ := captureOutputs()
	r.Output = sink

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Pin: the parent inventory entry still points at the PARENT, not the
	// child. With the pre-fix code, this fails because inv.Set(parentTuple,
	// childInstance) overwrites the parent.
	got, ok := inv.Lookup(parentTuple)
	if !ok {
		t.Fatalf("parent inventory entry missing after target-delta of inherited instance")
	}
	if got.ID != "mirror-parent" {
		t.Errorf("parent inventory entry shadowed by child; got id=%q want mirror-parent", got.ID)
	}
	if got.RecurringEventID != "" {
		t.Errorf("parent inventory shadowed by recurring instance; got RecurringEventID=%q", got.RecurringEventID)
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

// TestTargetDeltaPhase_StaleTokenClearedWhenStagedEmpty is the target-side
// equivalent of the source-side rule documented in SPEC line 1017: when a
// successful events.list returns no nextSyncToken (Google can omit it on
// long deltas) and no event errored, the in-memory targetSyncToken must be
// cleared so the next cycle runs another FullSync rather than a Tick with
// a stale token Google has already invalidated.
//
// Source-side analog:
// internal/sync/reconciler_test.go:200 (TestAdvanceTokens_StaleTokenClearedWhenStagedEmpty).
func TestTargetDeltaPhase_StaleTokenClearedWhenStagedEmpty(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"

	// Empty delta AND empty nextToken: the long-delta case where Google
	// omitted the nextSyncToken. The branch fires regardless of how many
	// events were in the delta as long as none errored.
	queueTargetIncrDelta(api, "tgt-A", nil, "")
	api.queueListIncr("src-A", nil, "tok-src-new")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if _, ok := r.targetSyncTokens["tgt-A"]; ok {
		t.Errorf("targetSyncTokens[tgt-A] should be cleared when nextToken is empty")
	}
	if !res.PerTarget["tgt-A"].NeedsFullResync {
		t.Errorf("PerTarget[tgt-A].NeedsFullResync should be true after empty nextToken")
	}
	if res.PerTarget["tgt-A"].SyncTokenChanged {
		t.Errorf("SyncTokenChanged should be false; the token was cleared, not advanced")
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

	// events.get returns api_auth_failed - a NON-transient error per the B18
	// matrix in isTransientClassifyReadError. Auth failures must keep the
	// token pinned so the next tick re-attempts after the operator fixes
	// credentials, not silently advance past the unprocessed event.
	api.queueGetErr("src-A", "src-evt",
		&gws.Error{Code: gws.CodeAPIAuthFailed, ExitCode: 2})

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := r.targetSyncTokens["tgt-A"]; got != "tok-tgt-old" {
		t.Errorf("token must NOT advance on error; got %q want tok-tgt-old", got)
	}
}

// TestTargetDeltaPhase_TransientReadAdvancesToken pins B18-style
// transient-read tolerance for the target-delta phase: a backend / 400 /
// 404 read flake on events.get / events.instances must be logged and
// skipped, NOT pin the targetSyncToken. Without this carve-out a single
// flaky read replays the same delta forever (the source-delta classify
// loop has the same tolerance for the same reason).
func TestTargetDeltaPhase_TransientReadAdvancesToken(t *testing.T) {
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

	// events.get returns backend_error - the transient flake the live-
	// observed Office Hours pattern surfaces. With the B18 tolerance this
	// is logged + skipped, the token advances, and the next tick re-fetches
	// fresh.
	api.queueGetErr("src-A", "src-evt",
		&gws.Error{Op: "events.get", Code: gws.CodeBackendError, ExitCode: 1})

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	if got := r.targetSyncTokens["tgt-A"]; got != "tok-tgt-new" {
		t.Errorf("transient must NOT pin token; got %q want tok-tgt-new", got)
	}
	if !res.PerTarget["tgt-A"].SyncTokenChanged {
		t.Error("PerTarget[tgt-A].SyncTokenChanged should be true after transient")
	}
}

// TestTargetDeltaPhase_NoInventoryDoesNotAdvanceToken pins the Finding-3
// fix: when seedTargetSyncTokens succeeds but rebuildInventories fails for
// a target, the token is set but the inventory is absent. Without the
// guard, target-delta would still list and advance the token, consuming
// the delta without reconciling it against any inventory. The fix bails
// out before EventsList and leaves the token alone for a future tick to
// pick up after a successful FullSync.
func TestTargetDeltaPhase_NoInventoryDoesNotAdvanceToken(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-stale"
	r.syncTokens["src-A"] = "tok-src-old"
	// Deliberately do NOT install an inventory for tgt-A. This simulates
	// the failure mode where seedTargetSyncTokens succeeded but
	// rebuildInventories errored for this target.

	// Source-delta still runs (independent of target-delta inventory state).
	api.queueListIncr("src-A", nil, "tok-src-new")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick: %v", err)
	}

	// Token must NOT advance.
	if got := r.targetSyncTokens["tgt-A"]; got != "tok-tgt-stale" {
		t.Errorf("missing inventory must NOT advance token; got %q want tok-tgt-stale", got)
	}
	if res.PerTarget["tgt-A"].SyncTokenChanged {
		t.Error("PerTarget[tgt-A].SyncTokenChanged must be false when inventory missing")
	}
	// No EventsList call should fire against tgt-A. Target-delta's whole
	// phase should be skipped, not just the per-event loop.
	for _, c := range api.callsByOp("EventsList") {
		if c.CalendarID == "tgt-A" {
			t.Errorf("missing-inventory target must not be listed; got %+v", c)
		}
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

// TestTickPhaseOrdering_TargetWritesFollowSourceRead pins the staged shape
// B29 item 5 introduced: the target delta is READ first, then the source
// delta is read (refreshing the exception catalog), and only then does any
// target-driven write happen.
//
// Reading targets first keeps the source view as close as possible to the
// reverse write, so an override that already existed when the target was read
// is guaranteed to be in the catalog. Writing only after the source read is
// what makes the membership answer trustworthy at write time.
func TestTickPhaseOrdering_TargetWritesFollowSourceRead(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	r.targetSyncTokens["tgt-A"] = "tok-tgt-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"

	m := makeMirrorWithUserEdit("mirror-1", "src-A:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T11:00:00Z",
		"Standup", "Standup-edited")

	queueTargetIncrDelta(api, "tgt-A", []gws.Event{*m}, "tok-tgt-new")
	api.queueListIncr("src-A", nil, "tok-src-new")

	srcEvent := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueGet("src-A", "src-evt", srcEvent)
	patchedSrc := *srcEvent
	patchedSrc.Summary = "Standup-edited"
	api.queuePatch(&patchedSrc)
	post := *m
	api.queuePatch(&post)
	api.queuePatch(&post)

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	tgtListIdx, srcListIdx, firstWriteIdx := -1, -1, -1
	for i, c := range api.calls {
		switch c.Op {
		case "EventsList":
			if c.ListParams.SyncToken == "" {
				continue
			}
			if c.CalendarID == "tgt-A" && tgtListIdx == -1 {
				tgtListIdx = i
			}
			if c.CalendarID == "src-A" && srcListIdx == -1 {
				srcListIdx = i
			}
		case "EventsPatch", "EventsInsert", "EventsDelete":
			if firstWriteIdx == -1 {
				firstWriteIdx = i
			}
		}
	}
	if tgtListIdx == -1 || srcListIdx == -1 || firstWriteIdx == -1 {
		t.Fatalf("expected target list, source list and a write; got %d/%d/%d",
			tgtListIdx, srcListIdx, firstWriteIdx)
	}
	if tgtListIdx >= srcListIdx {
		t.Errorf("target read must precede source read; tgt=%d src=%d", tgtListIdx, srcListIdx)
	}
	if srcListIdx >= firstWriteIdx {
		t.Errorf("no write may happen before the source read; src=%d write=%d",
			srcListIdx, firstWriteIdx)
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
