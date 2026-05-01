package cmd

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestVersionCmd_DefaultEmitsJSONL(t *testing.T) {
	stdout := &bytes.Buffer{}
	rt := &Runtime{Stdout: stdout, Stderr: &bytes.Buffer{}}

	if err := (&VersionCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	lines := strings.Split(strings.TrimRight(stdout.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (payload + _meta), got %d:\n%s", len(lines), stdout.String())
	}

	var payload versionPayload
	if err := json.Unmarshal([]byte(lines[0]), &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Version == "" {
		t.Errorf("Version field is empty")
	}
	if payload.Go == "" {
		t.Errorf("Go field is empty (should be runtime.Version())")
	}

	var meta struct {
		Meta metaCount `json:"_meta"`
	}
	if err := json.Unmarshal([]byte(lines[1]), &meta); err != nil {
		t.Fatalf("unmarshal meta: %v", err)
	}
	if meta.Meta.Count != 1 {
		t.Errorf("meta.count = %d, want 1", meta.Meta.Count)
	}
}

func TestVersionCmd_ShortEmitsBareString(t *testing.T) {
	stdout := &bytes.Buffer{}
	rt := &Runtime{Stdout: stdout, Stderr: &bytes.Buffer{}}

	prevVersion := Version
	t.Cleanup(func() { Version = prevVersion })
	Version = "1.2.3"

	if err := (&VersionCmd{Short: true}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got := stdout.String()
	if got != "1.2.3\n" {
		t.Errorf("--short output = %q, want %q", got, "1.2.3\n")
	}
}

func TestVersionCmd_QuietSuppresses(t *testing.T) {
	stdout := &bytes.Buffer{}
	rt := &Runtime{Stdout: nil, Stderr: stdout}

	if err := (&VersionCmd{}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stdout.Len() != 0 {
		t.Errorf("nil stdout should suppress output, got %q", stdout.String())
	}
}
