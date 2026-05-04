//go:build e2e

package e2e

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// reasonTransparencyTransparent mirrors SPEC's transparency-filter
// reason string (SPEC.md §"Filtering"). The sync package owns the
// typed constant; we pin the wire string here to keep e2e independent
// of implementation-layer imports.
//
// Why no `declined` / `tentative` constants here: those filters key
// off the source calendar owner's `self=true` attendee flag, which
// Google's API only sets on events visible to a USER's primary
// calendar where the user is INVITED. The harness's fixture calendars
// are group calendars owned by the auth user, so Google never echoes
// `self: true` on attendees there - declined/tentative detection is
// effectively unreachable through the API in this test setup. Those
// SPEC paths are covered by unit tests in internal/sync/classify_test.go
// using synthetic events with the flag set directly.
const reasonTransparencyTransparent = "transparency_transparent"

// TestE2E_Transparency_Skipped pins SPEC line 526's transparency-filter
// behavior: a source with `transparency=transparent` and no mirror
// produces a `skip(transparency_transparent)` outcome and no mirror is
// ever created on the target.
func TestE2E_Transparency_Skipped(t *testing.T) {
	h := Setup(t, SetupOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary:      h.Title("transparent-skip"),
		Start:        futureDateTime(0),
		End:          futureDateTime(1 * time.Hour),
		Transparency: gws.TransparencyTransparent,
	})

	res := h.Run(ctx)
	res.AssertSuccess(t)
	res.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionSkip),
		Reason:      reasonTransparencyTransparent,
		SourceEvent: source.ID,
	})

	// The mirror's deterministic ID must not exist on the target. A
	// successful events.get here would mean the filter leaked through
	// classification.
	wantID := mirror.DeterministicID(h.SourceCalID, source.ID)
	assertNoMirror(t, h, ctx, wantID)
}

// assertNoMirror fails the test if a mirror with the given target id
// exists in a non-cancelled state. A 404/410 from events.get is the
// happy path - the mirror was never created. A live tombstone
// (status=cancelled, returned by some API call paths) is also
// acceptable for filter scenarios; it means a previous run created
// the mirror and a subsequent filter-driven delete cancelled it. A
// confirmed event with our deterministic ID is the actual failure.
func assertNoMirror(t *testing.T, h *Harness, ctx context.Context, mirrorID string) {
	t.Helper()
	mir, err := h.GWS.EventsGet(ctx, h.TargetCalID, mirrorID)
	if err != nil {
		if errors.Is(err, gws.ErrAPINotFound) || errors.Is(err, gws.ErrAPIGone) {
			return
		}
		t.Fatalf("get mirror %s: %v", mirrorID, err)
	}
	if mir.Status == gws.EventStatusCancelled {
		return
	}
	t.Fatalf("expected no live mirror at %s, got %+v", mirrorID, mir)
}
