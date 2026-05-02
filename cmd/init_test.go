package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/config"
)

func TestInitCmd_WritesStarterConfig(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "config.toml")

	stdout := &bytes.Buffer{}
	rt := &Runtime{Stdout: stdout, Stderr: &bytes.Buffer{}}

	if err := (&InitCmd{Output: dest}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	written, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read written file: %v", err)
	}
	if !strings.Contains(string(written), "[[pairs]]") {
		t.Errorf("written file missing [[pairs]] section:\n%s", written)
	}

	var line struct {
		Path   string `json:"path"`
		Status string `json:"status"`
	}
	first := strings.SplitN(stdout.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &line); err != nil {
		t.Fatalf("unmarshal first line %q: %v", first, err)
	}
	if line.Path != dest {
		t.Errorf("path = %q, want %q", line.Path, dest)
	}
	if line.Status != "created" {
		t.Errorf("status = %q, want %q", line.Status, "created")
	}
}

func TestInitCmd_RefusesOverwriteWithoutForce(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(dest, []byte("# existing"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rt := &Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	err := (&InitCmd{Output: dest}).Run(rt)
	if err == nil {
		t.Fatalf("expected config_exists error")
	}
	code, _, _ := MapError(err)
	if code != "config_exists" {
		t.Errorf("code = %q, want config_exists", code)
	}
	got, _ := os.ReadFile(dest)
	if string(got) != "# existing" {
		t.Errorf("file was overwritten without --force; got %q", got)
	}
}

func TestInitCmd_ForceOverwrites(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(dest, []byte("# old"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}

	rt := &Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := (&InitCmd{Output: dest, Force: true}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}
	got, _ := os.ReadFile(dest)
	if !strings.Contains(string(got), "[[pairs]]") {
		t.Errorf("expected fresh content with --force, got %q", got)
	}
}

// TestInitCmd_StarterConfigPassesValidation pins the starter template
// against the validation rules: a freshly-installed `calendar-sync init`
// config must `config.Load` cleanly and pass `Validate()` without
// modification. Post-v2.0.0 the `direction` field is explicitly rejected
// at validation time, so a template that still emits `direction = "..."`
// would ship a broken default.
func TestInitCmd_StarterConfigPassesValidation(t *testing.T) {
	dir := t.TempDir()
	dest := filepath.Join(dir, "config.toml")

	rt := &Runtime{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}}
	if err := (&InitCmd{Output: dest}).Run(rt); err != nil {
		t.Fatalf("Run: %v", err)
	}

	cfg, err := config.Load(dest)
	if err != nil {
		t.Fatalf("config.Load on starter template: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate on starter template: %v", err)
	}
}
