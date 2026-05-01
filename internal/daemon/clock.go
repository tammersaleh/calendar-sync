package daemon

import "time"

// Clock is the time abstraction the scheduler reads. Production passes a
// realClock wrapping time.Now/time.After; tests inject a fakeClock so the
// scheduler fires deterministically without relying on wall-clock sleeps.
//
// The two methods mirror the bits of time.* the scheduler actually needs:
//
//   - Now returns the current wall-clock time. Used to compute the next-tick
//     boundary via SPEC §"Sleep and wake" (`now.Truncate(p).Add(p)`).
//   - After returns a channel that fires after d. Used by the main loop's
//     select to wake up at the next scheduler event.
type Clock interface {
	Now() time.Time
	After(d time.Duration) <-chan time.Time
}

// realClock is the production Clock implementation. Methods delegate to
// the time package.
type realClock struct{}

// Now returns time.Now().
func (realClock) Now() time.Time { return time.Now() }

// After returns time.After(d).
func (realClock) After(d time.Duration) <-chan time.Time { return time.After(d) }
