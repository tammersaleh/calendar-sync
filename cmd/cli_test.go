package cmd

import (
	"bytes"
	"strings"
	"testing"
)

func TestRun_VersionShortPrintsBareString(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	prev := Version
	t.Cleanup(func() { Version = prev })
	Version = "9.9.9"

	code := Run([]string{"version", "--short"}, stdout, stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if got := stdout.String(); !strings.Contains(got, "9.9.9") {
		t.Errorf("stdout = %q, want to contain 9.9.9", got)
	}
}

func TestRun_UnknownSubcommandIs64(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	code := Run([]string{"definitely-not-a-command"}, stdout, stderr)
	if code != 64 {
		t.Errorf("exit code = %d, want 64", code)
	}
	if stderr.Len() == 0 {
		t.Errorf("expected usage on stderr")
	}
}

func TestRun_NoArgsShowsUsage(t *testing.T) {
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}

	code := Run([]string{}, stdout, stderr)
	if code != 64 {
		t.Errorf("exit code = %d, want 64", code)
	}
}

// TestRun_HelpFlagDoesNotDispatchSubcommand pins the B1 invariant: when the
// user invokes any subcommand with --help, the subcommand's Run method MUST
// NOT be called. Before the fix, kong's helpFlag.BeforeReset called
// `ctx.Kong.Exit(0)` which our `kong.Exit(func(int){})` no-op'd; Parse then
// returned successfully and `kctx.Run(rt)` dispatched the subcommand,
// performing live writes against the user's calendar.
//
// This test drives the public Run() entrypoint exactly the way main.go does.
// The subcommands picked here are the dangerous ones - run, watch, install,
// uninstall, mirror prune.
//
// CRITICAL: this test must NEVER trigger live writes regardless of whether
// the bug is present. We point CALENDAR_SYNC_CONFIG at a nonexistent path
// AND clear PATH to make `gws` unfindable. With B1 unfixed the subcommand
// Run executes but errors out at loadConfig (config_not_found) before any
// gws call - so failure mode is "wrong exit code", never "writes to user
// calendar". Post-fix the test passes via the kong help short-circuit
// before loadConfig is ever called.
//
// The cases split into two columns:
//
//   - wantHelp: subcommand has no required positionals, so kong runs hooks
//     (including helpFlag.BeforeReset) before validation. Expectation:
//     exit 0, "Usage:" on stdout.
//   - wantParseError: subcommand has a required positional. kong's Trace
//     fails with "expected <name>" BEFORE BeforeReset can fire. Expectation:
//     exit 64 with the parse error on stderr. The subcommand's Run method
//     is still NOT dispatched, which is the load-bearing safety invariant.
//
// Both columns share the "no error envelope" and "no subcommand-side error
// code" check: at HEAD pre-fix, every case writes a CodeXxx envelope to
// stderr because RunCmd / WatchCmd / InstallCmd / UninstallCmd actually
// executed.
func TestRun_HelpFlagDoesNotDispatchSubcommand(t *testing.T) {
	// Belt-and-suspenders against accidental writes: even if B1 is unfixed,
	// the subcommand will error out at loadConfig long before reaching gws.
	t.Setenv("CALENDAR_SYNC_CONFIG", "/no/such/calendar-sync-config-file.toml")
	// Clear PATH so even if loadConfig somehow succeeded (it can't here),
	// gws.New()'s subprocess invocation would fail with ErrGWSNotFound
	// rather than hitting a real binary.
	t.Setenv("PATH", "")
	// And clear HOME so XDG_CONFIG_HOME fallback doesn't accidentally
	// resolve to a directory containing a real config.
	t.Setenv("HOME", "/no/such/home")
	t.Setenv("XDG_CONFIG_HOME", "/no/such/xdg")

	type expectation int
	const (
		wantHelp       expectation = iota // exit 0, "Usage:" on stdout
		wantParseError                    // exit 64, kong-style "expected <X>" on stderr
	)
	cases := []struct {
		name   string
		args   []string
		expect expectation
	}{
		{"run --help", []string{"run", "--help"}, wantHelp},
		{"run -h", []string{"run", "-h"}, wantHelp},
		{"run --pair=X --help", []string{"run", "--pair=X", "--help"}, wantHelp},
		{"run --dry-run --help", []string{"run", "--dry-run", "--help"}, wantHelp},
		{"watch --help", []string{"watch", "--help"}, wantHelp},
		{"install --help", []string{"install", "--help"}, wantHelp},
		{"install --force --help", []string{"install", "--force", "--help"}, wantHelp},
		{"uninstall --help", []string{"uninstall", "--help"}, wantHelp},
		{"mirror prune --help", []string{"mirror", "prune", "--help"}, wantParseError},
		{"mirror list --help", []string{"mirror", "list", "--help"}, wantParseError},
		{"pair test --help", []string{"pair", "test", "--help"}, wantParseError},
		{"config show --help", []string{"config", "show", "--help"}, wantHelp},
		{"config validate --help", []string{"config", "validate", "--help"}, wantHelp},
		{"pair list --help", []string{"pair", "list", "--help"}, wantHelp},
		{"init --help", []string{"init", "--help"}, wantHelp},
		{"status --help", []string{"status", "--help"}, wantHelp},
		{"version --help", []string{"version", "--help"}, wantHelp},
		{"skill --help", []string{"skill", "--help"}, wantHelp},
		{"top-level --help", []string{"--help"}, wantParseError},
		// Quiet + help: --quiet redirects stdout to nil; --help writes via
		// kong's writer (the original stdout we passed to kong.New).
		// Verify kong's stdout still gets the help text.
		{"run --quiet --help", []string{"run", "--quiet", "--help"}, wantHelp},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}

			code := Run(tc.args, stdout, stderr)

			// Subcommand-side error envelope must NEVER appear: that's the
			// load-bearing safety check. Kong parse errors print plain text
			// (e.g. "expected <calendar>") on stderr; subcommand-side
			// errors emit a JSON `{"error":...}` envelope. The latter is
			// what we're guarding against - it would prove the subcommand's
			// Run method executed.
			if strings.Contains(stderr.String(), `"error"`) {
				t.Errorf("stderr carries an error envelope - subcommand executed; stderr=%q", stderr.String())
			}

			switch tc.expect {
			case wantHelp:
				if code != 0 {
					t.Errorf("exit code = %d, want 0; stderr=%q", code, stderr.String())
				}
				if !strings.Contains(stdout.String(), "Usage:") {
					t.Errorf("stdout missing 'Usage:'; got %q", stdout.String())
				}
			case wantParseError:
				if code != 64 {
					t.Errorf("exit code = %d, want 64 (kong parse error); stderr=%q", code, stderr.String())
				}
				if stderr.Len() == 0 {
					t.Errorf("expected kong parse error on stderr; got empty")
				}
			}
		})
	}
}
