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

// reasonMirrorOnlyOverride is the propagate reason emitted when a target-
// delta event references a recurring instance the source has no override
// for. B17 Phase 2 (live as of v2.5.0) materializes the user's edit by
// patching the constructed source-instance ID, creating the source override.
// The reason name is preserved for stream continuity with the v2.4.0 Phase 1
// shape (`skip(mirror_only_override)`); only the action changed.
const reasonMirrorOnlyOverride mirror.Reason = "mirror_only_override"

// reasonSourceOrphan is the skip reason emitted when a target-delta event
// references a non-recurring source that no longer exists (events.get
// returned 404). The orphan walk's existing prune pass cleans the mirror;
// target-delta's job is just to surface the observation in the JSONL
// stream so the action log isn't silently incomplete.
const reasonSourceOrphan mirror.Reason = "source_orphan"

// runTargetDeltaPhase runs B17's per-target delta reconciliation. For each
// writable-source target, list the events.list delta against the seeded
// targetSyncToken; for each delta event that is a calendar-sync mirror,
// look up the owning pdir and dispatch the corresponding source event
// through the standard Classifier. The classifier's drift-detection then
// produces the propagate write back to source.
//
// This phase MUST run before the source-delta classify (per the design
// "Phase ordering" must-fix). If reversed, source-driven mirror rewrites
// can clobber target edits before target-delta lists them.
//
// Token advancement is conditional: the seeded targetSyncToken advances
// only if processing every event in the delta succeeds. Errors leave the
// token unchanged so the next tick re-delivers.
func (r *Reconciler) runTargetDeltaPhase(ctx context.Context, res *TickResult) {
	for _, target := range r.uniqueWritableTargets() {
		token, ok := r.targetSyncTokens[target]
		if !ok || token == "" {
			// No seed yet (cold start, or the previous seed/410 cleared
			// it). Skip silently; the next FullSync re-seeds.
			continue
		}

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

		hadErr := false
		for i := range events {
			ev := events[i]
			if err := r.processTargetDeltaEvent(ctx, target, &ev, &res.TargetDelta); err != nil {
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
					"target", target,
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
		if nextToken == "" {
			delete(r.targetSyncTokens, target)
			st := res.PerTarget[target]
			st.NeedsFullResync = true
			res.PerTarget[target] = st
			continue
		}
		r.targetSyncTokens[target] = nextToken
		st := res.PerTarget[target]
		st.SyncTokenChanged = true
		res.PerTarget[target] = st
	}
}

// processTargetDeltaEvent dispatches one target-delta event through the
// reconcile path. Skips silently for non-mirror events and mirrors with
// no owning pdir. For inherited-form recurring instances whose source
// has no override at the occurrence (404 on events.get), Phase 2
// materializes the source override via materializeSourceOverride
// instead of skipping.
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
			// 404 on the source events.get splits two ways:
			//
			//   - Non-recurring source orphan: the source was deleted between
			//     the last FullSync and this delta. The orphan-walk's prune
			//     pass cleans the mirror at the next FullSync; target-delta's
			//     job is just to surface the observation in the JSONL stream
			//     so the action log isn't silently incomplete.
			//
			//   - Recurring-instance mirror-only override (B17 Phase 2): the
			//     user edited a single occurrence on the mirror that has no
			//     source counterpart at that occurrence. We materialize the
			//     override on source by patching the constructed source-
			//     instance ID with the mirror's managed fields, then rewrite
			//     the mirror from the post-patch source state. See
			//     materializeSourceOverride for the per-step shape and the
			//     B16 guardrail (recurrence MUST NOT appear in the patch
			//     body).
			if mirrorEvent.RecurringEventID != "" {
				return r.materializeSourceOverride(ctx, pd, mirrorEvent, tuple, sourceEventID, counts)
			}
			r.emitTargetDeltaSkip(counts, mirrorEvent, sourceEventID, reasonSourceOrphan, pd)
			r.debug("sync.targetDelta: source 404; non-recurring orphan skip",
				"target", target,
				"target_event", mirrorEvent.ID,
				"source_event", sourceEventID,
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
	// runTargetDeltaPhase guarantees the per-target inventory is present
	// before dispatch (the loop bails out if r.inventories[target] is
	// missing). The lookup here is just to grab the *Inventory pointer.
	inv := r.inventories[target]
	classifier, _ := r.buildClassifier(pd, inv, counts)

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

// emitTargetDeltaSkip emits a skip outcome for the non-recurring source-
// orphan 404 path. Wired through the reconciler's Output sink so the
// observation appears in the JSONL stream alongside the source-delta
// outcomes. The recurring-instance mirror-only-override case used to
// share this helper under Phase 1; Phase 2 routes it through
// materializeSourceOverride instead, emitting a propagate via the
// Classifier-scoped output sink.
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

// materializeSourceOverride is B17 Phase 2's promotion of the recurring-
// instance mirror-only-override case from skip to propagate. The user edited
// an occurrence on the mirror that has no source counterpart (events.get on
// the constructed source-instance ID returned 404); this method creates the
// source override and rewrites the mirror to reflect the post-write source
// state.
//
// The flow mirrors the source-side propagate path in sync/drift.go's
// doPropagate, with two structural differences:
//
//   - The patch body is built via mirror.BuildSourceOverridePatchBody (full
//     managed fields, recurrence omitted by construction) rather than
//     mirror.BuildPropagatePatchBody (drifted-fields subset). The source
//     instance doesn't exist yet, so we materialize the user's full state
//     to source rather than computing a drift diff.
//
//   - The outcome reason is reasonMirrorOnlyOverride rather than
//     mirror.ReasonTargetEdited, so the JSONL stream stays self-describing
//     about the bootstrap-from-mirror-only nature of this case.
//
// The B16 guardrail is structural: BuildSourceOverridePatchBody NEVER emits
// recurrence regardless of the mirror's live recurrence value, so the
// per-instance patch can't be reinterpreted by Google as a parent-level
// update.
//
// Errors propagate up to runTargetDeltaPhase, which keeps the targetSyncToken
// pinned so the next tick re-delivers the user's edit.
func (r *Reconciler) materializeSourceOverride(
	ctx context.Context,
	pd config.PDir,
	mirrorEvent *gws.Event,
	tuple mirror.SourceTuple,
	sourceInstanceID string,
	counts *Counts,
) error {
	inv := r.inventories[pd.TargetCalendar]
	classifier, _ := r.buildClassifier(pd, inv, counts)

	// 1. Patch the source instance to materialize the override. The body
	//    omits recurrence by construction (B16 guardrail).
	patchedSource, err := r.API.EventsPatch(ctx, tuple.CalendarID, sourceInstanceID,
		mirror.BuildSourceOverridePatchBody(mirrorEvent))
	if err != nil {
		return fmt.Errorf("target-delta source-override patch %s/%s: %w",
			tuple.CalendarID, sourceInstanceID, err)
	}

	// 2. Rewrite the mirror from the post-patch source state, refreshing
	//    calendar-sync:source_updated and (via the checksum follow-up)
	//    calendar-sync:checksum so subsequent ticks classify as unchanged.
	//    Use BuildInstancePayloadWithTimeZone (NOT BuildPayload): this is
	//    always an instance, so recurrence on the rewritten mirror is nil.
	rewritten := mirror.BuildInstancePayloadWithTimeZone(tuple.CalendarID, patchedSource, pd.TimeZone)
	post, err := classifier.patchMirrorWithChecksum(ctx, pd.TargetCalendar, mirrorEvent.ID,
		mirror.BuildPatchPayload(rewritten))
	if err != nil {
		return fmt.Errorf("target-delta source-override mirror rewrite %s/%s: %w",
			pd.TargetCalendar, mirrorEvent.ID, err)
	}

	// 3. Update inventory at the per-instance source tuple. The override
	//    now exists at (tuple.CalendarID, sourceInstanceID); the mirror's
	//    calendar-sync:source updates to that managed-form tuple as part of
	//    the BuildInstancePayload rewrite, so subsequent ticks treat the
	//    instance as managed-form rather than inherited.
	inv.Set(mirror.SourceTuple{CalendarID: tuple.CalendarID, EventID: sourceInstanceID}, post)

	// 4. Emit the propagate outcome through the same wrapped sink the
	//    Classifier uses, so Counts and Output get wired identically.
	classifier.emit(Outcome{
		Action:        mirror.ActionPropagate,
		Reason:        reasonMirrorOnlyOverride,
		SourceEventID: sourceInstanceID,
		TargetEventID: post.ID,
		Summary:       mirrorEvent.Summary,
	})
	r.debug("sync.targetDelta: materialized source override",
		"target", pd.TargetCalendar,
		"target_event", post.ID,
		"source_event", sourceInstanceID,
	)
	return nil
}
