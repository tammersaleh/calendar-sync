package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
	"github.com/tammersaleh/calendar-sync/internal/recurring"
)

// Reason values the sync layer's classification logic can emit beyond the
// ones already defined in mirror (ReasonUnchanged, ReasonSourceUpdated,
// ReasonTargetEdited). Strings match SPEC.md's reason column verbatim so
// the eventual stdout printer can switch on a single Reason vocabulary.
const (
	ReasonIsMirror               mirror.Reason = "is_mirror"
	ReasonCancelled              mirror.Reason = "cancelled"
	ReasonSourceCancelled        mirror.Reason = "source_cancelled"
	ReasonDeclined               mirror.Reason = "declined"
	ReasonTentative              mirror.Reason = "tentative"
	ReasonTransparencyTransparent mirror.Reason = "transparency_transparent"
	ReasonOutsideHorizon         mirror.Reason = "outside_horizon"
	ReasonMigrationUpgrade       mirror.Reason = "migration_upgrade"
)

// Outcome is one classification decision. The eventual JSONL output layer
// emits these as action lines per SPEC.md "calendar-sync run" / actions
// table; the daemon (layer 7) is responsible for translating Conflict to
// a stderr warn log.
//
// TargetEventID is populated for any action that touches a mirror; for
// skip outcomes that don't correspond to a mirror it stays empty.
type Outcome struct {
	Action        mirror.Action
	Reason        mirror.Reason
	Conflict      mirror.Conflict
	Fields        []string
	Pair          string
	Direction     string
	SourceEventID string
	TargetEventID string
	Summary       string

	// SourceUpdated and MirrorUpdated record the timestamps that drove the
	// newer-wins decision when Conflict is non-empty (per SPEC §"Conflict
	// logging"). Both empty for non-conflict outcomes. Both also empty
	// when Conflict is migration_source_won - SPEC line 500 omits the
	// timestamps in the migration warn log because v1 mirrors have no
	// comparable user-edit timestamp.
	SourceUpdated string
	MirrorUpdated string
}

// Output is the sink for outcomes. The daemon will wire this to the JSONL
// stdout printer; tests use a closure that captures outcomes for
// assertion. A function value rather than an interface keeps test setup
// trivial - no mock to mock.
type Output func(Outcome)

// Classifier owns the per-pdir state needed to classify one source event.
// Construct one per pdir per reconciliation pass. The Classifier holds
// references to (not copies of) the per-target Inventory and per-pdir
// recurring.Handler so writes feed back into shared state.
//
// Now defaults to time.Now when nil; tests inject a fixed clock by setting
// it explicitly. Horizon mirrors SPEC.md's Settings.Horizon.
type Classifier struct {
	API API
	Now func() time.Time
	// Horizon is SPEC's Settings.Horizon - the lookahead window step 7
	// uses to filter events whose start (or, for recurring parents, any
	// instance) falls beyond now+Horizon.
	//
	// IMPORTANT: Horizon=0 (the zero value) is treated as "no horizon" -
	// every event passes step 7. This is convenient for tests that don't
	// care about horizon semantics but is a footgun in production. The
	// layer-7 daemon wiring MUST set a non-zero Horizon value before
	// constructing the Classifier; otherwise the daemon will mirror
	// arbitrarily-far-future events and never prune them.
	Horizon          time.Duration
	Pair             string
	Direction        string
	SourceCalendarID string
	TargetCalendarID string
	SourceWritable   bool
	Inventory        *Inventory
	Recurring        *recurring.Handler
	Output           Output
	// Log is the per-step diagnostic logger; nil silences output. The
	// reconciler-side wiring populates this from the Reconciler's Log
	// field; tests typically leave it nil.
	Log Logger
}

// debug is a nil-safe wrapper around c.Log.Debug.
func (c *Classifier) debug(msg string, args ...any) {
	if c.Log != nil {
		c.Log.Debug(msg, args...)
	}
}

// Classify runs SPEC.md "Classification logic" 8-step switch for one
// source event. May call c.Output zero or more times (most actions
// produce one outcome). Mutates c.Inventory in place after every write.
// Returns an error only on unrecoverable failure (gws errors, programmer
// bugs); SPEC-defined skip outcomes return nil error.
func (c *Classifier) Classify(ctx context.Context, source *gws.Event) error {
	// Step 1: already a mirror.
	if isMirror(source) {
		c.emit(Outcome{
			Action:        mirror.ActionSkip,
			Reason:        ReasonIsMirror,
			SourceEventID: source.ID,
			Summary:       source.Summary,
		})
		return nil
	}

	// Step 2: recurring instance - delegate to the recurring.Handler.
	if source.RecurringEventID != "" {
		return c.classifyRecurringInstance(ctx, source)
	}

	// Step 3: cancelled (non-recurring).
	if source.Status == gws.EventStatusCancelled {
		return c.deleteOrSkip(ctx, source, ReasonSourceCancelled, ReasonCancelled)
	}

	// Step 4: declined (source-owner attendee).
	switch mirror.SourceOwnerResponseStatus(source) {
	case gws.ResponseStatusDeclined:
		return c.deleteOrSkip(ctx, source, ReasonDeclined, ReasonDeclined)
	case gws.ResponseStatusTentative:
		// Step 5: tentative.
		return c.deleteOrSkip(ctx, source, ReasonTentative, ReasonTentative)
	}

	// Step 6: transparency=transparent.
	if source.Transparency == gws.TransparencyTransparent {
		return c.deleteOrSkip(ctx, source, ReasonTransparencyTransparent, ReasonTransparencyTransparent)
	}

	// Step 7: outside horizon.
	inHorizon, err := c.isInHorizon(ctx, source)
	if err != nil {
		return err
	}
	if !inHorizon {
		return c.deleteOrSkip(ctx, source, ReasonOutsideHorizon, ReasonOutsideHorizon)
	}

	// Step 8: normal reconciliation.
	return c.reconcileNormal(ctx, source)
}

// emit is the single call site through which a Classifier surfaces an
// Outcome. Centralizing it lets us populate the Pair/Direction fields
// uniformly without ceremony at every call site.
func (c *Classifier) emit(o Outcome) {
	if c.Output == nil {
		return
	}
	o.Pair = c.Pair
	o.Direction = c.Direction
	c.Output(o)
}

// isMirror returns whether the event carries the calendar-sync:source
// extended property - SPEC.md "Loop prevention" / step 1's bidirectional
// loop guard.
func isMirror(e *gws.Event) bool {
	if e == nil || e.ExtendedProperties == nil {
		return false
	}
	if e.ExtendedProperties.Private == nil {
		return false
	}
	_, ok := e.ExtendedProperties.Private[mirror.ExtKeySource]
	return ok
}

// classifyRecurringInstance dispatches step 2 to the recurring.Handler
// and folds the returned Result into an Outcome plus inventory updates.
//
// Per SPEC.md "In-memory state", the inventory is keyed by the source
// tuple at the EVENT level, not the parent level - so a recurring
// instance's mirror lives at (SourceCalendarID, source.ID), and the
// mirror parent lives at (SourceCalendarID, source.RecurringEventID).
// Both PostWriteMirrorParent and PostWriteMirrorInstance from the
// recurring Result feed back through Inventory.Set.
func (c *Classifier) classifyRecurringInstance(ctx context.Context, source *gws.Event) error {
	if c.Recurring == nil {
		return errors.New("sync: Classifier.Recurring is nil; cannot classify recurring instance")
	}
	res, err := c.Recurring.Handle(ctx, source)
	if err != nil {
		return err
	}

	// Apply inventory updates from the recurring handler. The handler's
	// own write paths (cancellation, patch, propagate, revert, migration)
	// all return PostWriteMirrorInstance; mirror-parent rewrites return
	// PostWriteMirrorParent. Both are folded back into our inventory.
	if res.PostWriteMirrorParent != nil {
		c.Inventory.Set(
			mirror.SourceTuple{CalendarID: c.SourceCalendarID, EventID: source.RecurringEventID},
			res.PostWriteMirrorParent,
		)
	}
	if res.PostWriteMirrorInstance != nil {
		c.Inventory.Set(
			mirror.SourceTuple{CalendarID: c.SourceCalendarID, EventID: source.ID},
			res.PostWriteMirrorInstance,
		)
	}

	out := Outcome{
		Action:        res.Action,
		Reason:        res.Reason,
		Conflict:      res.Conflict,
		Fields:        res.Fields,
		SourceEventID: source.ID,
		Summary:       source.Summary,
	}
	if res.PostWriteMirrorInstance != nil {
		out.TargetEventID = res.PostWriteMirrorInstance.ID
	}
	c.emit(out)
	return nil
}

// deleteOrSkip is the shared shape for steps 3-7 (the SPEC's "delete the
// mirror if one exists; otherwise skip" pattern). foundReason is used
// when a mirror is in inventory and we issue events.delete; missingReason
// is used when no mirror exists and we just skip.
//
// SPEC §"Cancelled (non-recurring)" uses different reason names for the
// two branches (source_cancelled vs cancelled); the rest of the steps use
// the same reason on both branches (declined / tentative / transparency_
// transparent / outside_horizon).
func (c *Classifier) deleteOrSkip(ctx context.Context, source *gws.Event, foundReason, missingReason mirror.Reason) error {
	tuple := mirror.SourceTuple{CalendarID: c.SourceCalendarID, EventID: source.ID}
	mirrorEvent, ok := c.Inventory.Lookup(tuple)
	if !ok {
		c.emit(Outcome{
			Action:        mirror.ActionSkip,
			Reason:        missingReason,
			SourceEventID: source.ID,
			Summary:       source.Summary,
		})
		return nil
	}

	if err := c.API.EventsDelete(ctx, c.TargetCalendarID, mirrorEvent.ID); err != nil {
		return fmt.Errorf("delete mirror %s/%s: %w", c.TargetCalendarID, mirrorEvent.ID, err)
	}
	c.Inventory.Delete(tuple)

	c.emit(Outcome{
		Action:        mirror.ActionDelete,
		Reason:        foundReason,
		SourceEventID: source.ID,
		TargetEventID: mirrorEvent.ID,
		Summary:       source.Summary,
	})
	return nil
}

// isInHorizon implements step 7's eligibility check. Non-recurring events
// compare their start to now+horizon; recurring parents (events with a
// non-empty Recurrence) call events.instances with a small bounded window
// and check whether any instance materialized.
//
// Horizon=0 (the zero-value default for an unconfigured Classifier) is
// treated as "no horizon" - everything is in. Production wiring fills in
// Settings.Horizon before constructing the Classifier.
func (c *Classifier) isInHorizon(ctx context.Context, source *gws.Event) (bool, error) {
	if c.Horizon == 0 {
		return true, nil
	}
	now := c.now()
	horizonEnd := now.Add(c.Horizon)

	if len(source.Recurrence) > 0 {
		// Recurring parent: ask Calendar API whether ANY instance falls in
		// [now, horizonEnd]. Empty response = outside horizon.
		params := gws.EventsInstancesParams{
			CalendarID:  c.SourceCalendarID,
			EventID:     source.ID,
			TimeMin:     now.Format(time.RFC3339),
			TimeMax:     horizonEnd.Format(time.RFC3339),
			MaxResults:  1,
			ShowDeleted: false,
		}
		instances, err := c.API.EventsInstances(ctx, params)
		if err != nil {
			return false, fmt.Errorf("horizon check for recurring parent %s: %w", source.ID, err)
		}
		return len(instances) > 0, nil
	}

	start, ok := parseEventStart(source.Start)
	if !ok {
		// Source has no parseable start - SPEC's "in horizon if start <=
		// now+horizon" can't be evaluated. Treat as in-horizon so the
		// source reaches step 8 (where validate-time issues will surface
		// against Calendar API). Failing closed here would mass-delete
		// mirrors of every event with a malformed start.
		return true, nil
	}
	return !start.After(horizonEnd), nil
}

func (c *Classifier) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// parseEventStart returns the source event's start time and whether it
// was parseable. Mirrors SPEC's "start = E.start.dateTime || E.start.date":
// dateTime preferred over date; an all-day event's date is treated as
// midnight UTC for the comparison. Empty/missing returns (zero, false).
func parseEventStart(d *gws.EventDateTime) (time.Time, bool) {
	if d == nil {
		return time.Time{}, false
	}
	if d.DateTime != "" {
		if t, err := time.Parse(time.RFC3339Nano, d.DateTime); err == nil {
			return t, true
		}
		if t, err := time.Parse(time.RFC3339, d.DateTime); err == nil {
			return t, true
		}
	}
	if d.Date != "" {
		if t, err := time.Parse("2006-01-02", d.Date); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}
