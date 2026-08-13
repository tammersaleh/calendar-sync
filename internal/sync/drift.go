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

	// B38: a recurring PARENT (reached here via the non-recurring/parent path
	// in reconcileNormal, so RecurringEventID is always "") must NEVER have
	// its anchor fields (start / end / recurrence) reverse-propagated. An
	// events.patch of start/end on the parent endpoint moves the DTSTART grid,
	// shifting every occurrence; a recurrence patch rewrites (or recreates) the
	// series. A single false-positive mirror-parent drift there destroys the
	// series irrecoverably - the live damage recorded in doc/bugs.md B38. The
	// asymmetry against the rare convenience of dragging/creating a whole
	// series from the mirror side makes refusal the safe default, so the
	// source stays authoritative for series timing regardless of
	// source_writable. Only the explicit safe allowlist propagates; every
	// other field (anchors AND any future managed field) fails closed.
	//
	// The guard fires when EITHER side is recurring, not just the source.
	// If the source cleared its RRULE while the mirror still carries the old
	// one, source.Recurrence is empty but mirrorEvent.Recurrence is not, and
	// the un-guarded path would re-propagate the stale RRULE and resurrect the
	// series on the source (Codex-flagged transition).
	if len(source.Recurrence) > 0 || len(mirrorEvent.Recurrence) > 0 {
		refused, propagatable := splitParentSafeFields(fields)
		if len(refused) > 0 {
			c.warn("sync.doPropagate: refusing to move recurring series anchor from mirror-side edit",
				"source_event", source.ID,
				"refused_fields", refused,
				"propagated_fields", propagatable,
			)
			fields = propagatable
			if len(fields) == 0 {
				// Nothing safe to send to source: revert the mirror parent to
				// source (mirror-only patch, no source write) so the series is
				// protected and the drift resolves. Delegate to doRevert so the
				// conflict label and timestamps from the four-way matrix survive.
				outcome.Action = mirror.ActionRevert
				return c.doRevert(ctx, source, mirrorEvent, desired, outcome)
			}
		}
	}

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

// recurringParentSafeFields is the allowlist of managed fields that MAY be
// reverse-propagated from a mirror to a recurring PARENT's events.patch
// endpoint. Everything not listed - the timing anchors start/end/recurrence,
// plus any managed field added in the future - is refused (fails closed),
// because a mistaken write of an unmodeled field to a parent could carry the
// same series-wide blast radius as an anchor move (B38).
var recurringParentSafeFields = map[string]bool{
	"summary":      true,
	"description":  true,
	"location":     true,
	"transparency": true,
	"visibility":   true,
}

// splitParentSafeFields partitions drifted field names into those refused for
// a recurring parent (not on the allowlist) and those safe to propagate.
func splitParentSafeFields(fields []string) (refused, propagatable []string) {
	for _, f := range fields {
		if recurringParentSafeFields[f] {
			propagatable = append(propagatable, f)
		} else {
			refused = append(refused, f)
		}
	}
	return refused, propagatable
}

// doRevert handles `!source_changed && mirror_drifted && !source_writable`
// (and the mirror-newer-and-source-readonly conflict cell). It is also the
// B38 anchor-only path: doPropagate delegates here (with outcome.Action forced
// to revert) when a recurring parent's only drift is in refused timing fields,
// so the mirror is rewritten from source and the series is never touched. The
// mirror's drifted fields are overwritten with the desired payload; no
// source-side write since the source is read-only (or, for B38, protected).
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

