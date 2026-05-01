package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

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
