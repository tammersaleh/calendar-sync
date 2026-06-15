package cmd

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/output"
)

// TestGwsClient_WiresScratchWorkDir is the cross-layer regression guard for
// B25: the launchd daemon's cwd defaults to "/" (read-only on macOS), and
// gws's events.delete writes a stray download.html into cwd on a 204
// response. The fix is that the production constructor passes WithWorkDir to
// a writable scratch dir. internal/gws's TestWithWorkDir_HonoredByExecute
// already proves WithWorkDir -> cmd.Dir; this test proves the cmd layer
// actually wires the option in. Without the wiring the fake reports the
// test's own cwd (the cmd package dir) rather than the scratch dir, so the
// assertion fails - which is exactly the regression we want to catch if
// someone drops the WithWorkDir line again.
//
// It exercises the logger-present branch because that is the production
// path: Run always constructs a logger.
func TestGwsClient_WiresScratchWorkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary is unix-only")
	}

	// Pin the cache dir into a temp location so the test neither pollutes
	// nor depends on the real user cache, and so gwsScratchDir() resolves
	// identically here and inside the spawned subprocess.
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))

	// A fake `gws` on PATH that prints its own cwd as the calendarList.get
	// JSON response, so CalendarListGet round-trips it back as Summary.
	binDir := t.TempDir()
	fake := "#!/bin/sh\nprintf '{\"id\":\"x\",\"accessRole\":\"owner\",\"summary\":\"%s\"}\\n' \"$(pwd)\"\nexit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "gws"), []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake gws: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	rt := &Runtime{
		Stdout: &bytes.Buffer{},
		Stderr: &bytes.Buffer{},
		Logger: output.NewLogger(&bytes.Buffer{}, "json", "info"),
	}

	entry, err := rt.gwsClient().CalendarListGet(context.Background(), "primary")
	if err != nil {
		t.Fatalf("CalendarListGet via fake gws: %v", err)
	}

	// Expected path computed independently of gwsScratchDir() so this stays a
	// pure cross-layer wiring assertion: gwsClient must run gws in the cache
	// scratch dir. (gwsScratchDir's own path logic is pinned by the tests
	// below.)
	cacheBase, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("os.UserCacheDir: %v", err)
	}
	want := filepath.Join(cacheBase, "calendar-sync")
	if entry.Summary != want {
		t.Errorf("gws subprocess cwd = %q, want scratch dir %q", entry.Summary, want)
	}
}

// TestGwsScratchDir_PrefersCacheDir pins the happy path: with a resolvable
// user cache dir, the scratch dir lands under it and is created.
func TestGwsScratchDir_PrefersCacheDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	cache := filepath.Join(home, "cache")
	t.Setenv("XDG_CACHE_HOME", cache)

	got := gwsScratchDir()
	if got == "" {
		t.Fatal("gwsScratchDir() = \"\", want a path under the cache dir")
	}
	base, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("os.UserCacheDir: %v", err)
	}
	if want := filepath.Join(base, "calendar-sync"); got != want {
		t.Errorf("gwsScratchDir() = %q, want %q", got, want)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Errorf("scratch dir %q not created as a directory (err=%v)", got, err)
	}
}

// TestGwsScratchDir_FallsBackToTempWhenCacheUnusable pins the fallback
// deterministically: the preferred cache candidate is sabotaged by planting a
// regular FILE where the scratch dir would go, so MkdirAll there fails (a
// file already occupies the target path) and the function must fall through
// to a subdir under the system temp dir. This exercises the actual
// MkdirAll-failure branch without depending on whether
// os.UserCacheDir() resolves, and is uid-independent (unlike a chmod-based
// sabotage, which root would bypass). TMPDIR is pinned so the fallback dir
// lands inside the test's auto-cleaned temp tree.
func TestGwsScratchDir_FallsBackToTempWhenCacheUnusable(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, "cache"))
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp) // os.TempDir() honors $TMPDIR on unix

	cacheBase, err := os.UserCacheDir()
	if err != nil {
		t.Fatalf("os.UserCacheDir: %v", err)
	}
	if err := os.MkdirAll(cacheBase, 0o700); err != nil {
		t.Fatalf("mkdir cache base: %v", err)
	}
	// A regular file occupying the would-be scratch dir path makes MkdirAll
	// fail there, forcing the fallback.
	if err := os.WriteFile(filepath.Join(cacheBase, "calendar-sync"), []byte("x"), 0o600); err != nil {
		t.Fatalf("plant blocking file: %v", err)
	}

	got := gwsScratchDir()
	want := filepath.Join(os.TempDir(), "calendar-sync")
	if got != want {
		t.Errorf("gwsScratchDir() fallback = %q, want %q", got, want)
	}
	if info, err := os.Stat(got); err != nil || !info.IsDir() {
		t.Errorf("fallback scratch dir %q not created as a directory (err=%v)", got, err)
	}
}
