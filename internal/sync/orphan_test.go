package sync

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// orphanOptions captures per-test OrphanWalker overrides; defaults match
// the rest of the layer-6 test suite (src-cal / tgt-cal / must2026).
type orphanOptions struct {
	pair             string
	direction        string
	sourceCalendarID string
	targetCalendarID string
	horizon          time.Duration
	now              time.Time
	concurrency      int
}

// newOrphanWalker mirrors newClassifier in spirit: fill defaults so each
// test only needs to express what's distinctive. Concurrency defaults to
// 1 so the existing non-thread-safe stubAPI is safe to use; the dedicated
// concurrency-cap test passes its own stub.
func newOrphanWalker(t *testing.T, api API, inv *Inventory, sink Output, opts orphanOptions) *OrphanWalker {
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
	concurrency := opts.concurrency
	if concurrency <= 0 {
		concurrency = 1
	}
	return &OrphanWalker{
		API:              api,
		Now:              fixedNow(now),
		Horizon:          opts.horizon,
		Pair:             opts.pair,
		Direction:        opts.direction,
		SourceCalendarID: opts.sourceCalendarID,
		TargetCalendarID: opts.targetCalendarID,
		Inventory:        inv,
		Output:           sink,
		ConcurrencyLimit: concurrency,
	}
}

// makeOrphanMirror builds a mirror Event with the calendar-sync extended
// properties enough to be tracked by inventory. The orphan walk doesn't
// inspect managed fields on the mirror itself - only on the source it
// fetches via events.get - so this fixture is intentionally minimal.
func makeOrphanMirror(id, sourceTuple string) *gws.Event {
	return &gws.Event{
		ID:      id,
		Status:  gws.EventStatusConfirmed,
		Summary: "Mirror " + id,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:  sourceTuple,
				mirror.ExtKeyVersion: mirror.SchemaVersion,
			},
		},
	}
}

// makeAliveSource builds a non-recurring source eligible for mirroring
// (status=confirmed, transparency=opaque, no Self-declined attendee, no
// excluded eventType). Tests that exercise filter cells mutate fields
// after construction.
func makeAliveSource(id string, start *gws.EventDateTime) *gws.Event {
	return &gws.Event{
		ID:           id,
		Status:       gws.EventStatusConfirmed,
		Summary:      "Source " + id,
		Start:        start,
		End:          &gws.EventDateTime{DateTime: addHourToDateTime(start.DateTime)},
		Transparency: gws.TransparencyOpaque,
		EventType:    gws.EventTypeDefault,
		Updated:      "2026-04-30T08:00:00Z",
	}
}

// ---------- baseline: empty inventory and all-visited ----------

func TestOrphanWalk_EmptyInventory(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{horizon: 30 * 24 * time.Hour})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	if len(*captured) != 0 {
		t.Errorf("empty inventory should emit no outcomes; got %v", *captured)
	}
	if len(api.calls) != 0 {
		t.Errorf("empty inventory should make no API calls; got %v", api.calls)
	}
}

func TestOrphanWalk_AllVisited_NoAPICalls(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	tuples := []mirror.SourceTuple{
		{CalendarID: "src-cal", EventID: "evt-1"},
		{CalendarID: "src-cal", EventID: "evt-2"},
	}
	for _, tup := range tuples {
		inv.Set(tup, makeOrphanMirror("m-"+tup.EventID, tup.String()))
	}
	visited := map[mirror.SourceTuple]bool{tuples[0]: true, tuples[1]: true}

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{horizon: 30 * 24 * time.Hour})
	if err := w.Walk(context.Background(), visited); err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	if len(*captured) != 0 {
		t.Errorf("all-visited should emit no outcomes; got %v", *captured)
	}
	if len(api.calls) != 0 {
		t.Errorf("all-visited should make no API calls; got %v", api.calls)
	}
	// Inventory entries must remain in place.
	for _, tup := range tuples {
		if _, ok := inv.Lookup(tup); !ok {
			t.Errorf("all-visited must not mutate inventory; missing %v", tup)
		}
	}
}

// ---------- cell 1: orphaned (404 / cancelled) ----------

func TestOrphanWalk_404_DeletesAsOrphaned(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "ghost"}
	mirrorEv := makeOrphanMirror("m-ghost", tuple.String())
	inv.Set(tuple, mirrorEv)
	api.queueGetErr("src-cal", "ghost", &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{horizon: 30 * 24 * time.Hour})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionDelete || got.Reason != ReasonOrphaned {
		t.Errorf("got %s/%s, want delete/orphaned", got.Action, got.Reason)
	}
	if got.SourceEventID != "ghost" || got.TargetEventID != "m-ghost" {
		t.Errorf("Outcome IDs = src %q tgt %q, want src=ghost tgt=m-ghost",
			got.SourceEventID, got.TargetEventID)
	}
	deletes := api.callsByOp("EventsDelete")
	if len(deletes) != 1 || deletes[0].CalendarID != "tgt-cal" || deletes[0].EventID != "m-ghost" {
		t.Errorf("expected one delete on tgt-cal/m-ghost; got %v", deletes)
	}
	if _, ok := inv.Lookup(tuple); ok {
		t.Errorf("inventory must be pruned after orphan delete")
	}
}

func TestOrphanWalk_Cancelled_DeletesAsOrphaned(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "evt-1"}
	mirrorEv := makeOrphanMirror("m-1", tuple.String())
	inv.Set(tuple, mirrorEv)
	cancelled := &gws.Event{ID: "evt-1", Status: gws.EventStatusCancelled}
	api.queueGet("src-cal", "evt-1", cancelled)

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{horizon: 30 * 24 * time.Hour})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionDelete || got.Reason != ReasonOrphaned {
		t.Errorf("got %s/%s, want delete/orphaned", got.Action, got.Reason)
	}
}

// ---------- cell 2: outside horizon (non-recurring) ----------

func TestOrphanWalk_NonRecurring_PastHorizon_DeletesAsOutsideHorizon(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "far-future"}
	inv.Set(tuple, makeOrphanMirror("m-far", tuple.String()))
	// horizon 30d, source starts beyond that.
	farFuture := makeAliveSource("far-future", &gws.EventDateTime{DateTime: "2026-12-01T12:00:00Z"})
	api.queueGet("src-cal", "far-future", farFuture)

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{
		horizon: 30 * 24 * time.Hour,
		now:     must2026(),
	})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionDelete || got.Reason != ReasonOutsideHorizon {
		t.Errorf("got %s/%s, want delete/outside_horizon", got.Action, got.Reason)
	}
	if len(api.callsByOp("EventsInstances")) != 0 {
		t.Errorf("non-recurring path must not call events.instances; got %v",
			api.callsByOp("EventsInstances"))
	}
}

// ---------- cell 3: recurring parent ----------

func TestOrphanWalk_RecurringParent_HasInstance_NoDelete(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "rec-1"}
	inv.Set(tuple, makeOrphanMirror("m-rec", tuple.String()))

	src := makeAliveSource("rec-1", &gws.EventDateTime{DateTime: "2025-01-01T12:00:00Z"})
	src.Recurrence = []string{"RRULE:FREQ=WEEKLY"}
	api.queueGet("src-cal", "rec-1", src)
	// At least one instance materializes in [now, now+horizon].
	api.queueInstances([]gws.Event{{ID: "any-instance"}})

	now := must2026()
	horizon := 30 * 24 * time.Hour
	w := newOrphanWalker(t, api, inv, sink, orphanOptions{horizon: horizon, now: now})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	if len(*captured) != 0 {
		t.Errorf("alive-and-eligible recurring parent should not emit; got %v", *captured)
	}
	if len(api.callsByOp("EventsDelete")) != 0 {
		t.Errorf("recurring with instance in window must not delete")
	}
	if _, ok := inv.Lookup(tuple); !ok {
		t.Errorf("inventory entry must remain")
	}
	// Verify the instances probe carried the SPEC-mandated parameters.
	calls := api.callsByOp("EventsInstances")
	if len(calls) != 1 {
		t.Fatalf("expected 1 EventsInstances call; got %d", len(calls))
	}
	p := calls[0].InstanceParams
	if p.MaxResults != 1 {
		t.Errorf("MaxResults = %d, want 1", p.MaxResults)
	}
	if p.ShowDeleted {
		t.Errorf("ShowDeleted must be false")
	}
	if got, want := p.TimeMin, now.Format(time.RFC3339); got != want {
		t.Errorf("TimeMin = %q, want %q", got, want)
	}
	if got, want := p.TimeMax, now.Add(horizon).Format(time.RFC3339); got != want {
		t.Errorf("TimeMax = %q, want %q", got, want)
	}
}

func TestOrphanWalk_RecurringParent_NoInstance_DeletesAsOutsideHorizon(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "rec-1"}
	inv.Set(tuple, makeOrphanMirror("m-rec", tuple.String()))
	src := makeAliveSource("rec-1", &gws.EventDateTime{DateTime: "2025-01-01T12:00:00Z"})
	src.Recurrence = []string{"RRULE:FREQ=WEEKLY"}
	api.queueGet("src-cal", "rec-1", src)
	api.queueInstances(nil)

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{
		horizon: 30 * 24 * time.Hour,
		now:     must2026(),
	})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionDelete || got.Reason != ReasonOutsideHorizon {
		t.Errorf("got %s/%s, want delete/outside_horizon", got.Action, got.Reason)
	}
}

// ---------- cell 4: source filtered (table) ----------

func TestOrphanWalk_SourceFiltered_Family(t *testing.T) {
	type tc struct {
		name    string
		mutator func(*gws.Event)
	}
	tests := []tc{
		{
			name:    "eventType=birthday",
			mutator: func(e *gws.Event) { e.EventType = gws.EventTypeBirthday },
		},
		{
			name:    "eventType=fromGmail",
			mutator: func(e *gws.Event) { e.EventType = gws.EventTypeFromGmail },
		},
		{
			name:    "eventType=workingLocation",
			mutator: func(e *gws.Event) { e.EventType = gws.EventTypeWorkingLocation },
		},
		{
			// SPEC §"Filtering" allows {default, outOfOffice, focusTime}; any
			// other eventType (current or future) must be filtered. The
			// allowlist guard catches this where a denylist would silently
			// retain.
			name:    "eventType=unknown_future_type",
			mutator: func(e *gws.Event) { e.EventType = "futureUnknownType" },
		},
		{
			name:    "transparency=transparent",
			mutator: func(e *gws.Event) { e.Transparency = gws.TransparencyTransparent },
		},
		{
			name: "owner-attendee declined",
			mutator: func(e *gws.Event) {
				e.Attendees = []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusDeclined}}
			},
		},
		{
			name: "owner-attendee tentative",
			mutator: func(e *gws.Event) {
				e.Attendees = []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusTentative}}
			},
		},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			api := newStubAPI()
			inv := NewInventory("tgt-cal")
			sink, captured := captureOutputs()

			tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "evt"}
			inv.Set(tuple, makeOrphanMirror("m-evt", tuple.String()))
			src := makeAliveSource("evt", &gws.EventDateTime{DateTime: "2026-05-15T12:00:00Z"})
			c.mutator(src)
			api.queueGet("src-cal", "evt", src)

			w := newOrphanWalker(t, api, inv, sink, orphanOptions{
				horizon: 30 * 24 * time.Hour,
				now:     must2026(),
			})
			if err := w.Walk(context.Background(), nil); err != nil {
				t.Fatalf("Walk error: %v", err)
			}
			got := firstOutcome(t, *captured)
			if got.Action != mirror.ActionDelete || got.Reason != ReasonSourceFiltered {
				t.Errorf("got %s/%s, want delete/source_filtered", got.Action, got.Reason)
			}
		})
	}
}

// recurring + filtered combination: a recurring parent that's in horizon
// AND now carries transparency=transparent should be deleted as
// source_filtered, not retained. SPEC step 5 lists the cells without
// explicitly enumerating recurring + filtered, but the four-cell ordering
// (404 -> horizon -> filtered) implies an in-horizon recurring parent
// still flows through the filter check.
func TestOrphanWalk_Recurring_InHorizonButFiltered_DeletesAsSourceFiltered(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "rec-1"}
	inv.Set(tuple, makeOrphanMirror("m-rec", tuple.String()))
	src := makeAliveSource("rec-1", &gws.EventDateTime{DateTime: "2025-01-01T12:00:00Z"})
	src.Recurrence = []string{"RRULE:FREQ=WEEKLY"}
	src.Transparency = gws.TransparencyTransparent
	api.queueGet("src-cal", "rec-1", src)
	api.queueInstances([]gws.Event{{ID: "any-instance"}}) // in horizon

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{
		horizon: 30 * 24 * time.Hour,
		now:     must2026(),
	})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionDelete || got.Reason != ReasonSourceFiltered {
		t.Errorf("got %s/%s, want delete/source_filtered", got.Action, got.Reason)
	}
}

// ---------- cell 5: alive AND eligible (the shouldn't-happen case) ----------

func TestOrphanWalk_AliveEligible_NoOpNoOutcome(t *testing.T) {
	// SPEC step 5 doesn't enumerate this combo. The chosen behavior is to
	// skip silently; document the gap. This test pins the behavior so a
	// future refactor that decides to delete or warn would deliberately
	// flip it rather than do so by accident.
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "evt"}
	inv.Set(tuple, makeOrphanMirror("m-evt", tuple.String()))
	src := makeAliveSource("evt", &gws.EventDateTime{DateTime: "2026-05-15T12:00:00Z"})
	api.queueGet("src-cal", "evt", src)

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{
		horizon: 30 * 24 * time.Hour,
		now:     must2026(),
	})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	if len(*captured) != 0 {
		t.Errorf("alive-and-eligible should not emit an outcome; got %v", *captured)
	}
	if len(api.callsByOp("EventsDelete")) != 0 {
		t.Errorf("alive-and-eligible should not delete; got %v", api.callsByOp("EventsDelete"))
	}
	if _, ok := inv.Lookup(tuple); !ok {
		t.Errorf("alive-and-eligible must leave inventory entry intact")
	}
}

// ---------- delete returning 404 is swallowed ----------

func TestOrphanWalk_DeleteReturns404_TreatedAsSuccess(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "ghost"}
	inv.Set(tuple, makeOrphanMirror("m-ghost", tuple.String()))
	api.queueGetErr("src-cal", "ghost", &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})
	api.deleteErrors = append(api.deleteErrors, &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{horizon: 30 * 24 * time.Hour})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("delete-404 must be swallowed; got %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionDelete || got.Reason != ReasonOrphaned {
		t.Errorf("got %s/%s, want delete/orphaned", got.Action, got.Reason)
	}
	if _, ok := inv.Lookup(tuple); ok {
		t.Errorf("inventory must still be pruned even when delete returns 404")
	}
}

// TestOrphanWalk_DeleteReturns410_TreatedAsSuccess pins B14: Calendar API
// returns HTTP 410 ("Resource has been deleted") on events.delete for
// mirrors whose underlying event was already cleaned up - typically a
// cancelled exception instance of a recurring event whose parent was
// deleted in the same pass, triggering a server-side cascade. The
// walker must treat that the same as 404: the cleanup happened (just
// not by us), so the inventory and outcome should reflect success.
func TestOrphanWalk_DeleteReturns410_TreatedAsSuccess(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "ghost"}
	inv.Set(tuple, makeOrphanMirror("m-ghost", tuple.String()))
	api.queueGetErr("src-cal", "ghost", &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})
	api.deleteErrors = append(api.deleteErrors, &gws.Error{Code: gws.CodeAPIGone, ExitCode: 1})

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{horizon: 30 * 24 * time.Hour})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("delete-410 must be swallowed; got %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionDelete || got.Reason != ReasonOrphaned {
		t.Errorf("got %s/%s, want delete/orphaned", got.Action, got.Reason)
	}
	if _, ok := inv.Lookup(tuple); ok {
		t.Errorf("inventory must still be pruned even when delete returns 410")
	}
}

// ---------- non-404 errors propagate but don't abort the walk ----------

func TestOrphanWalk_GetErrorOnOneEntry_OthersStillProgress(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	good := mirror.SourceTuple{CalendarID: "src-cal", EventID: "good"}
	bad := mirror.SourceTuple{CalendarID: "src-cal", EventID: "bad"}
	inv.Set(good, makeOrphanMirror("m-good", good.String()))
	inv.Set(bad, makeOrphanMirror("m-bad", bad.String()))

	// "bad" returns a backend_error (500); "good" returns 404 -> orphaned.
	api.queueGetErr("src-cal", "bad", &gws.Error{Code: gws.CodeBackendError, ExitCode: 1})
	api.queueGetErr("src-cal", "good", &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{horizon: 30 * 24 * time.Hour})
	err := w.Walk(context.Background(), nil)
	if err == nil {
		t.Fatal("expected an error wrapping the backend_error entry")
	}
	if !strings.Contains(err.Error(), "bad") {
		t.Errorf("error should mention the failing entry; got %v", err)
	}
	// "good" must still have been deleted despite the parallel failure.
	if _, ok := inv.Lookup(good); ok {
		t.Errorf("good entry should have been pruned despite bad's failure")
	}
	if _, ok := inv.Lookup(bad); !ok {
		t.Errorf("bad entry should remain in inventory after non-404 failure")
	}
	if len(*captured) != 1 {
		t.Errorf("expected one outcome (good's delete); got %d (%v)", len(*captured), *captured)
	}
}

func TestOrphanWalk_DeleteNon404Error_Propagates(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, _ := captureOutputs()

	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "ghost"}
	inv.Set(tuple, makeOrphanMirror("m-ghost", tuple.String()))
	api.queueGetErr("src-cal", "ghost", &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})
	api.deleteErrors = append(api.deleteErrors, errors.New("delete kaboom"))

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{horizon: 30 * 24 * time.Hour})
	err := w.Walk(context.Background(), nil)
	if err == nil {
		t.Fatal("expected wrapped delete error")
	}
}

// ---------- visited filter ----------

func TestOrphanWalk_VisitedSetSkipsEntry_NoEventsGetCall(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	visitedTup := mirror.SourceTuple{CalendarID: "src-cal", EventID: "visited"}
	unvisitedTup := mirror.SourceTuple{CalendarID: "src-cal", EventID: "unvisited"}
	inv.Set(visitedTup, makeOrphanMirror("m-visited", visitedTup.String()))
	inv.Set(unvisitedTup, makeOrphanMirror("m-unvisited", unvisitedTup.String()))

	api.queueGetErr("src-cal", "unvisited", &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{horizon: 30 * 24 * time.Hour})
	if err := w.Walk(context.Background(), map[mirror.SourceTuple]bool{visitedTup: true}); err != nil {
		t.Fatalf("Walk error: %v", err)
	}

	gets := api.callsByOp("EventsGet")
	if len(gets) != 1 || gets[0].EventID != "unvisited" {
		t.Errorf("expected exactly one events.get on the unvisited entry; got %v", gets)
	}
	if _, ok := inv.Lookup(visitedTup); !ok {
		t.Errorf("visited entry must remain")
	}
	if _, ok := inv.Lookup(unvisitedTup); ok {
		t.Errorf("unvisited entry should have been pruned")
	}
	if len(*captured) != 1 {
		t.Errorf("expected one outcome for the unvisited delete; got %d", len(*captured))
	}
}

// ---------- foreign source-calendar entries are silently skipped ----------

func TestOrphanWalk_OtherSourceCalendar_Ignored(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	mine := mirror.SourceTuple{CalendarID: "src-cal", EventID: "mine"}
	other := mirror.SourceTuple{CalendarID: "other-cal", EventID: "theirs"}
	inv.Set(mine, makeOrphanMirror("m-mine", mine.String()))
	inv.Set(other, makeOrphanMirror("m-other", other.String()))

	api.queueGetErr("src-cal", "mine", &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{horizon: 30 * 24 * time.Hour})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	gets := api.callsByOp("EventsGet")
	if len(gets) != 1 || gets[0].EventID != "mine" {
		t.Errorf("expected exactly one events.get for the in-scope entry; got %v", gets)
	}
	if _, ok := inv.Lookup(other); !ok {
		t.Errorf("other-source entry must remain - this walker doesn't manage it")
	}
	if len(*captured) != 1 {
		t.Errorf("expected one outcome; got %d", len(*captured))
	}
}

// ---------- concurrency cap ----------

// concurrentGetAPI wraps a stub used by the concurrency test. EventsGet
// signals on enter, waits until at least capLimit calls have piled up
// (the "saturation" signal), then returns a 404. The pile-up gate proves
// that at least capLimit calls were in-flight simultaneously; the
// in-flight counter independently verifies the cap was never exceeded.
type concurrentGetAPI struct {
	mu       sync.Mutex
	deletes  []string
	inFlight int32
	peak     int32

	saturate     int32         // capLimit: number of concurrent calls to wait for before unblocking
	saturateCh   chan struct{} // closed once saturate-many calls have arrived
	saturateOnce sync.Once     // guards the close above
}

func newConcurrentGetAPI(capLimit int) *concurrentGetAPI {
	return &concurrentGetAPI{
		saturate:   int32(capLimit),
		saturateCh: make(chan struct{}),
	}
}

func (c *concurrentGetAPI) EventsList(_ context.Context, _ gws.EventsListParams) ([]gws.Event, string, error) {
	return nil, "", nil
}

func (c *concurrentGetAPI) EventsGet(_ context.Context, _ string, _ string) (*gws.Event, error) {
	cur := atomic.AddInt32(&c.inFlight, 1)
	defer atomic.AddInt32(&c.inFlight, -1)
	for {
		prev := atomic.LoadInt32(&c.peak)
		if cur <= prev || atomic.CompareAndSwapInt32(&c.peak, prev, cur) {
			break
		}
	}
	// Once capLimit concurrent calls have arrived, unblock everyone (now
	// and going forward). Until then, wait. This proves the walker can
	// hit at least capLimit in-flight; the inFlight counter independently
	// proves it never exceeds.
	if cur >= c.saturate {
		c.saturateOnce.Do(func() { close(c.saturateCh) })
	}
	<-c.saturateCh
	return nil, &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1}
}

func (c *concurrentGetAPI) EventsInstances(_ context.Context, _ gws.EventsInstancesParams) ([]gws.Event, error) {
	return nil, nil
}

func (c *concurrentGetAPI) EventsInsert(_ context.Context, _ string, _ *gws.Event) (*gws.Event, error) {
	return nil, nil
}

func (c *concurrentGetAPI) EventsPatch(_ context.Context, _ string, _ string, _ *gws.Event) (*gws.Event, error) {
	return nil, nil
}

func (c *concurrentGetAPI) EventsDelete(_ context.Context, _ string, eventID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deletes = append(c.deletes, eventID)
	return nil
}

func TestOrphanWalk_ConcurrencyCap(t *testing.T) {
	const capLimit = 3
	const numEntries = 12

	api := newConcurrentGetAPI(capLimit)
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	for i := 0; i < numEntries; i++ {
		eid := "evt-" + string(rune('a'+i))
		tup := mirror.SourceTuple{CalendarID: "src-cal", EventID: eid}
		inv.Set(tup, makeOrphanMirror("m-"+eid, tup.String()))
	}

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{
		horizon:     30 * 24 * time.Hour,
		concurrency: capLimit,
	})

	// Time-bound the call so a serial-implementation regression fails
	// fast instead of hanging on the saturation gate.
	done := make(chan error, 1)
	go func() { done <- w.Walk(context.Background(), nil) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Walk error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Walk did not complete - implementation is serial or stuck (peak=%d, want %d)",
			atomic.LoadInt32(&api.peak), capLimit)
	}

	peak := atomic.LoadInt32(&api.peak)
	if peak > int32(capLimit) {
		t.Errorf("peak in-flight EventsGet = %d, want <= %d", peak, capLimit)
	}
	// Sanity: we must actually hit the cap. If peak < capLimit a serial
	// implementation would also pass and the test would be useless. The
	// saturation gate inside EventsGet only releases once peak >= capLimit,
	// so a serial implementation deadlocks - which is itself a failure
	// (the Walk call would never return). This branch covers the partial
	// case where the walker uses a non-1 concurrency that's still below
	// capLimit.
	if peak < int32(capLimit) {
		t.Errorf("peak in-flight EventsGet = %d, expected to reach cap %d", peak, capLimit)
	}

	if len(*captured) != numEntries {
		t.Errorf("expected %d delete outcomes; got %d", numEntries, len(*captured))
	}
	if len(api.deletes) != numEntries {
		t.Errorf("expected %d events.delete calls; got %d", numEntries, len(api.deletes))
	}
}

// ---------- pair / direction enrichment on outcomes ----------

func TestOrphanWalk_OutcomeCarriesPairAndDirection(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "ghost"}
	inv.Set(tuple, makeOrphanMirror("m-ghost", tuple.String()))
	api.queueGetErr("src-cal", "ghost", &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1})

	w := newOrphanWalker(t, api, inv, sink, orphanOptions{
		pair:      "work-personal",
		direction: "b_to_a",
		horizon:   30 * 24 * time.Hour,
	})
	if err := w.Walk(context.Background(), nil); err != nil {
		t.Fatalf("Walk error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Pair != "work-personal" || got.Direction != "b_to_a" {
		t.Errorf("Outcome Pair/Direction = %q/%q, want work-personal/b_to_a",
			got.Pair, got.Direction)
	}
}
