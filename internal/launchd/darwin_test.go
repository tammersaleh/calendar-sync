package launchd

import (
	"path/filepath"
	"testing"
)

// TestExpandHome covers the leading-tilde expansion + the no-op cases.
// SPEC §"calendar-sync install" wires LogDir under "~/" by default, so a
// silent failure here would either write logs to a literal "~" directory
// or break the install.
func TestExpandHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"~", home},
		{"~/", home},
		{"~/Library/Logs/calendar-sync", filepath.Join(home, "Library/Logs/calendar-sync")},
		{"/already/absolute", "/already/absolute"},
		{"relative/path", "relative/path"},
		{"/path/with/~/inside", "/path/with/~/inside"}, // only leading tilde expands
	}
	for _, tc := range cases {
		got, err := expandHome(tc.in)
		if err != nil {
			t.Errorf("expandHome(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("expandHome(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestLaunchAgentsDir confirms the directory is "~/Library/LaunchAgents",
// resolved against the configured HOME.
func TestLaunchAgentsDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := launchAgentsDir()
	if err != nil {
		t.Fatalf("launchAgentsDir: %v", err)
	}
	want := filepath.Join(home, "Library", "LaunchAgents")
	if got != want {
		t.Errorf("launchAgentsDir = %q, want %q", got, want)
	}
}

// TestDefaultLogDir confirms the default of "~/Library/Logs/calendar-sync"
// per SPEC line 752.
func TestDefaultLogDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got, err := defaultLogDir()
	if err != nil {
		t.Fatalf("defaultLogDir: %v", err)
	}
	want := filepath.Join(home, "Library", "Logs", "calendar-sync")
	if got != want {
		t.Errorf("defaultLogDir = %q, want %q", got, want)
	}
}

// TestPlistFilename ensures the launchd plist filename is always
// "<Label>.plist".
func TestPlistFilename(t *testing.T) {
	cases := []struct {
		label string
		want  string
	}{
		{"org.calendar-sync.agent", "org.calendar-sync.agent.plist"},
		{"org.example.foo", "org.example.foo.plist"},
	}
	for _, tc := range cases {
		if got := plistFilename(tc.label); got != tc.want {
			t.Errorf("plistFilename(%q) = %q, want %q", tc.label, got, tc.want)
		}
	}
}

// TestIsDarwin_StubGOOS swaps the goos function variable to simulate a
// non-Darwin platform without build tags. This is the seam Install /
// Uninstall consult to surface ErrNotMacOS.
func TestIsDarwin_StubGOOS(t *testing.T) {
	orig := goos
	t.Cleanup(func() { goos = orig })

	goos = func() string { return "darwin" }
	if !isDarwin() {
		t.Errorf("isDarwin() with stubbed darwin = false")
	}
	goos = func() string { return "linux" }
	if isDarwin() {
		t.Errorf("isDarwin() with stubbed linux = true")
	}
}
