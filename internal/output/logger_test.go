package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestLogger_JSONFormatProducesParseableLine: SPEC §"Output and
// Logging" (line 374) calls for "one JSON object per line". Pin the
// shape: every emitted line decodes to an object with at least
// time/level/msg.
func TestLogger_JSONFormatProducesParseableLine(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, FormatJSON, LevelInfo)
	l.Info("sync complete", "pair", "work-personal", "duration_ms", 847)

	got := buf.String()
	if strings.Count(got, "\n") != 1 {
		t.Errorf("expected one newline; got %d (output=%q)",
			strings.Count(got, "\n"), got)
	}

	var row map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &row); err != nil {
		t.Fatalf("decode: %v (line=%q)", err, got)
	}
	for _, k := range []string{"time", "level", "msg"} {
		if _, ok := row[k]; !ok {
			t.Errorf("missing key %q in JSON log line: %q", k, got)
		}
	}
	if row["msg"] != "sync complete" {
		t.Errorf("msg = %v, want 'sync complete'", row["msg"])
	}
	if row["pair"] != "work-personal" {
		t.Errorf("pair = %v, want work-personal", row["pair"])
	}
	if row["duration_ms"].(float64) != 847 {
		t.Errorf("duration_ms = %v, want 847", row["duration_ms"])
	}
}

// TestLogger_TextFormatIsHumanReadable: SPEC §"Output and Logging"
// (line 378) shows the text form as `<ts> LEVEL msg key=value...`.
// slog.NewTextHandler produces the same shape; pin recognizable
// substrings so an accidental swap (e.g. format=json) would fail.
func TestLogger_TextFormatIsHumanReadable(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, FormatText, LevelInfo)
	l.Info("sync complete", "pair", "work-personal")

	got := buf.String()
	// Text format includes plain "msg=" and "level=" keys (slog).
	if !strings.Contains(got, "msg=") {
		t.Errorf("text output should contain msg=; got %q", got)
	}
	if !strings.Contains(got, "level=") {
		t.Errorf("text output should contain level=; got %q", got)
	}
	if !strings.Contains(got, "pair=work-personal") {
		t.Errorf("text output should contain pair=work-personal; got %q", got)
	}
	// JSON format uses double-quoted keys; text doesn't. Make sure we
	// got text by ruling out the JSON marker.
	if strings.HasPrefix(strings.TrimSpace(got), `{`) {
		t.Errorf("text output should not start with `{`; got %q", got)
	}
}

// TestLogger_LevelFilteringSuppressesBelow: a logger constructed at
// info level must drop debug-level emissions. SPEC line 404 lists
// debug/info/warn/error in increasing severity.
func TestLogger_LevelFilteringSuppressesBelow(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, FormatJSON, LevelInfo)
	l.Debug("noisy detail", "k", "v")

	if buf.Len() != 0 {
		t.Errorf("debug emission at info level should be dropped; got %q", buf.String())
	}

	// And info goes through.
	l.Info("kept", "k", "v")
	if !strings.Contains(buf.String(), "kept") {
		t.Errorf("info emission should be kept at info level; got %q", buf.String())
	}
}

// TestLogger_LevelFilteringIncludesAtAndAbove: warn-level logger
// passes warn and error, drops info and debug.
func TestLogger_LevelFilteringIncludesAtAndAbove(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, FormatJSON, LevelWarn)
	l.Debug("a")
	l.Info("b")
	l.Warn("c")
	l.Error("d")

	out := buf.String()
	if strings.Contains(out, `"a"`) || strings.Contains(out, `"b"`) {
		t.Errorf("warn-level logger should drop debug/info; got %q", out)
	}
	if !strings.Contains(out, `"c"`) {
		t.Errorf("warn message should be kept; got %q", out)
	}
	if !strings.Contains(out, `"d"`) {
		t.Errorf("error message should be kept; got %q", out)
	}
}

// TestLogger_InvalidFormatDefaultsToJSON: an unknown format falls back
// to JSON. SPEC §"Global Flags" (line 405) lists json/text; the
// fallback is for code paths that bypass kong validation.
func TestLogger_InvalidFormatDefaultsToJSON(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, "yaml", LevelInfo)
	l.Info("x")

	// JSON output is parseable as JSON.
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &map[string]any{}); err != nil {
		t.Errorf("unknown format should default to JSON (parseable line); got %q (err=%v)", buf.String(), err)
	}
}

// TestLogger_InvalidLevelDefaultsToInfo: an unknown level value falls
// back to info. We exercise this by checking that Debug is still
// suppressed and Info still passes.
func TestLogger_InvalidLevelDefaultsToInfo(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, FormatJSON, "verbose")

	l.Debug("must drop")
	if buf.Len() != 0 {
		t.Errorf("unknown level should default to info (debug dropped); got %q", buf.String())
	}

	l.Info("must keep")
	if !strings.Contains(buf.String(), "must keep") {
		t.Errorf("unknown level should default to info (info kept); got %q", buf.String())
	}
}

// TestLogger_EmptyLevelDefaultsToInfo: empty string is the typical
// "unset" sentinel from settings.LogLevel; treat it as info.
func TestLogger_EmptyLevelDefaultsToInfo(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, FormatJSON, "")

	l.Debug("dropped")
	if buf.Len() != 0 {
		t.Errorf("empty level should default to info (debug dropped); got %q", buf.String())
	}
	l.Info("kept")
	if !strings.Contains(buf.String(), "kept") {
		t.Errorf("empty level should default to info (info kept); got %q", buf.String())
	}
}

// TestLogger_NilWriterIsSilent: a nil writer produces a Logger whose
// methods are no-ops. The CLI/daemon always pass os.Stderr, but tests
// often pass nil.
func TestLogger_NilWriterIsSilent(t *testing.T) {
	l := NewLogger(nil, FormatJSON, LevelDebug)
	// Reaching these calls without panic is the assertion.
	l.Debug("a")
	l.Info("b")
	l.Warn("c")
	l.Error("d")
}

// TestLogger_NilReceiverIsSilent: defensively guard against a nil
// *Logger, which can happen in early-startup paths before the logger
// is wired.
func TestLogger_NilReceiverIsSilent(t *testing.T) {
	var l *Logger
	l.Debug("a")
	l.Info("b")
	l.Warn("c")
	l.Error("d")
}

// TestLogger_LevelCaseInsensitive: uppercase level strings (e.g. from
// an env var) should still resolve. slog itself is case-insensitive
// for its own level parsing; mirror that here.
func TestLogger_LevelCaseInsensitive(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, FormatJSON, "DEBUG")

	l.Debug("kept")
	if !strings.Contains(buf.String(), "kept") {
		t.Errorf("DEBUG (uppercase) should match LevelDebug; got %q", buf.String())
	}
}

// TestLogger_FormatCaseInsensitive: same case-insensitivity for
// format. "TEXT" should resolve to FormatText.
func TestLogger_FormatCaseInsensitive(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, "TEXT", LevelInfo)
	l.Info("hi")

	got := buf.String()
	// Text format isn't a parseable JSON object.
	if json.Unmarshal(bytes.TrimSpace([]byte(got)), &map[string]any{}) == nil {
		t.Errorf("uppercase TEXT should produce text format, not JSON: %q", got)
	}
	if !strings.Contains(got, "msg=") {
		t.Errorf("uppercase TEXT should produce text format with msg=; got %q", got)
	}
}

// TestLogger_AllLevelsEmitWhenAtDebug: at debug level, all four
// methods produce output. Pins that the Debug/Info/Warn/Error
// dispatch is wired correctly to slog.LevelDebug etc.
func TestLogger_AllLevelsEmitWhenAtDebug(t *testing.T) {
	var buf bytes.Buffer
	l := NewLogger(&buf, FormatJSON, LevelDebug)
	l.Debug("debug-msg")
	l.Info("info-msg")
	l.Warn("warn-msg")
	l.Error("error-msg")

	out := buf.String()
	for _, want := range []string{"debug-msg", "info-msg", "warn-msg", "error-msg"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output; got %q", want, out)
		}
	}
	// Verify each level lands in its own line.
	if got := strings.Count(out, "\n"); got != 4 {
		t.Errorf("expected 4 newlines; got %d (output=%q)", got, out)
	}
}
