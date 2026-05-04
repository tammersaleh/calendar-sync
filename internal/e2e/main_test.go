//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// Package-level state populated by TestMain. Per-test Setup reads these
// instead of provisioning per-test (one calendar create+delete per test
// would balloon the suite well past its time budget).
var (
	binaryPath      string
	fixtureSourceID string
	fixtureTargetID string
)

// TestMain provisions the harness's fixture calendars, builds the
// calendar-sync binary, runs every test, and tears the fixtures down.
// All of that is gated behind CALENDAR_SYNC_E2E=1 so an accidental
// `go test -tags=e2e` does no destruction.
func TestMain(m *testing.M) {
	if os.Getenv(envGuard) != "1" {
		// Tests will Skip individually; TestMain still has to return
		// or the test binary hangs.
		os.Exit(m.Run())
	}

	tmpRoot, err := os.MkdirTemp("", "calendar-sync-e2e-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: mktemp: %v\n", err)
		os.Exit(1)
	}
	// We don't t.Cleanup tmpRoot from TestMain (no t available), so do
	// it manually before each Exit path.
	cleanupTmp := func() { _ = os.RemoveAll(tmpRoot) }

	bin, err := buildBinary(tmpRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: build calendar-sync: %v\n", err)
		cleanupTmp()
		os.Exit(1)
	}
	binaryPath = bin

	// Use a sandbox-rooted gws client throughout TestMain so any
	// stray `download.html` from gws lands in tmpRoot rather than
	// the user's repo.
	c := gws.New(gws.WithWorkDir(tmpRoot))

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	srcID, tgtID, err := createFixtures(ctx, c)
	cancel()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: create fixtures: %v\n", err)
		cleanupTmp()
		os.Exit(1)
	}
	fixtureSourceID = srcID
	fixtureTargetID = tgtID

	code := m.Run()

	// Always tear fixtures down, even on test failure or panic, so
	// repeated `mise run test:e2e` invocations don't leave stale
	// calendars accumulating in the user's calendarList.
	teardownCtx, teardownCancel := context.WithTimeout(context.Background(), 60*time.Second)
	if err := destroyFixtures(teardownCtx, c); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: destroy fixtures: %v\n", err)
		// Don't override the test exit code with a teardown error;
		// surface to stderr and keep the original m.Run() exit.
	}
	teardownCancel()

	cleanupTmp()
	os.Exit(code)
}

// buildBinary compiles the calendar-sync binary for the test run. One
// build per `go test` invocation; reused across every Test* function.
func buildBinary(tmpRoot string) (string, error) {
	out := filepath.Join(tmpRoot, "calendar-sync")
	cmd := exec.Command("go", "build", "-o", out, "github.com/tammersaleh/calendar-sync/cmd/calendar-sync")
	stdout, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("go build: %w (output: %s)", err, string(stdout))
	}
	return out, nil
}
