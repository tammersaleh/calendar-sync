//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// fixture names are stable strings the harness uses to identify its own
// calendars in the user's calendarList. Every fixture calendar carries
// fixtureMarker in its description; the harness checks for the marker
// before deleting anything it didn't create. A name collision with a
// real user calendar is then surfaced as a failure rather than a
// silent destruction.
const (
	sourceCalSummary = "calendar-sync-e2e-source"
	targetCalSummary = "calendar-sync-e2e-target"
	fixtureMarker    = "calendar-sync E2E test fixture; safe to delete"
	fixtureTimeZone  = "UTC"
)

// fixtureSummaries is the canonical ordering used by createFixtures /
// destroyFixtures so source and target calendars get the same role each
// run.
var fixtureSummaries = []string{sourceCalSummary, targetCalSummary}

// createFixtures provisions the harness's source/target calendars,
// destroying any pre-existing fixtures with matching summaries first
// so the run starts from a clean slate. Returns canonical IDs in the
// same order as fixtureSummaries.
//
// Safety: a calendar matching by summary but NOT carrying fixtureMarker
// in its description is treated as a real user calendar and the call
// fails. The harness will not delete anything it didn't create.
func createFixtures(ctx context.Context, c *gws.Client) (sourceID, targetID string, err error) {
	if err := destroyFixtures(ctx, c); err != nil {
		return "", "", fmt.Errorf("pre-create cleanup: %w", err)
	}

	created := make(map[string]string, len(fixtureSummaries))
	for _, summary := range fixtureSummaries {
		cal, err := c.CalendarsInsert(ctx, &gws.Calendar{
			Summary:     summary,
			Description: fixtureMarker,
			TimeZone:    fixtureTimeZone,
		})
		if err != nil {
			return "", "", fmt.Errorf("create fixture %q: %w", summary, err)
		}
		if cal.ID == "" {
			return "", "", fmt.Errorf("create fixture %q: response missing id", summary)
		}
		created[summary] = cal.ID
	}

	// Newly created calendars take a moment to appear in calendarList;
	// the events.* endpoints accept the new id immediately, so the
	// delay only matters for code paths that resolve by listing. None
	// of our scenarios do today (we hand IDs straight to config), but
	// poll briefly so a future "look up by summary mid-test" doesn't
	// flake.
	if err := waitForFixturesVisible(ctx, c, created); err != nil {
		return "", "", err
	}

	return created[sourceCalSummary], created[targetCalSummary], nil
}

// destroyFixtures deletes any calendar in the user's calendarList whose
// summary matches a fixture name AND whose description carries the
// fixture marker. Idempotent: a missing fixture is not an error. A
// summary collision without the marker is a hard error.
func destroyFixtures(ctx context.Context, c *gws.Client) error {
	entries, err := c.CalendarListList(ctx)
	if err != nil {
		return fmt.Errorf("list calendars: %w", err)
	}

	wanted := make(map[string]bool, len(fixtureSummaries))
	for _, s := range fixtureSummaries {
		wanted[s] = true
	}

	for _, e := range entries {
		if !wanted[e.Summary] {
			continue
		}
		if e.AccessRole != "owner" {
			return fmt.Errorf("fixture-name collision: calendar %q has accessRole=%q (not owner); refusing to delete", e.Summary, e.AccessRole)
		}
		// CalendarListEntry doesn't carry description; fetch the
		// underlying Calendar resource so the safety marker check
		// looks at the same field createFixtures populates.
		cal, err := c.CalendarsGet(ctx, e.ID)
		if err != nil {
			return fmt.Errorf("verify fixture marker on %q (%s): %w", e.Summary, e.ID, err)
		}
		if cal.Description != fixtureMarker {
			return fmt.Errorf("fixture-name collision: calendar %q (%s) does not carry the harness safety marker; refusing to delete (description=%q)", e.Summary, e.ID, cal.Description)
		}
		if err := c.CalendarsDelete(ctx, e.ID); err != nil {
			return fmt.Errorf("delete fixture %q (%s): %w", e.Summary, e.ID, err)
		}
	}
	return nil
}

// waitForFixturesVisible polls calendarList until every newly created
// fixture appears, or the deadline passes. Calendar creation is
// generally available immediately on the events endpoint but
// calendarList propagation lags a few seconds.
func waitForFixturesVisible(ctx context.Context, c *gws.Client, created map[string]string) error {
	deadline := time.Now().Add(20 * time.Second)
	for {
		entries, err := c.CalendarListList(ctx)
		if err != nil {
			return fmt.Errorf("list calendars during fixture wait: %w", err)
		}
		seen := make(map[string]bool, len(created))
		for _, e := range entries {
			if _, ok := created[e.Summary]; ok && e.ID == created[e.Summary] {
				seen[e.Summary] = true
			}
		}
		if len(seen) == len(created) {
			return nil
		}
		if time.Now().After(deadline) {
			missing := []string{}
			for s := range created {
				if !seen[s] {
					missing = append(missing, s)
				}
			}
			return fmt.Errorf("fixtures not visible in calendarList after 20s: %v", missing)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}
