package recurring

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// recordedCall captures one method invocation on the stub. Fields are unioned
// across all three operations so a single []recordedCall preserves call order
// across method boundaries; tests assert on (Op, ...) tuples per call.
type recordedCall struct {
	Op            string // "EventsGet", "EventsInstances", "EventsPatch"
	CalendarID    string
	EventID       string
	OriginalStart string
	Body          *gws.Event
}

// stubAPI is a hand-rolled in-process API stub. CLAUDE.md "Testing" prefers
// this over the fake-gws harness for unit-level work where the gws argv shape
// isn't what's under test.
//
// Each Op's response is a queue of values returned in FIFO order. A single-
// element queue covers most tests; the patch-with-checksum follow-up pattern
// (and the "force-patch then re-lookup" repair path) needs queues to model
// the multi-call flow.
type stubAPI struct {
	getResponses       map[[2]string][]*gws.Event
	getErrors          map[[2]string][]error
	instancesResponses [][]gws.Event
	instancesErrors    []error
	patchResponses     []*gws.Event
	patchErrors        []error
	calls              []recordedCall
}

func newStubAPI() *stubAPI {
	return &stubAPI{
		getResponses: make(map[[2]string][]*gws.Event),
		getErrors:    make(map[[2]string][]error),
	}
}

func (s *stubAPI) EventsGet(_ context.Context, calendarID, eventID string) (*gws.Event, error) {
	s.calls = append(s.calls, recordedCall{Op: "EventsGet", CalendarID: calendarID, EventID: eventID})
	key := [2]string{calendarID, eventID}
	if errs := s.getErrors[key]; len(errs) > 0 {
		head := errs[0]
		s.getErrors[key] = errs[1:]
		if head != nil {
			return nil, head
		}
	}
	resps := s.getResponses[key]
	if len(resps) == 0 {
		return nil, errors.New("stubAPI: no EventsGet response queued for " + calendarID + "/" + eventID)
	}
	head := resps[0]
	s.getResponses[key] = resps[1:]
	return head, nil
}

func (s *stubAPI) EventsInstances(_ context.Context, params gws.EventsInstancesParams) ([]gws.Event, error) {
	s.calls = append(s.calls, recordedCall{
		Op:            "EventsInstances",
		CalendarID:    params.CalendarID,
		EventID:       params.EventID,
		OriginalStart: params.OriginalStart,
	})
	if len(s.instancesErrors) > 0 {
		head := s.instancesErrors[0]
		s.instancesErrors = s.instancesErrors[1:]
		if head != nil {
			return nil, head
		}
	}
	if len(s.instancesResponses) == 0 {
		return nil, errors.New("stubAPI: no EventsInstances response queued")
	}
	head := s.instancesResponses[0]
	s.instancesResponses = s.instancesResponses[1:]
	return head, nil
}

func (s *stubAPI) EventsPatch(_ context.Context, calendarID, eventID string, body *gws.Event) (*gws.Event, error) {
	s.calls = append(s.calls, recordedCall{
		Op:         "EventsPatch",
		CalendarID: calendarID,
		EventID:    eventID,
		Body:       body,
	})
	if len(s.patchErrors) > 0 {
		head := s.patchErrors[0]
		s.patchErrors = s.patchErrors[1:]
		if head != nil {
			return nil, head
		}
	}
	if len(s.patchResponses) == 0 {
		return nil, errors.New("stubAPI: no EventsPatch response queued")
	}
	head := s.patchResponses[0]
	s.patchResponses = s.patchResponses[1:]
	return head, nil
}

// queueGet enqueues a response for a (calendarID, eventID) pair.
func (s *stubAPI) queueGet(calendarID, eventID string, resp *gws.Event) {
	key := [2]string{calendarID, eventID}
	s.getResponses[key] = append(s.getResponses[key], resp)
}

// queueInstances enqueues a list response for the next EventsInstances call.
func (s *stubAPI) queueInstances(events []gws.Event) {
	s.instancesResponses = append(s.instancesResponses, events)
}

// queuePatch enqueues a response for the next EventsPatch call.
func (s *stubAPI) queuePatch(resp *gws.Event) {
	s.patchResponses = append(s.patchResponses, resp)
}

// callsByOp returns just the calls matching op, preserving order. Useful for
// tests that want to assert "exactly N patches with these bodies, in order"
// without coupling to interleaving with other ops.
func (s *stubAPI) callsByOp(op string) []recordedCall {
	var out []recordedCall
	for _, c := range s.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}

// makeV2Mirror builds a v2 mirror with the given live fields and a checksum
// that hashes the same fields - representing a clean (non-drifted) mirror.
// To represent a drifted mirror, pass different live fields then call
// driftMirror with the user's edits.
func makeV2Mirror(id, storedSourceUpdated, liveUpdated string, fields managedFieldHints) *gws.Event {
	e := &gws.Event{
		ID:           id,
		Status:       gws.EventStatusConfirmed,
		Summary:      fields.summary,
		Description:  fields.description,
		Start:        fields.start,
		End:          fields.end,
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
		Updated:      liveUpdated,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:        "src-cal:src-evt",
				mirror.ExtKeySourceUpdated: storedSourceUpdated,
				mirror.ExtKeyVersion:       mirror.SchemaVersion,
			},
		},
	}
	e.ExtendedProperties.Private[mirror.ExtKeyChecksum] = mirror.Checksum(mirror.ManagedFieldsFromEvent(e))
	return e
}

// driftMirror simulates a user edit by mutating the live mirror fields but
// leaving the stored checksum (and stored source_updated) untouched. The
// resulting event reports MirrorDrifted=true via ComputeDriftSignal because
// the live managed fields no longer hash to the stored checksum.
func driftMirror(e *gws.Event, mutate func(*gws.Event)) *gws.Event {
	mutate(e)
	return e
}

// makeV1Mirror builds a v1 mirror (no checksum, version="1"). Used to test
// the schema-version-migration drift recomputation branch.
func makeV1Mirror(id, storedSourceUpdated, liveUpdated string, fields managedFieldHints) *gws.Event {
	return &gws.Event{
		ID:           id,
		Status:       gws.EventStatusConfirmed,
		Summary:      fields.summary,
		Description:  fields.description,
		Start:        fields.start,
		End:          fields.end,
		Transparency: gws.TransparencyOpaque,
		Visibility:   gws.VisibilityPrivate,
		Updated:      liveUpdated,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:        "src-cal:src-evt",
				mirror.ExtKeySourceUpdated: storedSourceUpdated,
				mirror.ExtKeyVersion:       "1",
				// no checksum
			},
		},
	}
}

// managedFieldHints is a tiny convenience for building mirrors in tests.
type managedFieldHints struct {
	summary     string
	description string
	start       *gws.EventDateTime
	end         *gws.EventDateTime
}

// makeSourceException constructs a typical recurring source exception.
func makeSourceException(updated, originalStart string, summary string) *gws.Event {
	return &gws.Event{
		ID:                "src-evt",
		Status:            gws.EventStatusConfirmed,
		Summary:           summary,
		Start:             &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z", TimeZone: "UTC"},
		End:               &gws.EventDateTime{DateTime: "2026-05-01T14:00:00Z", TimeZone: "UTC"},
		Updated:           updated,
		HTMLLink:          "https://www.google.com/calendar/event?eid=ABC",
		RecurringEventID:  "src-parent",
		OriginalStartTime: &gws.EventDateTime{DateTime: originalStart, TimeZone: "UTC"},
	}
}

// makeMirrorParent is a thin convenience for the inventory parent.
func makeMirrorParent(id string) *gws.Event {
	return &gws.Event{
		ID:         id,
		Status:     gws.EventStatusConfirmed,
		Summary:    "Standup",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"},
	}
}

// ---------- Step 1: parent lookup / repair ----------

func TestHandle_Step1_MirrorParentInInventory(t *testing.T) {
	// Mirror parent already in inventory -> handler proceeds to step 2
	// without calling EventsGet or ReconcileParent.
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")
	mirrorInst := makeV2Mirror("mi-1", "2026-04-29T20:00:00Z", "2026-04-29T20:00:00Z", managedFieldHints{
		summary:     source.Summary,
		description: source.Description + "\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	api.queueInstances([]gws.Event{*mirrorInst})

	reconcileCalled := 0
	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
		ReconcileParent: func(_ context.Context, _ *gws.Event) (*gws.Event, error) {
			reconcileCalled++
			return mirrorParent, nil
		},
	}

	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if reconcileCalled != 0 {
		t.Errorf("ReconcileParent should not be called when inventory has the parent; was called %d time(s)", reconcileCalled)
	}
	if len(api.callsByOp("EventsGet")) != 0 {
		t.Errorf("EventsGet should not be called when inventory has the parent; got %d", len(api.callsByOp("EventsGet")))
	}
	if got.Action != mirror.ActionSkip || got.Reason != mirror.ReasonUnchanged {
		t.Errorf("expected skip(unchanged); got %+v", got)
	}
	if got.PostWriteMirrorParent != nil {
		t.Errorf("PostWriteMirrorParent should be nil when no parent was written; got %+v", got.PostWriteMirrorParent)
	}
}

func TestHandle_Step1_RepairsMirrorParentViaReconciler(t *testing.T) {
	// Parent missing from inventory; ReconcileParent returns a mirror parent.
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")

	// The handler will fetch the source parent then call ReconcileParent.
	sourceParent := &gws.Event{
		ID:         "src-parent",
		Status:     gws.EventStatusConfirmed,
		Summary:    "Standup",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"},
	}
	api.queueGet("src-cal", "src-parent", sourceParent)

	// Once parent recovered, locate instance -> matches -> drift no-change.
	mirrorInst := makeV2Mirror("mi-1", "2026-04-29T20:00:00Z", "2026-04-29T20:00:00Z", managedFieldHints{
		summary:     source.Summary,
		description: source.Description + "\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	api.queueInstances([]gws.Event{*mirrorInst})

	reconcileCalls := 0
	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return nil, false
		},
		ReconcileParent: func(_ context.Context, sp *gws.Event) (*gws.Event, error) {
			reconcileCalls++
			if sp.ID != "src-parent" {
				t.Errorf("ReconcileParent got source parent ID %q, want src-parent", sp.ID)
			}
			return mirrorParent, nil
		},
	}

	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if reconcileCalls != 1 {
		t.Errorf("ReconcileParent should be called exactly once; got %d", reconcileCalls)
	}
	if got.PostWriteMirrorParent != mirrorParent {
		t.Errorf("PostWriteMirrorParent should reflect repaired parent; got %+v", got.PostWriteMirrorParent)
	}
	if got.Action != mirror.ActionSkip || got.Reason != mirror.ReasonUnchanged {
		t.Errorf("expected skip(unchanged); got %+v", got)
	}
}

func TestHandle_Step1_ParentNotEligible(t *testing.T) {
	// Parent missing; ReconcileParent returns nil (filtered).
	api := newStubAPI()
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")
	api.queueGet("src-cal", "src-parent", &gws.Event{ID: "src-parent", Status: gws.EventStatusCancelled})

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return nil, false
		},
		ReconcileParent: func(_ context.Context, _ *gws.Event) (*gws.Event, error) {
			return nil, nil
		},
	}

	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionSkip || got.Reason != ReasonParentNotEligible {
		t.Errorf("expected skip(parent_not_eligible); got %+v", got)
	}
	// No instance lookup or patch should have happened.
	if n := len(api.callsByOp("EventsInstances")); n != 0 {
		t.Errorf("EventsInstances should not be called; got %d", n)
	}
	if n := len(api.callsByOp("EventsPatch")); n != 0 {
		t.Errorf("EventsPatch should not be called; got %d", n)
	}
}

func TestHandle_Step1_ReconcileParentError(t *testing.T) {
	api := newStubAPI()
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")
	api.queueGet("src-cal", "src-parent", &gws.Event{ID: "src-parent"})

	wantErr := errors.New("reconcile boom")
	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return nil, false
		},
		ReconcileParent: func(_ context.Context, _ *gws.Event) (*gws.Event, error) {
			return nil, wantErr
		},
	}
	_, err := h.Handle(context.Background(), source)
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected wrapped reconcile error; got %v", err)
	}
}

// ---------- Step 2: instance lookup ----------

func TestHandle_Step2_InstanceFoundOnFirstLookup(t *testing.T) {
	// Smoke-test verified by the step-1-inventory case above; here we add
	// a small assertion that the originalStart param was forwarded.
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")
	mi := makeV2Mirror("mi-1", source.Updated, source.Updated, managedFieldHints{
		summary:     source.Summary,
		description: source.Description + "\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	api.queueInstances([]gws.Event{*mi})

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	if _, err := h.Handle(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	calls := api.callsByOp("EventsInstances")
	if len(calls) != 1 {
		t.Fatalf("expected 1 EventsInstances call; got %d", len(calls))
	}
	if calls[0].OriginalStart != "2026-05-01T12:00:00Z" {
		t.Errorf("originalStart = %q, want %q", calls[0].OriginalStart, "2026-05-01T12:00:00Z")
	}
	if calls[0].EventID != "mp-1" {
		t.Errorf("EventID = %q, want mp-1", calls[0].EventID)
	}
}

func TestHandle_Step2_AllDayOriginalStart(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := &gws.Event{
		ID:                "src-evt",
		Status:            gws.EventStatusConfirmed,
		Summary:           "All-day exception",
		Start:             &gws.EventDateTime{Date: "2026-05-01"},
		End:               &gws.EventDateTime{Date: "2026-05-02"},
		Updated:           "2026-04-29T20:00:00Z",
		HTMLLink:          "https://www.google.com/calendar/event?eid=AD",
		RecurringEventID:  "src-parent",
		OriginalStartTime: &gws.EventDateTime{Date: "2026-05-01"}, // YYYY-MM-DD only
	}
	mi := makeV2Mirror("mi-1", source.Updated, source.Updated, managedFieldHints{
		summary:     source.Summary,
		description: source.Description + "\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	api.queueInstances([]gws.Event{*mi})

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	if _, err := h.Handle(context.Background(), source); err != nil {
		t.Fatal(err)
	}
	calls := api.callsByOp("EventsInstances")
	if len(calls) != 1 || calls[0].OriginalStart != "2026-05-01" {
		t.Errorf("originalStart for all-day = %q (calls=%d), want 2026-05-01", func() string {
			if len(calls) > 0 {
				return calls[0].OriginalStart
			}
			return "<none>"
		}(), len(calls))
	}
}

func TestHandle_Step2_RepairAfterZeroResults(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")

	// First instances lookup: empty (need repair).
	api.queueInstances(nil)

	// Repair path fetches source parent.
	sourceParent := &gws.Event{
		ID:         "src-parent",
		Status:     gws.EventStatusConfirmed,
		Summary:    "Standup updated rules",
		Recurrence: []string{"RRULE:FREQ=DAILY"},
		Updated:    "2026-04-29T22:00:00Z",
		HTMLLink:   "https://www.google.com/calendar/event?eid=PP",
	}
	api.queueGet("src-cal", "src-parent", sourceParent)

	// Force-patch the mirror parent (main + checksum follow-up).
	postPatchParent := *mirrorParent
	postPatchParent.Recurrence = []string{"RRULE:FREQ=DAILY"}
	postPatchParent.Summary = "Standup updated rules"
	postPatchParent.Description = "\n\n---\nSource: " + sourceParent.HTMLLink
	postPatchParent.Transparency = gws.TransparencyOpaque
	postPatchParent.Visibility = gws.VisibilityPrivate
	api.queuePatch(&postPatchParent)
	postChecksumParent := postPatchParent
	postChecksumParent.ExtendedProperties = &gws.ExtendedProperties{Private: map[string]string{}}
	api.queuePatch(&postChecksumParent)

	// Retry instances: returns the now-materialized instance.
	mi := makeV2Mirror("mi-1", source.Updated, source.Updated, managedFieldHints{
		summary:     source.Summary,
		description: source.Description + "\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	api.queueInstances([]gws.Event{*mi})

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.PostWriteMirrorParent == nil {
		t.Errorf("PostWriteMirrorParent should be set after repair")
	}
	if n := len(api.callsByOp("EventsInstances")); n != 2 {
		t.Errorf("expected 2 EventsInstances calls (original + retry); got %d", n)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 EventsPatch calls (force + checksum); got %d", len(patches))
	}
	// Both patches MUST target the mirror parent on the target calendar.
	// A bug that pointed the force-patch at the source parent (or any other
	// id) would otherwise pass the count assertion silently.
	for i, p := range patches {
		if p.CalendarID != "tgt-cal" {
			t.Errorf("patches[%d].CalendarID = %q, want tgt-cal", i, p.CalendarID)
		}
		if p.EventID != "mp-1" {
			t.Errorf("patches[%d].EventID = %q, want mp-1 (mirror parent)", i, p.EventID)
		}
	}
}

func TestHandle_Step2_InstanceUnmaterializable(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	mirrorParent.Recurrence = []string{"RRULE:FREQ=WEEKLY"}
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")

	api.queueInstances(nil) // first lookup empty

	sourceParent := &gws.Event{
		ID:         "src-parent",
		Status:     gws.EventStatusConfirmed,
		Recurrence: []string{"RRULE:FREQ=MONTHLY"},
		Updated:    "2026-04-29T22:00:00Z",
		HTMLLink:   "https://www.google.com/calendar/event?eid=PP",
	}
	api.queueGet("src-cal", "src-parent", sourceParent)

	// Force-patch responses (main + checksum).
	postPatchParent := *mirrorParent
	postPatchParent.Recurrence = []string{"RRULE:FREQ=MONTHLY"}
	api.queuePatch(&postPatchParent)
	api.queuePatch(&postPatchParent)

	// Retry still empty.
	api.queueInstances(nil)

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionSkip || got.Reason != ReasonInstanceUnmaterializable {
		t.Errorf("expected skip(instance_unmaterializable); got %+v", got)
	}
	if !reflect.DeepEqual(got.SourceParentRecurrence, sourceParent.Recurrence) {
		t.Errorf("SourceParentRecurrence = %v, want %v", got.SourceParentRecurrence, sourceParent.Recurrence)
	}
	wantMirrorRec := []string{"RRULE:FREQ=MONTHLY"}
	if !reflect.DeepEqual(got.MirrorParentRecurrence, wantMirrorRec) {
		t.Errorf("MirrorParentRecurrence = %v, want %v", got.MirrorParentRecurrence, wantMirrorRec)
	}
}

func TestHandle_Step2_NoOriginalStartReturnsError(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	// Programmer-error case: no OriginalStartTime at all.
	source := &gws.Event{
		ID:               "src-evt",
		RecurringEventID: "src-parent",
		Updated:          "2026-04-29T20:00:00Z",
	}
	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	_, err := h.Handle(context.Background(), source)
	if err == nil {
		t.Fatal("expected an error for missing OriginalStartTime")
	}
}

// ---------- Step 3: cancellation-style outcomes ----------

func TestHandle_Step3_CancellationFamily(t *testing.T) {
	type tc struct {
		name        string
		mutateSrc   func(*gws.Event)
		mirrorState string // "confirmed" or "cancelled"
		wantAction  mirror.Action
		wantReason  mirror.Reason
	}
	tests := []tc{
		{
			name:       "source cancelled, mirror confirmed -> delete(source_cancelled)",
			mutateSrc:  func(e *gws.Event) { e.Status = gws.EventStatusCancelled },
			mirrorState: "confirmed",
			wantAction: mirror.ActionDelete,
			wantReason: ReasonSourceCancelled,
		},
		{
			name:       "source cancelled, mirror already cancelled -> skip(unchanged)",
			mutateSrc:  func(e *gws.Event) { e.Status = gws.EventStatusCancelled },
			mirrorState: "cancelled",
			wantAction: mirror.ActionSkip,
			wantReason: mirror.ReasonUnchanged,
		},
		{
			name: "source declined -> delete(declined)",
			mutateSrc: func(e *gws.Event) {
				e.Attendees = []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusDeclined}}
			},
			mirrorState: "confirmed",
			wantAction: mirror.ActionDelete,
			wantReason: ReasonDeclined,
		},
		{
			name: "source tentative -> delete(tentative)",
			mutateSrc: func(e *gws.Event) {
				e.Attendees = []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusTentative}}
			},
			mirrorState: "confirmed",
			wantAction: mirror.ActionDelete,
			wantReason: ReasonTentative,
		},
		{
			name:       "source transparency=transparent -> delete(transparency_transparent)",
			mutateSrc:  func(e *gws.Event) { e.Transparency = gws.TransparencyTransparent },
			mirrorState: "confirmed",
			wantAction: mirror.ActionDelete,
			wantReason: ReasonTransparencyTransparent,
		},
		{
			name: "source declined, mirror already cancelled -> skip(unchanged)",
			mutateSrc: func(e *gws.Event) {
				e.Attendees = []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusDeclined}}
			},
			mirrorState: "cancelled",
			wantAction:  mirror.ActionSkip,
			wantReason:  mirror.ReasonUnchanged,
		},
		{
			name: "source tentative, mirror already cancelled -> skip(unchanged)",
			mutateSrc: func(e *gws.Event) {
				e.Attendees = []gws.Attendee{{Self: true, ResponseStatus: gws.ResponseStatusTentative}}
			},
			mirrorState: "cancelled",
			wantAction:  mirror.ActionSkip,
			wantReason:  mirror.ReasonUnchanged,
		},
		{
			name:        "source transparent, mirror already cancelled -> skip(unchanged)",
			mutateSrc:   func(e *gws.Event) { e.Transparency = gws.TransparencyTransparent },
			mirrorState: "cancelled",
			wantAction:  mirror.ActionSkip,
			wantReason:  mirror.ReasonUnchanged,
		},
	}
	for _, c := range tests {
		t.Run(c.name, func(t *testing.T) {
			api := newStubAPI()
			mirrorParent := makeMirrorParent("mp-1")
			source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")
			c.mutateSrc(source)

			mi := makeV2Mirror("mi-1", source.Updated, source.Updated, managedFieldHints{
				summary:     source.Summary,
				description: source.Description + "\n\n---\nSource: " + source.HTMLLink,
				start:       source.Start,
				end:         source.End,
			})
			if c.mirrorState == "cancelled" {
				mi.Status = gws.EventStatusCancelled
			}
			api.queueInstances([]gws.Event{*mi})

			if c.wantAction == mirror.ActionDelete {
				postCancel := *mi
				postCancel.Status = gws.EventStatusCancelled
				api.queuePatch(&postCancel)
			}

			h := &Handler{
				API:              api,
				SourceCalendarID: "src-cal",
				TargetCalendarID: "tgt-cal",
				LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
					return mirrorParent, true
				},
			}
			got, err := h.Handle(context.Background(), source)
			if err != nil {
				t.Fatalf("Handle returned error: %v", err)
			}
			if got.Action != c.wantAction || got.Reason != c.wantReason {
				t.Errorf("got %s/%s; want %s/%s", got.Action, got.Reason, c.wantAction, c.wantReason)
			}
			patches := api.callsByOp("EventsPatch")
			if c.wantAction == mirror.ActionDelete {
				if len(patches) != 1 {
					t.Fatalf("expected exactly one EventsPatch (cancel); got %d", len(patches))
				}
				if patches[0].CalendarID != "tgt-cal" || patches[0].EventID != "mi-1" {
					t.Errorf("patch on %s/%s, want tgt-cal/mi-1", patches[0].CalendarID, patches[0].EventID)
				}
				if patches[0].Body == nil || patches[0].Body.Status != gws.EventStatusCancelled {
					t.Errorf("cancel patch body must set status=cancelled; got %+v", patches[0].Body)
				}
				if got.PostWriteMirrorInstance == nil {
					t.Errorf("PostWriteMirrorInstance should be set on cancel path")
				}
			} else {
				if len(patches) != 0 {
					t.Errorf("expected no EventsPatch on already-cancelled mirror; got %d", len(patches))
				}
			}
		})
	}
}

// ---------- Step 3: drift matrix ----------

func TestHandle_Step3_DriftNoChange(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")
	mi := makeV2Mirror("mi-1", source.Updated, source.Updated, managedFieldHints{
		summary:     source.Summary,
		description: source.Description + "\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	api.queueInstances([]gws.Event{*mi})

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionSkip || got.Reason != mirror.ReasonUnchanged {
		t.Errorf("expected skip(unchanged); got %+v", got)
	}
	if n := len(api.callsByOp("EventsPatch")); n != 0 {
		t.Errorf("expected no patches on no-change path; got %d", n)
	}
}

func TestHandle_Step3_SourceChangedOnly_PatchesMirror(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-30T10:00:00Z", "2026-05-01T12:00:00Z", "Standup updated")

	// Mirror records older source_updated; managed fields are mirror's still-current state.
	mi := makeV2Mirror("mi-1", "2026-04-29T20:00:00Z", "2026-04-29T20:00:00Z", managedFieldHints{
		summary:     "Standup",
		description: "Standup\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	api.queueInstances([]gws.Event{*mi})

	// Two EventsPatch calls: main + checksum.
	postMain := *mi
	postMain.Summary = "Standup updated"
	postMain.Updated = "2026-04-30T10:00:01Z"
	api.queuePatch(&postMain)
	postChecksum := postMain
	api.queuePatch(&postChecksum)

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("expected patch(source_updated); got %+v", got)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 EventsPatch (main + checksum); got %d", len(patches))
	}
	for i, p := range patches {
		if p.CalendarID != "tgt-cal" || p.EventID != "mi-1" {
			t.Errorf("patch[%d] on %s/%s, want tgt-cal/mi-1", i, p.CalendarID, p.EventID)
		}
		if p.Body != nil && p.Body.ID != "" {
			t.Errorf("patch[%d] body.ID should be empty (events.patch carries the id in the URL); got %q", i, p.Body.ID)
		}
	}
	// Second patch body must contain the checksum extended property only.
	checksumBody := patches[1].Body
	if checksumBody == nil || checksumBody.ExtendedProperties == nil {
		t.Fatalf("checksum patch body missing ExtendedProperties; got %+v", checksumBody)
	}
	if got, ok := checksumBody.ExtendedProperties.Private[mirror.ExtKeyChecksum]; !ok || got == "" {
		t.Errorf("checksum patch body missing %s; got %#v", mirror.ExtKeyChecksum, checksumBody.ExtendedProperties.Private)
	}
	if got.PostWriteMirrorInstance == nil {
		t.Errorf("PostWriteMirrorInstance should be set after patch+checksum")
	}
}

func TestHandle_Step3_MirrorDriftedOnly_Propagate(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")

	// Build a clean mirror first (live fields hash to stored checksum), then
	// drift the live summary to simulate a user edit. SourceUpdated stored
	// on the mirror matches source.Updated so SourceChanged=false; the
	// hash mismatch produces MirrorDrifted=true.
	mi := makeV2Mirror("mi-1", source.Updated, "2026-04-30T08:00:00Z", managedFieldHints{
		summary:     source.Summary,
		description: source.Description + "\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	driftMirror(mi, func(e *gws.Event) {
		e.Summary = "User edit"
		e.Description = "User edit\n\n---\nSource: " + source.HTMLLink
	})
	api.queueInstances([]gws.Event{*mi})

	// First patch is on the SOURCE calendar with drifted fields.
	postSrcPatch := *source
	postSrcPatch.Summary = "User edit"
	postSrcPatch.Updated = "2026-04-30T09:00:00Z"
	api.queuePatch(&postSrcPatch)

	// Then mirror main + checksum.
	postMirror := *mi
	postMirror.Summary = "User edit"
	api.queuePatch(&postMirror)
	api.queuePatch(&postMirror)

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionPropagate || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("expected propagate(target_edited); got %+v", got)
	}
	if !contains(got.Fields, "summary") {
		t.Errorf("Fields should include 'summary'; got %v", got.Fields)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 3 {
		t.Fatalf("expected 3 EventsPatch (source + mirror main + mirror checksum); got %d", len(patches))
	}
	if patches[0].CalendarID != "src-cal" || patches[0].EventID != "src-evt" {
		t.Errorf("first patch should be on the source; got %s/%s", patches[0].CalendarID, patches[0].EventID)
	}
	for i := 1; i < 3; i++ {
		if patches[i].CalendarID != "tgt-cal" || patches[i].EventID != "mi-1" {
			t.Errorf("patches[%d] should target the mirror instance; got %s/%s", i, patches[i].CalendarID, patches[i].EventID)
		}
	}
	// The propagate patch carries only drifted fields. Description must be
	// the mirror description with the trailer stripped (the source's true content),
	// not the trailered mirror description.
	if patches[0].Body == nil {
		t.Fatalf("propagate patch body is nil")
	}
	if patches[0].Body.Summary != "User edit" {
		t.Errorf("propagate patch body Summary = %q, want 'User edit'", patches[0].Body.Summary)
	}
	if patches[0].Body.Description != "User edit" {
		t.Errorf("propagate patch body Description = %q, want 'User edit' (trailer stripped)", patches[0].Body.Description)
	}
	// start/end didn't drift in this test, so the propagate body must NOT
	// carry them (SPEC's "Field-level propagate": only drifted fields).
	if patches[0].Body.Start != nil {
		t.Errorf("propagate patch body Start should be nil; got %+v", patches[0].Body.Start)
	}
	if patches[0].Body.End != nil {
		t.Errorf("propagate patch body End should be nil; got %+v", patches[0].Body.End)
	}
}

func TestHandle_Step3_MirrorDriftedOnly_Revert(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")

	mi := makeV2Mirror("mi-1", source.Updated, "2026-04-30T08:00:00Z", managedFieldHints{
		summary:     source.Summary,
		description: source.Description + "\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	driftMirror(mi, func(e *gws.Event) {
		e.Summary = "User edit"
		e.Description = "User edit\n\n---\nSource: " + source.HTMLLink
	})
	api.queueInstances([]gws.Event{*mi})

	// Read-only source: only mirror gets touched (main + checksum).
	postMirror := *mi
	postMirror.Summary = source.Summary
	api.queuePatch(&postMirror)
	api.queuePatch(&postMirror)

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   false,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionRevert || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("expected revert(target_edited); got %+v", got)
	}
	if !contains(got.Fields, "summary") {
		t.Errorf("Fields should include 'summary'; got %v", got.Fields)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches (mirror main + checksum) on revert; got %d", len(patches))
	}
	for _, p := range patches {
		if p.CalendarID != "tgt-cal" || p.EventID != "mi-1" {
			t.Errorf("revert patches must target mirror; got %s/%s", p.CalendarID, p.EventID)
		}
	}
}

func TestHandle_Step3_BothChanged_SourceNewer_Patch(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-30T11:00:00Z", "2026-05-01T12:00:00Z", "Source new")

	// Mirror's stored source_updated is older (SourceChanged=true) AND
	// the live fields drift from the stored checksum (MirrorDrifted=true).
	// Build clean, then drift.
	mi := makeV2Mirror("mi-1", "2026-04-29T20:00:00Z", "2026-04-30T10:00:00Z", managedFieldHints{
		summary:     "Standup",
		description: "Standup\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	driftMirror(mi, func(e *gws.Event) {
		e.Summary = "User edit"
		e.Description = "User edit\n\n---\nSource: " + source.HTMLLink
	})
	api.queueInstances([]gws.Event{*mi})

	postMain := *mi
	postMain.Summary = source.Summary
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("expected patch(source_updated); got %+v", got)
	}
	if got.Conflict != mirror.ConflictSourceWon {
		t.Errorf("expected ConflictSourceWon; got %q", got.Conflict)
	}
}

func TestHandle_Step3_BothChanged_MirrorNewer_Propagate(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-30T09:00:00Z", "2026-05-01T12:00:00Z", "Source new")

	// Mirror's live `updated` is newer than source.Updated, AND fields drift.
	mi := makeV2Mirror("mi-1", "2026-04-29T20:00:00Z", "2026-04-30T10:00:00Z", managedFieldHints{
		summary:     "Standup",
		description: "Standup\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	driftMirror(mi, func(e *gws.Event) {
		e.Summary = "User edit"
		e.Description = "User edit\n\n---\nSource: " + source.HTMLLink
	})
	api.queueInstances([]gws.Event{*mi})

	postSrc := *source
	postSrc.Summary = "User edit"
	postSrc.Updated = "2026-04-30T11:00:00Z"
	api.queuePatch(&postSrc)
	postMirror := *mi
	api.queuePatch(&postMirror)
	api.queuePatch(&postMirror)

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionPropagate || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("expected propagate(target_edited); got %+v", got)
	}
	if got.Conflict != mirror.ConflictTargetWon {
		t.Errorf("expected ConflictTargetWon; got %q", got.Conflict)
	}
}

func TestHandle_Step3_BothChanged_MirrorNewer_Revert(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-30T09:00:00Z", "2026-05-01T12:00:00Z", "Source new")

	mi := makeV2Mirror("mi-1", "2026-04-29T20:00:00Z", "2026-04-30T10:00:00Z", managedFieldHints{
		summary:     "Standup",
		description: "Standup\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	driftMirror(mi, func(e *gws.Event) {
		e.Summary = "User edit"
		e.Description = "User edit\n\n---\nSource: " + source.HTMLLink
	})
	api.queueInstances([]gws.Event{*mi})

	api.queuePatch(mi)
	api.queuePatch(mi)

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   false,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionRevert || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("expected revert(target_edited); got %+v", got)
	}
	if got.Conflict != mirror.ConflictTargetWon {
		t.Errorf("expected ConflictTargetWon; got %q", got.Conflict)
	}
}

// ---------- v1 migration drift handling ----------

func TestHandle_V1Mirror_NoActualDrift_PatchesAsMigrationUpgrade(t *testing.T) {
	// v1 mirror lacks checksum, so the standard ComputeDriftSignal would say
	// MirrorDrifted=true. The handler recomputes via live-vs-desired; if equal,
	// MirrorDrifted resolves to false. SPEC.md "Schema version migration"
	// then routes the !source_changed && !mirror_drifted cell to a
	// migration_upgrade patch (rewrite to add :checksum and bump :version=2),
	// NOT skip(unchanged).
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")

	mi := makeV1Mirror("mi-1", source.Updated, source.Updated, managedFieldHints{
		summary:     source.Summary,
		description: source.Description + "\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	api.queueInstances([]gws.Event{*mi})

	// The migration upgrade is a normal patch+checksum-followup pair.
	postMain := *mi
	api.queuePatch(&postMain)
	postChecksum := postMain
	api.queuePatch(&postChecksum)

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionPatch || got.Reason != ReasonMigrationUpgrade {
		t.Errorf("expected patch(migration_upgrade) for v1 mirror with no actual drift; got %+v", got)
	}
	if got.Conflict != mirror.ConflictNone {
		t.Errorf("migration_upgrade should not carry a conflict; got %q", got.Conflict)
	}
	if got.PostWriteMirrorInstance == nil {
		t.Errorf("PostWriteMirrorInstance should be set after migration patch")
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 EventsPatch (main + checksum followup); got %d", len(patches))
	}
	for i, p := range patches {
		if p.CalendarID != "tgt-cal" || p.EventID != "mi-1" {
			t.Errorf("patches[%d] should target the mirror instance on tgt-cal; got %s/%s", i, p.CalendarID, p.EventID)
		}
		// Migration upgrade only writes the mirror; the source must not be
		// touched, and no cancellation patch must be sent.
		if p.Body != nil && p.Body.Status == gws.EventStatusCancelled {
			t.Errorf("patches[%d] body must not set status=cancelled; got %+v", i, p.Body)
		}
	}
	// Stronger source-untouched assertion: scan the recorded calls for any
	// EventsPatch addressed to the source calendar.
	for _, c := range api.calls {
		if c.Op == "EventsPatch" && c.CalendarID == "src-cal" {
			t.Errorf("migration_upgrade must not patch source; got call %+v", c)
		}
	}
	// The main patch body must carry version=2 in extended properties (the
	// schema bump is the whole point of the upgrade).
	if patches[0].Body == nil || patches[0].Body.ExtendedProperties == nil {
		t.Fatalf("main patch body missing ExtendedProperties; got %+v", patches[0].Body)
	}
	if v := patches[0].Body.ExtendedProperties.Private[mirror.ExtKeyVersion]; v != mirror.SchemaVersion {
		t.Errorf("main patch body version = %q, want %q", v, mirror.SchemaVersion)
	}
	// Checksum followup carries the new :checksum.
	if patches[1].Body == nil || patches[1].Body.ExtendedProperties == nil {
		t.Fatalf("checksum patch body missing ExtendedProperties; got %+v", patches[1].Body)
	}
	if cs := patches[1].Body.ExtendedProperties.Private[mirror.ExtKeyChecksum]; cs == "" {
		t.Errorf("checksum patch body missing %s", mirror.ExtKeyChecksum)
	}
}

func TestHandle_V1Mirror_ActualDrift_Revert(t *testing.T) {
	// v1 mirror, source unchanged, live fields differ from desired -> the
	// recompute lands on !source_changed && mirror_drifted, which falls
	// through to Classify and behaves identically to the v2 mirror-only
	// drift cell. With SourceWritable=false this reverts the mirror.
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")

	mi := makeV1Mirror("mi-1", source.Updated, source.Updated, managedFieldHints{
		summary:     "User edit",
		description: "User edit\n\n---\nSource: " + source.HTMLLink,
		start:       source.Start,
		end:         source.End,
	})
	api.queueInstances([]gws.Event{*mi})

	api.queuePatch(mi)
	api.queuePatch(mi)

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   false, // -> revert
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionRevert || got.Reason != mirror.ReasonTargetEdited {
		t.Errorf("expected revert(target_edited) for v1 with actual drift; got %+v", got)
	}
}

func TestHandle_V1Mirror_BothChanged_MigrationSourceWon(t *testing.T) {
	// v1 mirror, source.Updated newer than stored source_updated AND live
	// fields differ from desired-from-source. SPEC.md "Schema version
	// migration" / "Conflict logging" mandates source-wins-by-default in
	// this cell with msg=migration_source_won (NOT newer-wins via Classify).
	// The mirror's live `Updated` is intentionally newer than source.Updated
	// so a buggy newer-wins path would route to propagate/revert; the fixed
	// code must still patch+migration_source_won.
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-30T11:00:00Z", "2026-05-01T12:00:00Z", "Source new")

	// stored source_updated older than source.Updated -> SourceChanged=true.
	// Live fields drift from desired -> MirrorDrifted=true (after the v1
	// recompute).
	mi := makeV1Mirror("mi-1",
		"2026-04-29T20:00:00Z",
		"2026-04-30T15:00:00Z", // mirror Updated NEWER than source -> would lose under newer-wins
		managedFieldHints{
			summary:     "User edit",
			description: "User edit\n\n---\nSource: " + source.HTMLLink,
			start:       source.Start,
			end:         source.End,
		},
	)
	api.queueInstances([]gws.Event{*mi})

	// Migration_source_won is a single patch+checksum on the mirror; the
	// source is NOT touched (no propagate followup).
	postMain := *mi
	postMain.Summary = source.Summary
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true, // even with writable source, source-wins-by-default applies
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("expected patch(source_updated) for v1 both-changed; got %+v", got)
	}
	if got.Conflict != mirror.ConflictMigrationSourceWon {
		t.Errorf("expected ConflictMigrationSourceWon; got %q", got.Conflict)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 EventsPatch (main + checksum) on migration_source_won; got %d", len(patches))
	}
	for i, p := range patches {
		if p.CalendarID != "tgt-cal" || p.EventID != "mi-1" {
			t.Errorf("patches[%d] should target the mirror; got %s/%s", i, p.CalendarID, p.EventID)
		}
	}
	for _, c := range api.calls {
		if c.Op == "EventsPatch" && c.CalendarID == "src-cal" {
			t.Errorf("migration_source_won must not propagate to source; got call %+v", c)
		}
	}
}

// TestHandle_V2Mirror_NeedsMigration_PatchesAsMigrationUpgrade pins the
// v2 -> v3 migration path through the recurring handler. SchemaVersion bumped
// to "3"; a v2 mirror reports NeedsMigration=true and the
// !source_changed && !mirror_drifted cell routes to migration_upgrade
// (rewrite at v3 with location + fresh checksum), not skip(unchanged).
func TestHandle_V2Mirror_NeedsMigration_PatchesAsMigrationUpgrade(t *testing.T) {
	api := newStubAPI()
	mirrorParent := makeMirrorParent("mp-1")
	source := makeSourceException("2026-04-29T20:00:00Z", "2026-05-01T12:00:00Z", "Standup")

	// Build a v2 mirror manually (with version="2", not mirror.SchemaVersion).
	// Live managed fields match the v3 desired payload, so the migration
	// recompute resolves MirrorDrifted=false.
	desired := mirror.BuildInstancePayload("src-cal", source)
	mi := &gws.Event{
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
				// v2 mirrors carry a checksum, but it was computed over the
				// v2 ManagedFields (no Location). NeedsMigration fires before
				// the stored checksum is consulted.
				mirror.ExtKeyChecksum: "sha256:legacy-v2-checksum",
			},
		},
	}
	api.queueInstances([]gws.Event{*mi})

	postMain := *mi
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	h := &Handler{
		API:              api,
		SourceCalendarID: "src-cal",
		TargetCalendarID: "tgt-cal",
		SourceWritable:   true,
		LookupMirrorParent: func(_ mirror.SourceTuple) (*gws.Event, bool) {
			return mirrorParent, true
		},
	}
	got, err := h.Handle(context.Background(), source)
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if got.Action != mirror.ActionPatch || got.Reason != ReasonMigrationUpgrade {
		t.Errorf("expected patch(migration_upgrade) for v2 mirror under v3 schema; got %+v", got)
	}
	if got.Conflict != mirror.ConflictNone {
		t.Errorf("migration_upgrade should not carry a conflict; got %q", got.Conflict)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 EventsPatch (main + checksum followup); got %d", len(patches))
	}
	if patches[0].Body == nil || patches[0].Body.ExtendedProperties == nil {
		t.Fatalf("main patch body missing ExtendedProperties; got %+v", patches[0].Body)
	}
	// The main patch body must carry the new SchemaVersion ("3") in
	// extended properties - that's what the upgrade is for.
	if v := patches[0].Body.ExtendedProperties.Private[mirror.ExtKeyVersion]; v != mirror.SchemaVersion {
		t.Errorf("main patch body version = %q, want %q", v, mirror.SchemaVersion)
	}
	// Migration upgrade must NOT patch the source.
	for _, c := range api.calls {
		if c.Op == "EventsPatch" && c.CalendarID == "src-cal" {
			t.Errorf("migration_upgrade must not patch source; got call %+v", c)
		}
	}
}

// ---------- helper-level tests ----------

func TestComputeOriginalStart(t *testing.T) {
	tests := []struct {
		name    string
		in      *gws.EventDateTime
		want    string
		wantErr bool
	}{
		{"datetime", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"}, "2026-05-01T12:00:00Z", false},
		{"all-day", &gws.EventDateTime{Date: "2026-05-01"}, "2026-05-01", false},
		{"datetime preferred over date", &gws.EventDateTime{Date: "2026-05-01", DateTime: "2026-05-01T12:00:00Z"}, "2026-05-01T12:00:00Z", false},
		{"nil pointer", nil, "", true},
		{"both empty", &gws.EventDateTime{}, "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := computeOriginalStart(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Errorf("expected error; got %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ---------- helpers used by tests ----------

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
