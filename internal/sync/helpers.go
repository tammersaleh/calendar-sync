package sync

import (
	"context"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// patchMirrorWithChecksum runs SPEC.md "Computing the checksum from the
// post-write event": a main events.patch followed by a follow-up patch
// that stores calendar-sync:checksum computed from the post-write
// resource. Returns the post-checksum response.
//
// This duplicates the helper of the same name in internal/recurring/
// handler.go. The duplication is intentional for now per the layer-6
// prompt - extracting a shared helper requires a careful home (likely a
// new exported function in mirror or a small writer struct) and isn't
// yet worth the design churn. The two copies are identical line-for-line
// and trivial to reconcile if either changes.
func (c *Classifier) patchMirrorWithChecksum(
	ctx context.Context,
	calendarID, eventID string,
	body *gws.Event,
) (*gws.Event, error) {
	post, err := c.API.EventsPatch(ctx, calendarID, eventID, body)
	if err != nil {
		return nil, err
	}
	checksum := mirror.Checksum(mirror.ManagedFieldsFromEvent(post))
	follow := &gws.Event{
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{mirror.ExtKeyChecksum: checksum},
		},
	}
	return c.API.EventsPatch(ctx, calendarID, eventID, follow)
}

// followUpChecksum runs the checksum-only patch after a main insert.
// Differs from patchMirrorWithChecksum in that the main write was an
// events.insert (not events.patch), so we already have the post-insert
// resource - only the checksum patch remains.
func (c *Classifier) followUpChecksum(
	ctx context.Context,
	calendarID, eventID string,
	post *gws.Event,
) (*gws.Event, error) {
	checksum := mirror.Checksum(mirror.ManagedFieldsFromEvent(post))
	follow := &gws.Event{
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{mirror.ExtKeyChecksum: checksum},
		},
	}
	return c.API.EventsPatch(ctx, calendarID, eventID, follow)
}
