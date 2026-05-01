package daemon

import (
	"context"
	"errors"
	"fmt"
)

// AuthChecker probes whether the gws subprocess can reach Google. Production
// wiring runs `gws auth status` and returns nil on exit 0; tests pass a
// closure. A non-nil error from AuthChecker fails Daemon.Run before any
// other side effects (no socket bind, no FullSync). Per SPEC §"Authentication"
// (lines 326-336), the caller is responsible for mapping the resulting error
// to exit code 2 - this package just returns it wrapped in ErrAuthFailed.
type AuthChecker func(ctx context.Context) error

// ErrAuthFailed is returned by Daemon.Run when AuthChecker reports a non-nil
// error. The caller (cmd/calendar-sync/watch.go) tests for this with
// errors.Is to map it to SPEC's exit code 2 / `gws_auth_failed`. The
// underlying AuthChecker error is wrapped via fmt.Errorf("%w: %w", ...) so
// callers get both signals - the sentinel for routing and the cause for
// surfacing as `cause` in the error JSON.
var ErrAuthFailed = errors.New("daemon: gws auth status failed")

// runAuthCheck calls d.AuthChecker if set. nil checker is a tests-only path
// (no auth probe). Returns nil on success, ErrAuthFailed wrapping the
// underlying cause on failure.
func runAuthCheck(ctx context.Context, checker AuthChecker) error {
	if checker == nil {
		return nil
	}
	if err := checker(ctx); err != nil {
		return fmt.Errorf("%w: %w", ErrAuthFailed, err)
	}
	return nil
}
