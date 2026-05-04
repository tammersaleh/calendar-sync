//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// TestE2E_OutsideHorizon_NoMirror pins horizon-filter behavior end to
// end: a non-recurring source past the configured horizon does not
// produce a mirror. Patching the source's start back inside the horizon
// triggers the insert on the next run.
//
// Implementation note: the production source-list call sets
// `TimeMax = now + horizon` (per internal/sync/reconciler.go's
// fullListSources), so an event past the horizon is filtered by
// Calendar API itself before classification ever runs. There is no
// `skip(outside_horizon)` outcome to assert in this single-pdir case;
// the SPEC reason fires for recurring parents whose first instance
// falls past the horizon (different test) and for events the API
// returns under a longer per-source horizon that one pdir's classifier
// then rejects per its own shorter horizon (multi-pdir setup, also a
// different test). The user-visible effect - no mirror exists - is
// what this test pins.
func TestE2E_OutsideHorizon_NoMirror(t *testing.T) {
	h := Setup(t, SetupOptions{Horizon: "1d"})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	// futureDateTime offsets from now+24h, so delta=24h yields ~48h from
	// now (well past the 1d horizon).
	source := mustInsertSource(t, h, ctx, &gws.Event{
		Summary: h.Title("outside-horizon"),
		Start:   futureDateTime(24 * time.Hour),
		End:     futureDateTime(25 * time.Hour),
	})

	res := h.Run(ctx)
	res.AssertSuccess(t)
	// Source is past horizon - the API filter excludes it from the
	// source list, so no outcome references it.
	res.AssertNoOutcomeForSource(t, source.ID)

	wantID := mirror.DeterministicID(h.SourceCalID, source.ID)
	assertNoMirror(t, h, ctx, wantID)

	// Patch start/end into the horizon window. delta=-12h yields ~12h
	// from now, safely under the 1d horizon.
	patched, err := h.GWS.EventsPatch(ctx, h.SourceCalID, source.ID, &gws.Event{
		Start: futureDateTime(-12 * time.Hour),
		End:   futureDateTime(-11 * time.Hour),
	})
	if err != nil {
		t.Fatalf("patch source %s into horizon: %v", source.ID, err)
	}
	if patched.Start == nil || patched.Start.DateTime == "" {
		t.Fatalf("post-patch source missing start.dateTime: %+v", patched.Start)
	}

	res2 := h.Run(ctx)
	res2.AssertSuccess(t)
	insertOut := res2.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionInsert),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: source.ID,
	})
	if insertOut.TargetEvent != wantID {
		t.Errorf("insert outcome target_event = %q, want deterministic %q", insertOut.TargetEvent, wantID)
	}

	// The mirror must now exist as a live event at the deterministic ID.
	mir, err := h.GWS.EventsGet(ctx, h.TargetCalID, wantID)
	if err != nil {
		t.Fatalf("get mirror %s after horizon patch: %v", wantID, err)
	}
	if mir.Status == gws.EventStatusCancelled {
		t.Errorf("mirror %s is cancelled after horizon patch; want live", wantID)
	}
}
