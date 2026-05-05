package sync

import (
	"context"
	"fmt"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// reconcileMigration runs SPEC.md "Schema version migration" for any mirror
// whose stored calendar-sync:version differs from the current SchemaVersion
// (v1 or v2 today; future legacy versions automatically). The standard
// mirror.ComputeDriftSignal would return MirrorDrifted=true for a v1 mirror
// (no stored checksum) and is unreliable for any pre-current-version mirror
// whose stored checksum was computed over a smaller managed-field set; this
// function recomputes MirrorDrifted via mirror.DriftedFieldNames (live vs
// desired-from-source), so the migration path's "drift" definition stays
// consistent with the propagate body's drifted set - both use the same
// transparency/visibility/recurrence normalization. Then routes by the
// four-way matrix:
//
//   - !source_changed && !mirror_drifted: migration_upgrade. Re-write the
//     mirror at the current SchemaVersion with a fresh checksum. SPEC.md
//     "Schema version migration" routes this cell here rather than to
//     skip(unchanged).
//   - mirror_drifted (with or without source_changed): source-wins-by-
//     default. We can't safely propagate during migration because the
//     "drift" may be schema-induced (e.g. a v3 field that didn't exist
//     in the v2 mirror, like Location) rather than a real user edit.
//     Distinguishing the two would require per-field schema-version
//     metadata we don't have. So source always wins on any drift during
//     migration; conflict=migration_source_won surfaces this to the user.
//     This is the conservative trade-off matching v1 semantics (v1 mirrors
//     have no reliable user-edit timestamp at all).
//   - source_changed && !mirror_drifted: standard patch(source_updated).
//     The cell behaves identically to the standard matrix; falls through
//     to the existing patch path so the legacy/current paths converge
//     afterward.
//
// Three of four cells are handled inline (migration_upgrade for the no-
// drift case, migration_source_won for any mirror drift). Only
// !MirrorDrifted && SourceChanged falls through to mirror.Classify, which
// correctly handles it as ActionPatch / ReasonSourceUpdated. This mirrors
// the recurring handler's migration routing in
// internal/recurring/handler.go.
func (c *Classifier) reconcileMigration(
	ctx context.Context,
	source, mirrorEvent *gws.Event,
	desired *gws.Event,
	signal mirror.DriftSignal,
) error {
	// Recompute MirrorDrifted via live-vs-desired managed-field comparison
	// (the SPEC's "Schema version migration" path). DriftedFieldNames
	// normalizes transparency/visibility defaults and uses order-insensitive
	// recurrence equality, matching the propagate body's drift set so the
	// two views can't disagree.
	signal.MirrorDrifted = len(mirror.DriftedFieldNames(mirrorEvent, desired)) > 0

	switch {
	case !signal.SourceChanged && !signal.MirrorDrifted:
		// migration_upgrade: rewrite at the current SchemaVersion with a
		// fresh checksum.
		return c.doMigrationUpgrade(ctx, source, mirrorEvent, desired)

	case signal.MirrorDrifted:
		// ANY mirror drift during migration (with or without source change)
		// is source-wins. Drift may be schema-induced (e.g. v2 mirrors lack
		// the Location field that v3 introduced); we cannot safely propagate
		// the mirror's value to source because the diff might not be a user
		// edit at all. The migration_source_won conflict warns the user that
		// the mirror's pre-migration content was overwritten by source -
		// this is acceptable because (a) v1 mirrors had no reliable user-
		// edit timestamp (existing rationale), and (b) v2->v3 mirror diffs
		// in the new Location field aren't user edits at all.
		return c.doMigrationSourceWon(ctx, source, mirrorEvent, desired)
	}

	// Only !MirrorDrifted && SourceChanged falls through. The standard
	// matrix routes this to ActionPatch / ReasonSourceUpdated, identical
	// to the standard cell.
	outcome := mirror.Classify(signal, c.SourceWritable, source.Updated, mirrorEvent.Updated)
	switch outcome.Action {
	case mirror.ActionSkip:
		// Unreachable: !MirrorDrifted && SourceChanged routes through
		// Classify to ActionPatch, never ActionSkip. The skip-eligible
		// (!source_changed && !mirror_drifted) and drift-handling
		// (mirror_drifted) cells are handled inline above.
		return fmt.Errorf("sync: unreachable %s in reconcileMigration", outcome.Action)
	case mirror.ActionPatch:
		return c.doPatchFromSource(ctx, source, mirrorEvent, desired, outcome)
	case mirror.ActionPropagate, mirror.ActionRevert:
		// Unreachable: ActionPropagate / ActionRevert require mirror_drifted=
		// true, which is handled by the inline migration_source_won case
		// above.
		return fmt.Errorf("sync: unreachable %s in reconcileMigration", outcome.Action)
	}
	// mirror.Classify returns only the four actions above; reaching here
	// would mean the matrix grew a new cell without updating this switch.
	return fmt.Errorf("sync: unexpected mirror.Classify action %q in reconcileMigration", outcome.Action)
}

// doMigrationUpgrade is SPEC.md "Schema version migration"'s
// !source_changed && !mirror_drifted cell. The mirror's managed fields
// already match the source; we just need to rewrite the extended-property
// layout (current SchemaVersion + fresh :checksum, plus any new managed
// fields the legacy schema didn't carry).
//
// BuildPayload writes the current SchemaVersion in the extended properties;
// the patch+checksum-followup pair is the same primitive as any other
// patch path.
func (c *Classifier) doMigrationUpgrade(
	ctx context.Context,
	source, mirrorEvent *gws.Event,
	desired *gws.Event,
) error {
	post, err := c.patchMirrorWithChecksum(ctx, c.TargetCalendarID, mirrorEvent.ID, mirror.BuildPatchPayload(desired))
	if err != nil {
		return err
	}
	tuple := mirror.SourceTuple{CalendarID: c.SourceCalendarID, EventID: source.ID}
	c.Inventory.Set(tuple, post)

	c.emit(Outcome{
		Action:        mirror.ActionPatch,
		Reason:        ReasonMigrationUpgrade,
		SourceEventID: source.ID,
		TargetEventID: post.ID,
		Summary:       source.Summary,
	})
	return nil
}

// doMigrationSourceWon is SPEC.md "Schema version migration"'s
// source_changed && mirror_drifted cell. Legacy mirrors take source-wins
// during migration regardless of stored timestamps: v1 mirrors lack a
// reliable user-edit timestamp, and v2 mirrors keep the same simpler
// behavior so the migration cell stays consistent across legacy versions.
// The mirror's edits are overwritten regardless of timestamps; the
// conflict logging surfaces this to the user as migration_source_won.
func (c *Classifier) doMigrationSourceWon(
	ctx context.Context,
	source, mirrorEvent *gws.Event,
	desired *gws.Event,
) error {
	post, err := c.patchMirrorWithChecksum(ctx, c.TargetCalendarID, mirrorEvent.ID, mirror.BuildPatchPayload(desired))
	if err != nil {
		return err
	}
	tuple := mirror.SourceTuple{CalendarID: c.SourceCalendarID, EventID: source.ID}
	c.Inventory.Set(tuple, post)

	c.emit(Outcome{
		Action:        mirror.ActionPatch,
		Reason:        mirror.ReasonSourceUpdated,
		Conflict:      mirror.ConflictMigrationSourceWon,
		SourceEventID: source.ID,
		TargetEventID: post.ID,
		Summary:       source.Summary,
	})
	return nil
}
