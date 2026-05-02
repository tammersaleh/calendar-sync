package launchd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// stubRunner records each launchctl invocation and replays canned
// results. Tests that care only about the argv shape don't bother
// providing results; the zero-value (exit 0, empty stdout/stderr) is the
// happy-path default.
type stubRunner struct {
	calls []stubCall
	// next is the queue of canned results. If empty, the runner returns
	// (nil, nil, 0, nil) - the happy path.
	next []stubResult
}

type stubCall struct {
	args []string
}

type stubResult struct {
	stdout   []byte
	stderr   []byte
	exitCode int
	err      error
}

func (r *stubRunner) Run(_ context.Context, args ...string) ([]byte, []byte, int, error) {
	// Defensive copy so tests that share the slice with the caller don't
	// see mutations later.
	a := make([]string, len(args))
	copy(a, args)
	r.calls = append(r.calls, stubCall{args: a})
	if len(r.next) == 0 {
		return nil, nil, 0, nil
	}
	res := r.next[0]
	r.next = r.next[1:]
	return res.stdout, res.stderr, res.exitCode, res.err
}

// stubDarwin pins goos="darwin" and HOME=tmpdir for the duration of a
// test. Returns the home tmpdir so tests can assert on absolute paths.
func stubDarwin(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	origGOOS := goos
	goos = func() string { return "darwin" }
	t.Cleanup(func() { goos = origGOOS })
	return home
}

// TestInstall_HappyPath: writes the plist under HOME/Library/LaunchAgents,
// runs `launchctl load -w`, returns Loaded=true.
func TestInstall_HappyPath(t *testing.T) {
	home := stubDarwin(t)
	runner := &stubRunner{}
	cfg := Config{
		BinaryPath: "/usr/local/bin/calendar-sync",
		ConfigPath: "/Users/alice/.config/calendar-sync/config.toml",
	}
	res, err := Install(context.Background(), cfg, runner)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}

	wantPlistPath := filepath.Join(home, "Library", "LaunchAgents", "org.calendar-sync.agent.plist")
	if res.PlistPath != wantPlistPath {
		t.Errorf("PlistPath = %q, want %q", res.PlistPath, wantPlistPath)
	}
	if !res.Loaded {
		t.Errorf("Loaded = false, want true")
	}

	// Plist file actually exists with mode 0644.
	info, err := os.Stat(wantPlistPath)
	if err != nil {
		t.Fatalf("stat plist: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o644 {
		t.Errorf("plist mode = %o, want 0644", mode)
	}

	// Log dir was created.
	logDir := filepath.Join(home, "Library", "Logs", "calendar-sync")
	if info, err := os.Stat(logDir); err != nil {
		t.Errorf("log dir not created: %v", err)
	} else if !info.IsDir() {
		t.Errorf("log dir is not a directory")
	}

	// Plist content - check the binary path appears, plus the redirected
	// log paths (which depend on the test's HOME). XML well-formedness
	// is covered in plist_test.go.
	body, err := os.ReadFile(wantPlistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	for _, want := range []string{
		"/usr/local/bin/calendar-sync",
		"<string>watch</string>",
		filepath.Join(logDir, "calendar-sync.out.log"),
		filepath.Join(logDir, "calendar-sync.err.log"),
		"<key>Label</key><string>org.calendar-sync.agent</string>",
		DefaultPATH,
		// B7: config-path WatchPaths entry.
		"<key>WatchPaths</key>",
		"<string>/Users/alice/.config/calendar-sync/config.toml</string>",
	} {
		if !strings.Contains(string(body), want) {
			t.Errorf("plist body missing %q", want)
		}
	}

	// launchctl was called with `load -w <plistPath>`.
	if len(runner.calls) != 1 {
		t.Fatalf("runner.calls len = %d, want 1", len(runner.calls))
	}
	got := runner.calls[0].args
	want := []string{"load", "-w", wantPlistPath}
	if len(got) != len(want) {
		t.Fatalf("launchctl args = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("launchctl args[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

// TestInstall_NoLoad: skips the launchctl invocation when cfg.NoLoad is
// set. The plist file is still written.
func TestInstall_NoLoad(t *testing.T) {
	home := stubDarwin(t)
	runner := &stubRunner{}
	cfg := Config{
		BinaryPath: "/path/to/calendar-sync",
		NoLoad:     true,
		ConfigPath: "/Users/alice/.config/calendar-sync/config.toml",
	}
	res, err := Install(context.Background(), cfg, runner)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if res.Loaded {
		t.Errorf("Loaded = true, want false")
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner.calls len = %d, want 0", len(runner.calls))
	}
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents", "org.calendar-sync.agent.plist")); err != nil {
		t.Errorf("plist not written: %v", err)
	}
}

// TestInstall_NoLoad_NilRunner: with NoLoad, runner can be nil. Catches
// regressions where a nil-check fires too early.
func TestInstall_NoLoad_NilRunner(t *testing.T) {
	stubDarwin(t)
	cfg := Config{
		BinaryPath: "/path/to/calendar-sync",
		NoLoad:     true,
		ConfigPath: "/Users/alice/.config/calendar-sync/config.toml",
	}
	if _, err := Install(context.Background(), cfg, nil); err != nil {
		t.Errorf("Install with NoLoad+nil runner: %v", err)
	}
}

// TestInstall_PlistExistsErr: an existing plist without --force returns
// ErrPlistExists. Catches the SPEC's plist_exists error code.
func TestInstall_PlistExistsErr(t *testing.T) {
	home := stubDarwin(t)
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "org.calendar-sync.agent.plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte("preexisting"), 0o644); err != nil {
		t.Fatalf("seed plist: %v", err)
	}

	runner := &stubRunner{}
	cfg := Config{
		BinaryPath: "/bin/calendar-sync",
		ConfigPath: "/Users/alice/.config/calendar-sync/config.toml",
	}
	_, err := Install(context.Background(), cfg, runner)
	if !errors.Is(err, ErrPlistExists) {
		t.Fatalf("err = %v, want ErrPlistExists", err)
	}

	// The original plist is left untouched.
	body, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	if string(body) != "preexisting" {
		t.Errorf("plist was overwritten: %q", body)
	}

	if len(runner.calls) != 0 {
		t.Errorf("runner.calls = %d, want 0 (no launchctl after early-out)", len(runner.calls))
	}
}

// TestInstall_ForceOverwrites: with --force, an existing plist is
// overwritten and the install proceeds.
func TestInstall_ForceOverwrites(t *testing.T) {
	home := stubDarwin(t)
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "org.calendar-sync.agent.plist")
	if err := os.MkdirAll(filepath.Dir(plistPath), 0o755); err != nil {
		t.Fatalf("seed dir: %v", err)
	}
	if err := os.WriteFile(plistPath, []byte("preexisting"), 0o644); err != nil {
		t.Fatalf("seed plist: %v", err)
	}

	runner := &stubRunner{}
	cfg := Config{
		BinaryPath: "/usr/local/bin/calendar-sync",
		Force:      true,
		ConfigPath: "/Users/alice/.config/calendar-sync/config.toml",
	}
	res, err := Install(context.Background(), cfg, runner)
	if err != nil {
		t.Fatalf("Install with force: %v", err)
	}
	if !res.Loaded {
		t.Errorf("Loaded = false, want true")
	}

	body, err := os.ReadFile(plistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	if string(body) == "preexisting" {
		t.Errorf("plist was NOT overwritten despite --force")
	}
	if !strings.Contains(string(body), "/usr/local/bin/calendar-sync") {
		t.Errorf("overwritten plist missing binary path: %s", body)
	}
}

// TestInstall_LaunchctlNonZero: a non-zero launchctl exit is wrapped as
// ErrLaunchctlFailed. The plist remains on disk so the user can inspect
// it.
func TestInstall_LaunchctlNonZero(t *testing.T) {
	home := stubDarwin(t)
	runner := &stubRunner{
		next: []stubResult{
			{stderr: []byte("Bootstrap failed: 5: Input/output error"), exitCode: 5},
		},
	}
	cfg := Config{
		BinaryPath: "/bin/calendar-sync",
		ConfigPath: "/Users/alice/.config/calendar-sync/config.toml",
	}
	_, err := Install(context.Background(), cfg, runner)
	if !errors.Is(err, ErrLaunchctlFailed) {
		t.Fatalf("err = %v, want ErrLaunchctlFailed", err)
	}
	if !strings.Contains(err.Error(), "Bootstrap failed") {
		t.Errorf("err message lost stderr context: %v", err)
	}

	// The plist file is left on disk; a follow-up uninstall (or a
	// human) cleans up.
	plistPath := filepath.Join(home, "Library", "LaunchAgents", "org.calendar-sync.agent.plist")
	if _, err := os.Stat(plistPath); err != nil {
		t.Errorf("plist removed after launchctl failure: %v", err)
	}
}

// TestInstall_LaunchctlSubprocessErr: a runErr (e.g. binary not found)
// also surfaces as ErrLaunchctlFailed.
func TestInstall_LaunchctlSubprocessErr(t *testing.T) {
	stubDarwin(t)
	runner := &stubRunner{
		next: []stubResult{
			{err: errors.New("exec: launchctl: not found"), exitCode: -1},
		},
	}
	cfg := Config{
		BinaryPath: "/bin/calendar-sync",
		ConfigPath: "/Users/alice/.config/calendar-sync/config.toml",
	}
	_, err := Install(context.Background(), cfg, runner)
	if !errors.Is(err, ErrLaunchctlFailed) {
		t.Fatalf("err = %v, want ErrLaunchctlFailed", err)
	}
}

// TestInstall_NotMacOS: on a non-Darwin platform, ErrNotMacOS is returned
// without touching the filesystem or running launchctl.
func TestInstall_NotMacOS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	origGOOS := goos
	goos = func() string { return "linux" }
	t.Cleanup(func() { goos = origGOOS })

	runner := &stubRunner{}
	_, err := Install(context.Background(), Config{}, runner)
	if !errors.Is(err, ErrNotMacOS) {
		t.Fatalf("err = %v, want ErrNotMacOS", err)
	}
	if len(runner.calls) != 0 {
		t.Errorf("runner.calls = %d, want 0 (early-out before launchctl)", len(runner.calls))
	}
	// Crucially, no file was written under HOME.
	if _, err := os.Stat(filepath.Join(home, "Library", "LaunchAgents")); err == nil {
		t.Errorf("LaunchAgents dir created on non-Darwin")
	}
}

// TestInstall_CustomLogDir: a Config.LogDir starting with "~/" is
// expanded against HOME, and the log paths in the plist reflect that.
func TestInstall_CustomLogDir(t *testing.T) {
	home := stubDarwin(t)
	runner := &stubRunner{}
	cfg := Config{
		BinaryPath: "/bin/calendar-sync",
		LogDir:     "~/custom-logs",
		ConfigPath: "/Users/alice/.config/calendar-sync/config.toml",
	}
	res, err := Install(context.Background(), cfg, runner)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Loaded {
		t.Errorf("Loaded = false, want true")
	}

	wantLogDir := filepath.Join(home, "custom-logs")
	if info, err := os.Stat(wantLogDir); err != nil {
		t.Errorf("custom log dir not created: %v", err)
	} else if !info.IsDir() {
		t.Errorf("custom log dir is not a directory")
	}

	body, err := os.ReadFile(res.PlistPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	if !strings.Contains(string(body), filepath.Join(wantLogDir, "calendar-sync.out.log")) {
		t.Errorf("plist missing custom stdout path; got: %s", body)
	}
}

// TestInstall_CustomLabel: a non-default Label is reflected in the plist
// filename and body.
func TestInstall_CustomLabel(t *testing.T) {
	home := stubDarwin(t)
	runner := &stubRunner{}
	cfg := Config{
		BinaryPath: "/bin/calendar-sync",
		Label:      "org.example.calsync",
		ConfigPath: "/Users/alice/.config/calendar-sync/config.toml",
	}
	res, err := Install(context.Background(), cfg, runner)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	wantPath := filepath.Join(home, "Library", "LaunchAgents", "org.example.calsync.plist")
	if res.PlistPath != wantPath {
		t.Errorf("PlistPath = %q, want %q", res.PlistPath, wantPath)
	}
	body, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatalf("read plist: %v", err)
	}
	if !strings.Contains(string(body), "<key>Label</key><string>org.example.calsync</string>") {
		t.Errorf("plist missing custom label: %s", body)
	}
}
