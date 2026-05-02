package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// inventoryGws returns canned inventory rows for EventsList queries that
// match the v2/v1 schema-version filter; otherwise empty.
type inventoryGws struct {
	stubGws
	mu       sync.Mutex
	v2events []gws.Event
	v1events []gws.Event
	deletes  []deleteCall
	get      map[string]*gws.Event
}

type deleteCall struct {
	calendarID string
	eventID    string
}

func (i *inventoryGws) EventsList(_ context.Context, params gws.EventsListParams) ([]gws.Event, string, error) {
	for _, p := range params.PrivateExtendedProperty {
		if strings.HasSuffix(p, "=2") {
			return i.v2events, "", nil
		}
		if strings.HasSuffix(p, "=1") {
			return i.v1events, "", nil
		}
	}
	return nil, "", nil
}

func (i *inventoryGws) EventsGet(_ context.Context, _, eventID string) (*gws.Event, error) {
	if e, ok := i.get[eventID]; ok {
		return e, nil
	}
	return nil, gws.ErrAPINotFound
}

func (i *inventoryGws) EventsDelete(_ context.Context, calendarID, eventID string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.deletes = append(i.deletes, deleteCall{calendarID, eventID})
	return nil
}

func makeMirrorEvent(id, sourceCal, sourceID, summary string) gws.Event {
	return gws.Event{
		ID:      id,
		Summary: summary,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:  (mirror.SourceTuple{CalendarID: sourceCal, EventID: sourceID}).String(),
				mirror.ExtKeyVersion: mirror.SchemaVersion,
			},
		},
	}
}

func TestMirrorListCmd_EmitsOneLinePerMirror(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	gwsClient := &inventoryGws{
		v2events: []gws.Event{
			makeMirrorEvent("m1", "work@example.com", "src1", "Standup"),
			makeMirrorEvent("m2", "work@example.com", "src2", "Lunch"),
		},
	}
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     gwsClient,
	}
	if err := (&MirrorListCmd{Calendar: "personal@example.com"}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 2 mirror lines + meta, got %d:\n%s", len(lines), stdout.String())
	}
	var m1 mirrorRow
	if err := json.Unmarshal([]byte(lines[0]), &m1); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m1.ID != "m1" {
		t.Errorf("first mirror id = %q, want m1", m1.ID)
	}
}

func TestMirrorPruneCmd_RequiresExactlyOneSelector(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	err := (&MirrorPruneCmd{Calendar: "primary"}).Run(rt)
	if err == nil {
		t.Fatalf("expected selector_required")
	}
	code, _, _ := MapError(err)
	if code != "selector_required" {
		t.Errorf("code = %q, want selector_required", code)
	}
}

func TestMirrorPruneCmd_AllDeletesEveryMirror(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	gwsClient := &inventoryGws{
		v2events: []gws.Event{
			makeMirrorEvent("m1", "work@example.com", "src1", "Standup"),
			makeMirrorEvent("m2", "work@example.com", "src2", "Lunch"),
		},
	}
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     gwsClient,
	}
	cmd := &MirrorPruneCmd{Calendar: "personal@example.com", All: true, Yes: true}
	if err := cmd.Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gwsClient.deletes) != 2 {
		t.Errorf("deletes = %d, want 2", len(gwsClient.deletes))
	}
}

func TestMirrorPruneCmd_DryRunDoesNotCallDelete(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	gwsClient := &inventoryGws{
		v2events: []gws.Event{
			makeMirrorEvent("m1", "work@example.com", "src1", "Standup"),
		},
	}
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     gwsClient,
	}
	cmd := &MirrorPruneCmd{Calendar: "personal@example.com", All: true, DryRun: true}
	if err := cmd.Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gwsClient.deletes) != 0 {
		t.Errorf("dry-run made %d delete calls, want 0", len(gwsClient.deletes))
	}
	if !strings.Contains(stdout.String(), "would_delete") {
		t.Errorf("expected would_delete action in output, got %q", stdout.String())
	}
}

// TestMirrorPruneCmd_PruneHorizonFiltersByStart pins the --prune-horizon
// behavior used for the user's phased mirror-cleanup workflow. With
// --prune-horizon=24h, only mirrors whose start falls in [now, now+24h]
// are deleted; past events, future-out-of-window events, and events
// without a parseable start are skipped.
//
// Boundaries are inclusive on both ends so a phased rollout that bumps
// the horizon by exactly the previous horizon's value (1d, 2d, 3d...)
// always covers the seam.
func TestMirrorPruneCmd_PruneHorizonFiltersByStart(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)

	fixedNow, err := time.Parse(time.RFC3339, "2026-05-01T12:00:00Z")
	if err != nil {
		t.Fatalf("parse fixedNow: %v", err)
	}

	withStart := func(id, dt string) gws.Event {
		ev := makeMirrorEvent(id, "work@example.com", "src-"+id, id)
		ev.Start = &gws.EventDateTime{DateTime: dt}
		return ev
	}
	noStart := makeMirrorEvent("nostart", "work@example.com", "src-nostart", "nostart")

	gwsClient := &inventoryGws{
		v2events: []gws.Event{
			withStart("past", "2026-04-30T12:00:00Z"),       // -24h - excluded
			withStart("at-now", "2026-05-01T12:00:00Z"),     // exactly now - included (lower edge)
			withStart("in-window", "2026-05-01T18:00:00Z"),  // +6h - included
			withStart("at-end", "2026-05-02T12:00:00Z"),     // exactly now+24h - included (upper edge)
			withStart("just-past", "2026-05-02T12:00:01Z"),  // +24h+1s - excluded
			withStart("future", "2026-05-03T12:00:00Z"),     // +48h - excluded
			noStart,                                          // no start - excluded
		},
	}
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     gwsClient,
	}
	cmd := &MirrorPruneCmd{
		Calendar:     "personal@example.com",
		All:          true,
		PruneHorizon: 24 * time.Hour,
		Yes:          true,
		now:          func() time.Time { return fixedNow },
	}
	if err := cmd.Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := make([]string, 0, len(gwsClient.deletes))
	for _, d := range gwsClient.deletes {
		got = append(got, d.eventID)
	}
	sort.Strings(got)
	want := []string{"at-end", "at-now", "in-window"}
	if !equalStringSlices(got, want) {
		t.Errorf("deleted = %v, want %v", got, want)
	}
}

// TestMirrorPruneCmd_SkipsAlreadyCancelledEvents pins B10: prune must not
// attempt to delete events that already have status=cancelled (the
// tombstone state of a previously-deleted event). Calendar API responds
// to such deletes with `api_invalid_request: Resource has been deleted`,
// which the prune handler doesn't treat as "already gone" and would
// abort the function early - leaving the remaining live mirrors
// undeleted.
//
// The fix filters status=cancelled events out of the candidate list
// entirely. They're already deleted; nothing to do.
func TestMirrorPruneCmd_SkipsAlreadyCancelledEvents(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)

	mkConfirmed := func(id, srcID string) gws.Event {
		ev := makeMirrorEvent(id, "work@example.com", srcID, id)
		ev.Status = gws.EventStatusConfirmed
		return ev
	}
	mkCancelled := func(id, srcID string) gws.Event {
		ev := makeMirrorEvent(id, "work@example.com", srcID, id)
		ev.Status = gws.EventStatusCancelled
		return ev
	}

	gwsClient := &inventoryGws{
		v2events: []gws.Event{
			mkConfirmed("live1", "src1"),
			mkCancelled("tomb1", "src2"),
			mkConfirmed("live2", "src3"),
			mkCancelled("tomb2", "src4"),
		},
	}
	cmd := &MirrorPruneCmd{
		Calendar: "personal@example.com",
		All:      true,
		Yes:      true,
	}
	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     gwsClient,
	}
	if err := cmd.Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	got := make([]string, 0, len(gwsClient.deletes))
	for _, d := range gwsClient.deletes {
		got = append(got, d.eventID)
	}
	sort.Strings(got)
	want := []string{"live1", "live2"}
	if !equalStringSlices(got, want) {
		t.Errorf("deleted = %v, want %v (cancelled events must be skipped)", got, want)
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
