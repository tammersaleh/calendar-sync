package cmd

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
