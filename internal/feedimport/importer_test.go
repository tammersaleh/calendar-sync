package feedimport

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/ical"
)

// --- in-process EventsAPI stub -------------------------------------------

type recordedInsert struct {
	calendarID string
	body       *gws.Event
}

type recordedPatch struct {
	calendarID string
	id         string
	body       *gws.PatchEvent
}

// stubAPI is a hand-rolled in-memory EventsAPI. It keys events by Google
// event ID, records every write, and honors the list filter + ShowDeleted
// the way Calendar API would so the tests can lean on realistic list results.
type stubAPI struct {
	events map[string]*gws.Event

	listParams []gws.EventsListParams
	inserts    []recordedInsert
	patches    []recordedPatch
	deletes    []string

	// insertConflictIDs forces EventsInsert to return gws.ErrAPIConflict for
	// the given deterministic IDs (simulating a cancelled/alive leftover).
	insertConflictIDs map[string]bool
}

func newStub() *stubAPI {
	return &stubAPI{events: map[string]*gws.Event{}, insertConflictIDs: map[string]bool{}}
}

func (s *stubAPI) EventsList(_ context.Context, params gws.EventsListParams) ([]gws.Event, string, error) {
	s.listParams = append(s.listParams, params)
	var out []gws.Event
	for _, ev := range s.events {
		if !params.ShowDeleted && ev.Status == gws.EventStatusCancelled {
			continue
		}
		if !matchesPrivateFilter(ev, params.PrivateExtendedProperty) {
			continue
		}
		out = append(out, *ev)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, "", nil
}

func matchesPrivateFilter(ev *gws.Event, filters []string) bool {
	for _, f := range filters {
		k, v, ok := strings.Cut(f, "=")
		if !ok {
			continue
		}
		if ev.ExtendedProperties == nil || ev.ExtendedProperties.Private[k] != v {
			return false
		}
	}
	return true
}

func (s *stubAPI) EventsGet(_ context.Context, _, eventID string) (*gws.Event, error) {
	ev, ok := s.events[eventID]
	if !ok {
		return nil, &gws.Error{Code: gws.CodeAPINotFound, ExitCode: 1}
	}
	cp := *ev
	return &cp, nil
}

func (s *stubAPI) EventsInsert(_ context.Context, calendarID string, body *gws.Event) (*gws.Event, error) {
	s.inserts = append(s.inserts, recordedInsert{calendarID: calendarID, body: body})
	if s.insertConflictIDs[body.ID] {
		return nil, &gws.Error{Code: gws.CodeAPIConflict, ExitCode: 1}
	}
	cp := *body
	s.events[body.ID] = &cp
	return &cp, nil
}

func (s *stubAPI) EventsPatch(_ context.Context, calendarID, eventID string, body *gws.PatchEvent) (*gws.Event, error) {
	s.patches = append(s.patches, recordedPatch{calendarID: calendarID, id: eventID, body: body})
	ev := s.events[eventID]
	if ev == nil {
		ev = &gws.Event{ID: eventID}
		s.events[eventID] = ev
	}
	if body.Status != nil {
		ev.Status = *body.Status
	}
	if body.Summary != nil {
		ev.Summary = *body.Summary
	}
	if body.Description != nil {
		ev.Description = *body.Description
	}
	if body.Location != nil {
		ev.Location = *body.Location
	}
	if body.Start != nil {
		ev.Start = body.Start
	}
	if body.End != nil {
		ev.End = body.End
	}
	if body.Transparency != nil {
		ev.Transparency = *body.Transparency
	}
	if body.ExtendedProperties != nil {
		ev.ExtendedProperties = body.ExtendedProperties
	}
	cp := *ev
	return &cp, nil
}

func (s *stubAPI) EventsDelete(_ context.Context, _, eventID string) error {
	s.deletes = append(s.deletes, eventID)
	delete(s.events, eventID)
	return nil
}

// --- fixtures ------------------------------------------------------------

const testFeedID = "feed-alpha"

func newImporter(api EventsAPI) *Importer {
	return &Importer{API: api, Target: "target@group.calendar.google.com", FeedID: testFeedID}
}

func allDayItem(uid, summary string, start, end time.Time) ical.Item {
	return ical.Item{
		UID:     uid,
		Summary: summary,
		Start:   ical.DateTime{AllDay: true, Time: start},
		End:     ical.DateTime{AllDay: true, Time: end},
		Status:  "CONFIRMED",
	}
}

func timedItem(uid, summary string, start, end time.Time) ical.Item {
	return ical.Item{
		UID:     uid,
		Summary: summary,
		Start:   ical.DateTime{Time: start},
		End:     ical.DateTime{Time: end},
		Status:  "CONFIRMED",
	}
}

// seedExisting inserts a feed-owned event into the stub as if a prior run had
// written it. checksum overrides the stored propChecksum so callers can force
// the unchanged (matching) or changed (stale) diff branch.
func seedExisting(im *Importer, s *stubAPI, it ical.Item, checksum string) *gws.Event {
	ev := im.buildEvent(it)
	ev.ID = DeterministicID(im.FeedID, it.UID)
	ev.ExtendedProperties.Private[propChecksum] = checksum
	s.events[ev.ID] = ev
	return ev
}

// --- tests ---------------------------------------------------------------

func TestReconcile_InsertNew(t *testing.T) {
	s := newStub()
	im := newImporter(s)

	spanStart := time.Date(2026, 7, 13, 0, 0, 0, 0, time.UTC)
	spanEnd := time.Date(2026, 7, 18, 0, 0, 0, 0, time.UTC)
	flightStart := time.Date(2026, 7, 13, 13, 0, 0, 0, time.UTC)
	flightEnd := time.Date(2026, 7, 13, 14, 44, 0, 0, time.UTC)

	items := []ical.Item{
		allDayItem("span-1", "Trip", spanStart, spanEnd),
		timedItem("flight-2", "Flight", flightStart, flightEnd),
	}

	res, err := im.Reconcile(context.Background(), items)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Inserted != 2 || res.Patched != 0 || res.Deleted != 0 || res.Unchanged != 0 {
		t.Fatalf("result = %+v, want Inserted=2 only", res)
	}
	if len(s.inserts) != 2 {
		t.Fatalf("got %d inserts, want 2", len(s.inserts))
	}

	byID := map[string]*gws.Event{}
	for _, ins := range s.inserts {
		byID[ins.body.ID] = ins.body
	}

	spanID := DeterministicID(testFeedID, "span-1")
	flightID := DeterministicID(testFeedID, "flight-2")

	span := byID[spanID]
	if span == nil {
		t.Fatalf("no insert with span deterministic ID %q", spanID)
	}
	if !strings.HasPrefix(span.ID, "csf") {
		t.Errorf("span ID %q missing csf prefix", span.ID)
	}
	if span.Start == nil || span.Start.Date != "2026-07-13" {
		t.Errorf("span start = %+v, want Date 2026-07-13", span.Start)
	}
	if span.End == nil || span.End.Date != "2026-07-18" {
		t.Errorf("span end = %+v, want Date 2026-07-18 (exclusive preserved)", span.End)
	}
	if span.Status != gws.EventStatusConfirmed {
		t.Errorf("span status = %q, want confirmed", span.Status)
	}

	// Extended props: feed namespace present, mirror namespace absent.
	priv := span.ExtendedProperties.Private
	if priv[propUID] != "span-1" {
		t.Errorf("propUID = %q, want span-1", priv[propUID])
	}
	if priv[propVersion] != schemaVersion {
		t.Errorf("propVersion = %q, want %q", priv[propVersion], schemaVersion)
	}
	if priv[propChecksum] == "" || !strings.HasPrefix(priv[propChecksum], "sha256:") {
		t.Errorf("propChecksum = %q, want sha256:...", priv[propChecksum])
	}
	for k := range priv {
		if strings.HasPrefix(k, "calendar-sync:") {
			t.Errorf("mirror-namespace key %q leaked onto feed event", k)
		}
	}

	flight := byID[flightID]
	if flight == nil {
		t.Fatalf("no insert with flight deterministic ID %q", flightID)
	}
	if flight.Start == nil || flight.Start.DateTime != "2026-07-13T13:00:00Z" {
		t.Errorf("flight start = %+v, want DateTime 2026-07-13T13:00:00Z", flight.Start)
	}
	if flight.Start.TimeZone != "UTC" {
		t.Errorf("flight start TZ = %q, want UTC", flight.Start.TimeZone)
	}
}

func TestReconcile_UnchangedSkip(t *testing.T) {
	s := newStub()
	im := newImporter(s)
	it := timedItem("evt-1", "Meeting",
		time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC))

	// Seed with the checksum buildEvent would compute -> matching -> unchanged.
	want := im.buildEvent(it)
	seedExisting(im, s, it, want.ExtendedProperties.Private[propChecksum])

	res, err := im.Reconcile(context.Background(), []ical.Item{it})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Unchanged != 1 {
		t.Fatalf("Unchanged = %d, want 1", res.Unchanged)
	}
	if len(s.patches) != 0 || len(s.inserts) != 0 || len(s.deletes) != 0 {
		t.Fatalf("expected zero writes, got inserts=%d patches=%d deletes=%d",
			len(s.inserts), len(s.patches), len(s.deletes))
	}
}

func TestReconcile_PatchChanged(t *testing.T) {
	s := newStub()
	im := newImporter(s)
	it := timedItem("evt-1", "New Summary",
		time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC))

	ev := seedExisting(im, s, it, "sha256:stale")
	id := ev.ID

	res, err := im.Reconcile(context.Background(), []ical.Item{it})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Patched != 1 {
		t.Fatalf("Patched = %d, want 1", res.Patched)
	}
	if len(s.patches) != 1 {
		t.Fatalf("got %d patches, want 1", len(s.patches))
	}
	p := s.patches[0]
	if p.id != id {
		t.Errorf("patched id = %q, want %q", p.id, id)
	}
	if p.body.Summary == nil || *p.body.Summary != "New Summary" {
		t.Errorf("patch summary = %v, want New Summary", p.body.Summary)
	}
	want := im.buildEvent(it)
	wantSum := want.ExtendedProperties.Private[propChecksum]
	if p.body.ExtendedProperties == nil || p.body.ExtendedProperties.Private[propChecksum] != wantSum {
		t.Errorf("patch checksum not rewritten to desired %q", wantSum)
	}
	// Managed-only patch: recurrence/visibility untouched.
	if p.body.Recurrence != nil || p.body.Visibility != nil {
		t.Errorf("patch touched recurrence/visibility: %+v", p.body)
	}
}

// TestReconcile_AbsentTransparencyDefaultsOpaque pins that a feed item without
// a TRANSP property (the common case - TripIt flights have none) maps to a
// VALID transparency enum on both insert and patch, never an empty string. An
// explicit "transparency":"" is rejected by the live Calendar API; PatchStr("")
// would serialize exactly that, so transparency() must default to opaque.
func TestReconcile_AbsentTransparencyDefaultsOpaque(t *testing.T) {
	flight := timedItem("flight-1", "Flight AB 100", // timedItem sets no Transparency
		time.Date(2026, 7, 13, 13, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 13, 14, 0, 0, 0, time.UTC))

	// Insert path.
	s := newStub()
	im := newImporter(s)
	if _, err := im.Reconcile(context.Background(), []ical.Item{flight}); err != nil {
		t.Fatalf("Reconcile (insert): %v", err)
	}
	if len(s.inserts) != 1 {
		t.Fatalf("got %d inserts, want 1", len(s.inserts))
	}
	if got := s.inserts[0].body.Transparency; got != gws.TransparencyOpaque {
		t.Errorf("inserted transparency = %q, want %q (absent TRANSP -> opaque)", got, gws.TransparencyOpaque)
	}

	// Patch path: seed a stale event so the same item forces a patch, and assert
	// the patch body carries a valid, non-empty transparency.
	s2 := newStub()
	im2 := newImporter(s2)
	seedExisting(im2, s2, flight, "sha256:stale")
	if _, err := im2.Reconcile(context.Background(), []ical.Item{flight}); err != nil {
		t.Fatalf("Reconcile (patch): %v", err)
	}
	if len(s2.patches) != 1 {
		t.Fatalf("got %d patches, want 1", len(s2.patches))
	}
	tp := s2.patches[0].body.Transparency
	if tp == nil {
		t.Fatal("patch transparency is nil, want a valid enum")
	}
	if *tp != gws.TransparencyOpaque {
		t.Errorf("patch transparency = %q, want %q (never empty)", *tp, gws.TransparencyOpaque)
	}
}

func TestReconcile_DeleteVanished(t *testing.T) {
	s := newStub()
	im := newImporter(s)
	gone := timedItem("gone-1", "Old",
		time.Date(2026, 7, 1, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC))
	ev := seedExisting(im, s, gone, "sha256:whatever")

	// Snapshot no longer contains gone-1.
	res, err := im.Reconcile(context.Background(), nil)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Deleted != 1 {
		t.Fatalf("Deleted = %d, want 1", res.Deleted)
	}
	if len(s.deletes) != 1 || s.deletes[0] != ev.ID {
		t.Fatalf("deletes = %v, want [%s]", s.deletes, ev.ID)
	}
}

func TestReconcile_DeleteSafety_FilterAndPresence(t *testing.T) {
	s := newStub()
	im := newImporter(s)

	// A non-feed event on the target (no propVersion). Must never be listed
	// or deleted.
	s.events["user-evt"] = &gws.Event{
		ID:      "user-evt",
		Status:  gws.EventStatusConfirmed,
		Summary: "Human event",
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{"calendar-sync:source": "somewhere"},
		},
	}

	// A feed-owned event that IS still present in the snapshot.
	present := timedItem("present-1", "Present",
		time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC))
	presentEv := im.buildEvent(present)
	seedExisting(im, s, present, presentEv.ExtendedProperties.Private[propChecksum])

	res, err := im.Reconcile(context.Background(), []ical.Item{present})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	// The list filter must scope to feed-owned events.
	if len(s.listParams) != 1 {
		t.Fatalf("got %d list calls, want 1", len(s.listParams))
	}
	// Both the version AND feed_id scopes must be applied: feed_id is what stops
	// one feed from deleting another feed's events on a shared target calendar.
	for _, wantFilter := range []string{
		propVersion + "=" + schemaVersion,
		propFeedID + "=" + testFeedID,
	} {
		found := false
		for _, f := range s.listParams[0].PrivateExtendedProperty {
			if f == wantFilter {
				found = true
			}
		}
		if !found {
			t.Errorf("list filter %v missing %q", s.listParams[0].PrivateExtendedProperty, wantFilter)
		}
	}
	if !s.listParams[0].SingleEvents {
		t.Errorf("list SingleEvents = false, want true")
	}

	// Nothing deleted: user event isn't feed-owned, present event still there.
	if res.Deleted != 0 || len(s.deletes) != 0 {
		t.Fatalf("Deleted = %d (%v), want 0", res.Deleted, s.deletes)
	}
	if _, ok := s.events["user-evt"]; !ok {
		t.Errorf("non-feed user event was deleted")
	}
}

// TestReconcile_DoesNotDeleteOtherFeedsEvents pins the FeedID delete scope: two
// feeds writing the same target calendar must not delete each other's events.
// feed-alpha reconciles a snapshot that does NOT mention feed-beta's event;
// without the propFeedID filter, feed-beta's event would be listed by alpha,
// found absent from alpha's snapshot, and wrongly deleted.
func TestReconcile_DoesNotDeleteOtherFeedsEvents(t *testing.T) {
	s := newStub()
	imAlpha := newImporter(s) // FeedID = testFeedID ("feed-alpha")
	imBeta := &Importer{API: s, Target: imAlpha.Target, FeedID: "feed-beta"}

	// A live event owned by feed-beta already sits on the shared target.
	betaItem := timedItem("beta-1", "Beta trip",
		time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC))
	betaEv := seedExisting(imBeta, s, betaItem, "sha256:beta")

	// feed-alpha reconciles its own (unrelated) snapshot.
	alphaItem := allDayItem("alpha-1", "Alpha trip",
		time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC),
		time.Date(2026, 9, 3, 0, 0, 0, 0, time.UTC))
	res, err := imAlpha.Reconcile(context.Background(), []ical.Item{alphaItem})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if res.Deleted != 0 || len(s.deletes) != 0 {
		t.Fatalf("feed-alpha deleted %d events %v, want 0 (feed-beta's event must survive)", res.Deleted, s.deletes)
	}
	if _, ok := s.events[betaEv.ID]; !ok {
		t.Error("feed-beta's event was deleted by feed-alpha")
	}
}

// TestReconcile_ListedEventMissingUIDIsSkipped: a feed-owned event that somehow
// lacks propUID must be skipped (not keyed under "", not deleted, not patched).
// It sits in the delete-decision path, so the skip is safety-critical.
func TestReconcile_ListedEventMissingUIDIsSkipped(t *testing.T) {
	s := newStub()
	im := newImporter(s)

	// Feed-owned (matches the version+feed_id filter) but has no propUID.
	s.events["orphan"] = &gws.Event{
		ID:     "orphan",
		Status: gws.EventStatusConfirmed,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{propVersion: schemaVersion, propFeedID: testFeedID},
		},
	}

	// A normal item so Reconcile still does work.
	it := timedItem("evt-1", "Meeting",
		time.Date(2026, 7, 13, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC))

	res, err := im.Reconcile(context.Background(), []ical.Item{it})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	// The orphan must not be deleted or patched; the normal item inserts.
	if len(s.deletes) != 0 {
		t.Errorf("deletes = %v, want none (missing-uid event must not be deleted)", s.deletes)
	}
	if _, ok := s.events["orphan"]; !ok {
		t.Error("missing-uid event was deleted")
	}
	if res.Inserted != 1 {
		t.Errorf("Inserted = %d, want 1", res.Inserted)
	}
}

func TestReconcile_CancelledItemSkippedAndDeleted(t *testing.T) {
	s := newStub()
	im := newImporter(s)

	// Cancelled feed item that does NOT yet exist -> skipped, no insert.
	cancelledNew := timedItem("cancel-new", "Cancelled",
		time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC))
	cancelledNew.Status = "CANCELLED"

	// Cancelled feed item that DOES exist (feed-owned, alive) -> treated as
	// absent from desired -> deleted.
	cancelledExisting := timedItem("cancel-old", "Was Alive",
		time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC))
	existingEv := seedExisting(im, s, cancelledExisting, "sha256:x")
	cancelledExisting.Status = "CANCELLED"

	res, err := im.Reconcile(context.Background(), []ical.Item{cancelledNew, cancelledExisting})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Skipped != 2 {
		t.Fatalf("Skipped = %d, want 2 (both cancelled)", res.Skipped)
	}
	if len(s.inserts) != 0 {
		t.Fatalf("cancelled item was inserted: %d inserts", len(s.inserts))
	}
	if res.Deleted != 1 || len(s.deletes) != 1 || s.deletes[0] != existingEv.ID {
		t.Fatalf("deletes = %v, want [%s]", s.deletes, existingEv.ID)
	}
}

func TestReconcile_DryRun(t *testing.T) {
	s := newStub()
	im := newImporter(s)
	im.DryRun = true

	// One insert, one patch, one delete needed.
	insertItem := timedItem("ins-1", "Insert",
		time.Date(2026, 7, 10, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 10, 10, 0, 0, 0, time.UTC))
	patchItem := timedItem("pat-1", "Patch",
		time.Date(2026, 7, 11, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 11, 10, 0, 0, 0, time.UTC))
	seedExisting(im, s, patchItem, "sha256:stale")
	gone := timedItem("del-1", "Delete",
		time.Date(2026, 7, 12, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC))
	seedExisting(im, s, gone, "sha256:x")

	res, err := im.Reconcile(context.Background(), []ical.Item{insertItem, patchItem})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Inserted != 1 || res.Patched != 1 || res.Deleted != 1 {
		t.Fatalf("result = %+v, want Inserted=1 Patched=1 Deleted=1", res)
	}
	if len(s.inserts) != 0 || len(s.patches) != 0 || len(s.deletes) != 0 {
		t.Fatalf("DryRun issued writes: inserts=%d patches=%d deletes=%d",
			len(s.inserts), len(s.patches), len(s.deletes))
	}
}

func TestReconcile_409Revive(t *testing.T) {
	s := newStub()
	im := newImporter(s)
	it := timedItem("revive-1", "Back From Cancel",
		time.Date(2026, 7, 20, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC))
	id := DeterministicID(testFeedID, "revive-1")

	// A cancelled leftover already occupies the deterministic ID. It is NOT
	// returned by the list (ShowDeleted:false), so the importer tries insert,
	// gets 409, then recovers.
	s.events[id] = &gws.Event{
		ID:     id,
		Status: gws.EventStatusCancelled,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{propUID: "revive-1", propVersion: schemaVersion, propChecksum: "sha256:old"},
		},
	}
	s.insertConflictIDs[id] = true

	res, err := im.Reconcile(context.Background(), []ical.Item{it})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.Inserted != 1 {
		t.Fatalf("Inserted = %d, want 1 (revive counts as insert)", res.Inserted)
	}
	if len(s.patches) != 1 {
		t.Fatalf("got %d patches, want 1 (revive patch)", len(s.patches))
	}
	p := s.patches[0]
	if p.id != id {
		t.Errorf("revive patched id = %q, want %q", p.id, id)
	}
	if p.body.Status == nil || *p.body.Status != gws.EventStatusConfirmed {
		t.Errorf("revive patch status = %v, want confirmed", p.body.Status)
	}
	if s.events[id].Status != gws.EventStatusConfirmed {
		t.Errorf("post-revive stored status = %q, want confirmed", s.events[id].Status)
	}
}

// Guard against accidental interface drift: *gws.Client must satisfy EventsAPI.
var _ EventsAPI = (*gws.Client)(nil)

// Guard: list-error is a hard error (no diff/delete attempted).
func TestReconcile_ListErrorIsHard(t *testing.T) {
	s := &errListAPI{}
	im := newImporter(s)
	_, err := im.Reconcile(context.Background(), []ical.Item{
		timedItem("x", "X", time.Now(), time.Now().Add(time.Hour)),
	})
	if err == nil {
		t.Fatal("want error from list failure")
	}
	if !errors.Is(err, gws.ErrBackendError) {
		t.Errorf("err = %v, want wrapped backend error", err)
	}
}

type errListAPI struct{ stubAPI }

func (e *errListAPI) EventsList(context.Context, gws.EventsListParams) ([]gws.Event, string, error) {
	return nil, "", &gws.Error{Code: gws.CodeBackendError, ExitCode: 1}
}
