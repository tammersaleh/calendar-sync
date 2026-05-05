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
//
// MirrorDrifted=true with an empty drifted-field set degrades to a
// stale_bookkeeping mirror-side patch - see the empty-fields branch below.
func (c *Classifier) doPropagate(
	ctx context.Context,
	source, mirrorEvent *gws.Event,
	desired *gws.Event,
	outcome mirror.Outcome,
) error {
	fields := mirror.DriftedFieldNames(mirrorEvent, desired)
	if len(fields) == 0 {
		// MirrorDrifted=true (stored checksum disagreed) but the live
		// managed fields actually match desired-from-source. The only
		// thing wrong is the mirror's stored bookkeeping; there's
		// nothing to send to the source. A naive source-side
		// EventsPatch here would marshal to `{}` and either be rejected
		// by Calendar API (perpetual pdir failure) or accepted as a
		// pointless no-op every tick.
		//
		// Repair the mirror locally: rewrite it from source via
		// patchMirrorWithChecksum (same shape as B23's stale_bookkeeping
		// cell, but reached via the writable-source side of the matrix
		// rather than via FieldsDisagree). The follow-up checksum patch
		// brings stored bookkeeping back in sync. SPEC's action/reason
		// pairing for stale_bookkeeping is patch (line 571), not
		// propagate, so override the caller's outcome.
		return c.degradePropagateToStaleBookkeeping(ctx, source, mirrorEvent)
	}
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
	rewritten := mirror.BuildPayloadWithTimeZone(c.SourceCalendarID, patchedSource, c.TimeZone)

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

// degradePropagateToStaleBookkeeping is the empty-drifted-fields branch of
// doPropagate. MirrorDrifted=true is a checksum-vs-recomputed-checksum
// mismatch on stored bookkeeping, but the live managed fields end up
// equal to desired-from-source. There's nothing to send to source; the
// mirror just needs its stored checksum refreshed.
//
// Mirrors B23's stale_bookkeeping shape (Action=patch, Reason=
// stale_bookkeeping, no Conflict, no Fields, no conflict timestamps). The
// caller passed an outcome with Action=propagate; we override it because
// the user-visible action is "we patched the mirror," not "we propagated
// edits to the source." A propagate outcome with no source write would
// be misleading.
func (c *Classifier) degradePropagateToStaleBookkeeping(
	ctx context.Context,
	source, mirrorEvent *gws.Event,
) error {
	rewritten := mirror.BuildPayloadWithTimeZone(c.SourceCalendarID, source, c.TimeZone)
	post, err := c.patchMirrorWithChecksum(ctx, c.TargetCalendarID, mirrorEvent.ID, mirror.BuildPatchPayload(rewritten))
	if err != nil {
		return err
	}
	tuple := mirror.SourceTuple{CalendarID: c.SourceCalendarID, EventID: source.ID}
	c.Inventory.Set(tuple, post)

	c.emit(Outcome{
		Action:        mirror.ActionPatch,
		Reason:        mirror.ReasonStaleBookkeeping,
		SourceEventID: source.ID,
		TargetEventID: post.ID,
		Summary:       source.Summary,
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

