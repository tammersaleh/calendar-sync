// Package recurring implements SPEC.md §"The recurring-instance handler".
//
// The handler reconciles one source recurring-instance exception per call. It
// owns the mirror-parent-repair, mirror-instance-locate, and the
// instance-level four-way drift matrix; the sync layer (layer 6) drives it
// once per source event whose RecurringEventID is set.
//
// The package is deliberately independent of the sync layer: it accepts a
// LookupMirrorParent + ParentReconciler pair as injected callbacks rather
// than reaching into the sync layer's inventory map (which would create an
// import cycle). Construct one Handler per pdir.
package recurring

import (
	"context"
	"errors"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// API is the gws-subprocess subset the recurring handler consumes.
// Production code passes *gws.Client; tests provide a hand-rolled in-process
// stub. Keeping the interface this narrow keeps the unit tests independent
// of the fake-gws harness per CLAUDE.md "Testing".
type API interface {
	EventsGet(ctx context.Context, calendarID, eventID string) (*gws.Event, error)
	EventsInstances(ctx context.Context, params gws.EventsInstancesParams) ([]gws.Event, error)
	EventsPatch(ctx context.Context, calendarID, eventID string, body *gws.Event) (*gws.Event, error)
}

// MirrorParentLookup returns the mirror-parent Event for a source-tuple from
// the per-target inventory the sync layer maintains, or (nil, false) if no
// mirror parent is known. The handler calls this in step 1 before falling
// back to the ReconcileParent repair path.
type MirrorParentLookup func(source mirror.SourceTuple) (*gws.Event, bool)

// ParentReconciler reconciles a source parent through the sync layer's
// classification path. Returns the post-write mirror parent, or nil if the
// source parent is ineligible (cancelled / declined / tentative /
// transparent / outside_horizon) and no mirror should exist. Errors
// propagate up unchanged.
//
// The sync layer implements this over its classification logic; layer 5
// cannot import layer 6 directly without a cycle, so the dependency is
// inverted via this function value.
type ParentReconciler func(ctx context.Context, sourceParent *gws.Event) (*gws.Event, error)

// Reason values the recurring handler can emit. Strings match SPEC.md's
// stdout reasons table verbatim. Typed as mirror.Reason so the eventual
// stdout printer can switch on a single Reason vocabulary.
//
// The handler also surfaces the standard mirror-package reasons
// (ReasonUnchanged, ReasonSourceUpdated, ReasonTargetEdited) when the drift
// matrix fires; only the recurring-specific reasons live in this file.
const (
	ReasonParentNotEligible        mirror.Reason = "parent_not_eligible"
	ReasonInstanceUnmaterializable mirror.Reason = "instance_unmaterializable"
	ReasonSourceCancelled          mirror.Reason = "source_cancelled"
	ReasonDeclined                 mirror.Reason = "declined"
	ReasonTentative                mirror.Reason = "tentative"
	ReasonTransparencyTransparent  mirror.Reason = "transparency_transparent"
	ReasonMigrationUpgrade         mirror.Reason = "migration_upgrade"
	// ReasonInheritedUpgrade is the recurring-handler reason for an
	// auto-materialized instance whose live managed fields already match
	// desired-from-source: rewrite explicitly so the instance carries its
	// own per-instance calendar-sync:source / :checksum / :source_updated
	// (no longer shared with the parent). See the inherited-instance branch
	// of applyDriftMatrix.
	ReasonInheritedUpgrade mirror.Reason = "inherited_upgrade"
)

// Result is the outcome of one Handle call. The Action / Reason / Conflict
// values match SPEC.md's stdout schema; the eventual output layer emits
// them verbatim. PostWrite* fields let the sync layer update its inventory
// without re-fetching, and *Recurrence fields support the
// instance_unmaterializable warn log.
type Result struct {
	// Action is the user-facing action (mirror.ActionSkip / Patch / Delete /
	// Propagate / Revert).
	Action mirror.Action

	// Reason matches SPEC.md's reason column. May be a recurring-specific
	// constant from this package or a standard mirror-package reason.
	Reason mirror.Reason

	// Conflict is non-empty when the four-way matrix had to break a
	// source_changed && mirror_drifted tie. Empty otherwise.
	Conflict mirror.Conflict

	// Fields lists the drifted managed-field names (per SPEC.md "Drift
	// handling"), set on the propagate and revert paths. Empty for other
	// outcomes.
	Fields []string

	// PostWriteMirrorInstance is the post-checksum-patch mirror instance for
	// the patch / revert / propagate paths, or the post-cancel resource for
	// the delete paths. Nil for skip outcomes.
	PostWriteMirrorInstance *gws.Event

	// PostWriteMirrorParent is the post-checksum-patch mirror parent when
	// step 1 or step 2's repair path wrote a parent. Nil otherwise. The
	// sync layer uses this to refresh its inventory.
	PostWriteMirrorParent *gws.Event

	// SourceParentRecurrence is captured for the instance_unmaterializable
	// warn log per SPEC.md "Zero-result instance lookup". Empty for other
	// outcomes.
	SourceParentRecurrence []string

	// MirrorParentRecurrence is the mirror parent's recurrence array as
	// last seen, paired with SourceParentRecurrence in the warn log.
	MirrorParentRecurrence []string
}

// Logger is the slog-style interface the recurring handler consumes for
// per-step diagnostics. Re-declared here to avoid an import cycle (the
// handler can't import internal/output without dragging in printer types
// it doesn't need). Production code passes *output.Logger which satisfies
// this interface naturally; nil is valid (every log call short-circuits).
type Logger interface {
	Debug(msg string, args ...any)
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
	Error(msg string, args ...any)
}

// Handler reconciles one source recurring-instance exception per SPEC.md
// §"The recurring-instance handler". Construct one per pdir; it owns no
// mutable state, so the same Handler may be reused across many Handle
// calls.
type Handler struct {
	// API is the gws-subprocess subset the handler consumes (see API above).
	API API

	// SourceCalendarID is the canonical source calendar ID for the pdir
	// this Handler serves; passed straight into mirror.BuildPayload and
	// the API's EventsGet calls.
	SourceCalendarID string

	// TargetCalendarID is the canonical target calendar ID; passed to all
	// EventsInstances and mirror-side EventsPatch calls.
	TargetCalendarID string

	// SourceWritable mirrors pdir.source_writable from SPEC.md "Validation
	// rules" / "Access role". When true, drift on the mirror routes to
	// propagate; when false, to revert.
	SourceWritable bool

	// LookupMirrorParent is the per-target inventory query the sync layer
	// supplies (see MirrorParentLookup).
	LookupMirrorParent MirrorParentLookup

	// ReconcileParent is the recovery callback used when the inventory
	// lacks the source's parent (see ParentReconciler).
	ReconcileParent ParentReconciler

	// Log is the per-step diagnostic logger; nil silences output. Wired
	// from sync.Reconciler.Log via buildClassifier.
	Log Logger
}

// debug is a nil-safe wrapper around h.Log.Debug.
func (h *Handler) debug(msg string, args ...any) {
	if h.Log != nil {
		h.Log.Debug(msg, args...)
	}
}

// Handle reconciles one source recurring-instance exception. The caller has
// already verified that source.RecurringEventID is set and routed the event
// to this handler via the classification logic per SPEC.md "Classification
// logic" step 2.
//
// The flow follows SPEC.md §"The recurring-instance handler" exactly:
//
//  1. find or repair the mirror parent;
//  2. locate the mirror instance (zero-result repair path included);
//  3. apply the cancellation/declined/tentative/transparent rules in order,
//     then the four-way drift matrix.
//
// Errors are returned only for unrecoverable I/O failures (gws errors, the
// programmer-error nil-OriginalStartTime case, ReconcileParent failures).
// Filtering and skip outcomes return a populated Result with no error.
func (h *Handler) Handle(ctx context.Context, source *gws.Event) (Result, error) {
	h.debug("recurring.Handle entry",
		"source_event", source.ID,
		"recurring_event_id", source.RecurringEventID,
		"summary", source.Summary,
		"status", source.Status,
	)
	mirrorParent, postWriteMirrorParent, ok, err := h.resolveMirrorParent(ctx, source)
	if err != nil {
		return Result{}, err
	}
	if !ok {
		h.debug("recurring.Handle: parent not eligible -> skip",
			"source_event", source.ID,
			"recurring_event_id", source.RecurringEventID,
		)
		return Result{Action: mirror.ActionSkip, Reason: ReasonParentNotEligible}, nil
	}

	mirrorInstance, parentAfterRepair, status, err := h.locateMirrorInstance(ctx, source, mirrorParent)
	if err != nil {
		return Result{}, err
	}
	if parentAfterRepair != nil {
		// Step 2's repair path force-rewrote the mirror parent; surface that
		// to the sync layer alongside any step-1 write.
		mirrorParent = parentAfterRepair
		postWriteMirrorParent = parentAfterRepair
	}
	if status.unmaterializable {
		h.debug("recurring.Handle: instance unmaterializable -> skip",
			"source_event", source.ID,
			"recurring_event_id", source.RecurringEventID,
			"mirror_parent", mirrorParent.ID,
		)
		return Result{
			Action:                 mirror.ActionSkip,
			Reason:                 ReasonInstanceUnmaterializable,
			PostWriteMirrorParent:  postWriteMirrorParent,
			SourceParentRecurrence: status.sourceRecurrence,
			MirrorParentRecurrence: mirrorParent.Recurrence,
		}, nil
	}

	res, err := h.reconcileInstance(ctx, source, mirrorInstance)
	if err != nil {
		return Result{}, err
	}
	res.PostWriteMirrorParent = postWriteMirrorParent
	return res, nil
}

// resolveMirrorParent runs SPEC.md §"Step 1". Returns the mirror parent
// and (if step 1 wrote it) the post-write resource to propagate up to the
// sync layer's inventory. The third return value is false when the source
// parent is ineligible and the caller should emit skip(parent_not_eligible).
func (h *Handler) resolveMirrorParent(ctx context.Context, source *gws.Event) (parent, postWrite *gws.Event, ok bool, err error) {
	tuple := mirror.SourceTuple{CalendarID: h.SourceCalendarID, EventID: source.RecurringEventID}
	if mp, found := h.LookupMirrorParent(tuple); found {
		h.debug("recurring.resolveMirrorParent: inventory hit",
			"source_event", source.ID,
			"recurring_event_id", source.RecurringEventID,
			"mirror_parent", mp.ID,
		)
		return mp, nil, true, nil
	}

	h.debug("recurring.resolveMirrorParent: inventory miss -> repair",
		"source_event", source.ID,
		"recurring_event_id", source.RecurringEventID,
	)
	sourceParent, err := h.API.EventsGet(ctx, h.SourceCalendarID, source.RecurringEventID)
	if err != nil {
		return nil, nil, false, err
	}

	mp, err := h.ReconcileParent(ctx, sourceParent)
	if err != nil {
		return nil, nil, false, err
	}
	if mp == nil {
		h.debug("recurring.resolveMirrorParent: parent not eligible (ReconcileParent returned nil)",
			"source_event", source.ID,
			"recurring_event_id", source.RecurringEventID,
		)
		return nil, nil, false, nil
	}
	h.debug("recurring.resolveMirrorParent: parent reconciled",
		"source_event", source.ID,
		"recurring_event_id", source.RecurringEventID,
		"mirror_parent", mp.ID,
	)
	return mp, mp, true, nil
}

// instanceLocateStatus captures the locate-and-repair outcome enough to
// drive Handle's branching without a multi-bool return. unmaterializable
// implies "skip(instance_unmaterializable)" with the populated source
// recurrence array attached to the warn log.
type instanceLocateStatus struct {
	unmaterializable bool
	sourceRecurrence []string
}

// locateMirrorInstance runs SPEC.md §"Step 2". Returns the located mirror
// instance, an updated mirror parent (when the repair path force-rewrote
// it), and a status carrying the unmaterializable signal. Callers should
// use the parentAfterRepair value if non-nil; otherwise the original parent
// passed in is still authoritative.
func (h *Handler) locateMirrorInstance(ctx context.Context, source, mirrorParent *gws.Event) (instance, parentAfterRepair *gws.Event, status instanceLocateStatus, err error) {
	originalStart, err := computeOriginalStart(source.OriginalStartTime)
	if err != nil {
		return nil, nil, instanceLocateStatus{}, err
	}

	first, err := h.API.EventsInstances(ctx, gws.EventsInstancesParams{
		CalendarID:    h.TargetCalendarID,
		EventID:       mirrorParent.ID,
		OriginalStart: originalStart,
		MaxResults:    1,
		ShowDeleted:   true,
	})
	if err != nil {
		return nil, nil, instanceLocateStatus{}, err
	}
	h.debug("recurring.locateMirrorInstance: first try",
		"source_event", source.ID,
		"mirror_parent", mirrorParent.ID,
		"original_start", originalStart,
		"items", len(first),
	)
	if len(first) > 0 {
		inst := first[0]
		return &inst, nil, instanceLocateStatus{}, nil
	}

	// Repair path: re-fetch the source parent, force-rewrite the mirror
	// parent, retry the lookup once. Per SPEC.md "Zero-result instance
	// lookup".
	h.debug("recurring.locateMirrorInstance: repair path",
		"source_event", source.ID,
		"mirror_parent", mirrorParent.ID,
	)
	sourceParent, err := h.API.EventsGet(ctx, h.SourceCalendarID, source.RecurringEventID)
	if err != nil {
		return nil, nil, instanceLocateStatus{}, err
	}

	repaired, err := h.forceRewriteMirrorParent(ctx, sourceParent, mirrorParent.ID)
	if err != nil {
		return nil, nil, instanceLocateStatus{}, err
	}

	second, err := h.API.EventsInstances(ctx, gws.EventsInstancesParams{
		CalendarID:    h.TargetCalendarID,
		EventID:       repaired.ID,
		OriginalStart: originalStart,
		MaxResults:    1,
		ShowDeleted:   true,
	})
	if err != nil {
		return nil, repaired, instanceLocateStatus{}, err
	}
	h.debug("recurring.locateMirrorInstance: repair retry",
		"source_event", source.ID,
		"repaired_parent", repaired.ID,
		"items", len(second),
	)
	if len(second) == 0 {
		return nil, repaired, instanceLocateStatus{
			unmaterializable: true,
			sourceRecurrence: sourceParent.Recurrence,
		}, nil
	}
	inst := second[0]
	return &inst, repaired, instanceLocateStatus{}, nil
}

// forceRewriteMirrorParent rebuilds the mirror parent payload from the
// source and patches it. The id field on the body is cleared because
// events.patch carries the event ID in the request URL, not the body.
// Returns the post-checksum mirror-parent resource.
func (h *Handler) forceRewriteMirrorParent(ctx context.Context, sourceParent *gws.Event, mirrorParentID string) (*gws.Event, error) {
	payload := mirror.BuildPayload(h.SourceCalendarID, sourceParent)
	payload.ID = ""
	return h.patchMirrorWithChecksum(ctx, h.TargetCalendarID, mirrorParentID, payload)
}

// reconcileInstance runs SPEC.md §"Step 3". Applies the
// cancelled/declined/tentative/transparent guards in order, then falls
// through to the four-way drift matrix.
func (h *Handler) reconcileInstance(ctx context.Context, source, mirrorInstance *gws.Event) (Result, error) {
	if source.Status == gws.EventStatusCancelled {
		return h.cancelMirrorInstance(ctx, mirrorInstance, ReasonSourceCancelled)
	}
	switch mirror.SourceOwnerResponseStatus(source) {
	case gws.ResponseStatusDeclined:
		return h.cancelMirrorInstance(ctx, mirrorInstance, ReasonDeclined)
	case gws.ResponseStatusTentative:
		return h.cancelMirrorInstance(ctx, mirrorInstance, ReasonTentative)
	}
	if source.Transparency == gws.TransparencyTransparent {
		return h.cancelMirrorInstance(ctx, mirrorInstance, ReasonTransparencyTransparent)
	}

	return h.applyDriftMatrix(ctx, source, mirrorInstance)
}

// cancelMirrorInstance is the shared shape for source_cancelled / declined /
// tentative / transparent: if the mirror is already cancelled it's a no-op,
// otherwise events.patch with status=cancelled. No checksum follow-up - the
// cancellation does not write a managed field, so the existing checksum
// remains accurate.
func (h *Handler) cancelMirrorInstance(ctx context.Context, mirrorInstance *gws.Event, reason mirror.Reason) (Result, error) {
	if mirrorInstance.Status == gws.EventStatusCancelled {
		return Result{Action: mirror.ActionSkip, Reason: mirror.ReasonUnchanged}, nil
	}
	body := &gws.Event{Status: gws.EventStatusCancelled}
	post, err := h.API.EventsPatch(ctx, h.TargetCalendarID, mirrorInstance.ID, body)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Action:                  mirror.ActionDelete,
		Reason:                  reason,
		PostWriteMirrorInstance: post,
	}, nil
}

// applyDriftMatrix applies SPEC.md "Drift detection model" / "Schema
// version migration" four-way matrix to the located mirror instance.
// Legacy mirrors (any version != current SchemaVersion) get the live-vs-
// desired drift recomputation per SPEC's migration rules.
//
// Three cells of the migration matrix diverge from the standard matrix
// and are handled here directly rather than by mirror.Classify:
//
//   - !source_changed && !mirror_drifted: SPEC routes this to
//     patch+migration_upgrade (rewrite at the current SchemaVersion with
//     a fresh checksum, picking up any new managed fields). The standard
//     matrix would skip(unchanged).
//   - mirror_drifted (with or without source_changed): SPEC says source-
//     wins-by-default during migration. We can't safely propagate during
//     migration because the "drift" may be schema-induced (e.g. v3's
//     Location field that didn't exist in v2 mirrors) rather than a real
//     user edit, and we have no per-field schema-version metadata to
//     tell the two apart. So source always wins on any drift; conflict=
//     migration_source_won surfaces this to the user. v1 mirrors lack a
//     reliable user-edit timestamp; v2 mirrors keep the same simpler
//     behavior so the migration cell stays consistent across legacy
//     versions.
//
// Only the source-only cell (!mirror_drifted && source_changed) falls
// through to mirror.Classify, where it correctly maps to ActionPatch /
// ReasonSourceUpdated.
func (h *Handler) applyDriftMatrix(ctx context.Context, source, mirrorInstance *gws.Event) (Result, error) {
	signal := mirror.ComputeDriftSignal(source, mirrorInstance)
	desired := mirror.BuildInstancePayload(h.SourceCalendarID, source)
	// An auto-materialized recurring-instance mirror inherits the parent's
	// extended properties, so its calendar-sync:source points at the source
	// PARENT (no instance suffix). Detection lets us treat such instances as
	// bootstrap state - never a peer for conflict resolution - even when the
	// stored schema version equals SchemaVersion. Without this, the standard
	// matrix's newer-wins tiebreak picks the freshly-materialized mirror over
	// a pre-existing source override and propagates the mirror's stale fields
	// back to the source, clobbering the user's reschedule.
	isInherited := mirror.IsInheritedRecurringInstance(mirrorInstance, source.RecurringEventID)
	h.debug("recurring.applyDriftMatrix",
		"source_event", source.ID,
		"mirror_instance", mirrorInstance.ID,
		"source_changed", signal.SourceChanged,
		"mirror_drifted", signal.MirrorDrifted,
		"needs_migration", signal.NeedsMigration,
		"is_inherited", isInherited,
	)
	if signal.NeedsMigration || isInherited {
		// Both the schema-version-migration path and the inherited-instance
		// path treat the mirror as bootstrap state and source-win on any
		// drift. They share the same recompute and write shape; only the
		// reason / conflict labels differ. Migration takes precedence when
		// both conditions hold (the schema bump is the more specific story).
		signal.MirrorDrifted = len(mirror.DriftedFieldNames(mirrorInstance, desired)) > 0

		upgradeReason := ReasonMigrationUpgrade
		sourceWonConflict := mirror.ConflictMigrationSourceWon
		if isInherited && !signal.NeedsMigration {
			upgradeReason = ReasonInheritedUpgrade
			sourceWonConflict = mirror.ConflictInheritedSourceWon
		}

		h.debug("recurring.applyDriftMatrix: bootstrap recompute",
			"source_event", source.ID,
			"source_changed", signal.SourceChanged,
			"mirror_drifted_after_recompute", signal.MirrorDrifted,
			"upgrade_reason", string(upgradeReason),
		)

		switch {
		case !signal.SourceChanged && !signal.MirrorDrifted:
			// Bootstrap upgrade: rewrite explicitly so the instance carries
			// per-instance metadata (calendar-sync:source matches the
			// instance ID, fresh checksum). Future drift detection compares
			// instance-vs-instance instead of inheriting the parent's value.
			desired.ID = ""
			post, err := h.patchMirrorWithChecksum(ctx, h.TargetCalendarID, mirrorInstance.ID, desired)
			if err != nil {
				return Result{}, err
			}
			return Result{
				Action:                  mirror.ActionPatch,
				Reason:                  upgradeReason,
				PostWriteMirrorInstance: post,
			}, nil
		case signal.MirrorDrifted:
			// Any mirror drift while in bootstrap state is source-wins.
			// During migration the drift may be schema-induced (e.g. v2
			// instances lack v3's Location field); for inherited instances
			// the drift may simply reflect the source's per-instance
			// override that the parent's RRULE projection didn't carry.
			// Either way the source is authoritative and we must not
			// propagate the mirror's value back.
			desired.ID = ""
			post, err := h.patchMirrorWithChecksum(ctx, h.TargetCalendarID, mirrorInstance.ID, desired)
			if err != nil {
				return Result{}, err
			}
			return Result{
				Action:                  mirror.ActionPatch,
				Reason:                  mirror.ReasonSourceUpdated,
				Conflict:                sourceWonConflict,
				PostWriteMirrorInstance: post,
			}, nil
		}
		// Source-only (!mirror_drifted && source_changed): falls through
		// to Classify, which routes to ActionPatch / ReasonSourceUpdated.
	}

	outcome := mirror.Classify(signal, h.SourceWritable, source.Updated, mirrorInstance.Updated)

	switch outcome.Action {
	case mirror.ActionSkip:
		return Result{Action: mirror.ActionSkip, Reason: outcome.Reason, Conflict: outcome.Conflict}, nil

	case mirror.ActionPatch:
		desired.ID = ""
		post, err := h.patchMirrorWithChecksum(ctx, h.TargetCalendarID, mirrorInstance.ID, desired)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Action:                  outcome.Action,
			Reason:                  outcome.Reason,
			Conflict:                outcome.Conflict,
			PostWriteMirrorInstance: post,
		}, nil

	case mirror.ActionPropagate:
		fields := mirror.DriftedFieldNames(mirrorInstance, desired)
		propagateBody := mirror.BuildPropagatePatchBody(mirrorInstance, fields)
		patchedSource, err := h.API.EventsPatch(ctx, h.SourceCalendarID, source.ID, propagateBody)
		if err != nil {
			return Result{}, err
		}
		// Re-write the mirror instance using the source's NEW state.
		desiredFromPatched := mirror.BuildInstancePayload(h.SourceCalendarID, patchedSource)
		desiredFromPatched.ID = ""
		post, err := h.patchMirrorWithChecksum(ctx, h.TargetCalendarID, mirrorInstance.ID, desiredFromPatched)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Action:                  outcome.Action,
			Reason:                  outcome.Reason,
			Conflict:                outcome.Conflict,
			Fields:                  fields,
			PostWriteMirrorInstance: post,
		}, nil

	case mirror.ActionRevert:
		fields := mirror.DriftedFieldNames(mirrorInstance, desired)
		desired.ID = ""
		post, err := h.patchMirrorWithChecksum(ctx, h.TargetCalendarID, mirrorInstance.ID, desired)
		if err != nil {
			return Result{}, err
		}
		return Result{
			Action:                  outcome.Action,
			Reason:                  outcome.Reason,
			Conflict:                outcome.Conflict,
			Fields:                  fields,
			PostWriteMirrorInstance: post,
		}, nil
	}

	// Unreachable: mirror.Classify produces only the four actions above.
	return Result{}, errors.New("recurring: unexpected outcome action " + string(outcome.Action))
}

// patchMirrorWithChecksum runs SPEC.md "Computing the checksum from the
// post-write event": a main events.patch followed by a follow-up patch
// that stores calendar-sync:checksum computed from the post-write resource.
// Returns the post-checksum response.
//
// Both patches target the same (calendarID, eventID); only the body
// differs. The follow-up body carries only the checksum extended property,
// not any managed field.
func (h *Handler) patchMirrorWithChecksum(ctx context.Context, calendarID, eventID string, body *gws.Event) (*gws.Event, error) {
	post, err := h.API.EventsPatch(ctx, calendarID, eventID, body)
	if err != nil {
		return nil, err
	}
	checksum := mirror.Checksum(mirror.ManagedFieldsFromEvent(post))
	follow := &gws.Event{
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{mirror.ExtKeyChecksum: checksum},
		},
	}
	return h.API.EventsPatch(ctx, calendarID, eventID, follow)
}
