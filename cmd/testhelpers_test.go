package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

// shortTempDir returns a tempdir with a short path so Unix sockets created
// inside it stay under macOS's 104-byte sun_path limit. t.TempDir's default
// path under macOS's per-test-name folder is too long for a socket, so we
// fall back to /tmp and clean up via t.Cleanup.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "cs-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	abs, err := filepath.Abs(dir)
	if err != nil {
		t.Fatalf("Abs(%q): %v", dir, err)
	}
	return abs
}
