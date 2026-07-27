package cmd

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/config"
)

// logConfigTOML is validConfigTOML with the log settings parameterized so a
// test can pin what `[settings]` asks for and then check what actually
// reaches the logger.
func logConfigTOML(level, format string) string {
	return `
[settings]
poll_interval      = "60s"
horizon            = "365d"
full_sync_interval = "24h"
log_level          = "` + level + `"
log_format         = "` + format + `"

[[pairs]]
name      = "work-personal"
source    = "work@example.com"
target    = "personal@example.com"
`
}

// runWithLogGlobals drives a full `run` against the stub gws client and
// returns whatever the invocation wrote to stderr. `run` is the cheapest
// command that emits log lines at more than one level: BuildInventory logs at
// info, runClassifyLoop at debug, and the target-token seed warns because the
// stub returns no syncToken.
func runWithLogGlobals(t *testing.T, cfgTOML string, g Globals) string {
	t.Helper()
	t.Setenv("TMPDIR", shortTempDir(t))

	g.Config = writeConfigFixture(t, cfgTOML)
	stderr := &bytes.Buffer{}
	rt := &Runtime{
		Stdout:  &bytes.Buffer{},
		Stderr:  stderr,
		Globals: g,
		Ctx:     context.Background(),
		Gws:     &stubGws{},
	}
	if err := (&RunCmd{}).Run(rt); err != nil {
		t.Fatalf("RunCmd.Run: %v\nstderr=%s", err, stderr.String())
	}
	return stderr.String()
}

// logShape describes what a stderr dump must look like for one
// level/format combination.
type logShape struct {
	level  string
	format string
}

// assertLogShape fails unless stderr carries a line at want.level in
// want.format and carries nothing below that level. Both handlers render the
// level verbatim, so the two encodings only differ in punctuation:
// `level=DEBUG` for text, `"level":"DEBUG"` for JSON.
func assertLogShape(t *testing.T, stderr string, want logShape) {
	t.Helper()
	if stderr == "" {
		t.Fatalf("no log output at all; want a %s line in %s format", want.level, want.format)
	}
	render := func(level string) string {
		if want.format == "text" {
			return "level=" + strings.ToUpper(level)
		}
		return `"level":"` + strings.ToUpper(level) + `"`
	}
	if !strings.Contains(stderr, render(want.level)) {
		t.Errorf("stderr has no %s line in %s format:\n%s", want.level, want.format, stderr)
	}
	// The opposite encoding must be absent, otherwise the format resolved
	// to the wrong handler.
	other := `"level":"`
	if want.format != "text" {
		other = "level=" + strings.ToUpper(want.level)
	}
	if strings.Contains(stderr, other) {
		t.Errorf("stderr carries %q, so the format is not %s:\n%s", other, want.format, stderr)
	}
}

// assertNoLevel fails when stderr carries any line at the named level, in
// either encoding. Used to pin that a level threshold actually filtered.
func assertNoLevel(t *testing.T, stderr, level string) {
	t.Helper()
	up := strings.ToUpper(level)
	if strings.Contains(stderr, "level="+up) || strings.Contains(stderr, `"level":"`+up+`"`) {
		t.Errorf("stderr carries a %s line but the threshold should have dropped it:\n%s", level, stderr)
	}
}

func strptr(s string) *string { return &s }

// parseGlobals runs args through the real kong parser and returns the
// resulting Globals. It never dispatches a subcommand, so it is safe to use
// with `run` and friends.
func parseGlobals(t *testing.T, args ...string) Globals {
	t.Helper()
	cli := &CLI{}
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	parser, err := newParser(cli, stdout, stderr, func(int) {})
	if err != nil {
		t.Fatalf("newParser: %v", err)
	}
	if _, err := parser.Parse(args); err != nil {
		t.Fatalf("Parse(%v): %v", args, err)
	}
	return cli.Globals
}

// TestLoggerPrecedence_ConfigAndFlags pins B30: `[settings].log_level` and
// `[settings].log_format` reach the logger, and each CLI flag overrides its
// own field without disturbing the other one.
func TestLoggerPrecedence_ConfigAndFlags(t *testing.T) {
	cases := []struct {
		name       string
		cfgLevel   string
		cfgFormat  string
		globals    Globals
		want       logShape
		wantAbsent string
	}{
		{
			name:      "config only",
			cfgLevel:  "debug",
			cfgFormat: "text",
			want:      logShape{level: "debug", format: "text"},
		},
		{
			name:      "config only, json info",
			cfgLevel:  "info",
			cfgFormat: "json",
			want:      logShape{level: "info", format: "json"},
			// BuildInventory logs at info and runClassifyLoop at debug, so a
			// debug line here would mean the threshold never applied.
			wantAbsent: "debug",
		},
		{
			name:      "level flag keeps config format",
			cfgLevel:  "error",
			cfgFormat: "text",
			globals:   Globals{LogLevel: strptr("debug")},
			want:      logShape{level: "debug", format: "text"},
		},
		{
			name:      "format flag keeps config level",
			cfgLevel:  "debug",
			cfgFormat: "json",
			globals:   Globals{LogFormat: strptr("text")},
			want:      logShape{level: "debug", format: "text"},
		},
		{
			name:      "both flags win",
			cfgLevel:  "error",
			cfgFormat: "json",
			globals:   Globals{LogLevel: strptr("debug"), LogFormat: strptr("text")},
			want:      logShape{level: "debug", format: "text"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stderr := runWithLogGlobals(t, logConfigTOML(tc.cfgLevel, tc.cfgFormat), tc.globals)
			assertLogShape(t, stderr, tc.want)
			if tc.wantAbsent != "" {
				assertNoLevel(t, stderr, tc.wantAbsent)
			}
		})
	}
}

// TestLoggerGlobals_AbsentIsNotEmpty pins the property the whole precedence
// chain rests on: with no flag and no env var, the log globals are nil rather
// than "". A kong `default:` on either field would break this and silently
// reinstate B30, so the check is on the pointers themselves.
func TestLoggerGlobals_AbsentIsNotEmpty(t *testing.T) {
	t.Setenv("CALENDAR_SYNC_LOG_LEVEL", "")
	t.Setenv("CALENDAR_SYNC_LOG_FORMAT", "")
	os.Unsetenv("CALENDAR_SYNC_LOG_LEVEL")
	os.Unsetenv("CALENDAR_SYNC_LOG_FORMAT")

	g := parseGlobals(t, "version")
	if g.LogLevel != nil {
		t.Errorf("LogLevel = %q, want nil (absent must not look explicit)", *g.LogLevel)
	}
	if g.LogFormat != nil {
		t.Errorf("LogFormat = %q, want nil (absent must not look explicit)", *g.LogFormat)
	}
}

// TestLoggerGlobals_EnvAndFlag pins the top two rungs of the precedence
// ladder. kong folds $CALENDAR_SYNC_LOG_* into the same field as the flag, so
// the env var reads as an explicit override and beats config, while an
// explicit flag beats the env var.
func TestLoggerGlobals_EnvAndFlag(t *testing.T) {
	cases := []struct {
		name       string
		envLevel   string
		envFormat  string
		args       []string
		wantLevel  string
		wantFormat string
	}{
		{
			name:       "env only",
			envLevel:   "debug",
			envFormat:  "text",
			args:       []string{"version"},
			wantLevel:  "debug",
			wantFormat: "text",
		},
		{
			name:       "flag beats env",
			envLevel:   "error",
			envFormat:  "json",
			args:       []string{"version", "--log-level=debug", "--log-format=text"},
			wantLevel:  "debug",
			wantFormat: "text",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CALENDAR_SYNC_LOG_LEVEL", tc.envLevel)
			t.Setenv("CALENDAR_SYNC_LOG_FORMAT", tc.envFormat)

			g := parseGlobals(t, tc.args...)
			if g.LogLevel == nil || *g.LogLevel != tc.wantLevel {
				t.Errorf("LogLevel = %v, want %q", g.LogLevel, tc.wantLevel)
			}
			if g.LogFormat == nil || *g.LogFormat != tc.wantFormat {
				t.Errorf("LogFormat = %v, want %q", g.LogFormat, tc.wantFormat)
			}

			// The resolved globals must also outrank config, which here asks
			// for the opposite of what the invocation set.
			buf := &bytes.Buffer{}
			g.newLogger(buf, config.Settings{
				LogLevel:  config.LogLevelError,
				LogFormat: config.LogFormatJSON,
			}).Debug("probe")
			assertLogShape(t, buf.String(), logShape{level: tc.wantLevel, format: tc.wantFormat})
		})
	}
}

// TestLoggerGlobals_InvalidValuesAreUsageErrors pins the second half of B30:
// a bad --log-level or --log-format must fail as a usage error (exit 64)
// rather than silently falling back inside NewLogger. The enum tags on the
// pointer fields are what enforce this.
func TestLoggerGlobals_InvalidValuesAreUsageErrors(t *testing.T) {
	// Keep every case away from a real config and a real gws binary: if a
	// case ever stopped failing at parse time it must still not reach a
	// live write.
	t.Setenv("CALENDAR_SYNC_CONFIG", "/no/such/calendar-sync-config-file.toml")
	t.Setenv("PATH", "")
	t.Setenv("HOME", "/no/such/home")
	t.Setenv("XDG_CONFIG_HOME", "/no/such/xdg")

	cases := []struct {
		name string
		args []string
	}{
		{"invalid level", []string{"version", "--log-level=trace"}},
		{"invalid format", []string{"version", "--log-format=yaml"}},
		{"invalid level on run", []string{"run", "--log-level=trace"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stdout := &bytes.Buffer{}
			stderr := &bytes.Buffer{}
			if code := Run(tc.args, stdout, stderr); code != 64 {
				t.Errorf("exit code = %d, want 64; stderr=%q", code, stderr.String())
			}
			if !strings.Contains(stderr.String(), "must be one of") {
				t.Errorf("stderr missing the enum usage message; got %q", stderr.String())
			}
		})
	}
}

// TestLoggerGlobals_InvalidEnvValueIsUsageError pins that the enum gate also
// covers the env var. launchd sets the daemon's environment, so a typo there
// must be as loud as a typo on the command line.
func TestLoggerGlobals_InvalidEnvValueIsUsageError(t *testing.T) {
	t.Setenv("CALENDAR_SYNC_LOG_LEVEL", "trace")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if code := Run([]string{"version"}, stdout, stderr); code != 64 {
		t.Errorf("exit code = %d, want 64; stderr=%q", code, stderr.String())
	}
}

// TestLoggerBootstrap_ConfiglessCommand pins that a command which never loads
// config still runs, and that its bootstrap logger honors an explicit flag.
// `version` is the configless case; the missing-config case checks that the
// bootstrap logger survives long enough for the error envelope to be written.
func TestLoggerBootstrap_ConfiglessCommand(t *testing.T) {
	t.Setenv("PATH", "")
	t.Setenv("HOME", "/no/such/home")
	t.Setenv("XDG_CONFIG_HOME", "/no/such/xdg")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if code := Run([]string{"version", "--log-level=debug", "--log-format=text"}, stdout, stderr); code != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if stdout.Len() == 0 {
		t.Errorf("version produced no stdout")
	}

	stdout, stderr = &bytes.Buffer{}, &bytes.Buffer{}
	t.Setenv("CALENDAR_SYNC_CONFIG", "/no/such/calendar-sync-config-file.toml")
	if code := Run([]string{"config", "show", "--log-level=debug"}, stdout, stderr); code == 0 {
		t.Fatalf("missing config should not exit 0; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "config_not_found") {
		t.Errorf("stderr missing the config_not_found envelope; got %q", stderr.String())
	}
}

// TestLoggerBootstrap_DefaultsMatchConfigLoader pins that the bootstrap
// logger and a freshly loaded config agree on the defaults. If they drift,
// the log format would change midway through a command for no visible reason.
func TestLoggerBootstrap_DefaultsMatchConfigLoader(t *testing.T) {
	buf := &bytes.Buffer{}
	Globals{}.newLogger(buf, config.Settings{}).Info("probe")
	assertLogShape(t, buf.String(), logShape{level: "info", format: "json"})

	loaded := &bytes.Buffer{}
	Globals{}.newLogger(loaded, config.Settings{
		LogLevel:  config.LogLevelInfo,
		LogFormat: config.LogFormatJSON,
	}).Info("probe")
	assertLogShape(t, loaded.String(), logShape{level: "info", format: "json"})
}

// TestLoggerGlobals_EmptyEnvIsAlsoRejected documents a deliberate consequence
// of the enum gate: kong treats a set-but-empty $CALENDAR_SYNC_LOG_LEVEL as a
// supplied value, so it fails the enum check rather than falling back to
// config. Before the enum tag this was a silent no-op. Anyone exporting the
// variable conditionally must unset it instead of setting it to "".
func TestLoggerGlobals_EmptyEnvIsAlsoRejected(t *testing.T) {
	t.Setenv("CALENDAR_SYNC_LOG_LEVEL", "")

	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	if code := Run([]string{"version"}, stdout, stderr); code != 64 {
		t.Errorf("exit code = %d, want 64; stderr=%q", code, stderr.String())
	}
}
