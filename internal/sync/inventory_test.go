package sync

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// makeMirrorWithSource builds a mirror Event with the calendar-sync extended
// properties enough for inventory parsing to succeed. Tests that need the
// full payload (checksum, source_updated, etc.) build mirrors directly.
func makeMirrorWithSource(id, source string, version string) *gws.Event {
	return &gws.Event{
		ID:     id,
		Status: gws.EventStatusConfirmed,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:  source,
				mirror.ExtKeyVersion: version,
			},
		},
	}
}

func TestBuildInventory_CurrentVersionOnly(t *testing.T) {
	api := newStubAPI()
	m1 := makeMirrorWithSource("m1", "src-cal:src-evt-A", mirror.SchemaVersion)
	m2 := makeMirrorWithSource("m2", "src-cal:src-evt-B", mirror.SchemaVersion)
	api.queueList([]gws.Event{*m1, *m2}, "")
	api.queueList(nil, "") // no v2 legacy mirrors
	api.queueList(nil, "") // no v1 legacy mirrors

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if got, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-A"}); !ok || got.ID != "m1" {
		t.Errorf("expected m1 indexed by src-cal:src-evt-A; got=%v ok=%v", got, ok)
	}
	if got, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-B"}); !ok || got.ID != "m2" {
		t.Errorf("expected m2 indexed by src-cal:src-evt-B; got=%v ok=%v", got, ok)
	}
	// One events.list call per known schema version: current first, then
	// each legacy version still in the wild. The order is current -> "2"
	// -> "1" so a re-bump in the future stays self-documenting.
	listCalls := api.callsByOp("EventsList")
	if len(listCalls) != 3 {
		t.Fatalf("expected 3 EventsList calls (current + 2 legacy); got %d", len(listCalls))
	}
	wantFilters := [][]string{
		{mirror.ExtKeyVersion + "=" + mirror.SchemaVersion},
		{mirror.ExtKeyVersion + "=2"},
		{mirror.ExtKeyVersion + "=1"},
	}
	for i, want := range wantFilters {
		if !reflect.DeepEqual(listCalls[i].ListParams.PrivateExtendedProperty, want) {
			t.Errorf("list[%d] filter = %v, want %v", i, listCalls[i].ListParams.PrivateExtendedProperty, want)
		}
		if !listCalls[i].ListParams.ShowDeleted {
			t.Errorf("list[%d] must request showDeleted=true", i)
		}
	}
}

func TestBuildInventory_V1Only(t *testing.T) {
	api := newStubAPI()
	api.queueList(nil, "") // current empty
	api.queueList(nil, "") // v=2 empty
	v1 := makeMirrorWithSource("v1mirror", "src-cal:src-evt-X", "1")
	api.queueList([]gws.Event{*v1}, "")

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if got, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-X"}); !ok || got.ID != "v1mirror" {
		t.Errorf("expected v1 mirror indexed; got=%v ok=%v", got, ok)
	}
}

func TestBuildInventory_V2Only(t *testing.T) {
	// v2 mirrors lack the location field but still carry a checksum + source
	// tuple. They get indexed and are routed through the migration path at
	// reconciliation time.
	api := newStubAPI()
	api.queueList(nil, "") // current empty
	v2 := makeMirrorWithSource("v2mirror", "src-cal:src-evt-Y", "2")
	api.queueList([]gws.Event{*v2}, "")
	api.queueList(nil, "") // v1 empty

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if got, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-Y"}); !ok || got.ID != "v2mirror" {
		t.Errorf("expected v2 mirror indexed; got=%v ok=%v", got, ok)
	}
}

func TestBuildInventory_MixedCurrentAndLegacy(t *testing.T) {
	api := newStubAPI()
	cur := makeMirrorWithSource("mcur", "src-cal:evtA", mirror.SchemaVersion)
	api.queueList([]gws.Event{*cur}, "")
	v2 := makeMirrorWithSource("mv2", "src-cal:evtB", "2")
	api.queueList([]gws.Event{*v2}, "")
	v1 := makeMirrorWithSource("mv1", "src-cal:evtC", "1")
	api.queueList([]gws.Event{*v1}, "")

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "evtA"}); !ok {
		t.Errorf("current-version mirror missing")
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "evtB"}); !ok {
		t.Errorf("v2 mirror missing")
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "evtC"}); !ok {
		t.Errorf("v1 mirror missing")
	}
}

func TestBuildInventory_ListErrorPropagates(t *testing.T) {
	api := newStubAPI()
	api.queueListErr(errors.New("calendar API kaboom"))

	_, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err == nil {
		t.Fatal("expected error from EventsList failure")
	}
}

func TestBuildInventory_SkipsMirrorsMissingSourceTuple(t *testing.T) {
	// Mirrors with no source tuple, malformed source tuple, or no extended
	// properties at all are silently dropped from the inventory.
	api := newStubAPI()
	missing := &gws.Event{ID: "no-ext", Status: gws.EventStatusConfirmed}
	emptySource := &gws.Event{
		ID: "empty-src",
		ExtendedProperties: &gws.ExtendedProperties{Private: map[string]string{
			mirror.ExtKeyVersion: mirror.SchemaVersion,
		}},
	}
	malformed := &gws.Event{
		ID: "malformed",
		ExtendedProperties: &gws.ExtendedProperties{Private: map[string]string{
			mirror.ExtKeySource:  "no-colon-here",
			mirror.ExtKeyVersion: mirror.SchemaVersion,
		}},
	}
	good := makeMirrorWithSource("good", "src-cal:src-evt-A", mirror.SchemaVersion)
	api.queueList([]gws.Event{*missing, *emptySource, *malformed, *good}, "")
	api.queueList(nil, "") // v=2 empty
	api.queueList(nil, "") // v=1 empty

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if len(inv.All()) != 1 {
		t.Errorf("expected only the parseable mirror to be indexed; got %d entries", len(inv.All()))
	}
}

// TestBuildInventory_SkipsCancelledTombstones pins B11: mirrors with
// status=cancelled (Calendar API tombstones from a previous events.delete)
// must not enter the inventory. They reach the listing because we pass
// ShowDeleted:true to surface them for the cancelled-and-revived path,
// but indexing them here would have downstream effects: the orphan walk
// would try to delete them again (Calendar API responds with
// api_invalid_request "Resource has been deleted", which the walker
// surfaces as a partial_failure), and the standard reconcile path
// would treat them as live mirrors needing drift checks.
//
// SPEC's "Cancelled-and-revived" flow doesn't depend on the inventory
// holding tombstones - revival is triggered by 409 on insert, then a
// per-event events.get to inspect status.
func TestBuildInventory_SkipsCancelledTombstones(t *testing.T) {
	api := newStubAPI()
	live := makeMirrorWithSource("live", "src-cal:src-evt-A", mirror.SchemaVersion)
	tomb := makeMirrorWithSource("tomb", "src-cal:src-evt-B", mirror.SchemaVersion)
	tomb.Status = gws.EventStatusCancelled
	api.queueList([]gws.Event{*live, *tomb}, "")
	api.queueList(nil, "") // v=2 empty
	api.queueList(nil, "") // v=1 empty

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if got := len(inv.All()); got != 1 {
		t.Errorf("expected only the live mirror to be indexed; got %d entries", got)
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-B"}); ok {
		t.Errorf("cancelled tombstone src-evt-B must NOT be in inventory")
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt-A"}); !ok {
		t.Errorf("live mirror src-evt-A must remain in inventory")
	}
}

func TestInventory_LookupSetDelete(t *testing.T) {
	inv := NewInventory("tgt-cal")
	if inv.Target() != "tgt-cal" {
		t.Errorf("Target = %q, want tgt-cal", inv.Target())
	}
	tuple := mirror.SourceTuple{CalendarID: "s", EventID: "e"}
	if _, ok := inv.Lookup(tuple); ok {
		t.Errorf("empty inventory shouldn't have %v", tuple)
	}
	e := &gws.Event{ID: "m1"}
	inv.Set(tuple, e)
	got, ok := inv.Lookup(tuple)
	if !ok || got.ID != "m1" {
		t.Errorf("Lookup after Set: got=%v ok=%v", got, ok)
	}
	inv.Delete(tuple)
	if _, ok := inv.Lookup(tuple); ok {
		t.Errorf("Delete didn't remove %v", tuple)
	}
}

func TestInventory_TuplesSortedAndAllAligned(t *testing.T) {
	inv := NewInventory("tgt-cal")
	tuples := []mirror.SourceTuple{
		{CalendarID: "calB", EventID: "ev2"},
		{CalendarID: "calA", EventID: "ev1"},
		{CalendarID: "calA", EventID: "ev0"},
	}
	for i, t0 := range tuples {
		inv.Set(t0, &gws.Event{ID: tuples[i].CalendarID + "-" + tuples[i].EventID})
	}
	got := inv.Tuples()
	want := []mirror.SourceTuple{
		{CalendarID: "calA", EventID: "ev0"},
		{CalendarID: "calA", EventID: "ev1"},
		{CalendarID: "calB", EventID: "ev2"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Tuples = %v, want %v", got, want)
	}
	all := inv.All()
	if len(all) != 3 {
		t.Fatalf("All length = %d, want 3", len(all))
	}
	// All must align with Tuples ordering.
	for i, ev := range all {
		expected := want[i].CalendarID + "-" + want[i].EventID
		if ev.ID != expected {
			t.Errorf("All[%d].ID = %q, want %q", i, ev.ID, expected)
		}
	}
}
