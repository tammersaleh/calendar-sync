package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// validConfigTOML is the smallest config that passes Validate. Direction
// uses target_to_source so canonicalize only requires writer access on the
// "primary" side.
const validConfigTOML = `
[settings]
poll_interval      = "60s"
horizon            = "365d"
full_sync_interval = "24h"
log_level          = "info"
log_format         = "json"

[[pairs]]
name      = "work-personal"
direction = "bidirectional"
source    = "work@example.com"
target    = "personal@example.com"
`

// stubGws is a hand-rolled GwsClient that satisfies sync.API +
// config.CalendarLister with zero-value responses. Subcommand tests that
// don't exercise the gws path inject this to avoid spawning the real
// binary.
type stubGws struct {
	calendars map[string]*gws.CalendarListEntry
}

func (s *stubGws) CalendarListGet(_ context.Context, id string) (*gws.CalendarListEntry, error) {
	if s.calendars == nil {
		return &gws.CalendarListEntry{ID: id, AccessRole: "owner"}, nil
	}
	if e, ok := s.calendars[id]; ok {
		return e, nil
	}
	return &gws.CalendarListEntry{ID: id, AccessRole: "owner"}, nil
}

func (s *stubGws) EventsList(context.Context, gws.EventsListParams) ([]gws.Event, string, error) {
	return nil, "", nil
}

func (s *stubGws) EventsGet(context.Context, string, string) (*gws.Event, error) {
	return nil, nil
}

func (s *stubGws) EventsInstances(context.Context, gws.EventsInstancesParams) ([]gws.Event, error) {
	return nil, nil
}

func (s *stubGws) EventsInsert(context.Context, string, *gws.Event) (*gws.Event, error) {
	return nil, nil
}

func (s *stubGws) EventsPatch(context.Context, string, string, *gws.Event) (*gws.Event, error) {
	return nil, nil
}

func (s *stubGws) EventsDelete(context.Context, string, string) error {
	return nil
}

func writeConfigFixture(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return path
}

func TestConfigShowCmd_EmitsResolvedSettings(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	if err := (&ConfigShowCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var payload configShowPayload
	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &payload); err != nil {
		t.Fatalf("unmarshal: %v\nstdout=%s", err, stdout.String())
	}
	if payload.Settings.PollInterval != "60s" {
		t.Errorf("poll_interval = %q, want 60s", payload.Settings.PollInterval)
	}
	if payload.Settings.FullSyncInterval != "24h" {
		t.Errorf("full_sync_interval = %q, want 24h", payload.Settings.FullSyncInterval)
	}
	if len(payload.Pairs) != 1 || payload.Pairs[0].Name != "work-personal" {
		t.Errorf("pairs = %+v, want one pair named work-personal", payload.Pairs)
	}
}

func TestConfigShowCmd_CanonicalizeResolvesSource(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws: &stubGws{
			calendars: map[string]*gws.CalendarListEntry{
				"work@example.com":     {ID: "work@example.com", AccessRole: "owner"},
				"personal@example.com": {ID: "personal@example.com", AccessRole: "owner"},
			},
		},
	}
	if err := (&ConfigShowCmd{Canonicalize: true}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var payload configShowPayload
	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(payload.Pairs) != 1 {
		t.Fatalf("want 1 pair, got %d", len(payload.Pairs))
	}
	if payload.Pairs[0].Source != "work@example.com" {
		t.Errorf("source = %q, want work@example.com", payload.Pairs[0].Source)
	}
}

func TestConfigShowCmd_MissingFileMapsToConfigNotFound(t *testing.T) {
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: "/nonexistent/config.toml"},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	err := (&ConfigShowCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected error")
	}
	code, _, _ := MapError(err)
	if code != "config_not_found" {
		t.Errorf("code = %q, want config_not_found", code)
	}
}

func TestConfigValidateCmd_OK(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	if err := (&ConfigValidateCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	var payload configValidatePayload
	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.Status != "ok" {
		t.Errorf("status = %q, want ok", payload.Status)
	}
	if payload.Pairs != 1 {
		t.Errorf("pairs = %d, want 1", payload.Pairs)
	}
	if payload.PDirs != 2 {
		t.Errorf("pdirs = %d, want 2 (bidirectional expands to 2)", payload.PDirs)
	}
}

func TestConfigValidateCmd_BadConfigMapsToConfigInvalid(t *testing.T) {
	const bad = `
[settings]
poll_interval = "1s"
horizon = "365d"
full_sync_interval = "24h"
log_level = "info"
log_format = "json"
`
	path := writeConfigFixture(t, bad)
	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	err := (&ConfigValidateCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected validation error")
	}
	code, _, _ := MapError(err)
	if code != "config_invalid" {
		t.Errorf("code = %q, want config_invalid", code)
	}
}
