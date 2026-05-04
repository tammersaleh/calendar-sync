//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// wipeCalendar deletes every alive event on calendarID. Idempotent:
// already-cancelled events drop out of the listing on showDeleted=false
// and don't need re-deleting; a 410 Gone on delete (the event vanished
// between list and delete) is treated as success.
//
// Recurring parents: events.delete on the parent cascades to all
// instances on Google's side. Standalone instance overrides without a
// live parent reference are deleted individually.
//
// Wipe is wholesale because the harness owns the entire calendar.
// We don't try to be surgical (e.g. delete only the events this test
// created) - simpler and more reliable.
func wipeCalendar(ctx context.Context, c *gws.Client, calendarID string) error {
	events, _, err := c.EventsList(ctx, gws.EventsListParams{
		CalendarID:  calendarID,
		ShowDeleted: false,
		MaxResults:  2500, // Calendar API max
	})
	if err != nil {
		return fmt.Errorf("list events for wipe (%s): %w", calendarID, err)
	}

	for _, ev := range events {
		if ev.ID == "" {
			continue
		}
		if err := c.EventsDelete(ctx, calendarID, ev.ID); err != nil {
			// 410 Gone after a successful list happens when a
			// recurring parent's delete cascades during this loop;
			// instances ahead of us in `events` may already be gone.
			// Treat as already-deleted.
			if errors.Is(err, gws.ErrAPIGone) {
				continue
			}
			// 404 likewise: the event vanished out from under us.
			// Tolerable for a wipe.
			if errors.Is(err, gws.ErrAPINotFound) {
				continue
			}
			return fmt.Errorf("delete %s/%s during wipe: %w", calendarID, ev.ID, err)
		}
	}
	return nil
}
