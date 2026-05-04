package gws_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// TestWithWorkDir_HonoredByExecute pins that cmd.Dir is set on the gws
// subprocess. The motivating production behavior: gws's events.delete /
// calendars.delete write a stray `download.html` in cwd on success
// (Calendar API 204 → gws renders an "empty downloaded file"). E2E and
// future production hardening rely on this option to confine those
// strays to a sandbox directory.
//
// We bypass the fake-gws harness because the harness runs the test
// binary itself as the fake, which makes asserting cwd brittle. Instead
// we drop a tiny shell-script "binary" that prints its own cwd, point
// the client at it, and verify the cwd matches the sandbox.
func TestWithWorkDir_HonoredByExecute(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary is unix-only")
	}

	tmp := t.TempDir()
	sandbox := filepath.Join(tmp, "sandbox")
	if err := os.Mkdir(sandbox, 0o755); err != nil {
		t.Fatalf("mkdir sandbox: %v", err)
	}

	fakePath := filepath.Join(tmp, "fakegws.sh")
	// The fake prints its cwd as JSON so the wrapper's unmarshal
	// downstream of CalendarListGet succeeds. Any client method whose
	// success path tolerates the response shape would work; we use
	// CalendarListGet because its response is a flat JSON object.
	fake := `#!/bin/sh
printf '{"id":"x","accessRole":"owner","summary":"%s"}\n' "$(pwd)"
exit 0
`
	if err := os.WriteFile(fakePath, []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}

	client := gws.New(gws.WithBinary(fakePath), gws.WithWorkDir(sandbox))
	got, err := client.CalendarListGet(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("CalendarListGet via fake: %v", err)
	}
	if got.Summary != sandbox {
		t.Errorf("subprocess cwd = %q, want sandbox %q", got.Summary, sandbox)
	}
}

// TestWithWorkDir_UnsetInheritsParentCwd pins the default behavior:
// without WithWorkDir, the subprocess inherits the parent's cwd. This
// is the long-standing behavior; the option should ADD a knob, not
// silently change the default.
func TestWithWorkDir_UnsetInheritsParentCwd(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake binary is unix-only")
	}

	tmp := t.TempDir()
	fakePath := filepath.Join(tmp, "fakegws.sh")
	fake := `#!/bin/sh
printf '{"id":"x","accessRole":"owner","summary":"%s"}\n' "$(pwd)"
exit 0
`
	if err := os.WriteFile(fakePath, []byte(fake), 0o755); err != nil {
		t.Fatalf("write fake: %v", err)
	}

	parentCwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}

	client := gws.New(gws.WithBinary(fakePath))
	got, err := client.CalendarListGet(context.Background(), "ignored")
	if err != nil {
		t.Fatalf("CalendarListGet via fake: %v", err)
	}
	if got.Summary != parentCwd {
		t.Errorf("subprocess cwd = %q, want parent cwd %q (default inherits)", got.Summary, parentCwd)
	}
}
