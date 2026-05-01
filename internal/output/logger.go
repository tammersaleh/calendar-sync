package output

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"strings"
	"time"
)

// Format constants for NewLogger. SPEC §"Output and Logging" (line 371)
// defines the two values; settings.log_format and the --log-format flag
// surface them to the user.
const (
	FormatJSON = "json"
	FormatText = "text"
)

// Level constants for NewLogger. SPEC §"Global Flags" (line 404) lists
// the four values; settings.log_level and the --log-level flag pass them
// through. They map 1:1 onto slog levels.
const (
	LevelDebug = "debug"
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// Logger writes structured diagnostic logs to stderr. SPEC §"Output and
// Logging" (line 371): `json` (default) emits one JSON object per line,
// `text` emits a human-readable form via slog's TextHandler.
//
// In production, the daemon's launchd plist redirects stderr to
// ~/Library/Logs/calendar-sync/calendar-sync.err.log. In a foreground
// run, stderr is the user's terminal.
type Logger struct {
	handler slog.Handler
}

// NewLogger constructs a Logger that writes to w in the chosen format.
// format is "json" or "text"; level is one of the Level* constants.
// Invalid format defaults to FormatJSON; invalid level defaults to
// LevelInfo. SPEC §"Global Flags" (line 414) makes invalid values a
// kong-validation problem in normal use; the defaults here are a safety
// net for code paths that bypass kong (e.g. early-startup logs before
// flags are parsed).
//
// A nil writer produces a Logger whose methods are silent no-ops, which
// is convenient for tests that don't care about log output.
func NewLogger(w io.Writer, format, level string) *Logger {
	if w == nil {
		return &Logger{handler: noopHandler{}}
	}
	opts := &slog.HandlerOptions{Level: parseLevel(level)}
	var h slog.Handler
	switch strings.ToLower(format) {
	case FormatText:
		h = slog.NewTextHandler(w, opts)
	default:
		h = slog.NewJSONHandler(w, opts)
	}
	return &Logger{handler: h}
}

// parseLevel maps SPEC's level string to a slog.Level. Unknown values
// fall back to info; an unset (empty) level also falls back to info.
func parseLevel(level string) slog.Level {
	switch strings.ToLower(level) {
	case LevelDebug:
		return slog.LevelDebug
	case LevelWarn:
		return slog.LevelWarn
	case LevelError:
		return slog.LevelError
	case LevelInfo, "":
		return slog.LevelInfo
	default:
		return slog.LevelInfo
	}
}

// Debug logs at debug level. args follow slog's key/value variadic.
func (l *Logger) Debug(msg string, args ...any) { l.log(slog.LevelDebug, msg, args...) }

// Info logs at info level. args follow slog's key/value variadic.
func (l *Logger) Info(msg string, args ...any) { l.log(slog.LevelInfo, msg, args...) }

// Warn logs at warn level. args follow slog's key/value variadic.
// SPEC §"Conflict logging" (line 488) routes its conflict_* messages
// through this level.
func (l *Logger) Warn(msg string, args ...any) { l.log(slog.LevelWarn, msg, args...) }

// Error logs at error level. args follow slog's key/value variadic.
func (l *Logger) Error(msg string, args ...any) { l.log(slog.LevelError, msg, args...) }

// log dispatches to the underlying slog.Handler. We bypass slog.Logger
// so we can keep this package independent of slog's global default
// logger (the daemon and CLI must each have their own configured
// destination).
func (l *Logger) log(level slog.Level, msg string, args ...any) {
	if l == nil || l.handler == nil {
		return
	}
	ctx := context.Background()
	if !l.handler.Enabled(ctx, level) {
		return
	}
	// Skip [runtime.Callers, this function, the caller's wrapper].
	var pcs [1]uintptr
	runtime.Callers(3, pcs[:])
	r := slog.NewRecord(time.Now(), level, msg, pcs[0])
	r.Add(args...)
	_ = l.handler.Handle(ctx, r)
}

// noopHandler is the zero-cost handler used when NewLogger receives a
// nil writer. Every method short-circuits before any allocation.
type noopHandler struct{}

func (noopHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (noopHandler) Handle(context.Context, slog.Record) error { return nil }
func (n noopHandler) WithAttrs([]slog.Attr) slog.Handler      { return n }
func (n noopHandler) WithGroup(string) slog.Handler           { return n }
