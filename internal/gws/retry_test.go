package gws

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Tests in this file are package-internal (not gws_test) because they
// exercise withRetry directly. CLAUDE.md "Testing" allows package-internal
// tests for functions only accessible within the package.

// programmedFn returns a func that emits the supplied errors in order. The
// last error is returned for any call beyond the configured length so we
// can detect "kept calling past the configured limit" by counting calls.
func programmedFn(t *testing.T, errs []error) (func() error, *int) {
	t.Helper()
	n := 0
	return func() error {
		idx := n
		n++
		if idx >= len(errs) {
			return errs[len(errs)-1]
		}
		return errs[idx]
	}, &n
}

// instantClock is a clock that records each requested wait but never
// actually sleeps. Tests use it to assert backoff-schedule fidelity
// without burning wall-clock seconds.
type instantClock struct {
	waits []time.Duration
}

func (c *instantClock) Sleep(_ context.Context, d time.Duration) error {
	c.waits = append(c.waits, d)
	return nil
}

// blockingClock is a clock that blocks on Sleep until either the supplied
// context fires or the test goroutine signals via release(). Used to model
// "we're mid-backoff and the parent ctx gets canceled" without timing
// races.
type blockingClock struct {
	release chan struct{}
}

func newBlockingClock() *blockingClock {
	return &blockingClock{release: make(chan struct{})}
}

func (c *blockingClock) Sleep(ctx context.Context, _ time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.release:
		return nil
	}
}

func TestRetry_RateLimitedRetriesUpToMax(t *testing.T) {
	rateErr := &Error{Code: CodeRateLimited, Op: "events.list"}
	successAfter := error(nil)
	fn, calls := programmedFn(t, []error{rateErr, rateErr, successAfter})

	clk := &instantClock{}
	err := withRetry(context.Background(), retryConfig{
		MaxAttempts: 5,
		Schedule:    []time.Duration{10 * time.Millisecond, 20 * time.Millisecond, 40 * time.Millisecond, 80 * time.Millisecond},
		Clock:       clk,
		Op:          "events.list",
	}, fn)

	if err != nil {
		t.Fatalf("withRetry returned err = %v, want nil", err)
	}
	if *calls != 3 {
		t.Errorf("call count = %d, want 3", *calls)
	}
	if len(clk.waits) != 2 {
		t.Errorf("backoff count = %d, want 2", len(clk.waits))
	}
}

func TestRetry_BackendErrorRetried(t *testing.T) {
	beErr := &Error{Code: CodeBackendError, Op: "events.insert"}
	fn, calls := programmedFn(t, []error{beErr, nil})

	clk := &instantClock{}
	err := withRetry(context.Background(), retryConfig{
		MaxAttempts: 5,
		Schedule:    []time.Duration{1 * time.Millisecond},
		Clock:       clk,
		Op:          "events.insert",
	}, fn)

	if err != nil {
		t.Fatalf("withRetry returned err = %v, want nil", err)
	}
	if *calls != 2 {
		t.Errorf("call count = %d, want 2", *calls)
	}
	if len(clk.waits) != 1 {
		t.Errorf("backoff count = %d, want 1", len(clk.waits))
	}
}

func TestRetry_NonRetryableReturnsImmediately(t *testing.T) {
	tests := []struct {
		name string
		code string
	}{
		{"api_auth_failed", CodeAPIAuthFailed},
		{"api_not_found", CodeAPINotFound},
		{"api_conflict", CodeAPIConflict},
		{"api_gone", CodeAPIGone},
		{"api_invalid_request", CodeAPIInvalidRequest},
		{"api_forbidden", CodeAPIForbidden},
		// network_error is gws's own internal-retry territory per SPEC
		// line 1404 ("the gws subprocess handles its own retries on those
		// internally"); calendar-sync does not retry these.
		{"network_error", CodeNetworkError},
		{"gws_auth_failed", CodeGWSAuthFailed},
		{"gws_not_found", CodeGWSNotFound},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gwsErr := &Error{Code: tc.code, Op: "events.list"}
			fn, calls := programmedFn(t, []error{gwsErr, gwsErr, gwsErr})

			clk := &instantClock{}
			err := withRetry(context.Background(), retryConfig{
				MaxAttempts: 5,
				Schedule:    []time.Duration{1 * time.Millisecond},
				Clock:       clk,
				Op:          "events.list",
			}, fn)

			if !errors.Is(err, gwsErr) {
				t.Errorf("err = %v, want errors.Is == %s", err, tc.code)
			}
			if *calls != 1 {
				t.Errorf("call count = %d, want 1 (no retry on non-retryable)", *calls)
			}
			if len(clk.waits) != 0 {
				t.Errorf("backoff count = %d, want 0 (no sleep on non-retryable)", len(clk.waits))
			}
		})
	}
}

func TestRetry_ExhaustsMaxAttempts(t *testing.T) {
	rateErr := &Error{Code: CodeRateLimited, Op: "events.list"}
	// Persistent failure across all 5 attempts.
	fn, calls := programmedFn(t, []error{rateErr, rateErr, rateErr, rateErr, rateErr})

	clk := &instantClock{}
	err := withRetry(context.Background(), retryConfig{
		MaxAttempts: 5,
		Schedule:    []time.Duration{1 * time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 8 * time.Millisecond},
		Clock:       clk,
		Op:          "events.list",
	}, fn)

	if !errors.Is(err, rateErr) {
		t.Errorf("err = %v, want errors.Is == %s after exhaustion", err, CodeRateLimited)
	}
	if *calls != 5 {
		t.Errorf("call count = %d, want 5 (max attempts)", *calls)
	}
	if len(clk.waits) != 4 {
		t.Errorf("backoff count = %d, want 4 (one less than max attempts)", len(clk.waits))
	}
}

func TestRetry_ContextCancelDuringBackoffAbortsCleanly(t *testing.T) {
	rateErr := &Error{Code: CodeRateLimited, Op: "events.list"}
	fn, calls := programmedFn(t, []error{rateErr, rateErr, rateErr})

	clk := newBlockingClock()
	ctx, cancel := context.WithCancel(context.Background())

	// Cancel the context once the first retry's backoff begins. We detect
	// "in backoff" by polling for a recorded call attempt of >= 1 to have
	// returned; the simplest signal is to cancel from a goroutine after a
	// short delay - the blockingClock.Sleep is what the goroutine races
	// against.
	done := make(chan error, 1)
	go func() {
		done <- withRetry(ctx, retryConfig{
			MaxAttempts: 5,
			Schedule:    []time.Duration{1 * time.Hour, 1 * time.Hour, 1 * time.Hour, 1 * time.Hour},
			Clock:       clk,
			Op:          "events.list",
		}, fn)
	}()

	// Give the goroutine time to call fn at least once and reach Sleep.
	// The blockingClock guarantees Sleep stays blocked until either
	// release or ctx.Done(); calling cancel() resolves the ctx.Done()
	// branch.
	time.Sleep(20 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("err = %v, want errors.Is(err, context.Canceled)", err)
		}
		if *calls < 1 {
			t.Errorf("call count = %d, want >= 1 (first attempt should have run)", *calls)
		}
		// We should NOT see a 2nd attempt since the ctx fired during the
		// backoff for attempt 1.
		if *calls > 1 {
			t.Errorf("call count = %d, want exactly 1 (no 2nd attempt after ctx cancel)", *calls)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("withRetry did not return after ctx cancel")
	}
}

// TestRetry_RetryAfterHonored is documented in SPEC line 1395 ("capped by
// Retry-After if present"). gws today does not expose Retry-After through
// its error envelope, so calendar-sync cannot honor it without a gws
// schema change. Skipped pending plumbing.
//
// TODO: lift this skip once gws emits Retry-After alongside the existing
// Calendar API error envelope.
func TestRetry_RetryAfterHonored(t *testing.T) {
	t.Skip("Retry-After is not exposed by gws today; SPEC line 1395 anticipates the field but the wire format does not carry it. TODO when gws plumbs it through.")
}
