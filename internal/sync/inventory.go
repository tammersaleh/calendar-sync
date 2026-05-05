package sync

import (
	"context"
	"fmt"
	"sort"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// API is the gws-subprocess subset layer 6 consumes. Production passes
// *gws.Client; tests provide hand-rolled in-process stubs per CLAUDE.md
// "Testing" - the fake-gws harness is reserved for end-to-end command
// tests where the gws argv shape is what's under test.
type API interface {
	EventsList(ctx context.Context, params gws.EventsListParams) (events []gws.Event, nextSyncToken string, err error)
	EventsGet(ctx context.Context, calendarID, eventID string) (*gws.Event, error)
	EventsInstances(ctx context.Context, params gws.EventsInstancesParams) ([]gws.Event, error)
	EventsInsert(ctx context.Context, calendarID string, body *gws.Event) (*gws.Event, error)
	EventsPatch(ctx context.Context, calendarID, eventID string, body *gws.PatchEvent) (*gws.Event, error)
	EventsDelete(ctx context.Context, calendarID, eventID string) error
}

// Inventory is the per-target mirror map per SPEC.md "In-memory state".
// Keyed by source-tuple parsed from each mirror's calendar-sync:source
// extended property; lookup by source-tuple finds legacy + deterministic-ID
// mirrors identically per SPEC's "Mirror identification" rule.
type Inventory struct {
	target string
	items  map[mirror.SourceTuple]*gws.Event
}

// NewInventory returns an empty Inventory keyed for the given target
// calendar. BuildInventory is the typical construction path; this lower-
// level constructor exists so tests can hand-roll inventories without
// faking out the events.list call.
func NewInventory(target string) *Inventory {
	return &Inventory{
		target: target,
		items:  make(map[mirror.SourceTuple]*gws.Event),
	}
}

// Target returns the canonical target calendar ID this inventory belongs
// to. Used by callers (the orphan walk, the parent-reconcile callback)
// that need to address writes back to the same calendar.
func (i *Inventory) Target() string { return i.target }

// Lookup returns the mirror Event for the given source-tuple, or
// (nil, false) if no mirror is known. This is the lookup the
// classification logic performs in steps 3-8 to decide between insert
// and patch/delete.
func (i *Inventory) Lookup(s mirror.SourceTuple) (*gws.Event, bool) {
	e, ok := i.items[s]
	return e, ok
}

// Set upserts a mirror keyed by source-tuple. Used after every successful
// write (insert, patch, propagate-followup) to keep the inventory tracking
// the post-write resource per SPEC.md "In-memory state" - "grown and
// pruned in place as the daemon makes inserts/patches/deletes".
func (i *Inventory) Set(s mirror.SourceTuple, e *gws.Event) {
	i.items[s] = e
}

// Delete removes the mirror for the given source-tuple. Used after a
// successful events.delete on the target side.
func (i *Inventory) Delete(s mirror.SourceTuple) {
	delete(i.items, s)
}

// All returns every mirror Event currently in the inventory in
// source-tuple-sorted order. Used by the orphan walk (layer 6.B) which
// needs a deterministic iteration order.
func (i *Inventory) All() []*gws.Event {
	tuples := i.Tuples()
	out := make([]*gws.Event, 0, len(tuples))
	for _, t := range tuples {
		out = append(out, i.items[t])
	}
	return out
}

// Tuples returns the source-tuples for every mirror in the inventory in
// alphabetical order (canonical ID then event ID). Useful for tests and
// for the orphan walk's deterministic sweep.
func (i *Inventory) Tuples() []mirror.SourceTuple {
	out := make([]mirror.SourceTuple, 0, len(i.items))
	for t := range i.items {
		out = append(out, t)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].CalendarID != out[b].CalendarID {
			return out[a].CalendarID < out[b].CalendarID
		}
		return out[a].EventID < out[b].EventID
	})
	return out
}

// BuildInventory runs SPEC.md "Mirror inventory rebuild": one events.list
// call per known schema version (current SchemaVersion plus every legacy
// version still in the wild), merged into one map keyed by source-tuple.
// Legacy entries are kept in inventory; the sync layer detects them at
// reconciliation time via mirror.ComputeDriftSignal's NeedsMigration field
// and routes them through the migration path.
//
// Mirrors that lack a parseable calendar-sync:source extended property are
// skipped with their tuple effectively dropped on the floor; SPEC.md
// considers those mirrors unmanageable. The orphan walk catches them
// indirectly when the user triggers `mirror prune`.
//
// Two-pass shape (see B16 in doc/bugs.md): when calendar-sync writes a
// recurring parent, Google auto-materializes the parent's instances and
// copies the parent's extendedProperties.private to each materialized
// instance. So an instance that has been overridden on the target carries
// the parent's calendar-sync:source value verbatim (EventID = source
// parent's ID, no instance suffix), parsing to the SAME source-tuple as
// the parent itself. Indexing both naively last-writer-wins, and a stray
// instance can shadow the real parent in inventory - then a normal
// source-parent reconcile fires drift detection against the instance's
// per-occurrence fields and propagates them back to the source parent,
// destroying the recurring series. The fix is to gather all events first,
// build a (mirror parent ID -> parsed source-tuple) map, then in pass 2
// drop any instance whose source-tuple matches its parent's source-tuple
// (mirror.IsInheritedRecurringInstance). Explicitly-managed instances
// carry a per-instance source-tuple (EventID with the `_<UTC>` suffix)
// and are kept.
//
// log may be nil; when non-nil it receives one info-level entry per version
// pass (after the events.list call returns) carrying target + count, so the
// daemon log surfaces the pre-reconcile inventory baseline that the rest of
// the pass operates against.
func BuildInventory(ctx context.Context, api API, target string, log Logger) (*Inventory, error) {
	inv := NewInventory(target)

	type taggedEvent struct {
		ev      gws.Event
		version string
	}
	var allEvents []taggedEvent

	// Pass 0: list per known schema version, accumulate.
	for _, version := range []string{mirror.SchemaVersion, "2", "1"} {
		params := gws.EventsListParams{
			CalendarID:              target,
			ShowDeleted:             true,
			PrivateExtendedProperty: []string{mirror.ExtKeyVersion + "=" + version},
		}
		events, _, err := api.EventsList(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("inventory rebuild for %s (version=%s): %w", target, version, err)
		}
		for i := range events {
			allEvents = append(allEvents, taggedEvent{ev: events[i], version: version})
		}
	}

	// Pass 1: for every event that looks like a parent (no RecurringEventID
	// AND parseable source-tuple), record its source-tuple under its mirror
	// ID. Cancelled tombstones are eligible parents in this pass - their
	// source-tuple still anchors the inheritance check for any live instance
	// whose recurringEventId points at them. The cancelled tombstone itself
	// won't be indexed in pass 2.
	parentSourceTuples := make(map[string]mirror.SourceTuple)
	for _, te := range allEvents {
		ev := te.ev
		if ev.RecurringEventID != "" {
			continue
		}
		tuple, ok := parseSourceFromMirror(&ev)
		if !ok {
			continue
		}
		parentSourceTuples[ev.ID] = tuple
	}

	// Pass 2: index each event, skipping cancelled tombstones, unparseable
	// source values, and inherited recurring instances.
	addedByVersion := make(map[string]int)
	skippedByVersion := make(map[string]int)
	for _, te := range allEvents {
		ev := te.ev
		// Tombstones (events deleted via events.delete; status=cancelled)
		// reach this listing because ShowDeleted:true is set above. Skip
		// them: SPEC's cancelled-and-revived flow inspects status via a
		// per-event events.get triggered by a 409 on insert, not via the
		// inventory. Indexing tombstones would mislead the orphan walk
		// (which would try to events.delete them and hit
		// api_invalid_request "Resource has been deleted") and the
		// standard reconcile path (which would treat them as live mirrors
		// needing drift checks).
		if ev.Status == gws.EventStatusCancelled {
			skippedByVersion[te.version]++
			continue
		}
		tuple, ok := parseSourceFromMirror(&ev)
		if !ok {
			skippedByVersion[te.version]++
			continue
		}
		// Inherited-instance filter: if this is a recurring instance and
		// its parent's source-tuple matches its own, the instance only
		// holds Google's auto-copied parent metadata. Indexing it would
		// shadow the real parent at the same key.
		if ev.RecurringEventID != "" {
			if parentTuple, found := parentSourceTuples[ev.RecurringEventID]; found {
				if mirror.IsInheritedRecurringInstance(&ev, parentTuple.EventID) {
					skippedByVersion[te.version]++
					continue
				}
			}
		}
		inv.Set(tuple, &ev)
		addedByVersion[te.version]++
	}

	if log != nil {
		// Emit one log line per version pass that actually saw events, with
		// the per-version added/skipped counts the legacy single-pass
		// implementation reported. Versions whose listing returned zero
		// events still log so dashboard scrapes see all three lines.
		eventsByVersion := make(map[string]int)
		for _, te := range allEvents {
			eventsByVersion[te.version]++
		}
		for _, version := range []string{mirror.SchemaVersion, "2", "1"} {
			log.Info("sync.BuildInventory pass",
				"target", target,
				"schema_version", version,
				"events_returned", eventsByVersion[version],
				"added", addedByVersion[version],
				"unparseable_skipped", skippedByVersion[version],
			)
		}
		log.Info("sync.BuildInventory complete",
			"target", target,
			"total_mirrors", len(inv.Tuples()),
		)
	}
	return inv, nil
}

// parseSourceFromMirror extracts the SourceTuple stored on a mirror's
// calendar-sync:source extended property. Returns (zero, false) when the
// extended properties are missing or the value is unparseable; callers
// skip such mirrors silently rather than failing the whole rebuild.
func parseSourceFromMirror(m *gws.Event) (mirror.SourceTuple, bool) {
	if m.ExtendedProperties == nil || m.ExtendedProperties.Private == nil {
		return mirror.SourceTuple{}, false
	}
	raw, ok := m.ExtendedProperties.Private[mirror.ExtKeySource]
	if !ok || raw == "" {
		return mirror.SourceTuple{}, false
	}
	tuple, err := mirror.ParseSourceTuple(raw)
	if err != nil {
		return mirror.SourceTuple{}, false
	}
	return tuple, true
}
