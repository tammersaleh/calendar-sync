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

func TestBuildInventory_V2Only(t *testing.T) {
	api := newStubAPI()
	m1 := makeMirrorWithSource("m1", "src-cal:src-evt-A", "2")
	m2 := makeMirrorWithSource("m2", "src-cal:src-evt-B", "2")
	api.queueList([]gws.Event{*m1, *m2}, "")
	api.queueList(nil, "") // no v1 mirrors

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
	// The two events.list calls should carry the v=2 then v=1 filter.
	listCalls := api.callsByOp("EventsList")
	if len(listCalls) != 2 {
		t.Fatalf("expected 2 EventsList calls; got %d", len(listCalls))
	}
	if !reflect.DeepEqual(listCalls[0].ListParams.PrivateExtendedProperty, []string{mirror.ExtKeyVersion + "=2"}) {
		t.Errorf("first list filter = %v, want [calendar-sync:version=2]", listCalls[0].ListParams.PrivateExtendedProperty)
	}
	if !reflect.DeepEqual(listCalls[1].ListParams.PrivateExtendedProperty, []string{mirror.ExtKeyVersion + "=1"}) {
		t.Errorf("second list filter = %v, want [calendar-sync:version=1]", listCalls[1].ListParams.PrivateExtendedProperty)
	}
	if !listCalls[0].ListParams.ShowDeleted || !listCalls[1].ListParams.ShowDeleted {
		t.Errorf("inventory rebuild must request showDeleted=true; got %+v / %+v", listCalls[0].ListParams.ShowDeleted, listCalls[1].ListParams.ShowDeleted)
	}
}

func TestBuildInventory_V1Only(t *testing.T) {
	api := newStubAPI()
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

func TestBuildInventory_MixedV2andV1(t *testing.T) {
	api := newStubAPI()
	v2 := makeMirrorWithSource("m2", "src-cal:evtA", "2")
	api.queueList([]gws.Event{*v2}, "")
	v1 := makeMirrorWithSource("m1", "src-cal:evtB", "1")
	api.queueList([]gws.Event{*v1}, "")

	inv, err := BuildInventory(context.Background(), api, "tgt-cal", nil)
	if err != nil {
		t.Fatalf("BuildInventory error: %v", err)
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "evtA"}); !ok {
		t.Errorf("v2 mirror missing")
	}
	if _, ok := inv.Lookup(mirror.SourceTuple{CalendarID: "src-cal", EventID: "evtB"}); !ok {
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
			mirror.ExtKeyVersion: "2",
		}},
	}
	malformed := &gws.Event{
		ID: "malformed",
		ExtendedProperties: &gws.ExtendedProperties{Private: map[string]string{
			mirror.ExtKeySource:  "no-colon-here",
			mirror.ExtKeyVersion: "2",
		}},
	}
	good := makeMirrorWithSource("good", "src-cal:src-evt-A", "2")
	api.queueList([]gws.Event{*missing, *emptySource, *malformed, *good}, "")
	api.queueList(nil, "")

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
	live := makeMirrorWithSource("live", "src-cal:src-evt-A", "2")
	tomb := makeMirrorWithSource("tomb", "src-cal:src-evt-B", "2")
	tomb.Status = gws.EventStatusCancelled
	api.queueList([]gws.Event{*live, *tomb}, "")
	api.queueList(nil, "")

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
