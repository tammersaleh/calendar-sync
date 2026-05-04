//go:build e2e

package e2e

import (
	"context"
	"errors"
	"fmt"
	"strings"
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
//
// Atomic-ish: if any insert or visibility-wait fails, every fixture
// already created during this call is best-effort deleted before the
// error propagates. The user shouldn't be left with an orphaned
// half-suite of fixtures.
func createFixtures(ctx context.Context, c *gws.Client) (sourceID, targetID string, err error) {
	if err := destroyFixtures(ctx, c); err != nil {
		return "", "", fmt.Errorf("pre-create cleanup: %w", err)
	}

	created := make(map[string]string, len(fixtureSummaries))
	rollback := func() {
		for _, id := range created {
			// Best-effort cleanup; surface failures to stderr only.
			// At this point we're already in an error path.
			rbCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_ = c.CalendarsDelete(rbCtx, id)
			cancel()
		}
	}

	for _, summary := range fixtureSummaries {
		cal, insErr := c.CalendarsInsert(ctx, &gws.Calendar{
			Summary:     summary,
			Description: fixtureMarker,
			TimeZone:    fixtureTimeZone,
		})
		if insErr != nil {
			rollback()
			return "", "", fmt.Errorf("create fixture %q: %w", summary, insErr)
		}
		if cal.ID == "" {
			rollback()
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
		rollback()
		return "", "", err
	}

	return created[sourceCalSummary], created[targetCalSummary], nil
}

// destroyFixtures deletes any calendar in the user's calendarList whose
// summary matches a fixture name AND whose description carries the
// fixture marker. Idempotent: a missing fixture is not an error, and a
// 404/410 from the actual delete (the calendar vanished between list
// and delete) is also treated as success.
//
// Errors from individual calendars accumulate; the function returns
// after attempting all of them, joining error messages so a failed
// teardown reports every problem rather than just the first one.
//
// A summary collision without the marker is a hard error - the harness
// refuses to delete anything it didn't create.
func destroyFixtures(ctx context.Context, c *gws.Client) error {
	entries, err := c.CalendarListList(ctx)
	if err != nil {
		return fmt.Errorf("list calendars: %w", err)
	}

	wanted := make(map[string]bool, len(fixtureSummaries))
	for _, s := range fixtureSummaries {
		wanted[s] = true
	}

	var errs []string
	for _, e := range entries {
		if !wanted[e.Summary] {
			continue
		}
		if e.AccessRole != "owner" {
			errs = append(errs, fmt.Sprintf("calendar %q is not owned by this account (accessRole=%q); refusing to delete", e.Summary, e.AccessRole))
			continue
		}
		// CalendarListEntry doesn't carry description; fetch the
		// underlying Calendar resource for the marker check.
		cal, getErr := c.CalendarsGet(ctx, e.ID)
		if getErr != nil {
			// 404 mid-loop: someone deleted it between list and get.
			// Tolerate.
			if errors.Is(getErr, gws.ErrAPINotFound) || errors.Is(getErr, gws.ErrAPIGone) {
				continue
			}
			errs = append(errs, fmt.Sprintf("verify marker on %q (%s): %v", e.Summary, e.ID, getErr))
			continue
		}
		if cal.Description != fixtureMarker {
			errs = append(errs, fmt.Sprintf("calendar %q (%s) does not carry the harness safety marker; refusing to delete (description=%q)", e.Summary, e.ID, cal.Description))
			continue
		}
		if delErr := c.CalendarsDelete(ctx, e.ID); delErr != nil {
			if errors.Is(delErr, gws.ErrAPINotFound) || errors.Is(delErr, gws.ErrAPIGone) {
				continue
			}
			errs = append(errs, fmt.Sprintf("delete %q (%s): %v", e.Summary, e.ID, delErr))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("destroyFixtures: %s", strings.Join(errs, "; "))
	}
	return nil
}

// waitForFixturesVisible polls calendarList until every newly created
// fixture appears, or the context's deadline (or a 20-second cap,
// whichever is sooner) passes. Calendar creation is generally
// available immediately on the events endpoint but calendarList
// propagation lags a few seconds.
func waitForFixturesVisible(ctx context.Context, c *gws.Client, created map[string]string) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	for {
		entries, err := c.CalendarListList(ctx)
		if err != nil {
			return fmt.Errorf("list calendars during fixture wait: %w", err)
		}
		seen := make(map[string]bool, len(created))
		for _, e := range entries {
			if id, ok := created[e.Summary]; ok && e.ID == id {
				seen[e.Summary] = true
			}
		}
		if len(seen) == len(created) {
			return nil
		}
		select {
		case <-ctx.Done():
			missing := []string{}
			for s := range created {
				if !seen[s] {
					missing = append(missing, s)
				}
			}
			return fmt.Errorf("fixtures not visible in calendarList: %v (%w)", missing, ctx.Err())
		case <-time.After(500 * time.Millisecond):
		}
	}
}
