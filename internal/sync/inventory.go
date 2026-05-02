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
	EventsPatch(ctx context.Context, calendarID, eventID string, body *gws.Event) (*gws.Event, error)
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

// BuildInventory runs SPEC.md "Mirror inventory rebuild": two events.list
// calls (privateExtendedProperty=calendar-sync:version=2 then version=1),
// merged into one map keyed by source-tuple. v1 entries are kept in
// inventory; the sync layer detects them at reconciliation time via
// mirror.ComputeDriftSignal's NeedsMigration field.
//
// Mirrors that lack a parseable calendar-sync:source extended property are
// skipped with their tuple effectively dropped on the floor; SPEC.md
// considers those mirrors unmanageable. The orphan walk catches them
// indirectly when the user triggers `mirror prune`.
//
// log may be nil; when non-nil it receives one info-level entry per version
// pass (after the events.list call returns) carrying target + count, so the
// daemon log surfaces the pre-reconcile inventory baseline that the rest of
// the pass operates against.
func BuildInventory(ctx context.Context, api API, target string, log Logger) (*Inventory, error) {
	inv := NewInventory(target)

	// Two passes: v2 first, then v1. The order is for SPEC documentation
	// fidelity; v2 mirrors are the common case and most relevant.
	for _, version := range []string{mirror.SchemaVersion, "1"} {
		params := gws.EventsListParams{
			CalendarID:             target,
			ShowDeleted:            true,
			PrivateExtendedProperty: []string{mirror.ExtKeyVersion + "=" + version},
		}
		events, _, err := api.EventsList(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("inventory rebuild for %s (version=%s): %w", target, version, err)
		}
		var added, skipped int
		for i := range events {
			ev := events[i]
			tuple, ok := parseSourceFromMirror(&ev)
			if !ok {
				skipped++
				continue
			}
			inv.Set(tuple, &ev)
			added++
		}
		if log != nil {
			log.Info("sync.BuildInventory pass",
				"target", target,
				"schema_version", version,
				"events_returned", len(events),
				"added", added,
				"unparseable_skipped", skipped,
			)
		}
	}
	if log != nil {
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
