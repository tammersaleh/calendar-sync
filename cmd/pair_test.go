package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestPairListCmd_EmitsOneLinePerPair(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	if err := (&PairListCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (1 pair + meta), got %d:\n%s", len(lines), stdout.String())
	}
	var p pairPayload
	if err := json.Unmarshal([]byte(lines[0]), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Name != "work-personal" {
		t.Errorf("name = %q, want work-personal", p.Name)
	}
}

func TestPairListCmd_EnabledOnlyFiltersDisabled(t *testing.T) {
	const cfg = `
[settings]
poll_interval = "60s"
horizon = "365d"
full_sync_interval = "24h"
log_level = "info"
log_format = "json"

[[pairs]]
name = "p1"
source = "a@example.com"
target = "b@example.com"

[[pairs]]
name = "p2"
source = "c@example.com"
target = "d@example.com"
enabled = false
`
	path := writeConfigFixture(t, cfg)
	stdout := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  stdout,
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	if err := (&PairListCmd{EnabledOnly: true}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (1 enabled pair + meta), got %d:\n%s", len(lines), stdout.String())
	}
	var p pairPayload
	if err := json.Unmarshal([]byte(lines[0]), &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if p.Name != "p1" {
		t.Errorf("name = %q, want p1", p.Name)
	}
}

func TestPairTestCmd_UnknownPairMapsToPairNotFound(t *testing.T) {
	path := writeConfigFixture(t, validConfigTOML)
	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  &bytes.Buffer{},
		Globals: Globals{Config: path},
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	err := (&PairTestCmd{Name: "nonexistent"}).Run(rt)
	if err == nil {
		t.Fatalf("expected pair_not_found")
	}
	code, _, _ := MapError(err)
	if code != "pair_not_found" {
		t.Errorf("code = %q, want pair_not_found", code)
	}
}
