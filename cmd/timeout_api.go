package cmd

import (
	"context"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// callTimeoutAPI wraps a GwsClient so each method derives its own
// time-bounded context from the caller's ctx. SPEC says
// `calendar-sync watch --timeout` is the "wall-clock cap for any single
// source-list or mirror-list call"; the wrapper applies the same cap
// uniformly across every method to keep the daemon from wedging on a
// stuck gws subprocess regardless of which call hit it.
//
// timeout <= 0 disables the wrapper (the wrapper passes ctx through
// unchanged). The caller's ctx still wins on cancellation; the wrapper
// only adds a deadline, never extends one.
type callTimeoutAPI struct {
	inner   GwsClient
	timeout time.Duration
}

// newCallTimeoutAPI returns a GwsClient that bounds every gws call to
// timeout. timeout <= 0 returns the inner client unchanged so callers
// don't have to special-case the disabled state.
func newCallTimeoutAPI(inner GwsClient, timeout time.Duration) GwsClient {
	if timeout <= 0 {
		return inner
	}
	return &callTimeoutAPI{inner: inner, timeout: timeout}
}

func (c *callTimeoutAPI) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}

func (c *callTimeoutAPI) CalendarListGet(ctx context.Context, id string) (*gws.CalendarListEntry, error) {
	cctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.inner.CalendarListGet(cctx, id)
}

func (c *callTimeoutAPI) CalendarListList(ctx context.Context) ([]gws.CalendarListEntry, error) {
	cctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.inner.CalendarListList(cctx)
}

func (c *callTimeoutAPI) EventsList(ctx context.Context, params gws.EventsListParams) ([]gws.Event, string, error) {
	cctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.inner.EventsList(cctx, params)
}

func (c *callTimeoutAPI) EventsGet(ctx context.Context, calendarID, eventID string) (*gws.Event, error) {
	cctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.inner.EventsGet(cctx, calendarID, eventID)
}

func (c *callTimeoutAPI) EventsInstances(ctx context.Context, params gws.EventsInstancesParams) ([]gws.Event, error) {
	cctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.inner.EventsInstances(cctx, params)
}

func (c *callTimeoutAPI) EventsInsert(ctx context.Context, calendarID string, body *gws.Event) (*gws.Event, error) {
	cctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.inner.EventsInsert(cctx, calendarID, body)
}

func (c *callTimeoutAPI) EventsPatch(ctx context.Context, calendarID, eventID string, body *gws.PatchEvent) (*gws.Event, error) {
	cctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.inner.EventsPatch(cctx, calendarID, eventID, body)
}

func (c *callTimeoutAPI) EventsDelete(ctx context.Context, calendarID, eventID string) error {
	cctx, cancel := c.withTimeout(ctx)
	defer cancel()
	return c.inner.EventsDelete(cctx, calendarID, eventID)
}
