package feedimport

import (
	"bytes"
	"context"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/feed"
	"github.com/tammersaleh/calendar-sync/internal/ical"
)

// FeedConfig is the minimal per-feed input, mapped from config.CanonicalFeed
// by the caller. feedimport intentionally does NOT import internal/config: the
// caller flattens the canonical feed into these primitives so the dependency
// direction stays one-way.
//
// URL is a bearer secret. It is held only on the feedEntry and handed to the
// fetcher; it never reaches the Importer, a log line, or a returned error.
type FeedConfig struct {
	Name            string
	URL             string
	TargetCalendar  string
	ForceAllDayBusy bool // force imported all-day events Busy regardless of TRANSP
}

// fetcher is the feed-fetch capability a feedEntry depends on. The production
// implementation is *feed.Fetcher, which is stateful (it carries ETag /
// Last-Modified validators and a cache gate across calls), so one is built per
// feed in NewRunner and reused on every tick. The interface exists so tests can
// inject a fake without standing up an httptest.Server per feed.
type fetcher interface {
	Fetch(ctx context.Context, url string) (feed.Result, error)
}

// feedEntry binds one feed's identity, its long-lived fetcher, and its
// importer. The URL secret lives here (passed to Fetch); the Importer never
// sees it.
type feedEntry struct {
	name     string
	url      string
	fetcher  fetcher
	importer *Importer
}

// Runner owns one Fetcher + one Importer per feed and runs them each tick.
// It is long-lived: construct once via NewRunner and call RunOnce every tick
// so each Fetcher's conditional-GET validators and cache gate persist.
type Runner struct {
	entries []*feedEntry
	log     Logger
}

// FeedResult is one feed's outcome for a single RunOnce, for the caller to log
// or aggregate. Err (when set) already has the URL redacted by the feed layer;
// callers must not re-derive it.
type FeedResult struct {
	Name    string
	Skipped bool   // cache gate short-circuited; no HTTP call was made
	Changed bool   // a fresh 200 body was fetched this call
	Import  Result // reconcile tally; zero unless Changed and the parse succeeded
	Err     error  // fetch, parse, or reconcile failure (URL already redacted)
}

// NewRunner builds a Runner from the flattened feed list. api is the shared gws
// EventsAPI (the timeout-wrapped client in production). dryRun sets each
// Importer.DryRun. now is injectable for tests (nil => time.Now) and is
// threaded into every feed.Fetcher. log is the shared nil-safe logger.
func NewRunner(api EventsAPI, feeds []FeedConfig, dryRun bool, now func() time.Time, log Logger) *Runner {
	r := &Runner{log: log}
	for _, f := range feeds {
		r.entries = append(r.entries, &feedEntry{
			name:    f.Name,
			url:     f.URL,
			fetcher: &feed.Fetcher{Now: now},
			importer: &Importer{
				API:             api,
				Target:          f.TargetCalendar,
				FeedID:          f.Name,
				DryRun:          dryRun,
				ForceAllDayBusy: f.ForceAllDayBusy,
				Log:             log,
			},
		})
	}
	return r
}

// RunOnce runs every feed once, isolating failures per feed. For each feed it
// fetches; a skipped (cache gate) or not-modified (304) fetch does nothing
// further; a changed fetch is parsed and reconciled. A per-feed error is logged
// (WITHOUT the URL) and does not abort the other feeds. Returns one FeedResult
// per feed in feed order; a Runner with no feeds returns an empty slice.
func (r *Runner) RunOnce(ctx context.Context) []FeedResult {
	results := make([]FeedResult, 0, len(r.entries))
	for _, e := range r.entries {
		results = append(results, r.runFeed(ctx, e))
	}
	return results
}

func (r *Runner) runFeed(ctx context.Context, e *feedEntry) FeedResult {
	fr := FeedResult{Name: e.name}

	res, err := e.fetcher.Fetch(ctx, e.url)
	if err != nil {
		// feed.Fetch already redacts the URL from its errors; never re-add it.
		fr.Err = err
		r.warn("feedimport: feed fetch failed", "feed", e.name, "err", err)
		return fr
	}
	if res.Skipped {
		fr.Skipped = true
		r.debug("feedimport: feed skipped (cache gate)", "feed", e.name)
		return fr
	}
	if !res.Changed {
		r.debug("feedimport: feed not modified", "feed", e.name)
		return fr
	}

	fr.Changed = true
	items, err := ical.Parse(bytes.NewReader(res.Body))
	if err != nil {
		fr.Err = err
		r.warn("feedimport: feed parse failed", "feed", e.name, "err", err)
		return fr
	}

	imp, err := e.importer.Reconcile(ctx, items)
	fr.Import = imp
	if err != nil {
		fr.Err = err
		r.error("feedimport: feed reconcile failed", "feed", e.name, "err", err)
		return fr
	}

	r.info("feedimport: feed imported",
		"feed", e.name,
		"inserted", imp.Inserted,
		"patched", imp.Patched,
		"deleted", imp.Deleted,
		"unchanged", imp.Unchanged,
		"skipped", imp.Skipped,
	)
	return fr
}

// nil-safe logger wrappers (same pattern as Importer / internal/sync).
func (r *Runner) debug(msg string, args ...any) {
	if r.log != nil {
		r.log.Debug(msg, args...)
	}
}

func (r *Runner) info(msg string, args ...any) {
	if r.log != nil {
		r.log.Info(msg, args...)
	}
}

func (r *Runner) warn(msg string, args ...any) {
	if r.log != nil {
		r.log.Warn(msg, args...)
	}
}

func (r *Runner) error(msg string, args ...any) {
	if r.log != nil {
		r.log.Error(msg, args...)
	}
}
