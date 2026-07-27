package sync

import (
	"context"
	"errors"
	"fmt"
	"regexp"

	"github.com/tammersaleh/calendar-sync/internal/config"
	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// instanceSuffixRE matches the trailing `_<UTC>` suffix Calendar API
// stamps onto recurring-instance IDs (e.g. `_20260520T160000Z`). Used by
// extractInstanceSuffix to derive the source-instance ID from a mirror
// instance whose calendar-sync:source carries the parent (inherited) form.
//
// SPEC.md "Recurring instance IDs" calls out this format. Anchored at the
// end so the suffix can never be confused with a calendar-sync deterministic
// prefix.
var instanceSuffixRE = regexp.MustCompile(`_\d{8}T\d{6}Z$`)

// reasonSourceOrphan is the skip reason emitted when a target-delta event
// references a non-recurring source that no longer exists (events.get
// returned 404). The orphan walk's existing prune pass cleans the mirror;
// target-delta's job is just to surface the observation in the JSONL
// stream so the action log isn't silently incomplete.
const reasonSourceOrphan mirror.Reason = "source_orphan"

// reasonInstanceNotInSeries is the skip reason for a recurring-instance
// mirror whose constructed source-instance ID 404s. Google answers 200 with
// a virtual occurrence for every slot the parent's RRULE does produce, so a
// 404 means the slot is not part of the series at all. Materializing an
// override for a non-occurrence would create an exception with nothing
// behind it, so this outcome is terminal and the event is consumed.
const reasonInstanceNotInSeries mirror.Reason = "instance_not_in_series"

// reasonTargetCancelled is the skip reason for a target-side deletion.
// Reverse cancellation is not implemented, so these are quarantined rather
// than classified - see processTargetDeltaEvent for why classifying them
// would resurrect the event the user just deleted.
const reasonTargetCancelled mirror.Reason = "target_cancelled"

// targetDeltaBatch is one target's staged delta read: the events the list
// returned plus the cursor to commit once every one of them is processed.
//
// Reads are staged separately from writes so the source-delta phase can
// refresh the source-exception catalog in between. The reverse-materialize
// decision depends on that catalog (B29), and the batch preflight needs the
// whole batch in hand before any of it is written.
type targetDeltaBatch struct {
	target    string
	events    []gws.Event
	nextToken string
}

// stageTargetDeltas runs the READ half of B17's per-target delta phase. For
// each writable-source target it lists the delta against the seeded
// targetSyncToken and returns the staged result. No writes happen here, and
// no token moves.
//
// A target that cannot be read at all (no cursor, no inventory, list error)
// is simply absent from the returned slice, which leaves its token where it
// was. A 410 is handled inline: the cursor is invalid, so it is cleared and
// NeedsFullResync is surfaced.
func (r *Reconciler) stageTargetDeltas(ctx context.Context, res *TickResult) []targetDeltaBatch {
	var batches []targetDeltaBatch
	for _, target := range r.uniqueWritableTargets() {
		token, ok := r.targetSyncTokens[target]
		if !ok || token == "" {
			// No usable cursor: cold start before the first FullSync, a
			// failed seed, a 410, or a delta that came back without a next
			// token. The next FullSync re-seeds (it only seeds targets that
			// are missing one).
			//
			// Warn on the transition, not every tick. B28 sat here silently
			// for months because this branch logged nothing above DEBUG
			// while two-way sync was completely off; a per-tick warning
			// would just be noise that gets filtered out again.
			if !r.targetDeltaDisabledWarned[target] {
				r.targetDeltaDisabledWarned[target] = true
				r.warn("sync.targetDelta: no target syncToken; two-way sync is OFF for this target",
					"target", target,
				)
			}
			continue
		}
		delete(r.targetDeltaDisabledWarned, target)

		// B17 missing-inventory guard: FullSync seeds tokens BEFORE
		// rebuilding inventories (so an edit landing in the seed-to-
		// inventory gap is visible to the next tick). If the inventory
		// rebuild then errored for this target, the seeded token is set
		// but the inventory is absent. Listing the delta now would either
		// be impossible to reconcile (no inventory to compare against) or
		// silently advance the token past unprocessed events. Skip the
		// whole phase for this target until a future FullSync rebuilds
		// the inventory.
		if _, ok := r.inventories[target]; !ok {
			r.debug("sync.targetDelta: no inventory; skipping target",
				"target", target,
			)
			continue
		}

		params := gws.EventsListParams{
			CalendarID:   target,
			SyncToken:    token,
			ShowDeleted:  true,
			SingleEvents: false,
			EventTypes:   sourceListEventTypes,
			MaxResults:   MaxResultsPerPage,
		}
		events, nextToken, err := r.API.EventsList(ctx, params)
		if err != nil {
			if errors.Is(err, gws.ErrAPIGone) {
				// 410 GONE: the seeded token is invalid. Clear it and
				// surface NeedsFullResync so the daemon schedules a
				// fast-track FullSync that re-seeds before the next tick.
				delete(r.targetSyncTokens, target)
				st := res.PerTarget[target]
				st.NeedsFullResync = true
				res.PerTarget[target] = st
				r.warn("sync.targetDelta: 410 GONE; cleared target token",
					"target", target,
				)
				continue
			}
			r.warn("sync.targetDelta: list failed",
				"target", target,
				"error", err.Error(),
			)
			continue
		}

		batches = append(batches, targetDeltaBatch{
			target:    target,
			events:    events,
			nextToken: nextToken,
		})
	}
	return batches
}

// applyTargetDeltas runs the WRITE half of the target-delta phase over the
// batches stageTargetDeltas produced, after the source-delta phase has
// refreshed the exception catalog.
//
// Token advancement is conditional: the seeded targetSyncToken advances only
// if every event in the batch is processed without a pinning error. Errors
// leave the token unchanged so the next tick re-delivers.
func (r *Reconciler) applyTargetDeltas(ctx context.Context, res *TickResult, batches []targetDeltaBatch) {
	for _, b := range batches {
		if source, blocked := r.blockingUnreadySource(b); blocked {
			// Batch preflight. Deliberate head-of-line blocking: writing the
			// safe prefix and pinning the token on a later event would
			// rewrite that prefix every 60 seconds for as long as the source
			// read stays broken.
			//
			// The target token is still valid, so NeedsFullResync stays
			// unset - reseeding would seed past the unconsumed edit and lose
			// it for good.
			r.warn("sync.targetDelta: source-exception catalog not ready; batch deferred",
				"target", b.target,
				"source", source,
				"events", len(b.events),
			)
			continue
		}

		hadErr := false
		for i := range b.events {
			ev := b.events[i]
			if err := r.processTargetDeltaEvent(ctx, b.target, &ev, &res.TargetDelta); err != nil {
				// B18 transient-read tolerance: a narrow set of
				// well-understood read flakes (events.get / events.instances
				// 5xx, 400, 404) get logged + skipped without pinning the
				// targetSyncToken. Without this carve-out a single flaky
				// read replays the same delta forever. Mirrors the
				// source-delta classify-loop's transient handling in
				// runClassifyLoop. See isTransientClassifyReadError for
				// the matrix.
				transient := isTransientClassifyReadError(err)
				r.warn("sync.targetDelta: process failed",
					"target", b.target,
					"target_event", ev.ID,
					"error", err.Error(),
					"transient", transient,
				)
				if !transient {
					hadErr = true
				}
			}
		}

		// Token advancement: conditional on all events processing without
		// error. If any event errored, leave the seeded token in place so
		// the next tick re-delivers the same delta. If Google omitted
		// nextToken on a long delta, clear the token so the next FullSync
		// re-seeds. Mirrors the source-delta conditional-advancement rule.
		if hadErr {
			continue
		}
		if b.nextToken == "" {
			delete(r.targetSyncTokens, b.target)
			st := res.PerTarget[b.target]
			st.NeedsFullResync = true
			res.PerTarget[b.target] = st
			continue
		}
		r.targetSyncTokens[b.target] = b.nextToken
		st := res.PerTarget[b.target]
		st.SyncTokenChanged = true
		res.PerTarget[b.target] = st
	}
}

// blockingUnreadySource reports the first source calendar in the batch whose
// exception catalog cannot currently answer a membership question.
//
// Only inherited-form recurring instances consult the catalog, so only they
// can block. A non-recurring mirror edit, a managed-form instance or a
// recurring parent reconciles fine against an Unknown catalog, and blocking
// those on an unrelated source read would trade a correctness fix for an
// availability regression.
func (r *Reconciler) blockingUnreadySource(b targetDeltaBatch) (string, bool) {
	for i := range b.events {
		ev := &b.events[i]
		if ev.RecurringEventID == "" {
			continue
		}
		if ev.Status == gws.EventStatusCancelled {
			// Quarantined before classification; never reaches the catalog.
			continue
		}
		tuple, ok := parseSourceFromMirror(ev)
		if !ok {
			continue
		}
		if instanceSuffixRE.MatchString(tuple.EventID) {
			// Managed form: the source instance ID is recorded on the mirror
			// itself, so there is no inherited-vs-user-edit ambiguity.
			continue
		}
		if _, ok := r.findOwningPDir(b.target, tuple.CalendarID); !ok {
			continue
		}
		if !r.sourceCatalogReady(tuple.CalendarID) {
			return tuple.CalendarID, true
		}
	}
	return "", false
}

// processTargetDeltaEvent dispatches one target-delta event through the
// reconcile path. Skips silently for non-mirror events and mirrors with
// no owning pdir. An inherited-form recurring instance whose source has no
// exception at the occurrence is routed to reverse materialization by the
// recurring handler's membership decision table, not by this function - see
// recurring.Handler.applyDriftMatrix.
//
// Routing rules (per Codex must-fix #4):
//
//   - The mirror's calendar-sync:source extended property identifies the
//     SOURCE tuple. The owning pdir is the SINGLE pdir whose source matches
//     that tuple's CalendarID AND whose target matches this target AND
//     whose effective writability gate is open.
//
//   - If the mirror is a recurring instance (RecurringEventID set), the
//     source-tuple's EventID may be in MANAGED form (already includes the
//     `_<UTC>` suffix) or INHERITED form (parent's id, no suffix). For
//     inherited form, the source-instance ID is the parent EventID with
//     the suffix parsed off the mirror's own ID.
func (r *Reconciler) processTargetDeltaEvent(
	ctx context.Context,
	target string,
	mirrorEvent *gws.Event,
	counts *Counts,
) error {
	// Skip non-mirror events (defensive; the seed-time filter is wide).
	tuple, ok := parseSourceFromMirror(mirrorEvent)
	if !ok {
		r.debug("sync.targetDelta: not a mirror; skipping",
			"target", target,
			"target_event", mirrorEvent.ID,
		)
		return nil
	}

	// Owning-pdir lookup. Multiple pdirs can share a target; routing must
	// pick the one whose source matches the mirror's source tuple.
	pd, ok := r.findOwningPDir(target, tuple.CalendarID)
	if !ok {
		// Stray mirror: the target carries a calendar-sync:source pointing
		// at a source that no enabled pdir mirrors here. Silent skip - this
		// is "left over from a since-disabled pdir" or a hand-crafted
		// extended property the user shouldn't see noise about.
		r.debug("sync.targetDelta: no owning pdir; skipping",
			"target", target,
			"target_event", mirrorEvent.ID,
			"source_tuple", tuple.String(),
		)
		return nil
	}

	// Target-cancellation quarantine. Target deltas list with
	// ShowDeleted=true, so a mirror the user deleted arrives here as
	// status=cancelled. Classifying it would reach a revive cell - B20's in
	// recurring/handler.go for an instance, the equivalent in
	// sync/reconcile.go for a non-recurring mirror - and recreate the event
	// the user just deleted. The reverse patch body carries no status, so
	// pushing the deletion to source instead is not possible yet either.
	//
	// Warn, skip, and CONSUME the event. Pinning the token here would let
	// the first deletion head-of-line block every later target edit until
	// reverse cancellation ships.
	if mirrorEvent.Status == gws.EventStatusCancelled {
		r.emitTargetDeltaSkip(counts, mirrorEvent, tuple.EventID, reasonTargetCancelled, pd)
		r.warn("sync.targetDelta: target-side deletion is not propagated to source",
			"target", target,
			"target_event", mirrorEvent.ID,
			"source_tuple", tuple.String(),
		)
		return nil
	}

	// Determine the source EventID to fetch. For non-recurring or recurring
	// parents, the tuple's EventID is the source ID directly. For recurring
	// instances, the tuple's EventID may be the parent (inherited form) or
	// the instance's own id (managed form).
	sourceEventID := tuple.EventID
	// inheritedInstance flags an inherited-form recurring-instance mirror:
	// the source-tuple's EventID equals the source PARENT's id (no `_<UTC>`
	// suffix) because Google copied the parent's extendedProperties to the
	// auto-materialized instance. Used below to skip the inventory refresh
	// that would otherwise shadow the real parent entry with the child
	// instance.
	inheritedInstance := false
	if mirrorEvent.RecurringEventID != "" {
		// Inherited form: the tuple's EventID is the source PARENT's id,
		// which equals the mirror parent's id only via the
		// IsInheritedRecurringInstance filter. The source-instance id is
		// the parent's id with the mirror's own `_<UTC>` suffix appended.
		// Managed form: the tuple's EventID already has the suffix; use as-is.
		if !instanceSuffixRE.MatchString(tuple.EventID) {
			inheritedInstance = true
			suffix, ok := extractInstanceSuffix(mirrorEvent.ID)
			if !ok {
				// The mirror instance lacks a parseable suffix. This is a
				// programmer-shape bug (Calendar API always stamps the
				// suffix on recurring-instance IDs); log and skip.
				r.warn("sync.targetDelta: recurring instance lacks suffix",
					"target", target,
					"target_event", mirrorEvent.ID,
				)
				return nil
			}
			sourceEventID = tuple.EventID + suffix
		}
	}

	// Fetch the source event. The Classifier will handle the standard
	// 8-step switch including drift detection -> propagate.
	sourceEvent, err := r.API.EventsGet(ctx, tuple.CalendarID, sourceEventID)
	if err != nil {
		if errors.Is(err, gws.ErrAPINotFound) {
			// 404 on the source events.get is terminal either way. Surface
			// the observation so the action log isn't silently incomplete,
			// then consume the event.
			//
			//   - Non-recurring source orphan: the source was deleted between
			//     the last FullSync and this delta. The orphan walk's prune
			//     pass cleans the mirror at the next FullSync.
			//
			//   - Recurring instance not in the series: Google answers 200
			//     with a virtual occurrence for every slot the parent's RRULE
			//     does produce, so a 404 means this slot is not part of the
			//     series at all. Materializing an override here would create
			//     an exception with no occurrence behind it. This is NOT the
			//     mirror-only-override case - that one 200s and is decided by
			//     the recurring handler's membership table (B29).
			reason := reasonSourceOrphan
			if mirrorEvent.RecurringEventID != "" {
				reason = reasonInstanceNotInSeries
			}
			r.emitTargetDeltaSkip(counts, mirrorEvent, sourceEventID, reason, pd)
			r.debug("sync.targetDelta: source 404; terminal skip",
				"target", target,
				"target_event", mirrorEvent.ID,
				"source_event", sourceEventID,
				"reason", string(reason),
			)
			return nil
		}
		return fmt.Errorf("target-delta events.get %s/%s: %w", tuple.CalendarID, sourceEventID, err)
	}
	if sourceEvent == nil {
		return nil
	}

	// Build a per-pdir Classifier and dispatch. This is the SAME path
	// source-delta uses, so the drift matrix's `!source_changed &&
	// mirror_drifted && source_writable -> propagate` cell fires naturally
	// once the inventory gets refreshed - but we don't refresh it here:
	// reading the inventory entry pre-target-edit means we're comparing
	// the user's new mirror state against the desired-from-source state,
	// which is exactly what propagate wants.
	//
	// stageTargetDeltas guarantees the per-target inventory is present
	// before dispatch (it skips a target whose r.inventories entry is
	// missing). The lookup here is just to grab the *Inventory pointer.
	inv := r.inventories[target]
	classifier, _ := r.buildTargetDeltaClassifier(pd, inv, counts)

	// Refresh the inventory entry with the live mirror state from the
	// delta. The pre-tick inventory was snapshotted at the last FullSync;
	// the target-delta event IS the live mirror with the user's edit
	// applied. Set the inventory to this live event so the Classifier's
	// drift detection compares against the user's new value.
	//
	// Exception: inherited-form recurring instances. The source-tuple's
	// EventID for an inherited instance is the source PARENT's id, so
	// inv.Set under that key would OVERWRITE the parent's inventory entry
	// with the child instance. The recurring handler's resolveMirrorParent
	// then looks up that same parent tuple and gets the shadowed child
	// back, after which locateMirrorInstance constructs the mirror instance
	// id from the child's id (not the parent's) - the same B16-class shadow
	// the inventory builder's pass-2 filter exists to prevent.
	//
	// Skipping the refresh here is safe: the recurring handler doesn't
	// consume the per-instance inventory entry during dispatch. It fetches
	// the mirror instance live via events.get and runs drift detection off
	// the result, then classify.classifyRecurringInstance writes the
	// post-write resource at
	// (SourceCalendarID, source.ID) - the proper per-instance key, not the
	// parent's tuple - after the handler returns.
	if !inheritedInstance {
		inv.Set(tuple, mirrorEvent)
	}

	if err := classifier.Classify(ctx, sourceEvent); err != nil {
		// Surface as a target-delta error so the caller pins the token.
		return fmt.Errorf("target-delta classify %s/%s: %w",
			tuple.CalendarID, sourceEvent.ID, err)
	}
	return nil
}

// emitTargetDeltaSkip emits a skip outcome for the target-delta paths that
// short-circuit before the Classifier: the two 404 shapes and the
// target-cancellation quarantine. Wired through the reconciler's Output sink
// so the observation appears in the JSONL stream alongside the source-delta
// outcomes.
func (r *Reconciler) emitTargetDeltaSkip(
	counts *Counts,
	mirrorEvent *gws.Event,
	sourceEventID string,
	reason mirror.Reason,
	pd config.PDir,
) {
	out := Outcome{
		Action:        mirror.ActionSkip,
		Reason:        reason,
		Pair:          pd.PairName,
		Direction:     pd.Direction,
		SourceEventID: sourceEventID,
		TargetEventID: mirrorEvent.ID,
		Summary:       mirrorEvent.Summary,
	}
	counts.observe(out)
	if r.Output != nil {
		r.Output(out)
	}
}

// findOwningPDir returns the (single) pdir that mirrors source-tuple
// `(sourceCalendar, *)` onto target. Match criteria:
//
//   - pd.TargetCalendar == target
//   - pd.SourceCalendar == sourceCalendar
//   - pd.SourceWritable && pd.PropagateTargetEdits (effective gate open)
//
// Returns the first matching pdir; per SPEC's pdir-collision rules a
// (target, source) tuple is unique, so "first" equals "only".
//
// Returns false when no pdir matches; the caller skips the delta event
// silently.
func (r *Reconciler) findOwningPDir(target, sourceCalendar string) (config.PDir, bool) {
	for _, pd := range r.Canonical.PDirs {
		if pd.TargetCalendar != target {
			continue
		}
		if pd.SourceCalendar != sourceCalendar {
			continue
		}
		if !pd.SourceWritable || !pd.PropagateTargetEdits {
			continue
		}
		return pd, true
	}
	return config.PDir{}, false
}

// extractInstanceSuffix returns the trailing `_<UTC>` suffix from a mirror
// instance ID. Calendar API stamps this suffix on every materialized
// recurring-instance ID (e.g. `cs2abc..._20260520T160000Z`); the suffix
// uniquely identifies the occurrence and can be appended to the source
// parent's ID to construct the source-instance ID.
//
// Returns ("", false) when the ID lacks a suffix - which would indicate
// either a parent event misrouted to this code path or a hand-crafted ID
// the daemon doesn't manage.
func extractInstanceSuffix(mirrorID string) (string, bool) {
	loc := instanceSuffixRE.FindStringIndex(mirrorID)
	if loc == nil {
		return "", false
	}
	return mirrorID[loc[0]:], true
}
