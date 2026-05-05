package cmd

import (
	"bytes"
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallCmd_NonDarwinReturnsNotMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("test exercises the non-darwin error path")
	}
	rt := &Runtime{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Ctx:    context.Background(),
	}
	err := (&InstallCmd{NoLoad: true}).Run(rt)
	if err == nil {
		t.Fatalf("expected not_macos on %s", runtime.GOOS)
	}
	code, _, _ := MapError(err)
	if code != "not_macos" {
		t.Errorf("code = %q, want not_macos", code)
	}
}

// TestInstall_RelativeConfigPathBecomesAbsolute pins the path-safety
// guarantee for the plist's WatchPaths directive: a relative --config
// argument MUST be canonicalized to an absolute path before being
// stamped into the plist. launchd resolves WatchPaths entries against
// the daemon's working directory at load time, not the operator's cwd
// at install time, so a relative path would silently produce a plist
// that watches a non-existent file.
func TestInstall_RelativeConfigPathBecomesAbsolute(t *testing.T) {
	relative := "./config.toml"
	got, err := resolveInstallConfigPath(relative)
	if err != nil {
		t.Fatalf("resolveInstallConfigPath(%q): %v", relative, err)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("resolved path %q is not absolute; relative --config must be canonicalized", got)
	}
	// The basename should still refer to config.toml so we know we
	// preserved the user's filename rather than re-resolving to the XDG
	// default.
	if !strings.HasSuffix(got, "config.toml") {
		t.Errorf("resolved path %q does not preserve the user's filename", got)
	}
}

// TestInstall_AbsoluteConfigPathPassesThroughUnchanged covers the
// invariant that filepath.Abs is a no-op on already-absolute paths.
func TestInstall_AbsoluteConfigPathPassesThroughUnchanged(t *testing.T) {
	abs := "/tmp/some/config.toml"
	got, err := resolveInstallConfigPath(abs)
	if err != nil {
		t.Fatalf("resolveInstallConfigPath(%q): %v", abs, err)
	}
	if got != abs {
		t.Errorf("absolute path %q was rewritten to %q; should pass through unchanged", abs, got)
	}
}

func TestUninstallCmd_NonDarwinReturnsNotMacOS(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("test exercises the non-darwin error path")
	}
	rt := &Runtime{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Ctx:    context.Background(),
	}
	err := (&UninstallCmd{}).Run(rt)
	if err == nil {
		t.Fatalf("expected not_macos on %s", runtime.GOOS)
	}
	code, _, _ := MapError(err)
	if code != "not_macos" {
		t.Errorf("code = %q, want not_macos", code)
	}
}
