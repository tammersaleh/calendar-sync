package sync

import (
	"context"
	"errors"
	"fmt"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// doInsert handles SPEC.md "Classification logic" step 8's "no mirror in
// inventory" branch. The deterministic mirror ID derived in
// mirror.BuildPayload is the dedup key Google uses; another process that
// raced through to insert the same source first surfaces as HTTP 409, which
// we recover by getting the existing mirror and either reviving it (if it
// was cancelled) or running the standard inventory-hit reconciliation
// against the fetched resource.
func (c *Classifier) doInsert(ctx context.Context, source *gws.Event) error {
	payload := mirror.BuildPayloadWithTimeZone(c.SourceCalendarID, source, c.TimeZone)

	inserted, err := c.API.EventsInsert(ctx, c.TargetCalendarID, payload)
	if err == nil {
		return c.completeInsert(ctx, source, inserted)
	}
	if !errors.Is(err, gws.ErrAPIConflict) {
		return fmt.Errorf("insert mirror %s/%s: %w", c.TargetCalendarID, payload.ID, err)
	}

	// 409 path: another process already created this mirror, OR a deleted
	// mirror's ID is still reserved on Google's side. Get the existing
	// resource and reconcile from there.
	existing, err := c.API.EventsGet(ctx, c.TargetCalendarID, payload.ID)
	if err != nil {
		// Wrap with the errInsertCollisionRead marker so the classify
		// loop's transient-read detector keeps this fatal: the read's
		// result drives a write decision (revive cancelled vs reconcile
		// alive), and a flake here can't be safely skipped without
		// leaving the colliding mirror in an unknown state.
		return fmt.Errorf("post-409 events.get %s/%s: %w (%w)",
			c.TargetCalendarID, payload.ID, err, errInsertCollisionRead)
	}
	if existing.Status == gws.EventStatusCancelled {
		return c.reviveCancelledMirror(ctx, source, existing)
	}
	// Alive existing mirror: treat as inventory hit and run the standard
	// reconciliation. Fold it into our inventory first so the inventory-hit
	// branch can find it; we re-route through reconcileNormal which will
	// hit the mirror-exists branch and run drift detection.
	tuple := mirror.SourceTuple{CalendarID: c.SourceCalendarID, EventID: source.ID}
	c.Inventory.Set(tuple, existing)
	return c.reconcileNormal(ctx, source)
}

// completeInsert runs the checksum follow-up after a successful main insert
// per SPEC.md "Computing the checksum from the post-write event". Two
// follow-up patches happen: the first is the checksum, and patchMirrorWith
// Checksum already does main+followup, but we already did the main insert.
// So here we just compute the checksum from the post-insert resource and
// fire one patch.
func (c *Classifier) completeInsert(ctx context.Context, source, inserted *gws.Event) error {
	final, err := c.followUpChecksum(ctx, c.TargetCalendarID, inserted.ID, inserted)
	if err != nil {
		return err
	}
	tuple := mirror.SourceTuple{CalendarID: c.SourceCalendarID, EventID: source.ID}
	c.Inventory.Set(tuple, final)

	c.emit(Outcome{
		Action:        mirror.ActionInsert,
		Reason:        mirror.ReasonSourceUpdated,
		SourceEventID: source.ID,
		TargetEventID: final.ID,
		Summary:       source.Summary,
	})
	return nil
}

// reviveCancelledMirror handles the 409-and-existing-is-cancelled case from
// SPEC.md step 8: events.patch with status=confirmed plus the full mirror
// payload to revive the mirror, then the standard checksum follow-up.
//
// The user-facing action stays "insert" (the effect on the user's mirror
// calendar is the same as a fresh insert); reason stays source_updated.
func (c *Classifier) reviveCancelledMirror(ctx context.Context, source, existing *gws.Event) error {
	payload := mirror.BuildPayloadWithTimeZone(c.SourceCalendarID, source, c.TimeZone)
	payload.ID = ""
	payload.Status = gws.EventStatusConfirmed

	post, err := c.patchMirrorWithChecksum(ctx, c.TargetCalendarID, existing.ID, payload)
	if err != nil {
		return err
	}
	tuple := mirror.SourceTuple{CalendarID: c.SourceCalendarID, EventID: source.ID}
	c.Inventory.Set(tuple, post)

	c.emit(Outcome{
		Action:        mirror.ActionInsert,
		Reason:        mirror.ReasonSourceUpdated,
		SourceEventID: source.ID,
		TargetEventID: post.ID,
		Summary:       source.Summary,
	})
	return nil
}

