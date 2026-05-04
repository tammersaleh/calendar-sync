//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// wipeCalendar removes every event on calendarID, both alive and
// tombstoned. The tombstone case matters: calendar-sync's source list
// passes `ShowDeleted=true` and treats `status=cancelled` events as
// classification inputs (they fire `skip(cancelled)` outcomes). A wipe
// that ignored tombstones would let test A's deleted event leak into
// test B's run output as `skip(cancelled)`, contaminating outcome
// assertions and growing wall-clock as the calendar accumulates dead
// history.
//
// Already-cancelled events come back from EventsList with
// status=cancelled; they're skipped because they're already in the
// state we want and a redundant delete would 410. A 410 Gone or 404
// Not Found on delete (the event vanished between list and delete -
// common when a recurring parent's delete cascades during the loop)
// is treated as already-deleted.
//
// Wholesale because the harness owns the entire calendar; no need to
// be surgical.
func wipeCalendar(ctx context.Context, c *gws.Client, calendarID string) error {
	events, _, err := c.EventsList(ctx, gws.EventsListParams{
		CalendarID:  calendarID,
		ShowDeleted: true,
		MaxResults:  2500, // Calendar API max
	})
	if err != nil {
		return fmt.Errorf("list events for wipe (%s): %w", calendarID, err)
	}

	for _, ev := range events {
		if ev.ID == "" {
			continue
		}
		if ev.Status == gws.EventStatusCancelled {
			// Already in the wanted state; another delete would 410.
			continue
		}
		if err := c.EventsDelete(ctx, calendarID, ev.ID); err != nil {
			if errors.Is(err, gws.ErrAPIGone) || errors.Is(err, gws.ErrAPINotFound) {
				continue
			}
			return fmt.Errorf("delete %s/%s during wipe: %w", calendarID, ev.ID, err)
		}
	}
	return nil
}
