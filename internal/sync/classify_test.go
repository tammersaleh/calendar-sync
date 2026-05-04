package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
	"github.com/tammersaleh/calendar-sync/internal/recurring"
)

// ---------- helpers ----------

// fixedNow returns a clock function that always reports t.
func fixedNow(t time.Time) func() time.Time {
	return func() time.Time { return t }
}

// must2026 returns a fixed wall-clock time used by horizon-related tests.
// Picked well before 2027 so events scheduled in 2026 pass an in-horizon
// check and events scheduled in 2030 do not.
func must2026() time.Time {
	t, err := time.Parse(time.RFC3339, "2026-04-30T00:00:00Z")
	if err != nil {
		panic(err)
	}
	return t
}

// captureOutputs returns an Output sink + a slice the caller can read after
// the Classify call returns.
func captureOutputs() (Output, *[]Outcome) {
	var collected []Outcome
	out := Output(func(o Outcome) { collected = append(collected, o) })
	return out, &collected
}

// makeNonRecurringSource builds a typical non-recurring source event.
//
// Description is set to the summary so that BuildPayload's
// `source.Description + trailerPrefix + source.HTMLLink` produces the
// same string makeCleanCurrentMirror writes for the mirror's body. This
// alignment matters for B23's FieldsDisagree signal: a source/mirror
// pair whose actual managed fields disagree is treated as drift, even
// when stored bookkeeping says clean.
func makeNonRecurringSource(id, updated string, start *gws.EventDateTime) *gws.Event {
	return &gws.Event{
		ID:           id,
		Status:       gws.EventStatusConfirmed,
		Summary:      "Standup",
		Description:  "Standup",
		Start:        start,
		End:          &gws.EventDateTime{DateTime: addHourToDateTime(start.DateTime)},
		Updated:      updated,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
		Transparency: gws.TransparencyOpaque,
	}
}

// addHourToDateTime is a tiny helper for end times in tests.
func addHourToDateTime(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.Add(time.Hour).Format(time.RFC3339)
}

// makeCleanCurrentMirror builds a clean current-schema mirror whose live managed fields match the
// stored checksum (i.e. MirrorDrifted=false). storedSourceUpdated is what
// the mirror recorded as the source's updated at last write; liveUpdated is
// the mirror's own updated. To produce drift, mutate the returned event
// before passing it to Classify.
func makeCleanCurrentMirror(id, source, storedSourceUpdated, liveUpdated string, summary string, start, end *gws.EventDateTime) *gws.Event {
	e := &gws.Event{
		ID:           id,
		Status:       gws.EventStatusConfirmed,
		Summary:      summary,
		Description:  summary + "\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC",
		Start:        start,
		End:          end,
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
		Updated:      liveUpdated,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:        source,
				mirror.ExtKeySourceUpdated: storedSourceUpdated,
				mirror.ExtKeyVersion:       mirror.SchemaVersion,
			},
		},
	}
	e.ExtendedProperties.Private[mirror.ExtKeyChecksum] = mirror.Checksum(mirror.ManagedFieldsFromEvent(e))
	return e
}

func makeV1Mirror(id, source, storedSourceUpdated, liveUpdated, summary string, start, end *gws.EventDateTime) *gws.Event {
	return &gws.Event{
		ID:           id,
		Status:       gws.EventStatusConfirmed,
		Summary:      summary,
		Description:  summary + "\n\n---\nSource: https://www.google.com/calendar/event?eid=ABC",
		Start:        start,
		End:          end,
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
		Updated:      liveUpdated,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:        source,
				mirror.ExtKeySourceUpdated: storedSourceUpdated,
				mirror.ExtKeyVersion:       "1",
				// no checksum for v1
			},
		},
	}
}

// classifyOptions captures the per-test classifier overrides; defaults make
// most tests one-line short.
type classifyOptions struct {
	pair             string
	direction        string
	sourceCalendarID string
	targetCalendarID string
	sourceWritable   bool
	horizon          time.Duration
	now              time.Time
	recurring        *recurring.Handler
}

func newClassifier(t *testing.T, api API, inv *Inventory, sink Output, opts classifyOptions) *Classifier {
	t.Helper()
	if opts.sourceCalendarID == "" {
		opts.sourceCalendarID = "src-cal"
	}
	if opts.targetCalendarID == "" {
		opts.targetCalendarID = "tgt-cal"
	}
	if opts.pair == "" {
		opts.pair = "test-pair"
	}
	if opts.direction == "" {
		opts.direction = "a_to_b"
	}
	now := opts.now
	if now.IsZero() {
		now = must2026()
	}
	c := &Classifier{
		API:              api,
		Now:              fixedNow(now),
		Horizon:          opts.horizon,
		Pair:             opts.pair,
		Direction:        opts.direction,
		SourceCalendarID: opts.sourceCalendarID,
		TargetCalendarID: opts.targetCalendarID,
		SourceWritable:   opts.sourceWritable,
		Inventory:        inv,
		Recurring:        opts.recurring,
		Output:           sink,
	}
	return c
}

// firstOutcome is a tiny convenience for tests that only emit one outcome.
func firstOutcome(t *testing.T, outcomes []Outcome) Outcome {
	t.Helper()
	if len(outcomes) == 0 {
		t.Fatal("no outcomes emitted")
	}
	return outcomes[0]
}

// ---------- step 1: already a mirror ----------

func TestClassify_Step1_IsMirror_SkipNoAPICalls(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.ExtendedProperties = &gws.ExtendedProperties{Private: map[string]string{
		mirror.ExtKeySource: "some-cal:some-evt",
	}}

	c := newClassifier(t, api, inv, sink, classifyOptions{})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	if len(api.calls) != 0 {
		t.Errorf("step 1 should make zero API calls; got %v", api.calls)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionSkip || got.Reason != ReasonIsMirror {
		t.Errorf("got %s/%s, want skip/is_mirror", got.Action, got.Reason)
	}
}

// ---------- step 2: recurring instance delegation ----------

func TestClassify_Step2_RecurringDelegation_UpdatesInventory(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := &gws.Event{
		ID:                "src-evt",
		Status:            gws.EventStatusConfirmed,
		Summary:           "Updated",
		Updated:           "2026-04-30T10:00:00Z",
		HTMLLink:          "https://www.google.com/calendar/event?eid=ABC",
		RecurringEventID:  "src-parent",
		OriginalStartTime: &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		Start:             &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		End:               &gws.EventDateTime{DateTime: "2026-05-01T14:00:00Z"},
		Transparency:      gws.TransparencyOpaque,
	}

	mirrorParent := &gws.Event{
		ID:         "mp-1",
		Summary:    "Standup",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"},
	}
	mirrorInst := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T08:00:00Z",
		"Standup", source.Start, source.End,
	)
	api.queueInstances([]gws.Event{*mirrorInst})

	// The recurring handler will compute drift, see the mirror needs the
	// source-changed patch, and fire a main+checksum patch pair on the
	// mirror.
	postMain := *mirrorInst
	postMain.Summary = "Updated"
	postMain.Updated = "2026-04-30T10:00:01Z"
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	rec := &recurring.Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}

	c := newClassifier(t, api, inv, sink, classifyOptions{
		sourceWritable: true,
		recurring:      rec,
	})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("delegated outcome = %s/%s, want patch/source_updated", got.Action, got.Reason)
	}
	if got.SourceEventID != source.ID {
		t.Errorf("Outcome.SourceEventID = %q, want %q", got.SourceEventID, source.ID)
	}
	// Inventory should be updated with the post-checksum mirror instance.
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: source.ID}
	got2, ok := inv.Lookup(tuple)
	if !ok || got2.Summary != "Updated" {
		t.Errorf("inventory entry not updated; got=%+v ok=%v", got2, ok)
	}
}

// TestClassify_Step2_RecurringDelegation_PartialRepairOnError_UpdatesInventory
// pins B19. When the recurring handler's locate-and-repair path
// successfully writes a new mirror parent (forceRewriteMirrorParent
// fires 2 patches) but the subsequent events.instances retry returns
// a transient error, classifyRecurringInstance must still apply the
// post-write mirror parent to the inventory. Without this propagation,
// the next tick's classify loop sees the stale inventory entry and
// re-fires the force-rewrite - bounded only by the next FullSync's
// inventory rebuild.
func TestClassify_Step2_RecurringDelegation_PartialRepairOnError_UpdatesInventory(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, _ := captureOutputs()

	source := &gws.Event{
		ID:                "src-evt",
		Status:            gws.EventStatusConfirmed,
		Summary:           "Standup",
		Description:       "Standup",
		Updated:           "2026-04-30T10:00:00Z",
		HTMLLink:          "https://www.google.com/calendar/event?eid=ABC",
		RecurringEventID:  "src-parent",
		OriginalStartTime: &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		Start:             &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		End:               &gws.EventDateTime{DateTime: "2026-05-01T14:00:00Z"},
		Transparency:      gws.TransparencyOpaque,
	}

	// Stale mirror parent in inventory (recurrence will be replaced by repair).
	parentTuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-parent"}
	staleParent := &gws.Event{
		ID:         "mp-1",
		Summary:    "Standup (stale)",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"}, // outdated; repair will rewrite
	}
	inv.Set(parentTuple, staleParent)

	// First instances lookup: empty (triggers repair). The sync stub's
	// EventsInstances dequeues from instancesErrors before instancesResp, so
	// a leading nil error sentinel keeps the first call on the success path.
	api.instancesErrors = append(api.instancesErrors, nil)
	api.queueInstances(nil)

	// Repair fetches the source parent.
	sourceParent := &gws.Event{
		ID:         "src-parent",
		Status:     gws.EventStatusConfirmed,
		Summary:    "Standup",
		Recurrence: []string{"RRULE:FREQ=DAILY"}, // new rule from source
		Updated:    "2026-04-30T09:00:00Z",
		HTMLLink:   "https://www.google.com/calendar/event?eid=PP",
	}
	api.queueGet("src-cal", "src-parent", sourceParent)

	// forceRewriteMirrorParent fires 2 patches (main + checksum follow-up).
	postPatchParent := *staleParent
	postPatchParent.Recurrence = []string{"RRULE:FREQ=DAILY"} // matches sourceParent
	api.queuePatch(&postPatchParent)
	postChecksumParent := postPatchParent
	api.queuePatch(&postChecksumParent)

	// Second instances lookup (the retry): transient 5xx.
	api.instancesErrors = append(api.instancesErrors, &gws.Error{
		Code:     gws.CodeBackendError,
		ExitCode: 1,
		Op:       "events.instances",
	})

	rec := &recurring.Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return staleParent, true
		},
	}

	c := newClassifier(t, api, inv, sink, classifyOptions{
		sourceWritable: true,
		recurring:      rec,
	})
	err := c.Classify(context.Background(), source)
	if err == nil {
		t.Fatal("Classify should return the transient retry error so the caller (runClassifyLoop) can decide whether to skip-and-advance")
	}

	// Inventory must hold the post-rewrite mirror parent now, not the stale one.
	got, ok := inv.Lookup(parentTuple)
	if !ok {
		t.Fatal("inventory must still hold the mirror parent")
	}
	if len(got.Recurrence) != 1 || got.Recurrence[0] != "RRULE:FREQ=DAILY" {
		t.Errorf("inventory mirror parent recurrence = %v, want [RRULE:FREQ=DAILY] (post-rewrite); got %+v",
			got.Recurrence, got)
	}
}

// TestClassify_Step2_RecurringDelegation_BothPostWritesUpdated covers the
// case where the recurring handler returns BOTH PostWriteMirrorParent AND
// PostWriteMirrorInstance non-nil - the step-2 force-rewrite path. Both
// inventory entries (parent at source.RecurringEventID, instance at
// source.ID) must get folded back per classifyRecurringInstance's contract.
func TestClassify_Step2_RecurringDelegation_BothPostWritesUpdated(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := &gws.Event{
		ID:                "src-evt",
		Status:            gws.EventStatusConfirmed,
		Summary:           "Standup",
		Updated:           "2026-04-30T10:00:00Z",
		HTMLLink:          "https://www.google.com/calendar/event?eid=ABC",
		RecurringEventID:  "src-parent",
		OriginalStartTime: &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		Start:             &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		End:               &gws.EventDateTime{DateTime: "2026-05-01T14:00:00Z"},
		Transparency:      gws.TransparencyOpaque,
	}

	// Existing mirror parent; step 1 finds it via the inventory callback.
	mirrorParent := &gws.Event{
		ID:         "mp-1",
		Summary:    "Standup",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"},
	}

	// Step 2 force-rewrite path: first events.instances call returns empty,
	// triggering the repair flow.
	api.queueInstances(nil)

	// Step 2 then re-fetches the source parent for forceRewriteMirrorParent.
	sourceParent := &gws.Event{
		ID:           "src-parent",
		Status:       gws.EventStatusConfirmed,
		Summary:      "Standup",
		Updated:      "2026-04-29T20:00:00Z",
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
		Recurrence:   []string{"RRULE:FREQ=WEEKLY"},
		Start:        &gws.EventDateTime{DateTime: "2026-04-25T13:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-04-25T14:00:00Z"},
		Transparency: gws.TransparencyOpaque,
	}
	api.queueGet("src-cal", "src-parent", sourceParent)

	// forceRewriteMirrorParent does main + checksum patch on the mirror parent.
	repairedParent := &gws.Event{
		ID:         "mp-1",
		Summary:    "Standup",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"},
	}
	api.queuePatch(repairedParent) // main rewrite
	api.queuePatch(repairedParent) // checksum followup

	// Step 2 retries events.instances; now it returns one instance.
	mirrorInst := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T08:00:00Z",
		"Standup", source.Start, source.End,
	)
	api.queueInstances([]gws.Event{*mirrorInst})

	// Step 3 drift matrix: source.Updated (2026-04-30T10:00:00Z) is newer than
	// stored source_updated on mirrorInst (2026-04-29T20:00:00Z), so source-
	// changed = true. Mirror is clean (matches checksum), so drift = false.
	// Outcome: patch + checksum followup with source_updated reason.
	postInst := *mirrorInst
	postInst.Summary = "Standup"
	api.queuePatch(&postInst) // main patch
	api.queuePatch(&postInst) // checksum followup

	rec := &recurring.Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}

	c := newClassifier(t, api, inv, sink, classifyOptions{
		sourceWritable: true,
		recurring:      rec,
	})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}

	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("delegated outcome = %s/%s, want patch/source_updated", got.Action, got.Reason)
	}

	// BOTH inventory entries must be updated:
	// - parent at (SourceCalendarID, source.RecurringEventID)
	parentTuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: source.RecurringEventID}
	gotParent, ok := inv.Lookup(parentTuple)
	if !ok {
		t.Errorf("parent inventory entry missing for %+v", parentTuple)
	} else if gotParent.ID != repairedParent.ID {
		t.Errorf("parent inventory ID = %q, want %q", gotParent.ID, repairedParent.ID)
	}

	// - instance at (SourceCalendarID, source.ID)
	instTuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: source.ID}
	gotInst, ok := inv.Lookup(instTuple)
	if !ok {
		t.Errorf("instance inventory entry missing for %+v", instTuple)
	} else if gotInst.ID != mirrorInst.ID {
		t.Errorf("instance inventory ID = %q, want %q", gotInst.ID, mirrorInst.ID)
	}
}

// TestClassify_Step2_RecurringDelegation_CancellationKeepsInventoryEntry
// documents the intentional behavior in classifyRecurringInstance: a
// cancellation result (Action=delete, source_cancelled) writes the post-
// cancel mirror back into the inventory rather than removing it. The
// non-recurring deleteOrSkip path DOES delete the inventory entry; the
// recurring path keeps it because recurring cancellations are
// status=cancelled patches, not events.delete - the cancelled mirror
// remains a real Calendar resource that future syncs need to track.
func TestClassify_Step2_RecurringDelegation_CancellationKeepsInventoryEntry(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := &gws.Event{
		ID:                "src-evt",
		Status:            gws.EventStatusCancelled,
		Updated:           "2026-04-30T10:00:00Z",
		HTMLLink:          "https://www.google.com/calendar/event?eid=ABC",
		RecurringEventID:  "src-parent",
		OriginalStartTime: &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
	}

	mirrorParent := &gws.Event{
		ID:         "mp-1",
		Summary:    "Standup",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"},
	}

	// Step 2 finds the live (not-yet-cancelled) mirror instance.
	mirrorInst := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T08:00:00Z",
		"Standup",
		&gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		&gws.EventDateTime{DateTime: "2026-05-01T14:00:00Z"},
	)
	api.queueInstances([]gws.Event{*mirrorInst})

	// Pre-populate inventory with the live instance entry. After Classify,
	// it should still be present (with the post-cancel resource).
	instTuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: source.ID}
	inv.Set(instTuple, mirrorInst)

	// Step 3 cancellation: events.patch with status=cancelled returns the
	// post-cancel mirror.
	postCancel := *mirrorInst
	postCancel.Status = gws.EventStatusCancelled
	api.queuePatch(&postCancel)

	rec := &recurring.Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}

	c := newClassifier(t, api, inv, sink, classifyOptions{
		sourceWritable: true,
		recurring:      rec,
	})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}

	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionDelete {
		t.Errorf("expected delete action; got %s/%s", got.Action, got.Reason)
	}

	// Inventory MUST still contain the entry (with the post-cancel resource);
	// classifyRecurringInstance keeps it intentionally because recurring
	// cancellation is a status=cancelled patch, not events.delete.
	gotEntry, ok := inv.Lookup(instTuple)
	if !ok {
		t.Fatalf("inventory entry should still exist after recurring cancellation")
	}
	if gotEntry.Status != gws.EventStatusCancelled {
		t.Errorf("inventory entry should reflect post-cancel resource; got Status=%q", gotEntry.Status)
	}
}

func TestClassify_Step2_RecurringDelegation_NilHandlerErrors(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, _ := captureOutputs()
	source := &gws.Event{
		ID:                "src-evt",
		RecurringEventID:  "src-parent",
		OriginalStartTime: &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
	}
	c := newClassifier(t, api, inv, sink, classifyOptions{})
	err := c.Classify(context.Background(), source)
	if err == nil {
		t.Fatal("expected error when Recurring handler is nil")
	}
}

// ---------- step 3: cancelled (non-recurring) ----------

func TestClassify_Step3_Cancelled_DeleteWhenMirrorPresent(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Status = gws.EventStatusCancelled

	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-29T20:00:00Z",
		"Standup", source.Start, source.End)
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionDelete || got.Reason != ReasonSourceCancelled {
		t.Errorf("got %s/%s, want delete/source_cancelled", got.Action, got.Reason)
	}
	deletes := api.callsByOp("EventsDelete")
	if len(deletes) != 1 || deletes[0].CalendarID != "tgt-cal" || deletes[0].EventID != "mi-1" {
		t.Errorf("expected one delete on tgt-cal/mi-1; got %v", deletes)
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}); ok {
		t.Errorf("inventory should be pruned after delete")
	}
}

// TestClassify_DeleteOrSkip_AlreadyGoneCarriesOn pins B22: an
// events.delete on a mirror that's already deleted server-side returns
// HTTP 410 (api_gone) or HTTP 404 (api_not_found) per Calendar API
// semantics. Both shapes mean "the operation's intent is already
// satisfied" and must be treated as success - matching the orphan
// walker's B14 fix at internal/sync/orphan.go:358.
//
// Without this, every tick where a source-cancelled (or declined /
// tentative / transparent / outside-horizon) event maps to an
// already-deleted mirror fails the pdir, gates token advancement, and
// drives the daemon into back-to-back FullSyncs. Live-observed against
// the work-personal pair where one mirror in inventory had already been
// deleted on the target calendar (likely by a prior cascade).
//
// SPEC's deleteOrSkip semantics: if the source is no longer eligible
// AND a mirror exists in inventory, delete it. If the deletion is a
// no-op because the mirror is already gone, the inventory must still be
// pruned and the Outcome must still emit so the caller sees a clean
// pdir result.
func TestClassify_DeleteOrSkip_AlreadyGoneCarriesOn(t *testing.T) {
	// deleteOrSkip is the shared handler for SPEC steps 3-7 (source_cancelled,
	// declined, tentative, transparency_transparent, outside_horizon). The
	// fix lives in the shared function so all five paths benefit, but the
	// test exercises a representative subset of cells × both error codes
	// to pin that the carry-on truly is shared and not specific to one cell.
	cells := []struct {
		name       string
		mutateSrc  func(*gws.Event)
		wantReason mirror.Reason
	}{
		{
			name: "source_cancelled",
			mutateSrc: func(e *gws.Event) {
				e.Status = gws.EventStatusCancelled
			},
			wantReason: ReasonSourceCancelled,
		},
		{
			name: "transparency_transparent",
			mutateSrc: func(e *gws.Event) {
				e.Transparency = gws.TransparencyTransparent
			},
			wantReason: ReasonTransparencyTransparent,
		},
		{
			name: "declined",
			mutateSrc: func(e *gws.Event) {
				e.Attendees = []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusDeclined}}
			},
			wantReason: ReasonDeclined,
		},
	}
	errs := []struct {
		name string
		err  error
	}{
		{name: "410_gone", err: &gws.Error{Code: gws.CodeAPIGone, ExitCode: 1, Op: "events.delete"}},
		{name: "404_not_found", err: &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1, Op: "events.delete"}},
	}
	for _, cell := range cells {
		for _, e := range errs {
			t.Run(cell.name+"/"+e.name, func(t *testing.T) {
				api := newStubAPI()
				inv := NewInventory("tgt-cal")
				sink, captured := captureOutputs()

				source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
				cell.mutateSrc(source)

				mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
					"2026-04-29T20:00:00Z", "2026-04-29T20:00:00Z",
					"Standup", source.Start, source.End)
				tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
				inv.Set(tuple, mirrorEv)

				api.deleteErrors = append(api.deleteErrors, e.err)

				c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
				if err := c.Classify(context.Background(), source); err != nil {
					t.Fatalf("Classify must NOT fail on already-gone mirror; got %v", err)
				}
				got := firstOutcome(t, *captured)
				if got.Action != mirror.ActionDelete || got.Reason != cell.wantReason {
					t.Errorf("outcome = %s/%s, want delete/%s",
						got.Action, got.Reason, cell.wantReason)
				}
				if _, ok := inv.Lookup(tuple); ok {
					t.Errorf("inventory must be pruned even when delete returned %s", e.name)
				}
			})
		}
	}
}

func TestClassify_Step3_Cancelled_SkipWhenNoMirror(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Status = gws.EventStatusCancelled

	c := newClassifier(t, api, inv, sink, classifyOptions{})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionSkip || got.Reason != ReasonCancelled {
		t.Errorf("got %s/%s, want skip/cancelled", got.Action, got.Reason)
	}
	if len(api.calls) != 0 {
		t.Errorf("no API calls expected; got %v", api.calls)
	}
}

// ---------- steps 4 / 5 / 6: declined / tentative / transparency ----------

func TestClassify_Steps4_5_6_FilterFamily(t *testing.T) {
	type tc struct {
		name             string
		mutateSrc        func(*gws.Event)
		mirrorInInv      bool
		wantAction       mirror.Action
		wantReasonHit    mirror.Reason // when mirror is present
		wantReasonMiss   mirror.Reason // when mirror is missing
	}
	tests := []tc{
		{
			name: "declined attendee, mirror present -> delete(declined)",
			mutateSrc: func(e *gws.Event) {
				e.Attendees = []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusDeclined}}
			},
			mirrorInInv: true,
			wantAction:  mirror.ActionDelete,
			wantReasonHit: ReasonDeclined,
		},
		{
			name: "declined attendee, no mirror -> skip(declined)",
			mutateSrc: func(e *gws.Event) {
				e.Attendees = []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusDeclined}}
			},
			mirrorInInv: false,
			wantAction:  mirror.ActionSkip,
			wantReasonMiss: ReasonDeclined,
		},
		{
			name: "tentative attendee, mirror present -> delete(tentative)",
			mutateSrc: func(e *gws.Event) {
				e.Attendees = []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusTentative}}
			},
			mirrorInInv: true,
			wantAction:  mirror.ActionDelete,
			wantReasonHit: ReasonTentative,
		},
		{
			name: "tentative attendee, no mirror -> skip(tentative)",
			mutateSrc: func(e *gws.Event) {
				e.Attendees = []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusTentative}}
			},
			mirrorInInv: false,
			wantAction:  mirror.ActionSkip,
			wantReasonMiss: ReasonTentative,
		},
		{
			name: "transparent, mirror present -> delete(transparency_transparent)",
			mutateSrc: func(e *gws.Event) {
				e.Transparency = gws.TransparencyTransparent
			},
			mirrorInInv: true,
			wantAction:  mirror.ActionDelete,
			wantReasonHit: ReasonTransparencyTransparent,
		},
		{
			name: "transparent, no mirror -> skip(transparency_transparent)",
			mutateSrc: func(e *gws.Event) {
				e.Transparency = gws.TransparencyTransparent
			},
			mirrorInInv: false,
			wantAction:  mirror.ActionSkip,
			wantReasonMiss: ReasonTransparencyTransparent,
		},
	}

	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			api := newStubAPI()
			inv := NewInventory("tgt-cal")
			sink, captured := captureOutputs()

			source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
			c.mutateSrc(source)

			tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
			if c.mirrorInInv {
				mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
					"2026-04-29T20:00:00Z", "2026-04-29T20:00:00Z",
					"Standup", source.Start, source.End)
				inv.Set(tuple, mirrorEv)
			}

			cl := newClassifier(t, api, inv, sink, classifyOptions{})
			if err := cl.Classify(context.Background(), source); err != nil {
				t.Fatalf("Classify error: %v", err)
			}
			got := firstOutcome(t, *captured)
			if got.Action != c.wantAction {
				t.Errorf("Action = %s, want %s", got.Action, c.wantAction)
			}
			if c.mirrorInInv {
				if got.Reason != c.wantReasonHit {
					t.Errorf("Reason = %s, want %s", got.Reason, c.wantReasonHit)
				}
				deletes := api.callsByOp("EventsDelete")
				if len(deletes) != 1 {
					t.Errorf("expected one delete; got %d", len(deletes))
				}
				if _, ok := inv.Lookup(tuple); ok {
					t.Errorf("inventory should be pruned after delete")
				}
			} else {
				if got.Reason != c.wantReasonMiss {
					t.Errorf("Reason = %s, want %s", got.Reason, c.wantReasonMiss)
				}
				if len(api.calls) != 0 {
					t.Errorf("no API calls expected; got %v", api.calls)
				}
			}
		})
	}
}

// ---------- step 7: outside horizon ----------

func TestClassify_Step7_NonRecurring_StartPastHorizon_DeleteOrSkip(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	now := must2026()
	// horizon 30d -> events more than 30d out are outside.
	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-12-01T12:00:00Z"})

	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-29T20:00:00Z",
		"Standup", source.Start, source.End)
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	c := newClassifier(t, api, inv, sink, classifyOptions{
		horizon: 30 * 24 * time.Hour,
		now:     now,
	})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionDelete || got.Reason != ReasonOutsideHorizon {
		t.Errorf("got %s/%s, want delete/outside_horizon", got.Action, got.Reason)
	}
	if len(api.callsByOp("EventsDelete")) != 1 {
		t.Errorf("expected delete call")
	}
}

func TestClassify_Step7_NonRecurring_NoMirror_Skip(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-12-01T12:00:00Z"})

	c := newClassifier(t, api, inv, sink, classifyOptions{
		horizon: 30 * 24 * time.Hour,
		now:     must2026(),
	})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionSkip || got.Reason != ReasonOutsideHorizon {
		t.Errorf("got %s/%s, want skip/outside_horizon", got.Action, got.Reason)
	}
}

func TestClassify_Step7_RecurringParent_HasInstanceInWindow_FallsThrough(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-12-01T12:00:00Z"})
	source.Recurrence = []string{"RRULE:FREQ=WEEKLY"}

	// events.instances returns one materialized instance -> in horizon.
	api.queueInstances([]gws.Event{{ID: "any-instance"}})

	// Expect a successful insert path because there's no mirror in inventory.
	insertResp := makeCleanCurrentMirror(mirror.DeterministicID("src-cal", "src-evt"),
		"src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T08:00:00Z",
		source.Summary, source.Start, source.End,
	)
	api.queueInsert(insertResp)
	api.queuePatch(insertResp) // checksum followup

	now := must2026()
	horizon := 30 * 24 * time.Hour
	c := newClassifier(t, api, inv, sink, classifyOptions{
		horizon:        horizon,
		now:            now,
		sourceWritable: true,
	})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	calls := api.callsByOp("EventsInstances")
	if len(calls) != 1 {
		t.Fatalf("expected one EventsInstances call for horizon check; got %d", len(calls))
	}
	// The horizon check sends the [now, now+horizon] window per SPEC step 7.
	p := calls[0].InstanceParams
	if got, want := p.TimeMin, now.Format(time.RFC3339); got != want {
		t.Errorf("TimeMin = %q, want %q", got, want)
	}
	if got, want := p.TimeMax, now.Add(horizon).Format(time.RFC3339); got != want {
		t.Errorf("TimeMax = %q, want %q", got, want)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionInsert {
		t.Errorf("expected insert outcome (horizon passed); got %s/%s", got.Action, got.Reason)
	}
}

func TestClassify_Step7_RecurringParent_NoInstanceInWindow_OutsideHorizon(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Recurrence = []string{"RRULE:FREQ=WEEKLY"}
	api.queueInstances(nil) // empty -> outside horizon

	now := must2026()
	horizon := 30 * 24 * time.Hour
	c := newClassifier(t, api, inv, sink, classifyOptions{
		horizon: horizon,
		now:     now,
	})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionSkip || got.Reason != ReasonOutsideHorizon {
		t.Errorf("got %s/%s, want skip/outside_horizon", got.Action, got.Reason)
	}
	// The instances request should carry the [now, horizonEnd] window with
	// MaxResults=1 and showDeleted=false per SPEC.
	calls := api.callsByOp("EventsInstances")
	if len(calls) != 1 {
		t.Fatalf("expected one EventsInstances call; got %d", len(calls))
	}
	p := calls[0].InstanceParams
	if p.MaxResults != 1 {
		t.Errorf("MaxResults = %d, want 1", p.MaxResults)
	}
	if p.ShowDeleted {
		t.Errorf("ShowDeleted should be false")
	}
	if got, want := p.TimeMin, now.Format(time.RFC3339); got != want {
		t.Errorf("TimeMin = %q, want %q", got, want)
	}
	if got, want := p.TimeMax, now.Add(horizon).Format(time.RFC3339); got != want {
		t.Errorf("TimeMax = %q, want %q", got, want)
	}
}

// ---------- step 8: inventory miss / hit ----------

func TestClassify_Step8_InventoryMiss_Insert(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	deterministic := mirror.DeterministicID("src-cal", "src-evt")
	insertedMirror := makeCleanCurrentMirror(deterministic, "src-cal:src-evt",
		source.Updated, "2026-04-30T08:00:00Z",
		source.Summary, source.Start, source.End,
	)
	api.queueInsert(insertedMirror)
	api.queuePatch(insertedMirror) // checksum followup

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionInsert || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("got %s/%s, want insert/source_updated", got.Action, got.Reason)
	}
	if got.TargetEventID != deterministic {
		t.Errorf("TargetEventID = %q, want %q", got.TargetEventID, deterministic)
	}
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
	if got2, ok := inv.Lookup(tuple); !ok || got2.ID != deterministic {
		t.Errorf("inventory not updated after insert")
	}

	// The first patch (the checksum followup) must touch only the checksum
	// extended property; managed fields untouched.
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 1 {
		t.Fatalf("expected 1 patch (checksum followup); got %d", len(patches))
	}
	body := patches[0].Body
	if body == nil || body.ExtendedProperties == nil {
		t.Fatal("checksum patch body missing")
	}
	if body.ExtendedProperties.Private[mirror.ExtKeyChecksum] == "" {
		t.Errorf("checksum patch body missing %s", mirror.ExtKeyChecksum)
	}
	if body.Summary != "" {
		t.Errorf("checksum patch must not carry managed fields; Summary=%q", body.Summary)
	}
}

// TestClassify_Step8_StaleBookkeeping_PatchesFromSource pins B23: a
// mirror whose stored bookkeeping reports both signals clean (source
// timestamp matches stored, checksum matches the mirror's current
// managed fields) but whose actual managed fields disagree with the
// source's must NOT silently skip. The new FieldsDisagree signal routes
// this to ActionPatch / ReasonStaleBookkeeping. The patch rewrites the
// mirror from source and runs the standard checksum follow-up.
//
// Concrete prior-art shape: 5/11 Lunch & Reading mirror sat at start=11:00
// while source's instance override was at 11:30. Both daemon-stored
// signals reported clean (because a prior managed-field-no-op write
// recorded the latest source.Updated alongside the existing
// 11:00-aligned checksum). Pre-B23 the daemon emitted skip(unchanged)
// every tick. Post-B23 the new cell catches the divergence.
func TestClassify_Step8_StaleBookkeeping_PatchesFromSource(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	// Mirror has DIFFERENT start time than source (the divergence) but
	// stored bookkeeping says clean. Build a mirror with start at 11:00
	// while source is at 12:00, then pin the stored checksum to the
	// mirror's current managed fields (so MirrorDrifted=false) and
	// stored source_updated == source.Updated (so SourceChanged=false).
	mirrorStart := &gws.EventDateTime{DateTime: "2026-05-01T11:00:00Z"}
	mirrorEnd := &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"}
	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		source.Updated, source.Updated,
		source.Summary, mirrorStart, mirrorEnd)
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
	inv.Set(tuple, mirrorEv)

	// Two EventsPatch calls expected: main (full payload from source) +
	// checksum follow-up.
	postMain := *mirrorEv
	postMain.Start = source.Start
	postMain.End = source.End
	postMain.Updated = "2026-04-29T20:00:01Z"
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
		t.Errorf("stale-bookkeeping cell must not emit a conflict; got %q", got.Conflict)
	}

	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 EventsPatch (main + checksum); got %d", len(patches))
	}
	// Inventory should hold the post-write resource.
	updated, ok := inv.Lookup(tuple)
	if !ok {
		t.Fatal("inventory must still hold the mirror after stale-bookkeeping patch")
	}
	if updated.Start == nil || updated.Start.DateTime != source.Start.DateTime {
		t.Errorf("inventory mirror's start should now match source; got %+v", updated.Start)
	}
}

// TestClassify_Step8_CancelledMirrorWithSyncableSource_Revives pins B20
// for the non-recurring path. By the time reconcileNormal runs (step 8),
// the source has passed steps 3-7 - it's not cancelled, declined,
// tentative, transparent, or outside-horizon. If the mirror in inventory
// is nonetheless at Status=cancelled with managed fields matching the
// stored checksum, the existing four-way drift matrix returns
// skip(unchanged) because Status isn't a managed field. The mirror stays
// cancelled forever.
//
// Symmetric with insert.go's reviveCancelledMirror (the post-409 path).
// Outcome: ActionInsert + ReasonSourceUpdated. Two patches: main (status=
// confirmed + full payload) and checksum follow-up.
func TestClassify_Step8_CancelledMirrorWithSyncableSource_Revives(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	// Mirror with managed fields matching source (so checksum still matches)
	// but status=cancelled - the stuck state that drift detection misses.
	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		source.Updated, source.Updated,
		source.Summary, source.Start, source.End)
	mirrorEv.Status = gws.EventStatusCancelled
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
	inv.Set(tuple, mirrorEv)

	// Two EventsPatch calls expected: main (full payload + status=confirmed) +
	// checksum follow-up.
	postMain := *mirrorEv
	postMain.Status = gws.EventStatusConfirmed
	postMain.Updated = "2026-04-29T20:00:01Z"
	api.queuePatch(&postMain)
	postChecksum := postMain
	api.queuePatch(&postChecksum)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify returned error: %v", err)
	}

	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionInsert || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("revive outcome = %s/%s, want insert/source_updated", got.Action, got.Reason)
	}

	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 EventsPatch (main + checksum); got %d", len(patches))
	}
	if patches[0].Body == nil || patches[0].Body.Status != gws.EventStatusConfirmed {
		t.Errorf("main patch must include status=confirmed; got %+v", patches[0].Body)
	}
	if patches[0].Body == nil || patches[0].Body.Summary == "" {
		t.Errorf("main patch must carry full managed-field payload; got %+v", patches[0].Body)
	}
	// Inventory should hold the post-write (confirmed) mirror, not the cancelled stub.
	updated, ok := inv.Lookup(tuple)
	if !ok {
		t.Fatal("inventory must still hold the mirror after revive")
	}
	if updated.Status == gws.EventStatusCancelled {
		t.Errorf("inventory mirror must be confirmed after revive; got cancelled")
	}
}

// TestClassify_Step8_CancelledLegacyMirror_RevivesAtCurrentSchema pins
// the interaction between B20's revive cell and the schema-version-
// migration path: a v1 mirror that's been cancelled is revived directly
// via reviveCancelledMirror rather than routing through reconcileMigration.
// The revive uses BuildPayload which writes the current SchemaVersion,
// so the migration is implicit in the rewrite. Important boundary because
// reconcileNormal's revive check is intentionally placed BEFORE the
// NeedsMigration branch.
func TestClassify_Step8_CancelledLegacyMirror_RevivesAtCurrentSchema(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	// v1 mirror (no checksum), status=cancelled.
	mirrorEv := makeV1Mirror("mi-1", "src-cal:src-evt",
		source.Updated, source.Updated,
		source.Summary, source.Start, source.End)
	mirrorEv.Status = gws.EventStatusCancelled
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
	inv.Set(tuple, mirrorEv)

	// Revive expects 2 EventsPatch calls (main + checksum).
	postMain := *mirrorEv
	postMain.Status = gws.EventStatusConfirmed
	if postMain.ExtendedProperties != nil && postMain.ExtendedProperties.Private != nil {
		postMain.ExtendedProperties.Private[mirror.ExtKeyVersion] = mirror.SchemaVersion
	}
	api.queuePatch(&postMain)
	postChecksum := postMain
	api.queuePatch(&postChecksum)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}

	got := firstOutcome(t, *captured)
	// The revive cell wins over migration: outcome is insert/source_updated,
	// not patch/migration_upgrade. The fresh write at the current schema
	// version is what the migration path would have done anyway, but the
	// revive's reason better reflects user intent ("mirror is back").
	if got.Action != mirror.ActionInsert || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("revive outcome = %s/%s, want insert/source_updated", got.Action, got.Reason)
	}
}

func TestClassify_Step8_InventoryHit_Unchanged_Skip(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		source.Updated, "2026-04-29T20:00:00Z",
		source.Summary, source.Start, source.End)
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionSkip || got.Reason != mirror.ReasonUnchanged {
		t.Errorf("got %s/%s, want skip/unchanged", got.Action, got.Reason)
	}
	if len(api.calls) != 0 {
		t.Errorf("expected no API calls on unchanged path; got %v", api.calls)
	}
}

func TestClassify_Step8_InventoryHit_SourceChanged_Patch(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-30T11:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Summary = "Updated"

	// stored source_updated older -> SourceChanged=true. Mirror managed fields
	// match its stored checksum -> MirrorDrifted=false.
	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-29T20:00:00Z",
		"Standup", source.Start, source.End)
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	postMain := *mirrorEv
	postMain.Summary = "Updated"
	postMain.Updated = "2026-04-30T11:00:01Z"
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("got %s/%s, want patch/source_updated", got.Action, got.Reason)
	}
	// Non-conflict outcome: timestamps must stay empty so the daemon's
	// warn-log emitter doesn't accidentally surface them.
	if got.SourceUpdated != "" || got.MirrorUpdated != "" {
		t.Errorf("non-conflict outcome should have empty timestamps; got src=%q mir=%q", got.SourceUpdated, got.MirrorUpdated)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches (main + checksum); got %d", len(patches))
	}
	for i, p := range patches {
		if p.CalendarID != "tgt-cal" || p.EventID != "mi-1" {
			t.Errorf("patches[%d] on %s/%s, want tgt-cal/mi-1", i, p.CalendarID, p.EventID)
		}
	}
}

func TestClassify_Step8_InventoryHit_MirrorDriftedOnly_Propagate(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	// Build a clean mirror, then drift the live summary so the stored checksum
	// no longer matches.
	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		source.Updated, "2026-04-30T08:00:00Z",
		source.Summary, source.Start, source.End)
	mirrorEv.Summary = "User edit"
	mirrorEv.Description = "User edit\n\n---\nSource: " + source.HTMLLink
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	// First patch is on the SOURCE with drifted fields; then mirror main + checksum.
	postSrc := *source
	postSrc.Summary = "User edit"
	postSrc.Updated = "2026-04-30T09:00:00Z"
	api.queuePatch(&postSrc)

	postMirror := *mirrorEv
	postMirror.Summary = "User edit"
	api.queuePatch(&postMirror)
	api.queuePatch(&postMirror)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPropagate || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("got %s/%s, want propagate/target_edited", got.Action, got.Reason)
	}
	if !contains(got.Fields, "summary") {
		t.Errorf("Fields should include summary; got %v", got.Fields)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 3 {
		t.Fatalf("expected 3 patches (src + mirror main + mirror checksum); got %d", len(patches))
	}
	if patches[0].CalendarID != "src-cal" || patches[0].EventID != "src-evt" {
		t.Errorf("first patch should be on the source; got %s/%s", patches[0].CalendarID, patches[0].EventID)
	}
	for i := 1; i < 3; i++ {
		if patches[i].CalendarID != "tgt-cal" || patches[i].EventID != "mi-1" {
			t.Errorf("patches[%d] should target the mirror; got %s/%s", i, patches[i].CalendarID, patches[i].EventID)
		}
	}
}

func TestClassify_Step8_InventoryHit_MirrorDriftedOnly_Revert(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		source.Updated, "2026-04-30T08:00:00Z",
		source.Summary, source.Start, source.End)
	mirrorEv.Summary = "User edit"
	mirrorEv.Description = "User edit\n\n---\nSource: " + source.HTMLLink
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	api.queuePatch(mirrorEv)
	api.queuePatch(mirrorEv)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: false})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionRevert || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("got %s/%s, want revert/target_edited", got.Action, got.Reason)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 mirror-side patches on revert; got %d", len(patches))
	}
	for _, p := range patches {
		if p.CalendarID != "tgt-cal" || p.EventID != "mi-1" {
			t.Errorf("revert must target the mirror; got %s/%s", p.CalendarID, p.EventID)
		}
	}
}

func TestClassify_Step8_InventoryHit_BothChanged_SourceNewer(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-30T11:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Summary = "Source new"

	// Mirror's stored source_updated older AND live fields differ. Mirror's
	// own Updated older than source -> source-newer wins.
	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T10:00:00Z",
		"Standup", source.Start, source.End)
	mirrorEv.Summary = "User edit"
	mirrorEv.Description = "User edit\n\n---\nSource: " + source.HTMLLink
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	postMain := *mirrorEv
	postMain.Summary = "Source new"
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("got %s/%s, want patch/source_updated", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictSourceWon {
		t.Errorf("Conflict = %q, want conflict_source_won", got.Conflict)
	}
	// Conflict timestamps populated for v2 conflict cells (per SPEC §"Conflict logging").
	if got.SourceUpdated != source.Updated {
		t.Errorf("SourceUpdated = %q, want %q", got.SourceUpdated, source.Updated)
	}
	if got.MirrorUpdated != mirrorEv.Updated {
		t.Errorf("MirrorUpdated = %q, want %q", got.MirrorUpdated, mirrorEv.Updated)
	}
}

func TestClassify_Step8_InventoryHit_BothChanged_MirrorNewer_Propagate(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-30T09:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Summary = "Source new"

	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T10:00:00Z", // mirror Updated NEWER
		"Standup", source.Start, source.End)
	mirrorEv.Summary = "User edit"
	mirrorEv.Description = "User edit\n\n---\nSource: " + source.HTMLLink
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	postSrc := *source
	postSrc.Summary = "User edit"
	postSrc.Updated = "2026-04-30T11:00:00Z"
	api.queuePatch(&postSrc)
	postMirror := *mirrorEv
	api.queuePatch(&postMirror)
	api.queuePatch(&postMirror)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPropagate || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("got %s/%s, want propagate/target_edited", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictTargetWon {
		t.Errorf("Conflict = %q, want conflict_target_won", got.Conflict)
	}
	// Conflict timestamps populated for v2 conflict cells (per SPEC §"Conflict logging").
	if got.SourceUpdated != source.Updated {
		t.Errorf("SourceUpdated = %q, want %q", got.SourceUpdated, source.Updated)
	}
	if got.MirrorUpdated != mirrorEv.Updated {
		t.Errorf("MirrorUpdated = %q, want %q", got.MirrorUpdated, mirrorEv.Updated)
	}
}

func TestClassify_Step8_InventoryHit_BothChanged_MirrorNewer_Revert(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-30T09:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Summary = "Source new"

	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T10:00:00Z",
		"Standup", source.Start, source.End)
	mirrorEv.Summary = "User edit"
	mirrorEv.Description = "User edit\n\n---\nSource: " + source.HTMLLink
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	api.queuePatch(mirrorEv)
	api.queuePatch(mirrorEv)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: false})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionRevert || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("got %s/%s, want revert/target_edited", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictTargetWon {
		t.Errorf("Conflict = %q, want conflict_target_won", got.Conflict)
	}
	// Conflict timestamps populated for v2 conflict cells (per SPEC §"Conflict logging").
	if got.SourceUpdated != source.Updated {
		t.Errorf("SourceUpdated = %q, want %q", got.SourceUpdated, source.Updated)
	}
	if got.MirrorUpdated != mirrorEv.Updated {
		t.Errorf("MirrorUpdated = %q, want %q", got.MirrorUpdated, mirrorEv.Updated)
	}
}

// ---------- v1 migration cells ----------

func TestClassify_Step8_V1Mirror_NoActualDrift_MigrationUpgrade(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	// v1 mirror whose live managed fields match BuildPayload(source). Hand-build
	// rather than going through makeV1Mirror so the description exactly matches
	// what the migration check produces (BuildPayload treats source.Description
	// as empty in this fixture, so the mirror's description is just the
	// trailer with no summary prefix).
	desired := mirror.BuildPayload("src-cal", source)
	mirrorEv := &gws.Event{
		ID:           "mi-1",
		Status:       gws.EventStatusConfirmed,
		Summary:      desired.Summary,
		Description:  desired.Description,
		Start:        desired.Start,
		End:          desired.End,
		Transparency: desired.Transparency,
		Visibility:   desired.Visibility,
		Updated:      source.Updated,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:        "src-cal:src-evt",
				mirror.ExtKeySourceUpdated: source.Updated,
				mirror.ExtKeyVersion:       "1",
				// no checksum for v1
			},
		},
	}
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	postMain := *mirrorEv
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != ReasonMigrationUpgrade {
		t.Errorf("got %s/%s, want patch/migration_upgrade", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictNone {
		t.Errorf("migration_upgrade should have no conflict; got %q", got.Conflict)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches (main + checksum); got %d", len(patches))
	}
	for _, p := range patches {
		if p.CalendarID != "tgt-cal" || p.EventID != "mi-1" {
			t.Errorf("patches must target the mirror; got %s/%s", p.CalendarID, p.EventID)
		}
	}
	// Migration upgrade must NOT touch the source.
	for _, c := range api.calls {
		if c.Op == "EventsPatch" && c.CalendarID == "src-cal" {
			t.Errorf("migration_upgrade must not patch source; got %v", c)
		}
	}
	// Main patch body carries version=2.
	body := patches[0].Body
	if body == nil || body.ExtendedProperties == nil {
		t.Fatal("main patch body missing ExtendedProperties")
	}
	if v := body.ExtendedProperties.Private[mirror.ExtKeyVersion]; v != mirror.SchemaVersion {
		t.Errorf("main patch body version = %q, want %q", v, mirror.SchemaVersion)
	}
}

// TestClassify_Step8_V1Mirror_FieldsDisagreeOnly_MigrationSourceWonNotStaleBookkeeping
// pins the contract that the v1 migration path consumes its own drift
// logic, not B23's stale_bookkeeping cell. A v1 mirror with SC=F and
// FieldsDisagree=true must route through reconcileMigration's
// MirrorDrifted-override branch and emit migration_source_won, NOT the
// new stale_bookkeeping reason.
//
// Why this contract matters: ComputeDriftSignal computes FieldsDisagree
// independently from MirrorDrifted. reconcileMigration overrides
// MirrorDrifted via DriftedFieldNames (which has the same answer as
// FieldsDisagree). After the override the migration cells handle their
// own routing inline; they never call mirror.Classify with the
// possibly-FD-true signal. A future refactor that re-orders the
// migration switch or accidentally drops the override would let
// FieldsDisagree leak through to Classify and the new stale_bookkeeping
// cell would fire, masking what's actually a migration scenario. This
// test locks the contract.
func TestClassify_Step8_V1Mirror_FieldsDisagreeOnly_MigrationSourceWonNotStaleBookkeeping(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	// Stored source_updated == source.Updated -> SourceChanged=false.
	// Live mirror managed fields differ from desired-from-source ->
	// FieldsDisagree=true at ComputeDriftSignal. v1 mirror -> NeedsMigration=true.
	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	mirrorEv := makeV1Mirror("mi-1", "src-cal:src-evt",
		source.Updated, source.Updated,
		"User edit", source.Start, source.End)
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	postMain := *mirrorEv
	postMain.Summary = source.Summary
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Reason == mirror.ReasonStaleBookkeeping {
		t.Fatalf("v1 migration path must not route to stale_bookkeeping; got %s/%s", got.Action, got.Reason)
	}
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("got %s/%s, want patch/source_updated", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictMigrationSourceWon {
		t.Errorf("Conflict = %q, want migration_source_won", got.Conflict)
	}
}

func TestClassify_Step8_V1Mirror_BothChanged_MigrationSourceWon(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	// stored source_updated older than source.Updated -> SourceChanged=true.
	// Live fields differ from desired-from-source -> MirrorDrifted=true.
	source := makeNonRecurringSource("src-evt", "2026-04-30T11:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Summary = "Source new"

	// Mirror's live Updated NEWER than source -> would lose under newer-wins,
	// but the migration cell is source-wins-by-default so it must still patch.
	mirrorEv := makeV1Mirror("mi-1", "src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-30T15:00:00Z",
		"User edit", source.Start, source.End)
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	postMain := *mirrorEv
	postMain.Summary = source.Summary
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("got %s/%s, want patch/source_updated", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictMigrationSourceWon {
		t.Errorf("Conflict = %q, want migration_source_won", got.Conflict)
	}
	// SPEC line 500: timestamps are omitted from the migration_source_won
	// warn log because v1 mirrors have no comparable user-edit timestamp.
	if got.SourceUpdated != "" {
		t.Errorf("SourceUpdated should be empty on migration_source_won; got %q", got.SourceUpdated)
	}
	if got.MirrorUpdated != "" {
		t.Errorf("MirrorUpdated should be empty on migration_source_won; got %q", got.MirrorUpdated)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 mirror patches; got %d", len(patches))
	}
	for _, c := range api.calls {
		if c.Op == "EventsPatch" && c.CalendarID == "src-cal" {
			t.Errorf("migration_source_won must not propagate to source; got %v", c)
		}
	}
}

// TestClassify_Step8_V2Mirror_NeedsMigration_PatchesAsMigrationUpgrade pins
// the v2 -> v3 migration path. With SchemaVersion bumped to "3", a mirror
// stored at version="2" reports NeedsMigration=true via ComputeDriftSignal,
// even though it carries a real checksum. The migration cell with no actual
// drift (live managed fields match desired) routes to migration_upgrade
// (rewrite mirror with version=3 + fresh checksum), NOT skip(unchanged).
//
// Builds the v2 mirror manually with version="2" rather than mirror.SchemaVersion
// so this test exercises the v2 -> v3 cell specifically; if SchemaVersion
// is bumped again this fixture stays version="2" and continues to pin the
// migration path for legacy mirrors.
func TestClassify_Step8_V2Mirror_NeedsMigration_PatchesAsMigrationUpgrade(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	desired := mirror.BuildPayload("src-cal", source)
	mirrorEv := &gws.Event{
		ID:           "mi-1",
		Status:       gws.EventStatusConfirmed,
		Summary:      desired.Summary,
		Description:  desired.Description,
		Start:        desired.Start,
		End:          desired.End,
		Transparency: desired.Transparency,
		Visibility:   desired.Visibility,
		Updated:      source.Updated,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:        "src-cal:src-evt",
				mirror.ExtKeySourceUpdated: source.Updated,
				mirror.ExtKeyVersion:       "2",
				// A v2 mirror DOES have a checksum, but it was computed over
				// the v2 ManagedFields (no Location). The checksum here is
				// arbitrary; ComputeDriftSignal sees version != SchemaVersion
				// and routes to the migration path before consulting the
				// stored checksum.
				mirror.ExtKeyChecksum: "sha256:legacy-v2-checksum",
			},
		},
	}
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	postMain := *mirrorEv
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != ReasonMigrationUpgrade {
		t.Errorf("got %s/%s, want patch/migration_upgrade", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictNone {
		t.Errorf("migration_upgrade should have no conflict; got %q", got.Conflict)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches (main + checksum); got %d", len(patches))
	}
	// Main patch body must carry the new SchemaVersion ("3") in the
	// extended properties - that's the whole point of the upgrade.
	body := patches[0].Body
	if body == nil || body.ExtendedProperties == nil {
		t.Fatal("main patch body missing ExtendedProperties")
	}
	if v := body.ExtendedProperties.Private[mirror.ExtKeyVersion]; v != mirror.SchemaVersion {
		t.Errorf("main patch body version = %q, want %q", v, mirror.SchemaVersion)
	}
	// Migration upgrade must NOT touch the source.
	for _, c := range api.calls {
		if c.Op == "EventsPatch" && c.CalendarID == "src-cal" {
			t.Errorf("migration_upgrade must not patch source; got %v", c)
		}
	}
}

// TestClassify_Step8_V2Mirror_EmptyTransparencyDoesNotPropagate pins the
// fix for the migration drift recompute. Google's events.list omits
// transparency when its value equals the default ("opaque"); a v2 mirror
// whose Transparency comes back empty must NOT count as drifted just because
// the desired-from-source payload writes "opaque" explicitly. A raw Checksum
// comparison would say drifted; DriftedFieldNames normalizes the default
// and reports zero drifted fields. The migration cell must therefore route
// to migration_upgrade, not propagate.
func TestClassify_Step8_V2Mirror_EmptyTransparencyDoesNotPropagate(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	desired := mirror.BuildPayload("src-cal", source)
	mirrorEv := &gws.Event{
		ID:          "mi-1",
		Status:      gws.EventStatusConfirmed,
		Summary:     desired.Summary,
		Description: desired.Description,
		Start:       desired.Start,
		End:         desired.End,
		// Transparency intentionally empty: Google's API omits "opaque"
		// from list responses. With the buggy raw-Checksum recompute this
		// hashed differently from desired's explicit "opaque" and flipped
		// MirrorDrifted=true.
		Transparency: "",
		Visibility:   desired.Visibility,
		Updated:      source.Updated,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:        "src-cal:src-evt",
				mirror.ExtKeySourceUpdated: source.Updated,
				mirror.ExtKeyVersion:       "2",
				// v2 checksum was computed over the v2 ManagedFields
				// (no Location). NeedsMigration fires regardless.
				mirror.ExtKeyChecksum: "sha256:legacy-v2-checksum",
			},
		},
	}
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	// Queue three patch responses so the buggy propagate path
	// (EventsPatch source + EventsPatch mirror + checksum follow-up) and
	// the fixed migration path (EventsPatch mirror + checksum follow-up)
	// both complete without queue-exhaustion noise; the assertions below
	// are the actual red/green signal.
	postMain := *mirrorEv
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != ReasonMigrationUpgrade {
		t.Errorf("got %s/%s, want patch/migration_upgrade (empty transparency must normalize to opaque)", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictNone {
		t.Errorf("migration_upgrade should have no conflict; got %q", got.Conflict)
	}
	// Migration upgrade must NOT touch the source. The bug routed this
	// case to propagate, which would patch source-side fields.
	for _, c := range api.calls {
		if c.Op == "EventsPatch" && c.CalendarID == "src-cal" {
			t.Errorf("migration_upgrade must not patch source; got %v", c)
		}
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches (main + checksum); got %d", len(patches))
	}
	body := patches[0].Body
	if body == nil || body.ExtendedProperties == nil {
		t.Fatal("main patch body missing ExtendedProperties")
	}
	if v := body.ExtendedProperties.Private[mirror.ExtKeyVersion]; v != mirror.SchemaVersion {
		t.Errorf("main patch body version = %q, want %q", v, mirror.SchemaVersion)
	}
}

// TestClassify_Step8_V2Mirror_LocationDriftRoutesToMigrationSourceWon pins
// the data-loss bug fix for the v2 -> v3 migration. v2 mirrors lack the
// `location` managed field; when the source has a location and the v2 mirror
// has empty Location, DriftedFieldNames reports MirrorDrifted=true. The
// previous code fell through to the standard Classify, which routed
// !source_changed && mirror_drifted to propagate (with sourceWritable=true),
// patching SOURCE with the mirror's empty location and clobbering source's
// real value. The fix routes any MirrorDrifted=true during migration to
// migration_source_won so the source is never written to.
func TestClassify_Step8_V2Mirror_LocationDriftRoutesToMigrationSourceWon(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Location = "Conference Room A / video: https://meet.example.com/xyz"

	// v2 mirror with empty Location (the v2 schema didn't manage it).
	// Live managed fields for everything ELSE match desired-from-source, so
	// the only drift is the mirror's missing location.
	desired := mirror.BuildPayload("src-cal", source)
	mirrorEv := &gws.Event{
		ID:           "mi-1",
		Status:       gws.EventStatusConfirmed,
		Summary:      desired.Summary,
		Description:  desired.Description,
		Start:        desired.Start,
		End:          desired.End,
		Location:     "", // v2 mirror has no location
		Transparency: desired.Transparency,
		Visibility:   desired.Visibility,
		Updated:      source.Updated,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:        "src-cal:src-evt",
				mirror.ExtKeySourceUpdated: source.Updated,
				mirror.ExtKeyVersion:       "2",
				mirror.ExtKeyChecksum:      "sha256:legacy-v2-checksum",
			},
		},
	}
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	// Queue three patch responses so the buggy propagate path
	// (EventsPatch source + EventsPatch mirror + checksum follow-up) and
	// the fixed migration path (EventsPatch mirror + checksum follow-up)
	// both complete without queue-exhaustion noise; the assertions below
	// are the actual red/green signal.
	postMain := *mirrorEv
	postMain.Location = source.Location
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("got %s/%s, want patch/source_updated (must NOT propagate empty location to source)", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictMigrationSourceWon {
		t.Errorf("Conflict = %q, want migration_source_won", got.Conflict)
	}
	// Critical assertion: NO source-side EventsPatch (no clobbering source's
	// location with the mirror's empty value).
	for _, c := range api.calls {
		if c.Op == "EventsPatch" && c.CalendarID == "src-cal" {
			t.Errorf("migration_source_won must not patch source; got call %+v", c)
		}
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 EventsPatch (mirror main + checksum); got %d", len(patches))
	}
	for i, p := range patches {
		if p.CalendarID != "tgt-cal" || p.EventID != "mi-1" {
			t.Errorf("patches[%d] should target the mirror; got %s/%s", i, p.CalendarID, p.EventID)
		}
	}
	// Main patch body must carry the new SchemaVersion ("3") and source's
	// location - the mirror is rewritten with v3 schema + source's content.
	body := patches[0].Body
	if body == nil || body.ExtendedProperties == nil {
		t.Fatal("main patch body missing ExtendedProperties")
	}
	if v := body.ExtendedProperties.Private[mirror.ExtKeyVersion]; v != mirror.SchemaVersion {
		t.Errorf("main patch body version = %q, want %q", v, mirror.SchemaVersion)
	}
	if body.Location != source.Location {
		t.Errorf("main patch body Location = %q, want %q (source's location)", body.Location, source.Location)
	}
}

// TestClassify_Step8_V2Mirror_RealUserEditOnSummaryRoutesToMigrationSourceWon
// pins that ANY mirror drift during migration routes to migration_source_won,
// even when it's a genuine user edit (not just a schema-induced diff). The
// conservative trade-off matches v1 migration: the mirror's pre-migration
// edits are overwritten by source. We can't reliably distinguish schema-
// induced from user-edit drift during migration, so source always wins.
func TestClassify_Step8_V2Mirror_RealUserEditOnSummaryRoutesToMigrationSourceWon(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	// v2 mirror with a real user edit on summary. Source unchanged
	// (storedSourceUpdated == source.Updated -> SourceChanged=false).
	desired := mirror.BuildPayload("src-cal", source)
	mirrorEv := &gws.Event{
		ID:           "mi-1",
		Status:       gws.EventStatusConfirmed,
		Summary:      "User edit", // genuine user edit
		Description:  "User edit\n\n---\nSource: " + source.HTMLLink,
		Start:        desired.Start,
		End:          desired.End,
		Transparency: desired.Transparency,
		Visibility:   desired.Visibility,
		Updated:      "2026-04-30T08:00:00Z", // mirror Updated newer than source
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:        "src-cal:src-evt",
				mirror.ExtKeySourceUpdated: source.Updated,
				mirror.ExtKeyVersion:       "2",
				mirror.ExtKeyChecksum:      "sha256:legacy-v2-checksum",
			},
		},
	}
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	// Queue three patch responses (see comment in
	// TestClassify_Step8_V2Mirror_LocationDriftRoutesToMigrationSourceWon).
	postMain := *mirrorEv
	postMain.Summary = source.Summary
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("got %s/%s, want patch/source_updated", got.Action, got.Reason)
	}
	if got.Conflict != mirror.ConflictMigrationSourceWon {
		t.Errorf("Conflict = %q, want migration_source_won", got.Conflict)
	}
	// Critical: NO source-side patch. Even a real user edit on summary
	// during migration is overwritten by source rather than propagated.
	for _, c := range api.calls {
		if c.Op == "EventsPatch" && c.CalendarID == "src-cal" {
			t.Errorf("migration_source_won must not patch source; got call %+v", c)
		}
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 EventsPatch (mirror main + checksum); got %d", len(patches))
	}
	// Main patch body must overwrite the mirror's summary with source's.
	body := patches[0].Body
	if body == nil {
		t.Fatal("main patch body nil")
	}
	if body.Summary != source.Summary {
		t.Errorf("main patch body Summary = %q, want %q (mirror's user edit must be overwritten)", body.Summary, source.Summary)
	}
}

// TestClassify_Step8_V2Mirror_SourceChangedNoMirrorDriftStillPatchesNormally
// pins that the source_changed && !mirror_drifted cell still falls through
// to the standard Classify path during migration - it should patch from
// source as a normal source_updated, no migration_source_won conflict.
// This is the only cell that doesn't divert to a migration-specific outcome.
func TestClassify_Step8_V2Mirror_SourceChangedNoMirrorDriftStillPatchesNormally(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-30T11:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Summary = "Source new"

	// v2 mirror with stored source_updated OLDER than source.Updated
	// -> SourceChanged=true. Live fields match desired-from-source EXCEPT
	// for the new source.Summary - so we have to drift the mirror's summary
	// AWAY from source's new value while still keeping the no-drift signal.
	// Trick: set mirror's summary == source.Summary (the new one) so
	// DriftedFieldNames(mirrorEv, desired) reports zero drifted fields.
	desired := mirror.BuildPayload("src-cal", source)
	mirrorEv := &gws.Event{
		ID:           "mi-1",
		Status:       gws.EventStatusConfirmed,
		Summary:      desired.Summary,
		Description:  desired.Description,
		Start:        desired.Start,
		End:          desired.End,
		Transparency: desired.Transparency,
		Visibility:   desired.Visibility,
		Updated:      "2026-04-30T10:00:00Z",
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:        "src-cal:src-evt",
				mirror.ExtKeySourceUpdated: "2026-04-29T20:00:00Z", // older -> SourceChanged=true
				mirror.ExtKeyVersion:       "2",
				mirror.ExtKeyChecksum:      "sha256:legacy-v2-checksum",
			},
		},
	}
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)

	postMain := *mirrorEv
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("got %s/%s, want patch/source_updated", got.Action, got.Reason)
	}
	// No conflict: this cell is the standard source-only path; no
	// migration_source_won banner because there's no mirror drift to lose.
	if got.Conflict != mirror.ConflictNone {
		t.Errorf("Conflict = %q, want none (no mirror drift -> standard patch)", got.Conflict)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 EventsPatch (mirror main + checksum); got %d", len(patches))
	}
	// Main patch must carry v3 schema (the upgrade still runs even on the
	// fall-through cell).
	body := patches[0].Body
	if body == nil || body.ExtendedProperties == nil {
		t.Fatal("main patch body missing ExtendedProperties")
	}
	if v := body.ExtendedProperties.Private[mirror.ExtKeyVersion]; v != mirror.SchemaVersion {
		t.Errorf("main patch body version = %q, want %q", v, mirror.SchemaVersion)
	}
}

// ---------- error propagation ----------

func TestClassify_DeleteErrorPropagates(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, _ := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Status = gws.EventStatusCancelled

	mirrorEv := makeCleanCurrentMirror("mi-1", "src-cal:src-evt",
		source.Updated, source.Updated,
		"Standup", source.Start, source.End)
	inv.Set(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}, mirrorEv)
	api.deleteErrors = append(api.deleteErrors, errors.New("boom"))

	c := newClassifier(t, api, inv, sink, classifyOptions{})
	err := c.Classify(context.Background(), source)
	if err == nil {
		t.Fatal("expected wrapped delete error")
	}
}

// contains reports whether haystack contains needle. Defined here too because
// recurring's contains is in another package.
func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
