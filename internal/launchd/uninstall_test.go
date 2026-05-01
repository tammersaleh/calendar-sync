package launchd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// seedPlist creates ~/Library/LaunchAgents/<Label>.plist for an
// uninstall-flow test. Returns the absolute plist path.
func seedPlist(t *testing.T, home, label string) string {
	t.Helper()
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("seed LaunchAgents: %v", err)
	}
	p := filepath.Join(dir, label+".plist")
	if err := os.WriteFile(p, []byte("<plist/>"), 0o644); err != nil {
		t.Fatalf("seed plist: %v", err)
	}
	return p
}

// TestUninstall_HappyPath: launchctl unload runs, then the plist file is
// removed. The result reports unloaded=true, removed=true.
func TestUninstall_HappyPath(t *testing.T) {
	home := stubDarwin(t)
	plistPath := seedPlist(t, home, DefaultLabel)
	runner := &stubRunner{}

	res, err := Uninstall(context.Background(), UninstallConfig{}, runner)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if res.PlistPath != plistPath {
		t.Errorf("PlistPath = %q, want %q", res.PlistPath, plistPath)
	}
	if !res.Unloaded {
		t.Errorf("Unloaded = false, want true")
	}
	if !res.Removed {
		t.Errorf("Removed = false, want true")
	}
	if _, err := os.Stat(plistPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist file still on disk: %v", err)
	}

	// launchctl unload <path>
	if len(runner.calls) != 1 {
		t.Fatalf("runner.calls = %d, want 1", len(runner.calls))
	}
	got := runner.calls[0].args
	want := []string{"unload", plistPath}
	if len(got) != len(want) {
		t.Fatalf("launchctl args = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("launchctl args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestUninstall_KeepPlist: --keep-plist runs unload but leaves the file.
func TestUninstall_KeepPlist(t *testing.T) {
	home := stubDarwin(t)
	plistPath := seedPlist(t, home, DefaultLabel)
	runner := &stubRunner{}

	res, err := Uninstall(context.Background(), UninstallConfig{KeepPlist: true}, runner)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !res.Unloaded {
		t.Errorf("Unloaded = false, want true")
	}
	if res.Removed {
		t.Errorf("Removed = true, want false")
	}
	if _, err := os.Stat(plistPath); err != nil {
		t.Errorf("plist removed despite KeepPlist: %v", err)
	}
}

// TestUninstall_PlistNotFound: with no plist on disk, returns
// ErrPlistNotFound. The unload step still ran (so launchd's loaded list
// is consistent), but the function reports plist_not_found.
func TestUninstall_PlistNotFound(t *testing.T) {
	stubDarwin(t)
	// launchctl reports "not loaded" because nothing's loaded; we
	// recognize that as success and proceed to the file check.
	runner := &stubRunner{
		next: []stubResult{
			{stderr: []byte("Could not find specified service"), exitCode: 1},
		},
	}
	_, err := Uninstall(context.Background(), UninstallConfig{}, runner)
	if !errors.Is(err, ErrPlistNotFound) {
		t.Fatalf("err = %v, want ErrPlistNotFound", err)
	}
}

// TestUninstall_LaunchctlNotLoadedSwallowed: launchctl exits non-zero
// with a "not loaded" stderr; that's idempotent-success. The plist is
// still removed.
func TestUninstall_LaunchctlNotLoadedSwallowed(t *testing.T) {
	home := stubDarwin(t)
	plistPath := seedPlist(t, home, DefaultLabel)
	runner := &stubRunner{
		next: []stubResult{
			{stderr: []byte("launchctl unload: Could not find specified service"), exitCode: 1},
		},
	}
	res, err := Uninstall(context.Background(), UninstallConfig{}, runner)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !res.Unloaded {
		t.Errorf("Unloaded = false, want true (swallowed)")
	}
	if !res.Removed {
		t.Errorf("Removed = false, want true")
	}
	if _, err := os.Stat(plistPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist still on disk: %v", err)
	}
}

// TestUninstall_LaunchctlNotLoadedAlternate: the alternate "no such file"
// phrasing is also recognized.
func TestUninstall_LaunchctlNotLoadedAlternate(t *testing.T) {
	home := stubDarwin(t)
	plistPath := seedPlist(t, home, DefaultLabel)
	runner := &stubRunner{
		next: []stubResult{
			{stderr: []byte("launchctl: No such file or directory"), exitCode: 1},
		},
	}
	res, err := Uninstall(context.Background(), UninstallConfig{}, runner)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !res.Unloaded {
		t.Errorf("Unloaded = false, want true")
	}
	if !res.Removed {
		t.Errorf("Removed = false, want true")
	}
	if _, err := os.Stat(plistPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("plist still on disk: %v", err)
	}
}

// TestUninstall_LaunchctlOtherError: an unrecognized non-zero exit
// surfaces as ErrLaunchctlFailed without removing the plist.
func TestUninstall_LaunchctlOtherError(t *testing.T) {
	home := stubDarwin(t)
	plistPath := seedPlist(t, home, DefaultLabel)
	runner := &stubRunner{
		next: []stubResult{
			{stderr: []byte("Unload failed: domain.system uid 0 reason 5"), exitCode: 5},
		},
	}
	_, err := Uninstall(context.Background(), UninstallConfig{}, runner)
	if !errors.Is(err, ErrLaunchctlFailed) {
		t.Fatalf("err = %v, want ErrLaunchctlFailed", err)
	}
	// Plist NOT removed (we only reach removal after unload succeeds).
	if _, err := os.Stat(plistPath); err != nil {
		t.Errorf("plist removed despite unload failure: %v", err)
	}
}

// TestUninstall_LaunchctlSubprocessErr: a subprocess-level err (binary
// missing, fork failed) surfaces as ErrLaunchctlFailed.
func TestUninstall_LaunchctlSubprocessErr(t *testing.T) {
	home := stubDarwin(t)
	seedPlist(t, home, DefaultLabel)
	runner := &stubRunner{
		next: []stubResult{
			{err: errors.New("exec: launchctl: not found"), exitCode: -1},
		},
	}
	_, err := Uninstall(context.Background(), UninstallConfig{}, runner)
	if !errors.Is(err, ErrLaunchctlFailed) {
		t.Fatalf("err = %v, want ErrLaunchctlFailed", err)
	}
}

// TestUninstall_NotMacOS: returns ErrNotMacOS and runs no launchctl.
func TestUninstall_NotMacOS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	origGOOS := goos
	goos = func() string { return "linux" }
	t.Cleanup(func() { goos = origGOOS })

	runner := &stubRunner{}
	_, err := Uninstall(context.Background(), UninstallConfig{}, runner)
	if !errors.Is(err, ErrNotMacOS) {
		t.Fatalf("err = %v, want ErrNotMacOS", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner.calls = %d, want 0", len(runner.calls))
	}
}

// TestUninstall_CustomLabel: a non-default Label uses the matching plist
// filename.
func TestUninstall_CustomLabel(t *testing.T) {
	home := stubDarwin(t)
	plistPath := seedPlist(t, home, "org.example.calsync")
	runner := &stubRunner{}

	res, err := Uninstall(context.Background(), UninstallConfig{Label: "org.example.calsync"}, runner)
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if res.PlistPath != plistPath {
		t.Errorf("PlistPath = %q, want %q", res.PlistPath, plistPath)
	}
	// launchctl was called with the custom plist path.
	if got := runner.calls[0].args[1]; got != plistPath {
		t.Errorf("launchctl path arg = %q, want %q", got, plistPath)
	}
}

// TestIsLaunchctlNotLoaded covers the marker-string match logic
// directly, including case-insensitivity.
func TestIsLaunchctlNotLoaded(t *testing.T) {
	cases := []struct {
		stderr string
		want   bool
	}{
		{"Could not find specified service", true},
		{"could not find specified service", true},
		{"launchctl unload: Could not find specified service", true},
		{"No such file or directory", true},
		{"launchctl: NO SUCH FILE OR DIRECTORY", true},
		{"some unrelated launchctl failure", false},
		{"", false},
	}
	for _, tc := range cases {
		got := isLaunchctlNotLoaded([]byte(tc.stderr))
		if got != tc.want {
			t.Errorf("isLaunchctlNotLoaded(%q) = %v, want %v", tc.stderr, got, tc.want)
		}
	}
}
