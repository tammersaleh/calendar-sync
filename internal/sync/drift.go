package sync

import (
	"context"
	"fmt"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// doPropagate handles `!source_changed && mirror_drifted && source_writable`
// (and the mirror-newer-and-source-writable conflict cell). Per SPEC.md
// "Drift handling":
//
//  1. Compute the drifted-field set from live mirror vs desired-from-source.
//  2. events.patch the SOURCE with only those fields (description trailer
//     stripped via mirror.BuildPropagatePatchBody).
//  3. Re-write the mirror from the post-patch source state, then run the
//     checksum follow-up.
//
// Inventory entry replaced with the second post-write resource.
func (c *Classifier) doPropagate(
	ctx context.Context,
	source, mirrorEvent *gws.Event,
	desired *gws.Event,
	outcome mirror.Outcome,
) error {
	fields := mirror.DriftedFieldNames(mirrorEvent, desired)
	patchBody := mirror.BuildPropagatePatchBody(mirrorEvent, fields)

	patchedSource, err := c.API.EventsPatch(ctx, c.SourceCalendarID, source.ID, patchBody)
	if err != nil {
		return fmt.Errorf("propagate to source %s/%s: %w", c.SourceCalendarID, source.ID, err)
	}

	// Rebuild the mirror payload from the post-patch source so the mirror
	// gets the new state (and the new calendar-sync:source_updated value
	// pulled from patchedSource.Updated). Convert via BuildPatchPayload so
	// every managed field is set explicitly (clear-intent on whatever the
	// post-patch source has cleared).
	rewritten := mirror.BuildPayload(c.SourceCalendarID, patchedSource)

	post, err := c.patchMirrorWithChecksum(ctx, c.TargetCalendarID, mirrorEvent.ID, mirror.BuildPatchPayload(rewritten))
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
		Fields:        fields,
		SourceEventID: source.ID,
		TargetEventID: post.ID,
		Summary:       source.Summary,
		SourceUpdated: srcUpdated,
		MirrorUpdated: mirUpdated,
	})
	return nil
}

// doRevert handles `!source_changed && mirror_drifted && !source_writable`
// (and the mirror-newer-and-source-readonly conflict cell). The mirror's
// drifted fields are overwritten with the desired payload; no source-side
// write since the source is read-only.
//
// Inventory entry replaced with the post-checksum mirror resource.
func (c *Classifier) doRevert(
	ctx context.Context,
	source, mirrorEvent *gws.Event,
	desired *gws.Event,
	outcome mirror.Outcome,
) error {
	fields := mirror.DriftedFieldNames(mirrorEvent, desired)
	post, err := c.patchMirrorWithChecksum(ctx, c.TargetCalendarID, mirrorEvent.ID, mirror.BuildPatchPayload(desired))
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
		Fields:        fields,
		SourceEventID: source.ID,
		TargetEventID: post.ID,
		Summary:       source.Summary,
		SourceUpdated: srcUpdated,
		MirrorUpdated: mirUpdated,
	})
	return nil
}

