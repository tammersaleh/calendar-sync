//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// TestE2E_409_RecoversAndReconciles pins SPEC §"Mirror identification" /
// "Cancelled-and-revived" step 4: when a stranger event already occupies
// the deterministic mirror ID and is alive (status=confirmed, no
// calendar-sync extended properties), calendar-sync's insert must 409,
// fetch the existing event, and run reconciliation against it. The result
// is a single live event at the deterministic ID carrying the full
// managed-field bookkeeping. The phantom's pre-existing managed fields
// (summary, start/end) get overwritten by the source's because the v1/v2
// migration path source-wins on any drift (SPEC line 555).
func TestE2E_409_RecoversAndReconciles(t *testing.T) {
	h := Setup(t, SetupOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	sourceTitle := h.Title("collision-source")
	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary: sourceTitle,
		Start:   futureDateTime(0),
		End:     futureDateTime(1 * time.Hour),
	})

	detID := mirror.DeterministicID(h.SourceCalID, source.ID)

	// Pre-create the phantom at the exact ID calendar-sync will attempt
	// to insert at. The phantom carries NO calendar-sync extended
	// properties, so it looks like a v1/v2-shaped legacy mirror to
	// reconcileMigration. Calendar API requires Start, End, and a
	// non-empty Summary; without those the insert is rejected with 400.
	phantomTitle := h.Title("collision-phantom")
	phantom, err := h.GWS.EventsInsert(ctx, h.TargetCalID, &gws.Event{
		ID:      detID,
		Summary: phantomTitle,
		Start:   futureDateTime(2 * time.Hour),
		End:     futureDateTime(3 * time.Hour),
	})
	if err != nil {
		t.Fatalf("pre-create phantom at %s: %v", detID, err)
	}
	if phantom.ID != detID {
		t.Fatalf("phantom landed at %q, want %q", phantom.ID, detID)
	}

	res := h.Run(ctx)
	res.AssertSuccess(t)

	// The 409-recovery path routes the alive phantom through
	// reconcileMigration. Without our extended properties the mirror is
	// !needs_migration-eligible AND drifted vs desired, so the
	// migration_source_won cell fires: action=patch, reason=source_updated,
	// conflict=migration_source_won.
	out := res.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionPatch),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: source.ID,
		TargetEvent: detID,
	})
	if got, want := out.Conflict, string(mirror.ConflictMigrationSourceWon); got != want {
		t.Errorf("outcome.conflict = %q, want %q", got, want)
	}

	// No duplicate mirror: list the target and count events at detID.
	events, _, err := h.GWS.EventsList(ctx, gws.EventsListParams{
		CalendarID:  h.TargetCalID,
		ShowDeleted: true,
		MaxResults:  2500,
	})
	if err != nil {
		t.Fatalf("list target: %v", err)
	}
	live := 0
	for i := range events {
		if events[i].ID == detID && events[i].Status != gws.EventStatusCancelled {
			live++
		}
	}
	if live != 1 {
		t.Fatalf("expected exactly one live event at deterministic ID %s, got %d", detID, live)
	}

	mir, err := h.GWS.EventsGet(ctx, h.TargetCalID, detID)
	if err != nil {
		t.Fatalf("get reconciled mirror %s: %v", detID, err)
	}
	if mir.ExtendedProperties == nil || mir.ExtendedProperties.Private == nil {
		t.Fatalf("reconciled mirror missing extendedProperties.private; got %+v", mir.ExtendedProperties)
	}
	priv := mir.ExtendedProperties.Private
	if got := priv[mirror.ExtKeySource]; got == "" {
		t.Errorf("reconciled mirror %s is empty", mirror.ExtKeySource)
	}
	if got := priv[mirror.ExtKeyVersion]; got != mirror.SchemaVersion {
		t.Errorf("reconciled mirror %s = %q, want %q", mirror.ExtKeyVersion, got, mirror.SchemaVersion)
	}
	if got := priv[mirror.ExtKeyChecksum]; got == "" {
		t.Errorf("reconciled mirror %s is empty", mirror.ExtKeyChecksum)
	}
	if mir.Summary != sourceTitle {
		t.Errorf("reconciled mirror.summary = %q, want %q (migration_source_won overwrites the phantom)", mir.Summary, sourceTitle)
	}
}
