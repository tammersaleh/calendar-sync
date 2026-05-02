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
// function recomputes MirrorDrifted via direct managed-field comparison
// (live vs desired-from-source) and then routes by the four-way matrix:
//
//   - !source_changed && !mirror_drifted: migration_upgrade. Re-write the
//     mirror at the current SchemaVersion with a fresh checksum. SPEC.md
//     "Schema version migration" routes this cell here rather than to
//     skip(unchanged).
//   - source_changed && mirror_drifted: source-wins-by-default. SPEC says
//     newer-wins isn't reliable across schema versions (v1 mirrors lack a
//     usable user-edit timestamp; v2 mirrors have one but we keep v1
//     semantics for simplicity and consistency), so we patch from source
//     unconditionally with conflict=migration_source_won.
//   - source_changed && !mirror_drifted: standard patch(source_updated).
//     The cell behaves identically to the standard matrix; falls through
//     to the existing patch path so the legacy/current paths converge
//     afterward.
//   - !source_changed && mirror_drifted: standard propagate or revert.
//     Falls through to mirror.Classify for the same reason.
//
// The two cells that diverge from the standard matrix are handled inline;
// the other two fall through to the standard mirror.Classify dispatch.
// This mirrors the recurring handler's migration routing in
// internal/recurring/handler.go.
func (c *Classifier) reconcileMigration(
	ctx context.Context,
	source, mirrorEvent *gws.Event,
	desired *gws.Event,
	signal mirror.DriftSignal,
) error {
	// Recompute MirrorDrifted via live-vs-desired managed-field comparison
	// (the SPEC's "Schema version migration" path).
	signal.MirrorDrifted = mirror.Checksum(mirror.ManagedFieldsFromEvent(mirrorEvent)) !=
		mirror.Checksum(mirror.ManagedFieldsFromEvent(desired))

	switch {
	case !signal.SourceChanged && !signal.MirrorDrifted:
		// migration_upgrade: rewrite at the current SchemaVersion with a
		// fresh checksum.
		return c.doMigrationUpgrade(ctx, source, mirrorEvent, desired)

	case signal.SourceChanged && signal.MirrorDrifted:
		// migration_source_won: source wins regardless of timestamps.
		return c.doMigrationSourceWon(ctx, source, mirrorEvent, desired)
	}

	// Source-only or mirror-only: identical to the standard matrix. Fall
	// through to the standard outcome dispatch so the propagate/revert/
	// patch logic stays in one place.
	outcome := mirror.Classify(signal, c.SourceWritable, source.Updated, mirrorEvent.Updated)
	switch outcome.Action {
	case mirror.ActionSkip:
		// Unreachable: the v1 cells we'd land here for are source-only or
		// mirror-only, both of which are non-skip outcomes via Classify.
		// The two skip-eligible cells (!source_changed && !mirror_drifted,
		// and source_changed && mirror_drifted) are handled inline above.
		return fmt.Errorf("sync: unreachable %s in reconcileMigration", outcome.Action)
	case mirror.ActionPatch:
		return c.doPatchFromSource(ctx, source, mirrorEvent, desired, outcome)
	case mirror.ActionPropagate:
		return c.doPropagate(ctx, source, mirrorEvent, desired, outcome)
	case mirror.ActionRevert:
		return c.doRevert(ctx, source, mirrorEvent, desired, outcome)
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
	desired.ID = ""
	post, err := c.patchMirrorWithChecksum(ctx, c.TargetCalendarID, mirrorEvent.ID, desired)
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
	desired.ID = ""
	post, err := c.patchMirrorWithChecksum(ctx, c.TargetCalendarID, mirrorEvent.ID, desired)
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
