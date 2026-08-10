// Package feedimport projects a parsed iCal feed snapshot into one Google
// calendar it owns (a "shadow"/target calendar) via a full-snapshot diff.
//
// It is a one-way importer: the feed is the source of truth, the target
// calendar is a projection. Every event it writes carries the feed extended
// properties (calendar-sync-feed:*) - a namespace disjoint from the mirror
// engine's calendar-sync:* - so the events look like ordinary user events to
// the sync engine and get mirrored into the work<->personal mesh normally.
//
// The importer consumes a FULL, freshly-parsed snapshot on every call.
// Deletion is defined as "a feed-owned target event whose UID is absent from
// the snapshot", so a partial/stale snapshot would delete live events; Layer 5
// guarantees Reconcile is only invoked after a fresh 200+parse.
package feedimport

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/ical"
)

// listMaxResults caps a single events.list page. SPEC uses 250 elsewhere; a
// feed calendar is small so one page is the expected case.
const listMaxResults = 250

// EventsAPI is the gws subset the importer needs. *gws.Client satisfies it.
type EventsAPI interface {
	EventsList(ctx context.Context, params gws.EventsListParams) ([]gws.Event, string, error)
	EventsGet(ctx context.Context, calendarID, eventID string) (*gws.Event, error)
	EventsInsert(ctx context.Context, calendarID string, body *gws.Event) (*gws.Event, error)
	EventsPatch(ctx context.Context, calendarID, eventID string, body *gws.PatchEvent) (*gws.Event, error)
	EventsDelete(ctx context.Context, calendarID, eventID string) error
}

// Logger is a nil-safe re-declared interface (same pattern as internal/sync);
// this package does NOT import internal/output so the dependency direction
// stays one-way. Every log call short-circuits when Log is nil.
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Importer reconciles one feed into one target calendar.
type Importer struct {
	API    EventsAPI
	Target string // canonical target calendar ID
	FeedID string // stable feed identifier; namespaces deterministic IDs
	DryRun bool
	// ForceAllDayBusy forces every imported all-day event to opaque (Busy),
	// overriding the feed's TRANSP. Timed events keep their own TRANSP. See
	// build.go:transparencyFor.
	ForceAllDayBusy bool
	Log             Logger
}

// Result tallies what Reconcile did (or, under DryRun, would have done).
// Skipped counts cancelled feed items that were not projected.
type Result struct {
	Inserted  int
	Patched   int
	Deleted   int
	Unchanged int
	Skipped   int
}

func (im *Importer) debug(msg string, args ...any) {
	if im.Log != nil {
		im.Log.Debug(msg, args...)
	}
}

func (im *Importer) info(msg string, args ...any) {
	if im.Log != nil {
		im.Log.Info(msg, args...)
	}
}

func (im *Importer) warn(msg string, args ...any) {
	if im.Log != nil {
		im.Log.Warn(msg, args...)
	}
}

// Reconcile projects a full feed snapshot into the target calendar. It lists
// the feed-owned events, diffs them against the snapshot, and inserts/patches/
// deletes to converge. Per-write failures are logged and collected; the first
// hard error is returned after every other action is attempted, so one bad
// event does not strand the rest. A list failure is fatal and returns
// immediately (no safe diff is possible without the current target state).
func (im *Importer) Reconcile(ctx context.Context, items []ical.Item) (Result, error) {
	var res Result

	existing, err := im.listExisting(ctx)
	if err != nil {
		return res, err
	}

	desired, uids := im.buildDesired(items, &res)

	var firstErr error
	record := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	// Inserts and patches, in deterministic UID order.
	for _, uid := range uids {
		want := desired[uid]
		cur, ok := existing[uid]
		if !ok {
			record(im.insert(ctx, uid, want, &res))
			continue
		}
		if checksumProp(cur) == checksumProp(want) {
			res.Unchanged++
			im.debug("feedimport: unchanged", "uid", uid, "id", cur.ID)
			continue
		}
		record(im.patch(ctx, cur.ID, want, &res))
	}

	// Deletes: feed-owned events absent from the snapshot.
	var stale []string
	for uid := range existing {
		if _, ok := desired[uid]; !ok {
			stale = append(stale, uid)
		}
	}
	sort.Strings(stale)
	for _, uid := range stale {
		record(im.delete(ctx, existing[uid], &res))
	}

	return res, firstErr
}

// listExisting lists the feed-owned events on the target and keys them by
// their propUID value. The PrivateExtendedProperty filter is the load-bearing
// delete guard: only events Google returns are ever delete candidates, so
// non-feed events can never be pruned. It scopes on BOTH propVersion AND
// propFeedID - without the feed_id scope, two feeds sharing a target calendar
// would each list the other's events, find them absent from its own snapshot,
// and delete them. Listed events missing propUID are skipped with a warning
// rather than keyed under "".
func (im *Importer) listExisting(ctx context.Context) (map[string]*gws.Event, error) {
	params := gws.EventsListParams{
		CalendarID: im.Target,
		PrivateExtendedProperty: []string{
			propVersion + "=" + schemaVersion,
			propFeedID + "=" + im.FeedID,
		},
		ShowDeleted:  false,
		SingleEvents: true,
		MaxResults:   listMaxResults,
	}
	events, _, err := im.API.EventsList(ctx, params)
	if err != nil {
		return nil, fmt.Errorf("feedimport: list target %s: %w", im.Target, err)
	}
	out := make(map[string]*gws.Event, len(events))
	for i := range events {
		ev := &events[i]
		uid := ""
		if ev.ExtendedProperties != nil {
			uid = ev.ExtendedProperties.Private[propUID]
		}
		if uid == "" {
			im.warn("feedimport: listed event missing uid prop, skipping", "id", ev.ID)
			continue
		}
		out[uid] = ev
	}
	return out, nil
}

// buildDesired maps the snapshot into desired events keyed by UID, tallying
// cancelled items as Skipped. The returned uids slice is sorted for
// deterministic write ordering. A cancelled item is treated as absent from the
// desired set, so an existing feed-owned event for it falls into the delete
// path.
func (im *Importer) buildDesired(items []ical.Item, res *Result) (map[string]*gws.Event, []string) {
	desired := make(map[string]*gws.Event, len(items))
	var uids []string
	for _, it := range items {
		if strings.EqualFold(it.Status, gws.EventStatusCancelled) {
			res.Skipped++
			continue
		}
		if _, dup := desired[it.UID]; !dup {
			uids = append(uids, it.UID)
		}
		desired[it.UID] = im.buildEvent(it)
	}
	sort.Strings(uids)
	return desired, uids
}

// insert creates a new feed event with the deterministic ID. On a 409 (the ID
// already exists as a cancelled or alive leftover) it hands off to
// recoverConflict. Success (or a dry-run) tallies Inserted.
func (im *Importer) insert(ctx context.Context, uid string, want *gws.Event, res *Result) error {
	id := DeterministicID(im.FeedID, uid)
	want.ID = id
	im.info("feedimport: insert", "uid", uid, "id", id, "summary", want.Summary, "dry_run", im.DryRun)
	if im.DryRun {
		res.Inserted++
		return nil
	}
	if _, err := im.API.EventsInsert(ctx, im.Target, want); err != nil {
		if errors.Is(err, gws.ErrAPIConflict) {
			return im.recoverConflict(ctx, id, want, res)
		}
		im.warn("feedimport: insert failed", "uid", uid, "id", id, "err", err)
		return fmt.Errorf("feedimport: insert %s: %w", uid, err)
	}
	res.Inserted++
	return nil
}

// recoverConflict handles the 409-on-insert case. It reads the existing event:
// a cancelled leftover is patched back to confirmed with the full desired
// payload (tallied Inserted, matching the intent); an alive one takes the
// ordinary update path (tallied Patched).
func (im *Importer) recoverConflict(ctx context.Context, id string, want *gws.Event, res *Result) error {
	cur, err := im.API.EventsGet(ctx, im.Target, id)
	if err != nil {
		im.warn("feedimport: conflict get failed", "id", id, "err", err)
		return fmt.Errorf("feedimport: conflict get %s: %w", id, err)
	}
	patch := im.buildPatch(want)
	if cur.Status == gws.EventStatusCancelled {
		patch.Status = gws.PatchStr(gws.EventStatusConfirmed)
		if _, err := im.API.EventsPatch(ctx, im.Target, id, patch); err != nil {
			im.warn("feedimport: revive patch failed", "id", id, "err", err)
			return fmt.Errorf("feedimport: revive %s: %w", id, err)
		}
		im.info("feedimport: revived cancelled leftover", "id", id)
		res.Inserted++
		return nil
	}
	if _, err := im.API.EventsPatch(ctx, im.Target, id, patch); err != nil {
		im.warn("feedimport: conflict update patch failed", "id", id, "err", err)
		return fmt.Errorf("feedimport: conflict update %s: %w", id, err)
	}
	res.Patched++
	return nil
}

// patch updates an existing feed event to match want. Managed fields plus the
// feed extended properties only; recurrence/visibility/reminders untouched.
func (im *Importer) patch(ctx context.Context, id string, want *gws.Event, res *Result) error {
	im.info("feedimport: patch", "id", id, "summary", want.Summary, "dry_run", im.DryRun)
	if im.DryRun {
		res.Patched++
		return nil
	}
	if _, err := im.API.EventsPatch(ctx, im.Target, id, im.buildPatch(want)); err != nil {
		im.warn("feedimport: patch failed", "id", id, "err", err)
		return fmt.Errorf("feedimport: patch %s: %w", id, err)
	}
	res.Patched++
	return nil
}

// delete removes a feed-owned event that vanished from the snapshot.
func (im *Importer) delete(ctx context.Context, ev *gws.Event, res *Result) error {
	im.info("feedimport: delete", "id", ev.ID, "dry_run", im.DryRun)
	if im.DryRun {
		res.Deleted++
		return nil
	}
	if err := im.API.EventsDelete(ctx, im.Target, ev.ID); err != nil {
		im.warn("feedimport: delete failed", "id", ev.ID, "err", err)
		return fmt.Errorf("feedimport: delete %s: %w", ev.ID, err)
	}
	res.Deleted++
	return nil
}

// checksumProp reads the stored feed checksum, or "" when absent.
func checksumProp(ev *gws.Event) string {
	if ev == nil || ev.ExtendedProperties == nil {
		return ""
	}
	return ev.ExtendedProperties.Private[propChecksum]
}
