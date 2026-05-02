package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/daemon"
	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/launchd"
	"github.com/tammersaleh/calendar-sync/internal/output"
)

// TestMapError covers MapError's full sentinel-routing surface. Each row
// asserts the (code, hint-presence) pair and that detail is non-empty so
// the SPEC's stderr envelope always carries something useful for the user
// to act on.
//
// SPEC's hint column (lines 1212-1245 / per-command hint expectations) is
// non-exhaustive - several codes have no documented hint, so we only
// assert hint presence for codes whose SPEC entry shows one. Everything
// else falls under "hint may or may not be set; we don't care".
func TestMapError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode string
		wantHint bool // true when SPEC documents a hint for this code
	}{
		{
			name:     "nil error returns empty code",
			err:      nil,
			wantCode: "",
			wantHint: false,
		},
		{
			name:     "cmdError pass-through preserves explicit code",
			err:      newCmdError(output.CodePairNotFound, "no such pair", "Check `pair list`.", nil),
			wantCode: output.CodePairNotFound,
			wantHint: true,
		},
		{
			name:     "cmdError without hint is fine",
			err:      newCmdError(output.CodeConfigInvalid, "bad", "", nil),
			wantCode: output.CodeConfigInvalid,
			wantHint: false,
		},
		{
			name:     "context.DeadlineExceeded -> timeout",
			err:      context.DeadlineExceeded,
			wantCode: output.CodeTimeout,
			wantHint: false,
		},
		{
			name:     "wrapped context.DeadlineExceeded -> timeout",
			err:      fmt.Errorf("call X: %w", context.DeadlineExceeded),
			wantCode: output.CodeTimeout,
			wantHint: false,
		},
		{
			name:     "context.Canceled -> timeout",
			err:      context.Canceled,
			wantCode: output.CodeTimeout,
			wantHint: false,
		},
		{
			name:     "config.ErrInvalid -> config_invalid",
			err:      config.ErrInvalid,
			wantCode: output.CodeConfigInvalid,
			wantHint: false,
		},
		{
			name:     "wrapped config.ErrInvalid -> config_invalid",
			err:      fmt.Errorf("validate: %w", config.ErrInvalid),
			wantCode: output.CodeConfigInvalid,
			wantHint: false,
		},
		{
			name:     "fs.ErrNotExist from a config path -> config_not_found with hint",
			err:      fmt.Errorf("config: read %q: %w", "/nonexistent/config.toml", fs.ErrNotExist),
			wantCode: output.CodeConfigNotFound,
			wantHint: true,
		},
		{
			name:     "fs.ErrNotExist NOT from config path -> default api_invalid_request",
			err:      fmt.Errorf("some other path: %w", fs.ErrNotExist),
			wantCode: output.CodeAPIInvalidRequest,
			wantHint: false,
		},
		{
			name:     "gws.ErrGWSNotFound -> gws_not_found with brew hint",
			err:      gws.ErrGWSNotFound,
			wantCode: output.CodeGWSNotFound,
			wantHint: true,
		},
		{
			name:     "wrapped gws.ErrGWSNotFound -> gws_not_found",
			err:      fmt.Errorf("launch: %w", gws.ErrGWSNotFound),
			wantCode: output.CodeGWSNotFound,
			wantHint: true,
		},
		{
			name:     "gws.ErrAPIAuthFailed -> api_auth_failed with hint",
			err:      gws.ErrAPIAuthFailed,
			wantCode: output.CodeAPIAuthFailed,
			wantHint: true,
		},
		{
			name:     "gws.ErrGWSAuthFailed -> gws_auth_failed with hint",
			err:      gws.ErrGWSAuthFailed,
			wantCode: output.CodeGWSAuthFailed,
			wantHint: true,
		},
		{
			name:     "gws.ErrAPINotFound -> api_not_found",
			err:      gws.ErrAPINotFound,
			wantCode: output.CodeAPINotFound,
			wantHint: false,
		},
		{
			name:     "gws.ErrAPIConflict -> api_conflict",
			err:      gws.ErrAPIConflict,
			wantCode: output.CodeAPIConflict,
			wantHint: false,
		},
		{
			name:     "gws.ErrAPIForbidden -> api_forbidden",
			err:      gws.ErrAPIForbidden,
			wantCode: output.CodeAPIForbidden,
			wantHint: false,
		},
		{
			name:     "gws.ErrRateLimited -> rate_limited",
			err:      gws.ErrRateLimited,
			wantCode: output.CodeRateLimited,
			wantHint: false,
		},
		{
			name:     "gws.ErrBackendError -> backend_error",
			err:      gws.ErrBackendError,
			wantCode: output.CodeBackendError,
			wantHint: false,
		},
		{
			name:     "gws.ErrNetworkError -> network_error",
			err:      gws.ErrNetworkError,
			wantCode: output.CodeNetworkError,
			wantHint: false,
		},
		{
			// SPEC's user-facing exit-code table doesn't enumerate 410 GONE;
			// the wrapper falls back to api_invalid_request.
			name:     "gws.ErrAPIGone -> api_invalid_request fallback",
			err:      gws.ErrAPIGone,
			wantCode: output.CodeAPIInvalidRequest,
			wantHint: false,
		},
		{
			name:     "gws.ErrAPIInvalidRequest -> api_invalid_request",
			err:      gws.ErrAPIInvalidRequest,
			wantCode: output.CodeAPIInvalidRequest,
			wantHint: false,
		},
		{
			name:     "daemon.ErrDaemonAlreadyRunning -> daemon_already_running with hint",
			err:      daemon.ErrDaemonAlreadyRunning,
			wantCode: output.CodeDaemonAlreadyRunning,
			wantHint: true,
		},
		{
			name:     "daemon.ErrAuthFailed -> gws_auth_failed with hint",
			err:      daemon.ErrAuthFailed,
			wantCode: output.CodeGWSAuthFailed,
			wantHint: true,
		},
		{
			name:     "launchd.ErrNotMacOS -> not_macos",
			err:      launchd.ErrNotMacOS,
			wantCode: output.CodeNotMacOS,
			wantHint: false,
		},
		{
			name:     "launchd.ErrPlistExists -> plist_exists with hint",
			err:      launchd.ErrPlistExists,
			wantCode: output.CodePlistExists,
			wantHint: true,
		},
		{
			name:     "launchd.ErrPlistNotFound -> plist_not_found",
			err:      launchd.ErrPlistNotFound,
			wantCode: output.CodePlistNotFound,
			wantHint: false,
		},
		{
			name:     "launchd.ErrLaunchctlFailed -> launchctl_failed",
			err:      launchd.ErrLaunchctlFailed,
			wantCode: output.CodeLaunchctlFailed,
			wantHint: false,
		},
		{
			name:     "launchd.ErrBinaryNotResolvable -> binary_not_resolvable",
			err:      launchd.ErrBinaryNotResolvable,
			wantCode: output.CodeBinaryNotResolvable,
			wantHint: false,
		},
		{
			name:     "unknown error falls through to api_invalid_request",
			err:      errors.New("totally unrelated failure"),
			wantCode: output.CodeAPIInvalidRequest,
			wantHint: false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, detail, hint := MapError(tc.err)
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
			// nil error: every field empty.
			if tc.err == nil {
				if detail != "" || hint != "" {
					t.Errorf("nil err must produce empty detail/hint, got detail=%q hint=%q",
						detail, hint)
				}
				return
			}
			if detail == "" {
				t.Errorf("detail empty; SPEC requires a non-empty detail on every error")
			}
			if tc.wantHint && hint == "" {
				t.Errorf("hint empty; SPEC documents a hint for code %q", tc.wantCode)
			}
		})
	}
}

// TestMapError_GWSNotFoundHintMentionsBrew pins the SPEC's user-facing
// guidance: a missing gws binary should tell the user how to install it.
func TestMapError_GWSNotFoundHintMentionsBrew(t *testing.T) {
	_, _, hint := MapError(gws.ErrGWSNotFound)
	if !strings.Contains(hint, "brew install") {
		t.Errorf("hint = %q, want mention of brew install", hint)
	}
}

// TestMapError_DetailIncludesUnderlyingMessage ensures the SPEC's `detail`
// surfaces the wrapped error's message, not a stripped-down code-only
// string. Important for debugging - the user needs to see WHAT failed.
func TestMapError_DetailIncludesUnderlyingMessage(t *testing.T) {
	wrapped := fmt.Errorf("call to events.list: %w", gws.ErrAPIConflict)
	_, detail, _ := MapError(wrapped)
	if !strings.Contains(detail, "events.list") {
		t.Errorf("detail = %q, want it to include the call context", detail)
	}
}

// TestUnwrapCause covers the helper that populates ErrorEnvelope.Cause from
// the wrapped error chain. The partial_failure path (cmd/run.go) wraps the
// first PDir error as the cmdError's cause; without unwrapCause the
// underlying gws/sync error never reaches stderr.
func TestUnwrapCause(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{
			name: "no cause returns empty",
			err:  errors.New("no cause"),
			want: "",
		},
		{
			name: "cmdError with cause returns cause text",
			err: newCmdError("partial_failure", "1 pdir(s) failed",
				"", fmt.Errorf("classify src/evt: %w", gws.ErrAPIConflict)),
			want: "classify src/evt: " + gws.ErrAPIConflict.Error(),
		},
		{
			name: "fmt.Errorf wrapped error returns the wrapped message",
			err:  fmt.Errorf("outer: %w", errors.New("inner")),
			want: "inner",
		},
		{
			// Production partial_failure shape: Reconciler.runClassifyLoop
			// aggregates per-event classify errors via errors.Join, and the
			// joined value becomes cmdError.cause. errors.Unwrap returns nil
			// on a joinError, so the previous implementation silently dropped
			// the cause; reading cmdError.cause directly preserves it.
			name: "cmdError wrapping errors.Join surfaces the joined text",
			err: newCmdError("partial_failure", "1 pdir(s) failed", "",
				errors.Join(
					fmt.Errorf("classify a/b: %w", gws.ErrAPIConflict),
					fmt.Errorf("classify a/c: %w", gws.ErrAPIConflict),
				)),
			want: fmt.Errorf("classify a/b: %w", gws.ErrAPIConflict).Error() +
				"\n" +
				fmt.Errorf("classify a/c: %w", gws.ErrAPIConflict).Error(),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := unwrapCause(tc.err)
			if got != tc.want {
				t.Errorf("unwrapCause = %q, want %q", got, tc.want)
			}
		})
	}
}
