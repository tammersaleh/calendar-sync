//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// TestE2E_F1_SummaryRefResolves pins the F1 contract: a TOML pair
// configured with `source = {summary = "..."}` and
// `target = {summary = "..."}` resolves correctly through
// CalendarListList, finds the fixture calendars by their display
// summaries, and runs a normal source-to-mirror sync. Proves the
// entire summary→canonical-ID resolution chain works end-to-end
// against live Google Calendar.
func TestE2E_F1_SummaryRefResolves(t *testing.T) {
	h := Setup(t, SetupOptions{
		UseSummarySourceRef: true,
		UseSummaryTargetRef: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	title := h.Title("f1-summary-ref")
	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary: title,
		Start:   futureDateTime(0),
		End:     futureDateTime(1 * time.Hour),
	})

	res := h.Run(ctx)
	res.AssertSuccess(t)
	out := res.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionInsert),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: source.ID,
	})

	// Deterministic mirror ID is derived from the canonical source
	// calendar ID. If summary→ID resolution misfired, the outcome's
	// target_event would be derived from a different (or empty)
	// calendar ID and this assertion would fail.
	wantID := mirror.DeterministicID(h.SourceCalID, source.ID)
	if out.TargetEvent != wantID {
		t.Errorf("insert outcome target_event = %q, want deterministic %q", out.TargetEvent, wantID)
	}

	// Confirm the mirror physically lives on the target fixture
	// calendar (resolved from `target = {summary = ...}`), not
	// elsewhere - this is the F1-specific behavior under test.
	mir, err := h.GWS.EventsGet(ctx, h.TargetCalID, wantID)
	if err != nil {
		t.Fatalf("get mirror %s on target %s: %v", wantID, h.TargetCalID, err)
	}
	if mir.Summary != title {
		t.Errorf("mirror.summary = %q, want %q", mir.Summary, title)
	}
}
