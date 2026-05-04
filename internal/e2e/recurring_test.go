//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// reasonInheritedSourceWon is SPEC's wire string for the inherited
// recurring-instance conflict label (SPEC §"Inherited recurring-instance
// handling"). We pin the literal here to keep e2e free of recurring-package
// imports - the wire shape is the contract.
const reasonInheritedSourceWon = "inherited_source_won"

// TestE2E_RecurringParent_With_InstanceOverride pins SPEC §"Recurring
// Events" / "The recurring-instance handler". A recurring source with one
// modified instance produces:
//
//   - one outcome for the parent: insert/source_updated at the deterministic
//     mirror-parent ID, with the source's recurrence array copied verbatim.
//   - one outcome for the instance override: action=patch + reason=
//     source_updated + conflict=inherited_source_won. The override's mirror
//     was auto-materialized when Google projected the parent's RRULE; the
//     handler routes auto-materialized instances through the inherited
//     bootstrap path, and any drift between the source override's managed
//     fields and the auto-projection's defaults sends it down the
//     inherited-source-wins cell (SPEC line 235).
//
// The mechanics of constructing a source-side instance override on Google
// Calendar: create the recurring parent, query events.instances to get the
// auto-projected occurrence IDs, patch the chosen projection (its ID has
// the form `<parent>_<UTC_timestamp>`). Patching a projection promotes it
// to a real exception override that events.list?singleEvents=false then
// returns alongside the parent.
func TestE2E_RecurringParent_With_InstanceOverride(t *testing.T) {
	h := Setup(t, SetupOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	parent, override := mustInsertRecurringSourceWithOverride(t, h, ctx, "recurring-with-override")

	res := h.Run(ctx)
	res.AssertSuccess(t)

	// Parent reconciliation: standard insert/source_updated at the
	// deterministic mirror-parent ID.
	wantMirrorParentID := mirror.DeterministicID(h.SourceCalID, parent.ID)
	parentOut := res.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionInsert),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: parent.ID,
		TargetEvent: wantMirrorParentID,
	})
	if parentOut.TargetEvent != wantMirrorParentID {
		t.Fatalf("parent outcome target_event = %q, want deterministic %q", parentOut.TargetEvent, wantMirrorParentID)
	}

	// Instance override: routed through the recurring handler's inherited-
	// instance branch. The override's start was moved relative to the
	// auto-projection, so source-changed AND mirror-drifted both fire on the
	// inherited mirror, taking the inherited_source_won cell.
	overrideOut := res.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionPatch),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: override.ID,
	})
	if got := overrideOut.Conflict; got != reasonInheritedSourceWon {
		t.Errorf("override outcome conflict = %q, want %q (SPEC §\"Inherited recurring-instance handling\")",
			got, reasonInheritedSourceWon)
	}
	if overrideOut.TargetEvent == "" {
		t.Fatal("override outcome has empty target_event")
	}

	// Verify the mirror parent on the target: managed-field bookkeeping plus
	// source's Recurrence array.
	mirrorParent, err := h.GWS.EventsGet(ctx, h.TargetCalID, wantMirrorParentID)
	if err != nil {
		t.Fatalf("get mirror parent %s: %v", wantMirrorParentID, err)
	}
	if mirrorParent.ExtendedProperties == nil || mirrorParent.ExtendedProperties.Private == nil {
		t.Fatalf("mirror parent missing extendedProperties.private; got %+v", mirrorParent.ExtendedProperties)
	}
	if got := mirrorParent.ExtendedProperties.Private[mirror.ExtKeyVersion]; got != mirror.SchemaVersion {
		t.Errorf("mirror parent %s = %q, want %q", mirror.ExtKeyVersion, got, mirror.SchemaVersion)
	}
	if got := mirrorParent.ExtendedProperties.Private[mirror.ExtKeyChecksum]; got == "" {
		t.Errorf("mirror parent %s is empty (parent insert ran the standard checksum follow-up)",
			mirror.ExtKeyChecksum)
	}
	if len(mirrorParent.Recurrence) == 0 {
		t.Errorf("mirror parent has empty Recurrence; want copy of source %v", parent.Recurrence)
	}

	// Verify the mirror instance via events.instances on the mirror parent
	// using the source override's originalStart - that's how the handler
	// itself locates the right mirror instance (SPEC §"Step 2").
	originalStart := override.OriginalStartTime.DateTime
	if originalStart == "" {
		t.Fatalf("override.OriginalStartTime.DateTime is empty (got %+v)", override.OriginalStartTime)
	}
	mirrorInstances, err := h.GWS.EventsInstances(ctx, gws.EventsInstancesParams{
		CalendarID:    h.TargetCalID,
		EventID:       wantMirrorParentID,
		OriginalStart: originalStart,
		MaxResults:    1,
		ShowDeleted:   true,
	})
	if err != nil {
		t.Fatalf("list mirror instances: %v", err)
	}
	if len(mirrorInstances) == 0 {
		t.Fatalf("no mirror instance found at originalStart %q under parent %s", originalStart, wantMirrorParentID)
	}
	mirrorInstance := mirrorInstances[0]

	// The instance must point at the mirror parent (not stand alone) and
	// carry the source override's moved start/end + summary.
	if mirrorInstance.RecurringEventID != wantMirrorParentID {
		t.Errorf("mirror instance.RecurringEventID = %q, want mirror parent %q",
			mirrorInstance.RecurringEventID, wantMirrorParentID)
	}
	if mirrorInstance.Summary != override.Summary {
		t.Errorf("mirror instance.Summary = %q, want %q", mirrorInstance.Summary, override.Summary)
	}
	// The override's Start carries the moved time; mirror instance must match.
	if mirrorInstance.Start == nil || override.Start == nil {
		t.Fatalf("nil start: mirror=%v override=%v", mirrorInstance.Start, override.Start)
	}
	if mirrorInstance.Start.DateTime != override.Start.DateTime {
		t.Errorf("mirror instance.Start.DateTime = %q, want %q (matches source override's moved start)",
			mirrorInstance.Start.DateTime, override.Start.DateTime)
	}
	// originalStartTime preserves the original RRULE-projected start.
	if mirrorInstance.OriginalStartTime == nil {
		t.Fatalf("mirror instance has nil OriginalStartTime")
	}
	if mirrorInstance.OriginalStartTime.DateTime != originalStart {
		t.Errorf("mirror instance.OriginalStartTime.DateTime = %q, want %q",
			mirrorInstance.OriginalStartTime.DateTime, originalStart)
	}
}

// TestE2E_InstanceOverridePropagates pins the source-edited cell on a
// recurring instance override: a follow-up patch of the source override's
// Summary produces action=patch, reason=source_updated on the same mirror
// instance. The conflict label is empty - the second run sees an
// explicitly-managed mirror instance (calendar-sync:source carries the
// instance's own ID, written by scenario 1's reconciliation), so the
// inherited branch no longer fires.
func TestE2E_InstanceOverridePropagates(t *testing.T) {
	h := Setup(t, SetupOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	parent, override := mustInsertRecurringSourceWithOverride(t, h, ctx, "recurring-propagate")

	// First run: establishes mirror parent + reconciles the override
	// through the inherited path so the next reconciliation sees an
	// explicitly-managed mirror instance.
	res := h.Run(ctx)
	res.AssertSuccess(t)
	wantMirrorParentID := mirror.DeterministicID(h.SourceCalID, parent.ID)
	res.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionInsert),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: parent.ID,
		TargetEvent: wantMirrorParentID,
	})
	overrideOut := res.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionPatch),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: override.ID,
	})
	mirrorInstanceID := overrideOut.TargetEvent
	if mirrorInstanceID == "" {
		t.Fatal("first-run override outcome has empty target_event")
	}

	// Patch the source override's Summary. EventsPatch returns the post-
	// write source; its Updated stamp drives the next reconciliation's
	// source_changed signal.
	editedTitle := h.Title("recurring-propagate-edited")
	patched, err := h.GWS.EventsPatch(ctx, h.SourceCalID, override.ID, &gws.Event{
		Summary: editedTitle,
	})
	if err != nil {
		t.Fatalf("patch source override %s: %v", override.ID, err)
	}
	if patched.Summary != editedTitle {
		t.Fatalf("post-patch source override.summary = %q, want %q", patched.Summary, editedTitle)
	}

	res2 := h.Run(ctx)
	res2.AssertSuccess(t)
	patchOut := res2.AssertOutcome(t, OutcomeMatch{
		Action:      string(mirror.ActionPatch),
		Reason:      string(mirror.ReasonSourceUpdated),
		SourceEvent: override.ID,
		TargetEvent: mirrorInstanceID,
	})
	// Second-run path: explicitly-managed instance -> standard four-way
	// drift matrix -> source_changed && !mirror_drifted -> patch with no
	// conflict label. (Inherited cell is bypassed because scenario 1's
	// write rewrote calendar-sync:source to the instance-tuple form.)
	if got := patchOut.Conflict; got != "" {
		t.Errorf("second-run propagate-into-mirror outcome conflict = %q, want empty (explicitly-managed instance)", got)
	}

	// Confirm the mirror instance picked up the new summary.
	postMirror, err := h.GWS.EventsGet(ctx, h.TargetCalID, mirrorInstanceID)
	if err != nil {
		t.Fatalf("get mirror instance %s post-patch: %v", mirrorInstanceID, err)
	}
	if postMirror.Summary != editedTitle {
		t.Errorf("post-patch mirror instance.summary = %q, want %q", postMirror.Summary, editedTitle)
	}
}

// mustInsertRecurringSourceWithOverride creates a source recurring parent
// (3 weekly occurrences) and overrides the second occurrence by patching
// the auto-projected instance with a moved start/end and a distinct
// summary. Returns (parent, override) - both as the post-write resources
// from Calendar API. Failure is fatal; the recurring scenarios can't
// proceed without both.
//
// scenarioTag prefixes the Summaries so each test's events stay
// distinguishable in the shared fixture calendar's history during
// debugging.
func mustInsertRecurringSourceWithOverride(t *testing.T, h *Harness, ctx context.Context, scenarioTag string) (parent, override *gws.Event) {
	t.Helper()

	// Anchor the series 24h from now and step by week so all three
	// occurrences are inside the harness's default 365d horizon and the
	// API's source-list TimeMin/TimeMax never excludes them.
	parentStart := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Minute)
	parentEnd := parentStart.Add(time.Hour)
	parentTitle := h.Title(scenarioTag + "-parent")
	parent, err := h.GWS.EventsInsert(ctx, h.SourceCalID, &gws.Event{
		Summary:    parentTitle,
		Start:      &gws.EventDateTime{DateTime: parentStart.Format(time.RFC3339), TimeZone: "UTC"},
		End:        &gws.EventDateTime{DateTime: parentEnd.Format(time.RFC3339), TimeZone: "UTC"},
		Recurrence: []string{"RRULE:FREQ=WEEKLY;COUNT=3"},
	})
	if err != nil {
		t.Fatalf("insert recurring source parent: %v", err)
	}

	// Wait briefly for the projected instances to become queryable. Google
	// generally returns projections immediately on a freshly-inserted
	// recurring event (they're generated from the RRULE, not stored), but
	// the harness has hit transient empty-list returns on the very first
	// query in some shared-fixture runs. Two retries with a short delay
	// covers that without padding the test runtime when the first call
	// already succeeds.
	timeMin := parentStart.Format(time.RFC3339)
	timeMax := parentStart.Add(8 * 24 * time.Hour).Format(time.RFC3339)
	var sourceInstances []gws.Event
	for retry := 0; retry < 3; retry++ {
		sourceInstances, err = h.GWS.EventsInstances(ctx, gws.EventsInstancesParams{
			CalendarID: h.SourceCalID,
			EventID:    parent.ID,
			TimeMin:    timeMin,
			TimeMax:    timeMax,
			MaxResults: 5,
		})
		if err == nil && len(sourceInstances) >= 2 {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("list source projected instances: %v", err)
	}
	if len(sourceInstances) < 2 {
		t.Fatalf("expected at least 2 projected instances, got %d (parent=%s)", len(sourceInstances), parent.ID)
	}

	// Pick the SECOND occurrence so the override is well clear of the
	// parent's own start time. Google's projected instance ID encodes the
	// original-start as a UTC timestamp; patching that ID is what promotes
	// the projection to a real source-side override.
	target := sourceInstances[1]
	if target.OriginalStartTime == nil || target.OriginalStartTime.DateTime == "" {
		t.Fatalf("projected instance %s has no OriginalStartTime.DateTime: %+v", target.ID, target.OriginalStartTime)
	}

	// Move the override 2 hours later relative to the original projection
	// and rename so any drift detection cleanly distinguishes the override
	// from the auto-projection.
	originalStart, err := time.Parse(time.RFC3339, target.Start.DateTime)
	if err != nil {
		t.Fatalf("parse projected start %q: %v", target.Start.DateTime, err)
	}
	movedStart := originalStart.Add(2 * time.Hour)
	movedEnd := movedStart.Add(time.Hour)
	overrideTitle := h.Title(scenarioTag + "-override")
	override, err = h.GWS.EventsPatch(ctx, h.SourceCalID, target.ID, &gws.Event{
		Summary: overrideTitle,
		Start:   &gws.EventDateTime{DateTime: movedStart.Format(time.RFC3339), TimeZone: "UTC"},
		End:     &gws.EventDateTime{DateTime: movedEnd.Format(time.RFC3339), TimeZone: "UTC"},
	})
	if err != nil {
		t.Fatalf("patch projection %s into override: %v", target.ID, err)
	}
	if override.RecurringEventID != parent.ID {
		t.Fatalf("override.RecurringEventID = %q, want parent ID %q", override.RecurringEventID, parent.ID)
	}
	if override.OriginalStartTime == nil || override.OriginalStartTime.DateTime == "" {
		t.Fatalf("post-patch override missing OriginalStartTime.DateTime: %+v", override.OriginalStartTime)
	}
	return parent, override
}
