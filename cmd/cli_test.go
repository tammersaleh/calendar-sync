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
// The post-fix expectation: exit code 0, stdout contains the help text,
// stderr empty (no error envelope). That triple proves help short-circuited.
//
// Pre-fix this test fails because exit code is non-zero (config_not_found
// from loadConfig) and stderr carries an ErrorEnvelope.
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

	cases := []struct {
		name string
		args []string
	}{
		{"run --help", []string{"run", "--help"}},
		{"run -h", []string{"run", "-h"}},
		{"watch --help", []string{"watch", "--help"}},
		{"install --help", []string{"install", "--help"}},
		{"uninstall --help", []string{"uninstall", "--help"}},
		{"mirror prune --help", []string{"mirror", "prune", "--help"}},
		{"mirror list --help", []string{"mirror", "list", "--help"}},
		{"pair test --help", []string{"pair", "test", "--help"}},
		{"config show --help", []string{"config", "show", "--help"}},
		{"init --help", []string{"init", "--help"}},
		{"top-level --help", []string{"--help"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}

			code := Run(tc.args, stdout, stderr)

			if code != 0 {
				t.Errorf("exit code = %d, want 0; stderr=%q", code, stderr.String())
			}
			// Help text always contains "Usage:".
			if !strings.Contains(stdout.String(), "Usage:") {
				t.Errorf("stdout missing 'Usage:'; got %q", stdout.String())
			}
			// No error envelope on stderr.
			if strings.Contains(stderr.String(), `"error"`) {
				t.Errorf("stderr carries an error envelope - subcommand executed; stderr=%q", stderr.String())
			}
		})
	}
}
