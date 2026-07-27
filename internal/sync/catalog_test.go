package sync

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/recurring"
)

// occurrenceAt builds the minimal source-instance shape the catalog lookup
// reads: an ID for the exact-match index plus start/end for the coverage
// check.
func occurrenceAt(id, start, end string) *gws.Event {
	return &gws.Event{
		ID:    id,
		Start: &gws.EventDateTime{DateTime: start},
		End:   &gws.EventDateTime{DateTime: end},
	}
}

// exception builds a source recurring-instance exception as it appears in an
// unexpanded events.list response.
func exception(id, parentID, start, end, status string) gws.Event {
	ev := *occurrenceAt(id, start, end)
	ev.RecurringEventID = parentID
	ev.Status = status
	return ev
}

func TestSourceCatalog_LookupFourStates(t *testing.T) {
	coverageMin := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	coverageMax := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		build func() *sourceCatalog
		inst  *gws.Event
		want  recurring.Membership
	}{
		{
			name:  "no catalog at all",
			build: func() *sourceCatalog { return nil },
			inst:  occurrenceAt("p_1", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z"),
			want:  recurring.MembershipUnknown,
		},
		{
			name: "catalog marked unknown",
			build: func() *sourceCatalog {
				c := newSourceCatalog(coverageMin, coverageMax)
				c.addException(&gws.Event{ID: "p_1", RecurringEventID: "p"})
				return c // readiness left at the zero value
			},
			inst: occurrenceAt("p_1", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z"),
			want: recurring.MembershipUnknown,
		},
		{
			name: "ready, occurrence after coverage",
			build: func() *sourceCatalog {
				c := newSourceCatalog(coverageMin, coverageMax)
				c.readiness = readinessReady
				return c
			},
			inst: occurrenceAt("p_1", "2026-07-10T12:00:00Z", "2026-07-10T13:00:00Z"),
			want: recurring.MembershipOutOfScope,
		},
		{
			name: "ready, occurrence entirely before coverage",
			build: func() *sourceCatalog {
				c := newSourceCatalog(coverageMin, coverageMax)
				c.readiness = readinessReady
				return c
			},
			inst: occurrenceAt("p_1", "2026-04-10T12:00:00Z", "2026-04-10T13:00:00Z"),
			want: recurring.MembershipOutOfScope,
		},
		{
			name: "ready, occurrence with no parseable time",
			build: func() *sourceCatalog {
				c := newSourceCatalog(coverageMin, coverageMax)
				c.readiness = readinessReady
				return c
			},
			inst: &gws.Event{ID: "p_1"},
			want: recurring.MembershipOutOfScope,
		},
		{
			name: "ready, in coverage, indexed",
			build: func() *sourceCatalog {
				c := newSourceCatalog(coverageMin, coverageMax)
				c.addException(&gws.Event{ID: "p_1", RecurringEventID: "p"})
				c.readiness = readinessReady
				return c
			},
			inst: occurrenceAt("p_1", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z"),
			want: recurring.MembershipPresent,
		},
		{
			name: "ready, in coverage, not indexed",
			build: func() *sourceCatalog {
				c := newSourceCatalog(coverageMin, coverageMax)
				c.addException(&gws.Event{ID: "p_other", RecurringEventID: "p"})
				c.readiness = readinessReady
				return c
			},
			inst: occurrenceAt("p_1", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z"),
			want: recurring.MembershipAbsent,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.build().lookup(tt.inst); got != tt.want {
				t.Errorf("lookup = %s, want %s", got, tt.want)
			}
		})
	}
}

// TestSourceCatalog_InHorizonOccurrenceIsCovered pins the coverage boundary
// the way production uses it: the source list is bounded by [now, now+horizon]
// while the target delta is unbounded, so an occurrence a user can plausibly
// edit inside the horizon must come back Absent (actionable) rather than
// OutOfScope (inert).
func TestSourceCatalog_InHorizonOccurrenceIsCovered(t *testing.T) {
	now := must2026()
	horizon := 30 * 24 * time.Hour
	c := newSourceCatalog(now, now.Add(horizon))
	c.readiness = readinessReady

	tests := []struct {
		name  string
		start time.Time
		want  recurring.Membership
	}{
		{"one hour into the horizon", now.Add(time.Hour), recurring.MembershipAbsent},
		{"mid horizon", now.Add(15 * 24 * time.Hour), recurring.MembershipAbsent},
		{"just inside the far edge", now.Add(horizon - time.Hour), recurring.MembershipAbsent},
		{"past the far edge", now.Add(horizon + time.Hour), recurring.MembershipOutOfScope},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			inst := occurrenceAt("p_1",
				tt.start.Format(time.RFC3339),
				tt.start.Add(45*time.Minute).Format(time.RFC3339))
			if got := c.lookup(inst); got != tt.want {
				t.Errorf("lookup(start=%s) = %s, want %s", tt.start, got, tt.want)
			}
		})
	}
}

// TestSourceCatalog_IndexesCancelledExceptions pins the case that killed the
// managed-field-comparison approach: a cancelled source exception whose start
// equals its originalStartTime. Status is not a managed field, so a
// field-level comparison reports "no source intent" - but the exception is
// real, and treating it as virtual would resurrect an occurrence the user
// deliberately removed.
func TestSourceCatalog_IndexesCancelledExceptions(t *testing.T) {
	api := newStubAPI()
	r := newTestReconciler(api, makeCanonical(makeWritablePDir("p1", "src-A", "tgt-A")))

	r.rebuildSourceCatalog("src-A", []gws.Event{
		exception("p_20260714T154500Z", "p", "2026-07-14T15:45:00Z", "2026-07-14T16:30:00Z",
			gws.EventStatusCancelled),
	}, time.Time{}, time.Time{})

	got := r.lookupSourceException("src-A",
		occurrenceAt("p_20260714T154500Z", "2026-07-14T15:45:00Z", "2026-07-14T16:30:00Z"))
	if got != recurring.MembershipPresent {
		t.Errorf("cancelled exception must be Present; got %s", got)
	}
}

// TestSourceCatalog_ParentTombstoneRemovesIndexedChildren pins the byParent
// reverse index. Google reports a series deletion once, on the parent, not
// once per exception; without the cascade the children would answer Present
// forever and block any later reverse write at that occurrence.
func TestSourceCatalog_ParentTombstoneRemovesIndexedChildren(t *testing.T) {
	api := newStubAPI()
	r := newTestReconciler(api, makeCanonical(makeWritablePDir("p1", "src-A", "tgt-A")))

	r.rebuildSourceCatalog("src-A", []gws.Event{
		exception("p_1", "p", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z", gws.EventStatusConfirmed),
		exception("p_2", "p", "2026-05-17T12:00:00Z", "2026-05-17T13:00:00Z", gws.EventStatusConfirmed),
		exception("q_1", "q", "2026-05-11T12:00:00Z", "2026-05-11T13:00:00Z", gws.EventStatusConfirmed),
	}, time.Time{}, time.Time{})

	// The delta carries the parent tombstone only.
	r.applySourceDeltaToCatalog("src-A", []gws.Event{
		{ID: "p", Status: gws.EventStatusCancelled},
	})

	for _, id := range []string{"p_1", "p_2"} {
		got := r.lookupSourceException("src-A",
			occurrenceAt(id, "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z"))
		if got != recurring.MembershipAbsent {
			t.Errorf("child %s of a tombstoned parent = %s, want absent", id, got)
		}
	}
	// A sibling series is untouched.
	got := r.lookupSourceException("src-A",
		occurrenceAt("q_1", "2026-05-11T12:00:00Z", "2026-05-11T13:00:00Z"))
	if got != recurring.MembershipPresent {
		t.Errorf("unrelated series child = %s, want present", got)
	}
}

// TestSourceCatalog_DeltaAddsCancelledException pins that a delta carrying a
// newly cancelled EXCEPTION indexes it rather than removing it. Only a
// tombstone with no recurringEventId is a series deletion.
func TestSourceCatalog_DeltaAddsCancelledException(t *testing.T) {
	api := newStubAPI()
	r := newTestReconciler(api, makeCanonical(makeWritablePDir("p1", "src-A", "tgt-A")))

	r.rebuildSourceCatalog("src-A", nil, time.Time{}, time.Time{})
	r.applySourceDeltaToCatalog("src-A", []gws.Event{
		exception("p_1", "p", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z", gws.EventStatusCancelled),
	})

	got := r.lookupSourceException("src-A",
		occurrenceAt("p_1", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z"))
	if got != recurring.MembershipPresent {
		t.Errorf("cancelled exception from a delta = %s, want present", got)
	}
}

// TestSourceCatalog_DeltaIgnoredWhileUnknown pins that an overlay cannot
// promote an Unknown calendar to Ready. Only a complete full source-list can
// prove the collection, so a delta folded into a stale set would let a
// missing exception answer Absent.
func TestSourceCatalog_DeltaIgnoredWhileUnknown(t *testing.T) {
	api := newStubAPI()
	r := newTestReconciler(api, makeCanonical(makeWritablePDir("p1", "src-A", "tgt-A")))

	r.markSourceCatalogUnknown("src-A")
	r.applySourceDeltaToCatalog("src-A", []gws.Event{
		exception("p_1", "p", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z", gws.EventStatusConfirmed),
	})

	if r.sourceCatalogReady("src-A") {
		t.Error("a delta must not make an unknown catalog ready")
	}
	got := r.lookupSourceException("src-A",
		occurrenceAt("p_1", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z"))
	if got != recurring.MembershipUnknown {
		t.Errorf("lookup on an unknown catalog = %s, want unknown", got)
	}
}

// TestSourceCatalog_NoteSourceExceptionMakesPresent pins the immediate insert
// the reverse-materialization path performs between its two writes.
func TestSourceCatalog_NoteSourceExceptionMakesPresent(t *testing.T) {
	api := newStubAPI()
	r := newTestReconciler(api, makeCanonical(makeWritablePDir("p1", "src-A", "tgt-A")))
	r.rebuildSourceCatalog("src-A", nil, time.Time{}, time.Time{})

	inst := occurrenceAt("p_1", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z")
	inst.RecurringEventID = "p"
	if got := r.lookupSourceException("src-A", inst); got != recurring.MembershipAbsent {
		t.Fatalf("precondition: lookup = %s, want absent", got)
	}

	r.noteSourceException("src-A", inst)

	if got := r.lookupSourceException("src-A", inst); got != recurring.MembershipPresent {
		t.Errorf("after noteSourceException lookup = %s, want present", got)
	}
}

// TestFullSync_FailedSourceListMarksCatalogUnknown pins that a failed full
// source-list never installs a partial replacement and never leaves a prior
// snapshot answering Absent.
func TestFullSync_FailedSourceListMarksCatalogUnknown(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	r := newTestReconciler(api, canonical)
	// A prior FullSync had proven the collection.
	r.rebuildSourceCatalog("src-A", []gws.Event{
		exception("p_1", "p", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z", gws.EventStatusConfirmed),
	}, time.Time{}, time.Time{})
	if !r.sourceCatalogReady("src-A") {
		t.Fatal("precondition: catalog should be ready")
	}

	api.queueListFullErr("src-A", errors.New("list boom"))
	queueWritableTargetSeed(api, "tgt-A", "tok-tgt-1")
	queueEmptyInventory(api, "tgt-A")

	if _, err := r.FullSync(context.Background()); err != nil {
		t.Fatalf("FullSync: %v", err)
	}

	if r.sourceCatalogReady("src-A") {
		t.Error("a failed source list must mark the catalog unknown")
	}
	// The indexed data survives for diagnostics but can no longer answer.
	got := r.lookupSourceException("src-A",
		occurrenceAt("p_1", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z"))
	if got != recurring.MembershipUnknown {
		t.Errorf("lookup after failed list = %s, want unknown", got)
	}
}

// TestFullSync_BuildsCatalogWithHorizonCoverage pins that FullSync records
// the same [timeMin, timeMax] window it issued the source list with, so
// Absent stays authoritative exactly where the snapshot reached.
func TestFullSync_BuildsCatalogWithHorizonCoverage(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	canonical := makeCanonical(pd)

	api.queueListFull("src-A", []gws.Event{
		exception("p_1", "p", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z", gws.EventStatusConfirmed),
	}, "tok-src-1")
	queueWritableTargetSeed(api, "tgt-A", "tok-tgt-1")
	queueEmptyInventory(api, "tgt-A")

	r := newTestReconciler(api, canonical)
	if _, err := r.FullSync(context.Background()); err != nil {
		t.Fatalf("FullSync: %v", err)
	}

	if !r.sourceCatalogReady("src-A") {
		t.Fatal("FullSync should leave the catalog ready")
	}
	// makeTestPDir's horizon is 30d off must2026 (2026-04-30).
	inside := r.lookupSourceException("src-A",
		occurrenceAt("p_2", "2026-05-20T12:00:00Z", "2026-05-20T13:00:00Z"))
	if inside != recurring.MembershipAbsent {
		t.Errorf("in-horizon unindexed occurrence = %s, want absent", inside)
	}
	beyond := r.lookupSourceException("src-A",
		occurrenceAt("p_3", "2026-09-20T12:00:00Z", "2026-09-20T13:00:00Z"))
	if beyond != recurring.MembershipOutOfScope {
		t.Errorf("beyond-horizon occurrence = %s, want out_of_scope", beyond)
	}
}

// TestTick_SourceDeltaFailureMarksCatalogUnknown pins that a failed or 410'd
// source delta invalidates the calendar rather than leaving the last
// snapshot to answer for exceptions created since.
func TestTick_SourceDeltaFailureMarksCatalogUnknown(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{"transport failure", errors.New("delta boom")},
		{"410 gone", &gws.Error{Code: gws.CodeAPIGone, ExitCode: 1}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newStubAPI()
			pd := makeWritablePDir("p1", "src-A", "tgt-A")
			r := newTestReconciler(api, makeCanonical(pd))
			r.inventories["tgt-A"] = NewInventory("tgt-A")
			r.syncTokens["src-A"] = "tok-src-old"
			r.rebuildSourceCatalog("src-A", nil, time.Time{}, time.Time{})

			api.queueListIncrErr("src-A", tt.err)

			if _, err := r.Tick(context.Background()); err != nil {
				t.Fatalf("Tick: %v", err)
			}
			if r.sourceCatalogReady("src-A") {
				t.Error("a failed source delta must mark the catalog unknown")
			}
		})
	}
}

// TestTick_MissingSourceTokenMarksCatalogUnknown pins the cold-start / post-410
// case: no delta was read at all, so the catalog cannot be current.
func TestTick_MissingSourceTokenMarksCatalogUnknown(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.rebuildSourceCatalog("src-A", nil, time.Time{}, time.Time{})
	// No r.syncTokens["src-A"].

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if r.sourceCatalogReady("src-A") {
		t.Error("a tick that read no source delta must mark the catalog unknown")
	}
}

// TestTick_SourceDeltaKeepsCatalogCurrent pins the overlay path end to end:
// an exception created since the last FullSync is Present on the same tick
// that the delta reports it.
func TestTick_SourceDeltaKeepsCatalogCurrent(t *testing.T) {
	api := newStubAPI()
	pd := makeWritablePDir("p1", "src-A", "tgt-A")
	r := newTestReconciler(api, makeCanonical(pd))
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	r.syncTokens["src-A"] = "tok-src-old"
	r.rebuildSourceCatalog("src-A", nil, time.Time{}, time.Time{})

	newException := exception("p_20260510T120000Z", "p",
		"2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z", gws.EventStatusConfirmed)
	newException.Summary = "Standup"
	api.queueListIncr("src-A", []gws.Event{newException}, "tok-src-new")
	// The classify loop reaches the recurring handler, which resolves the
	// parent through the inventory-miss repair path. Not what this test is
	// about; a source parent that reconciles to nothing ends the walk.
	api.queueGet("src-A", "p", &gws.Event{
		ID:     "p",
		Status: gws.EventStatusCancelled,
	})

	if _, err := r.Tick(context.Background()); err != nil {
		t.Fatalf("Tick: %v", err)
	}

	got := r.lookupSourceException("src-A",
		occurrenceAt("p_20260510T120000Z", "2026-05-10T12:00:00Z", "2026-05-10T13:00:00Z"))
	if got != recurring.MembershipPresent {
		t.Errorf("exception from this tick's delta = %s, want present", got)
	}
}
