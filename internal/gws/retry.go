package gws

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// SPEC §"Retry policy" (line 1393): retry events.list / events.insert /
// events.patch / events.delete on rate-limited or backend-error responses
// with exponential backoff and jitter, 5 attempts max, capped by
// Retry-After when present.
//
// Defaults SPEC pins:
//   - 5 attempts total (so 4 backoffs between them).
//   - Schedule 1s, 2s, 4s, 8s, 16s. With 5 attempts we use the first four
//     entries; the 16s slot is unused at the current ceiling.
//   - Per-attempt jitter applied as a multiplicative factor in
//     [1-jitterFraction, 1+jitterFraction]; defaults to 25%.
//
// Retry-After is documented in SPEC but gws does not expose it in its
// error envelope today, so the implementation has no path to honor it.
// Tracked upstream in https://github.com/googleworkspace/cli/issues/777;
// once gws plumbs the header value through, wire it in here as a
// per-attempt override of the schedule.
const (
	defaultMaxAttempts    = 5
	defaultJitterFraction = 0.25
)

// defaultRetrySchedule is the 4-entry schedule SPEC line 1395 specifies.
// Each entry is the backoff to wait BEFORE the next attempt (so
// attempt 1 always runs immediately).
var defaultRetrySchedule = []time.Duration{
	1 * time.Second,
	2 * time.Second,
	4 * time.Second,
	8 * time.Second,
}

// retryClock abstracts time.Sleep so unit tests can drive backoff timing
// without burning wall-clock seconds. The production implementation is
// realClock and uses a context-cancelable timer.
type retryClock interface {
	// Sleep blocks for d or until ctx fires, whichever happens first.
	// Returns ctx.Err() on cancellation, nil on completion.
	Sleep(ctx context.Context, d time.Duration) error
}

type realClock struct{}

func (realClock) Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// retryConfig drives one withRetry call. Production wiring leaves Schedule
// nil (defaultRetrySchedule kicks in) and Clock nil (realClock); tests
// inject both. MaxAttempts <= 0 falls through to defaultMaxAttempts.
//
// Op is the SPEC's `endpoint` log field (events.list, events.patch, etc.).
// It is also embedded in the warn-log emitted on every retry.
//
// Logger is optional: nil silences retry warns (the rest of the gws
// client follows the same nil-safe convention).
//
// JitterFraction == 0 falls back to the default 25%. Negative values
// disable jitter (tests use this for exact schedule assertions).
//
// JitterRand is a PRNG handle; nil falls back to rand.Float64() against
// the global default source. Set in tests for deterministic jitter.
type retryConfig struct {
	MaxAttempts    int
	Schedule       []time.Duration
	Clock          retryClock
	Logger         Logger
	Op             string
	JitterFraction float64
	JitterRand     *rand.Rand
}

func (c retryConfig) maxAttempts() int {
	if c.MaxAttempts <= 0 {
		return defaultMaxAttempts
	}
	return c.MaxAttempts
}

func (c retryConfig) schedule() []time.Duration {
	if len(c.Schedule) == 0 {
		return defaultRetrySchedule
	}
	return c.Schedule
}

func (c retryConfig) clock() retryClock {
	if c.Clock == nil {
		return realClock{}
	}
	return c.Clock
}

// jitter returns a multiplicative factor in [1-f, 1+f]. f == 0 uses the
// default fraction; f < 0 disables jitter (returns 1.0 exactly). f >= 1
// is clamped to <1 to keep the multiplier positive.
func (c retryConfig) jitter() float64 {
	if c.JitterFraction < 0 {
		return 1.0
	}
	f := c.JitterFraction
	if f == 0 {
		f = defaultJitterFraction
	}
	if f >= 1 {
		f = 0.99
	}
	var r float64
	if c.JitterRand != nil {
		r = c.JitterRand.Float64()
	} else {
		r = rand.Float64() //nolint:gosec // jitter is non-cryptographic
	}
	// Map r in [0, 1) onto [1-f, 1+f).
	return 1 + ((2*r)-1)*f
}

// withRetry runs fn up to cfg.MaxAttempts times. After each failed
// attempt that returns a *gws.Error in the retryable set, withRetry
// sleeps for the next entry in cfg.Schedule (with jitter) and tries
// again. Non-retryable errors short-circuit immediately. ctx
// cancellation during a sleep aborts cleanly with ctx.Err().
//
// The maxAttempts ceiling counts ATTEMPTS, not retries: 5 attempts means
// fn runs up to 5 times. The schedule supplies the sleeps BETWEEN
// attempts; if the schedule is shorter than maxAttempts-1, the last
// schedule entry repeats for any tail attempts (defensive; production
// pairs them 1:1).
func withRetry(ctx context.Context, cfg retryConfig, fn func() error) error {
	max := cfg.maxAttempts()
	schedule := cfg.schedule()
	clk := cfg.clock()

	var lastErr error
	for attempt := 1; attempt <= max; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if !isRetryable(lastErr) {
			return lastErr
		}
		if attempt == max {
			return lastErr
		}

		// Pick the schedule entry for the gap between this attempt and
		// the next. Index is attempt-1 (attempt 1 → schedule[0] etc.).
		// If the schedule is shorter, clamp to the final entry so we
		// degrade to a constant tail rather than panicking.
		idx := attempt - 1
		if idx >= len(schedule) {
			idx = len(schedule) - 1
		}
		wait := time.Duration(float64(schedule[idx]) * cfg.jitter())
		if wait < 0 {
			wait = schedule[idx]
		}

		warnRetry(cfg.Logger, cfg.Op, attempt, max, wait, lastErr)

		if err := clk.Sleep(ctx, wait); err != nil {
			return err
		}
	}
	return lastErr
}

// isRetryable returns true when err is a *gws.Error whose Code is in the
// retry set SPEC line 1395 enumerates: rate_limited (429 / 403 rate
// reasons) and backend_error (500 / 503). Every other code returns false
// immediately (auth failures, not-found, gone, conflict, invalid request,
// generic forbidden, network error, gws-launch failures - none of those
// are improved by retrying).
func isRetryable(err error) bool {
	var gerr *Error
	if !errors.As(err, &gerr) {
		return false
	}
	switch gerr.Code {
	case CodeRateLimited, CodeBackendError:
		return true
	default:
		return false
	}
}

// warnRetry emits SPEC line 1408's warn-log for one retry decision. Field
// names match the SPEC sample: endpoint, attempt, wait_ms (rounded to
// milliseconds), and the typed cause string. Status and reason are best-
// effort: when the underlying *gws.Error carries them they are surfaced
// alongside the rest. A nil logger silences output.
func warnRetry(log Logger, op string, attempt, max int, wait time.Duration, cause error) {
	if log == nil {
		return
	}
	args := []any{
		"endpoint", op,
		"attempt", attempt,
		"max_attempts", max,
		"wait_ms", wait.Milliseconds(),
		"cause", cause.Error(),
	}
	var gerr *Error
	if errors.As(cause, &gerr) {
		if gerr.HTTPStatus != 0 {
			args = append(args, "status", gerr.HTTPStatus)
		}
		if gerr.Reason != "" {
			args = append(args, "reason", gerr.Reason)
		}
	}
	log.Warn("retrying", args...)
}
