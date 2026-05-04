//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// wipeCalendar moves every alive event on calendarID to cancelled.
// Already-cancelled events are skipped (a redundant delete would 410).
// 404/410 from the delete (the event vanished between list and delete -
// common when a recurring parent's delete cascades through its
// instances) is treated as already-deleted.
//
// Wholesale because the harness owns the entire calendar.
//
// Important: this DOES NOT remove tombstones. Calendar API retains
// cancelled events for a window (typically ~30 days) and surfaces them
// on `EventsList(ShowDeleted=true)`, which is what calendar-sync's
// source list uses. So test A's deleted source event continues to
// appear in test B's run output as `skip(cancelled)`.
//
// Tests must filter outcome assertions by SourceEvent ID
// (OutcomeMatch.SourceEvent or AssertNoOutcomeForSource) to ignore
// tombstones from prior tests. Asserting total outcome counts or
// `_meta.skips` would false-fail as the calendar accumulates history.
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
