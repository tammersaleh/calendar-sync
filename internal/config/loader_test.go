package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestFindPath(t *testing.T) {
	t.Run("flag wins over env and XDG", func(t *testing.T) {
		t.Setenv(envConfigPath, "/etc/foo.toml")
		t.Setenv("XDG_CONFIG_HOME", "/etc/xdg")
		got := FindPath("/explicit.toml")
		if got != "/explicit.toml" {
			t.Errorf("got %q, want /explicit.toml", got)
		}
	})

	t.Run("env wins over XDG when flag empty", func(t *testing.T) {
		t.Setenv(envConfigPath, "/etc/foo.toml")
		t.Setenv("XDG_CONFIG_HOME", "/etc/xdg")
		got := FindPath("")
		if got != "/etc/foo.toml" {
			t.Errorf("got %q, want /etc/foo.toml", got)
		}
	})

	t.Run("XDG fallback when flag and env empty", func(t *testing.T) {
		t.Setenv(envConfigPath, "")
		t.Setenv("XDG_CONFIG_HOME", "/custom/xdg")
		got := FindPath("")
		want := "/custom/xdg/calendar-sync/config.toml"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})

	t.Run("HOME fallback when XDG unset", func(t *testing.T) {
		t.Setenv(envConfigPath, "")
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("HOME", "/home/alice")
		got := FindPath("")
		want := "/home/alice/.config/calendar-sync/config.toml"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}

func TestLoad_FileNotFoundWrapsErrNotExist(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.toml")

	_, err := Load(missing)
	if err == nil {
		t.Fatal("expected error when file does not exist")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error does not wrap os.ErrNotExist: %v", err)
	}
}

func TestLoad_AppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[[pairs]]
name = "work-personal"
direction = "bidirectional"
source = "alice@example.com"
target = "primary"
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned error: %v", err)
	}

	wantSettings := Settings{
		PollInterval:         Duration(60 * time.Second),
		Horizon:              Duration(365 * 24 * time.Hour),
		FullSyncInterval:     Duration(24 * time.Hour),
		LogLevel:             LogLevelInfo,
		LogFormat:            LogFormatJSON,
		DryRun:               false,
		PropagateTargetEdits: false, // SPEC: defaults off; one-way sync until opted in.
	}
	if !reflect.DeepEqual(cfg.Settings, wantSettings) {
		t.Errorf("Settings = %#v\nwant     %#v", cfg.Settings, wantSettings)
	}
	if len(cfg.Pairs) != 1 {
		t.Fatalf("len(Pairs) = %d, want 1", len(cfg.Pairs))
	}
	p := cfg.Pairs[0]
	if p.Name != "work-personal" {
		t.Errorf("pair name = %q", p.Name)
	}
	if !p.IsEnabled() {
		t.Errorf("pair without explicit enabled should default to true")
	}
}

func TestLoad_ExplicitSettingsOverrideDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	contents := `
[settings]
poll_interval = "30s"
horizon = "180d"
full_sync_interval = "12h"
log_level = "debug"
log_format = "text"
dry_run = true
propagate_target_edits = true

[[pairs]]
name = "p1"
direction = "source_to_target"
source = "a@example.com"
target = "b@example.com"
enabled = false
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Settings.PollInterval.Duration() != 30*time.Second {
		t.Errorf("PollInterval = %v", cfg.Settings.PollInterval.Duration())
	}
	if cfg.Settings.Horizon.Duration() != 180*24*time.Hour {
		t.Errorf("Horizon = %v", cfg.Settings.Horizon.Duration())
	}
	if cfg.Settings.FullSyncInterval.Duration() != 12*time.Hour {
		t.Errorf("FullSyncInterval = %v", cfg.Settings.FullSyncInterval.Duration())
	}
	if cfg.Settings.LogLevel != LogLevelDebug {
		t.Errorf("LogLevel = %q", cfg.Settings.LogLevel)
	}
	if cfg.Settings.LogFormat != LogFormatText {
		t.Errorf("LogFormat = %q", cfg.Settings.LogFormat)
	}
	if !cfg.Settings.DryRun {
		t.Errorf("DryRun = %v, want true", cfg.Settings.DryRun)
	}
	if !cfg.Settings.PropagateTargetEdits {
		t.Errorf("PropagateTargetEdits = %v, want true", cfg.Settings.PropagateTargetEdits)
	}
	if cfg.Pairs[0].IsEnabled() {
		t.Errorf("explicit enabled=false ignored")
	}
}

func TestLoad_MalformedTOMLReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(path, []byte("this is not [valid toml"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(path)
	if err == nil {
		t.Fatal("expected parse error on malformed TOML")
	}
}
