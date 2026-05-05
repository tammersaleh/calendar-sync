package gws

import "time"

// NewWithFastRetryForTest constructs a gws.Client with a near-zero
// inter-attempt backoff so integration tests can exercise the full retry
// path without burning wall-clock seconds. MaxAttempts stays at SPEC's
// 5; only the schedule is compressed.
//
// _test.go suffix scopes the symbol to test builds; production callers
// cannot import it, which keeps the user-facing surface limited to
// WithMaxAttempts.
func NewWithFastRetryForTest() *Client {
	return New(withRetryConfig(retryConfig{
		MaxAttempts: 5,
		Schedule: []time.Duration{
			time.Nanosecond,
			time.Nanosecond,
			time.Nanosecond,
			time.Nanosecond,
		},
		// Negative disables jitter; the schedule is already at the
		// minimum sleep so jitter on top would just round-trip
		// through the timer for no benefit.
		JitterFraction: -1,
	}))
}
