package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/daemon"
	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/launchd"
	"github.com/tammersaleh/calendar-sync/internal/output"
)

// cmdError is the typed error subcommands return when they want to attach a
// SPEC error code without forcing the caller to match by sentinel. Outer
// MapError unwraps it preferentially; non-cmdError errors fall through to
// the sentinel-matching path.
type cmdError struct {
	code   string
	detail string
	hint   string
	cause  error
}

// Error returns a human-readable rendering. The detail field is the primary
// surface; cause is appended when present so log lines retain provenance.
func (e *cmdError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %s", e.code, e.detail, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.code, e.detail)
}

// Unwrap exposes the wrapped cause for errors.Is / errors.As traversal.
func (e *cmdError) Unwrap() error { return e.cause }

// newCmdError builds a typed error a subcommand can return directly.
func newCmdError(code, detail, hint string, cause error) *cmdError {
	return &cmdError{code: code, detail: detail, hint: hint, cause: cause}
}

// MapError translates an error returned from a subcommand into the SPEC
// (code, detail, hint) triple that the outer Run loop emits via
// output.EmitError. Order matters: cmdError wraps win (the subcommand
// already chose a code); after that we walk the typed sentinels each
// internal package exposes; the default case is api_invalid_request which
// SPEC's exit-code table maps to exit 1.
func MapError(err error) (code, detail, hint string) {
	if err == nil {
		return "", "", ""
	}

	// Subcommand-attached error: prefer the explicit code.
	var ce *cmdError
	if errors.As(err, &ce) {
		return ce.code, ce.detail, ce.hint
	}

	// Context cancellations from --timeout / SIGINT.
	if errors.Is(err, context.DeadlineExceeded) {
		return output.CodeTimeout, err.Error(), ""
	}
	if errors.Is(err, context.Canceled) {
		return output.CodeTimeout, err.Error(), ""
	}

	// Config layer.
	switch {
	case errors.Is(err, config.ErrInvalid):
		return output.CodeConfigInvalid, err.Error(), ""
	}
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
		// fs.ErrNotExist commonly bubbles up from config.Load when the file
		// is missing. SPEC §"calendar-sync config show" maps this to
		// config_not_found with a suggestion to run `init`.
		if isConfigPathError(err) {
			return output.CodeConfigNotFound, err.Error(),
				"Run `calendar-sync init` to generate a starter config."
		}
	}

	// gws layer. ErrGWSNotFound is a *gws.Error - it does NOT unwrap to
	// fs.ErrNotExist, so the fs.ErrNotExist branch above won't accidentally
	// claim it. The ordering is safe.
	switch {
	case errors.Is(err, gws.ErrGWSNotFound):
		return output.CodeGWSNotFound, err.Error(),
			"Install gws via `brew install googleworkspace/cli/gws` and ensure it's on PATH."
	case errors.Is(err, gws.ErrAPIAuthFailed):
		return output.CodeAPIAuthFailed, err.Error(), "Run `gws auth login` and retry."
	case errors.Is(err, gws.ErrGWSAuthFailed):
		return output.CodeGWSAuthFailed, err.Error(), "Run `gws auth login` and retry."
	case errors.Is(err, gws.ErrAPINotFound):
		return output.CodeAPINotFound, err.Error(), ""
	case errors.Is(err, gws.ErrAPIConflict):
		return output.CodeAPIConflict, err.Error(), ""
	case errors.Is(err, gws.ErrAPIForbidden):
		return output.CodeAPIForbidden, err.Error(), ""
	case errors.Is(err, gws.ErrRateLimited):
		return output.CodeRateLimited, err.Error(), ""
	case errors.Is(err, gws.ErrBackendError):
		return output.CodeBackendError, err.Error(), ""
	case errors.Is(err, gws.ErrNetworkError):
		return output.CodeNetworkError, err.Error(), ""
	case errors.Is(err, gws.ErrAPIGone):
		return output.CodeAPIInvalidRequest, err.Error(), ""
	case errors.Is(err, gws.ErrAPIInvalidRequest):
		return output.CodeAPIInvalidRequest, err.Error(), ""
	}

	// daemon layer.
	switch {
	case errors.Is(err, daemon.ErrDaemonAlreadyRunning):
		return output.CodeDaemonAlreadyRunning, err.Error(),
			"Stop the running daemon first (`calendar-sync uninstall`)."
	case errors.Is(err, daemon.ErrAuthFailed):
		return output.CodeGWSAuthFailed, err.Error(), "Run `gws auth login` and retry."
	}

	// launchd layer.
	switch {
	case errors.Is(err, launchd.ErrNotMacOS):
		return output.CodeNotMacOS, err.Error(), ""
	case errors.Is(err, launchd.ErrPlistExists):
		return output.CodePlistExists, err.Error(), "Re-run with --force to overwrite."
	case errors.Is(err, launchd.ErrPlistNotFound):
		return output.CodePlistNotFound, err.Error(), ""
	case errors.Is(err, launchd.ErrLaunchctlFailed):
		return output.CodeLaunchctlFailed, err.Error(), ""
	case errors.Is(err, launchd.ErrBinaryNotResolvable):
		return output.CodeBinaryNotResolvable, err.Error(), ""
	}

	// Fallthrough: untyped error. SPEC's "general error" bucket maps to
	// exit 1 via api_invalid_request, which is the safest catch-all that
	// still lands in the documented error vocabulary.
	return output.CodeAPIInvalidRequest, err.Error(), ""
}

// isConfigPathError heuristically reports whether an os.ErrNotExist
// originates from config.Load. The config package wraps the path into the
// error message via fmt.Errorf, so a substring match is sufficient and
// avoids tying MapError to the exact wrap shape.
func isConfigPathError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "config:") ||
		strings.Contains(msg, "config.toml")
}
