package sync

import (
	"context"
	"fmt"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// reconcileNormal implements SPEC.md "Classification logic" step 8 for the
// non-recurring / parent path. Inventory miss routes to insert (with the
// 409-handling subroutine in insert.go). Inventory hit runs drift detection
// and the four-way matrix, including the two v1 migration cells per CLAUDE.md
// "v1 migration cells live in callers".
func (c *Classifier) reconcileNormal(ctx context.Context, source *gws.Event) error {
	tuple := mirror.SourceTuple{CalendarID: c.SourceCalendarID, EventID: source.ID}
	mirrorEvent, ok := c.Inventory.Lookup(tuple)
	if !ok {
		c.debug("sync.reconcileNormal: inventory miss -> insert",
			"source_calendar", c.SourceCalendarID,
			"source_event", source.ID,
			"summary", source.Summary,
		)
		return c.doInsert(ctx, source)
	}

	// B20 revive cell: the source passed steps 3-7 (not cancelled, declined,
	// tentative, transparent, or outside-horizon - it's syncable) but the
	// mirror in inventory sits at status=cancelled. Status isn't a managed
	// field, so the standard drift signal would emit skip(unchanged) and
	// leave the mirror cancelled forever. Route to the same revive shape
	// insert.go uses for the post-409 case.
	if mirrorEvent.Status == gws.EventStatusCancelled {
		c.debug("sync.reconcileNormal: cancelled mirror with syncable source -> revive",
			"source_event", source.ID,
			"mirror_event", mirrorEvent.ID,
		)
		return c.reviveCancelledMirror(ctx, source, mirrorEvent)
	}

	signal := mirror.ComputeDriftSignal(source, mirrorEvent)
	desired := mirror.BuildPayload(c.SourceCalendarID, source)

	c.debug("sync.reconcileNormal: inventory hit",
		"source_calendar", c.SourceCalendarID,
		"source_event", source.ID,
		"mirror_event", mirrorEvent.ID,
		"source_changed", signal.SourceChanged,
		"mirror_drifted", signal.MirrorDrifted,
		"needs_migration", signal.NeedsMigration,
		"summary", source.Summary,
	)

	if signal.NeedsMigration {
		c.debug("sync.reconcileNormal: routing to reconcileMigration",
			"source_event", source.ID,
			"mirror_event", mirrorEvent.ID,
			"source_changed", signal.SourceChanged,
			"mirror_drifted", signal.MirrorDrifted,
		)
		return c.reconcileMigration(ctx, source, mirrorEvent, desired, signal)
	}

	outcome := mirror.Classify(signal, c.SourceWritable, source.Updated, mirrorEvent.Updated)
	c.debug("sync.reconcileNormal: action chosen",
		"source_event", source.ID,
		"action", string(outcome.Action),
		"reason", string(outcome.Reason),
		"conflict", string(outcome.Conflict),
	)
	switch outcome.Action {
	case mirror.ActionSkip:
		c.emit(Outcome{
			Action:        outcome.Action,
			Reason:        outcome.Reason,
			Conflict:      outcome.Conflict,
			SourceEventID: source.ID,
			TargetEventID: mirrorEvent.ID,
			Summary:       source.Summary,
		})
		return nil

	case mirror.ActionPatch:
		return c.doPatchFromSource(ctx, source, mirrorEvent, desired, outcome)

	case mirror.ActionPropagate:
		return c.doPropagate(ctx, source, mirrorEvent, desired, outcome)

	case mirror.ActionRevert:
		return c.doRevert(ctx, source, mirrorEvent, desired, outcome)
	}
	// mirror.Classify returns only the four actions above; reaching here
	// would mean the matrix grew a new cell without updating this switch.
	return fmt.Errorf("sync: unexpected mirror.Classify action %q", outcome.Action)
}

// doPatchFromSource handles `source_changed && !mirror_drifted` (and the
// source-newer conflict cell): write the desired payload to the mirror,
// then a follow-up checksum patch using the post-write resource. Updates
// the inventory with the post-checksum response.
func (c *Classifier) doPatchFromSource(
	ctx context.Context,
	source, mirrorEvent *gws.Event,
	desired *gws.Event,
	outcome mirror.Outcome,
) error {
	desired.ID = ""
	post, err := c.patchMirrorWithChecksum(ctx, c.TargetCalendarID, mirrorEvent.ID, desired)
	if err != nil {
		return err
	}
	tuple := mirror.SourceTuple{CalendarID: c.SourceCalendarID, EventID: source.ID}
	c.Inventory.Set(tuple, post)
	srcUpdated, mirUpdated := conflictTimestamps(outcome.Conflict, source.Updated, mirrorEvent.Updated)
	c.emit(Outcome{
		Action:        outcome.Action,
		Reason:        outcome.Reason,
		Conflict:      outcome.Conflict,
		SourceEventID: source.ID,
		TargetEventID: post.ID,
		Summary:       source.Summary,
		SourceUpdated: srcUpdated,
		MirrorUpdated: mirUpdated,
	})
	return nil
}

// conflictTimestamps returns the (sourceUpdated, mirrorUpdated) pair to
// surface in the warn log per SPEC §"Conflict logging". Both values are
// empty for non-conflict outcomes and for the migration_source_won case,
// where SPEC line 500 omits the timestamps because v1 mirrors have no
// comparable user-edit timestamp. The v2 conflict cells
// (conflict_source_won / conflict_target_won) carry both timestamps so
// the user can verify which side won.
func conflictTimestamps(conflict mirror.Conflict, sourceUpdated, mirrorUpdated string) (string, string) {
	switch conflict {
	case mirror.ConflictSourceWon, mirror.ConflictTargetWon:
		return sourceUpdated, mirrorUpdated
	}
	return "", ""
}
