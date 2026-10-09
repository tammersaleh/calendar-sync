package sync

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

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

	// duplicates holds live mirrors that lost a same-tuple collision in
	// BuildInventory (B40). They are redundant copies of items[tuple]; the
	// orphan walk deletes them. Keyed by tuple so each pdir's walker can
	// claim only the losers for its own source calendar.
	duplicates map[mirror.SourceTuple][]*gws.Event
}

// NewInventory returns an empty Inventory keyed for the given target
// calendar. BuildInventory is the typical construction path; this lower-
// level constructor exists so tests can hand-roll inventories without
// faking out the events.list call.
func NewInventory(target string) *Inventory {
	return &Inventory{
		target:     target,
		items:      make(map[mirror.SourceTuple]*gws.Event),
		duplicates: make(map[mirror.SourceTuple][]*gws.Event),
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

// addDuplicate records a losing copy for tuple.
func (i *Inventory) addDuplicate(s mirror.SourceTuple, e *gws.Event) {
	i.duplicates[s] = append(i.duplicates[s], e)
}

// dropDuplicate forgets the loser with mirrorID under tuple, removing the
// tuple key once no losers remain. It never touches items[tuple] - that is
// the winner, which Delete owns. It rewrites the slice in place, so callers
// iterating duplicates[tuple] must copy it first.
func (i *Inventory) dropDuplicate(s mirror.SourceTuple, mirrorID string) {
	kept := i.duplicates[s][:0]
	for _, e := range i.duplicates[s] {
		if e.ID != mirrorID {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		delete(i.duplicates, s)
		return
	}
	i.duplicates[s] = kept
}

// loserIDs returns the Google event IDs of every recorded duplicate.
func (i *Inventory) loserIDs() map[string]bool {
	out := make(map[string]bool)
	for _, losers := range i.duplicates {
		for _, l := range losers {
			out[l.ID] = true
		}
	}
	return out
}

// index places e at tuple, resolving a same-tuple collision (B40): the
// loser is recorded as a duplicate, the winner is set. losingParents holds
// the IDs of mirror parents that already lost their own collision; a
// recurring instance under one of them is demoted and loses to any
// instance under a surviving parent regardless of Updated, because the
// orphan walk's delete of the losing parent cascades to its instances. Two
// instances under surviving (or both under losing) parents fall through to
// preferMirror.
func (i *Inventory) index(s mirror.SourceTuple, e *gws.Event, losingParents map[string]bool) {
	cur, ok := i.items[s]
	if !ok {
		i.items[s] = e
		return
	}
	if cur.ID == e.ID {
		// The same Google event listed twice is one mirror, not a
		// collision; recording it as its own loser would have the orphan
		// walk delete the live mirror.
		return
	}
	demoted := func(ev *gws.Event) bool {
		return ev.RecurringEventID != "" && losingParents[ev.RecurringEventID]
	}
	var eWins bool
	switch ed, cd := demoted(e), demoted(cur); {
	case ed != cd:
		eWins = cd
	default:
		eWins = preferMirror(s, e, cur)
	}
	if eWins {
		i.items[s] = e
		i.addDuplicate(s, cur)
		return
	}
	i.addDuplicate(s, e)
}

// preferMirror reports whether candidate should win the inventory slot for
// tuple over incumbent when both are live mirrors of the same source.
// Order (B40, see doc/plans/B40-duplicate-mirrors.md):
//
//  1. newest parseable Updated - the copy the daemon has been maintaining
//     is the one that moved most recently; an unparseable or empty Updated
//     counts as oldest.
//  2. the deterministic mirror ID for the tuple (the daemon's own insert).
//  3. the lexically smaller ID, so the choice is stable across list order.
func preferMirror(s mirror.SourceTuple, candidate, incumbent *gws.Event) bool {
	ct, cok := parseUpdated(candidate.Updated)
	it, iok := parseUpdated(incumbent.Updated)
	switch {
	case cok && !iok:
		return true
	case !cok && iok:
		return false
	case cok && iok && !ct.Equal(it):
		return ct.After(it)
	}
	det := mirror.DeterministicID(s.CalendarID, s.EventID)
	switch {
	case candidate.ID == det && incumbent.ID != det:
		return true
	case incumbent.ID == det && candidate.ID != det:
		return false
	}
	return candidate.ID < incumbent.ID
}

func parseUpdated(u string) (time.Time, bool) {
	if u == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, u)
	return t, err == nil
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
	// source values, and inherited recurring instances. Parents and
	// non-recurring events go first (2a), then recurring instances (2b),
	// so that by the time an instance collision is resolved the set of
	// LOSING parents is known and an instance under one of them is
	// demoted (see Inventory.index). Without that ordering the parent and
	// instance tuples could pick winners from opposite series and the
	// orphan walk would delete both copies of an occurrence (B40).
	addedByVersion := make(map[string]int)
	skippedByVersion := make(map[string]int)
	losingParents := map[string]bool{}
	for _, instancePass := range []bool{false, true} {
		for _, te := range allEvents {
			ev := te.ev
			if (ev.RecurringEventID != "") != instancePass {
				continue
			}
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
			if instancePass {
				if parentTuple, found := parentSourceTuples[ev.RecurringEventID]; found {
					if mirror.IsInheritedRecurringInstance(&ev, parentTuple.EventID) {
						skippedByVersion[te.version]++
						continue
					}
				}
			}
			inv.index(tuple, &ev, losingParents)
			addedByVersion[te.version]++
		}
		if !instancePass {
			losingParents = inv.loserIDs()
		}
	}

	if log != nil {
		// One warning per colliding tuple, after pass 2 so a three-way
		// collision reports the final winner rather than an intermediate.
		for _, tuple := range sortedTuples(inv.duplicates) {
			losers := make([]string, 0, len(inv.duplicates[tuple]))
			for _, l := range inv.duplicates[tuple] {
				losers = append(losers, l.ID)
			}
			sort.Strings(losers)
			log.Warn("sync.BuildInventory: duplicate mirrors for source tuple",
				"target", target,
				"source_tuple", tuple.String(),
				"winner", inv.items[tuple].ID,
				"losers", strings.Join(losers, ","),
			)
		}
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
			"duplicate_tuples", len(inv.duplicates),
		)
	}
	return inv, nil
}

// sortedTuples returns the keys of m in Tuples() order.
func sortedTuples(m map[mirror.SourceTuple][]*gws.Event) []mirror.SourceTuple {
	out := make([]mirror.SourceTuple, 0, len(m))
	for t := range m {
		out = append(out, t)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].String() < out[b].String() })
	return out
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
