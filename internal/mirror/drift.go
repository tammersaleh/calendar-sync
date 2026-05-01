package mirror

import (
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// Action is the operation reconciliation will perform on the mirror (or
// in the propagate case, on the source). String values match SPEC.md's
// stdout-action enum exactly so the output layer can emit them verbatim.
type Action string

// User-facing action names from SPEC.md "calendar-sync run" / actions table.
// The full list (insert/patch/delete/propagate/revert/skip) is what we
// model here; the recurring-instance handler maps some of these onto
// different API primitives but reports the user-facing name.
const (
	ActionSkip      Action = "skip"
	ActionInsert    Action = "insert"
	ActionPatch     Action = "patch"
	ActionDelete    Action = "delete"
	ActionPropagate Action = "propagate"
	ActionRevert    Action = "revert"
)

// Reason is the SPEC.md reason code for one outcome. Filtering reasons
// (cancelled, declined, transparency_transparent, etc.) belong to the
// sync layer's classification logic; the four below are what the drift
// matrix can produce.
type Reason string

const (
	ReasonUnchanged     Reason = "unchanged"
	ReasonSourceUpdated Reason = "source_updated"
	ReasonTargetEdited  Reason = "target_edited"
)

// Conflict is the warning category SPEC.md emits on stderr when both drift
// signals fire at the same reconciliation. Empty string for the no-conflict
// path. The strings match SPEC's "Conflict logging" `msg` values exactly.
type Conflict string

const (
	ConflictNone               Conflict = ""
	ConflictSourceWon          Conflict = "conflict_source_won"
	ConflictTargetWon          Conflict = "conflict_target_won"
	ConflictMigrationSourceWon Conflict = "migration_source_won"
)

// Outcome is what the drift-handling matrix decides for one (source, mirror)
// pair. Conflict is empty unless both drift signals fired and we had to
// pick a winner; the caller logs the warning when set.
type Outcome struct {
	Action   Action
	Reason   Reason
	Conflict Conflict
}

// DriftSignal carries what the drift-handling decision needs from the
// (source, mirror) pair, per SPEC.md "Drift detection model".
//
// When NeedsMigration is true the mirror is pre-v2 (no
// calendar-sync:checksum stored). The MirrorDrifted field is undefined in
// that case - the SPEC's "Schema version migration" path runs a different
// drift check (compare live managed fields to desired-from-source) which
// the sync layer performs itself. Callers MUST branch on NeedsMigration
// before consuming MirrorDrifted.
type DriftSignal struct {
	SourceChanged  bool
	MirrorDrifted  bool
	NeedsMigration bool
}

// ComputeDriftSignal compares a source event to the mirror Event resource
// and produces the signals the drift matrix needs.
//
//   - source_changed = source.updated > mirror's stored source_updated.
//     The "stored" value is mirror.extendedProperties.private[calendar-sync:source_updated],
//     written at the time of the last reconciliation. Comparison is via
//     parsed timestamps so RFC 3339 nanosecond precision differences don't
//     cause spurious mismatches.
//   - mirror_drifted = sha256(canonical(mirror's managed fields)) !=
//     mirror's stored checksum.
//   - needs_migration = mirror's stored calendar-sync:version != current
//     SchemaVersion. The sync layer routes these to the migration path
//     per SPEC.md "Schema version migration" rather than the standard
//     four-way matrix.
func ComputeDriftSignal(source, mirror *gws.Event) DriftSignal {
	storedSourceUpdated := extPropPrivate(mirror, ExtKeySourceUpdated)
	storedChecksum := extPropPrivate(mirror, ExtKeyChecksum)
	storedVersion := extPropPrivate(mirror, ExtKeyVersion)

	sourceChanged := compareTimestamps(source.Updated, storedSourceUpdated) > 0
	mirrorDrifted := Checksum(ManagedFieldsFromEvent(mirror)) != storedChecksum

	return DriftSignal{
		SourceChanged:  sourceChanged,
		MirrorDrifted:  mirrorDrifted,
		NeedsMigration: storedVersion != SchemaVersion,
	}
}

// extPropPrivate looks up a key in mirror.extendedProperties.private,
// returning empty string for any nil/missing path. Used when mirror is a
// gws.Event that may have a partial extended-properties subdocument.
func extPropPrivate(e *gws.Event, key string) string {
	if e == nil || e.ExtendedProperties == nil || e.ExtendedProperties.Private == nil {
		return ""
	}
	return e.ExtendedProperties.Private[key]
}

// compareTimestamps returns -1, 0, or 1 like cmp.Compare for two RFC 3339
// strings. Comparison rules, in order:
//
//  1. Both parse: numeric compare on the parsed time.Time values.
//  2. Exactly one parses: the parseable side sorts LATER. A real
//     timestamp is "newer" than a missing or corrupted one - that's the
//     useful answer for SourceChanged calculation when, say, a mirror
//     has been hand-edited and its calendar-sync:source_updated value
//     is no longer parseable.
//  3. Neither parses: lexicographic byte compare. Deterministic but
//     meaningless; in practice this branch is unreachable because
//     calendar-sync writes RFC 3339 itself.
func compareTimestamps(a, b string) int {
	ta, aok := parseTimestamp(a)
	tb, bok := parseTimestamp(b)
	switch {
	case aok && bok:
		switch {
		case ta.Before(tb):
			return -1
		case ta.After(tb):
			return 1
		default:
			return 0
		}
	case aok && !bok:
		return 1
	case !aok && bok:
		return -1
	}
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

func parseTimestamp(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// Classify implements SPEC.md "Drift detection model" four-way matrix.
// The conflict tiebreak is newer-wins by source.updated vs mirror.updated;
// equal-or-source-wins. sourceUpdated and mirrorUpdated are the live
// timestamps from the two events (NOT the stored calendar-sync:source_updated
// value used to compute SourceChanged).
//
//	!source_changed && !mirror_drifted -> skip(unchanged)
//	source_changed && !mirror_drifted  -> patch(source_updated)
//	!source_changed && mirror_drifted  -> propagate or revert (target_edited)
//	source_changed && mirror_drifted   -> conflict; newer wins (source ties)
//
// sourceWritable=true routes the drift-only branch to propagate; false
// routes to revert. SPEC's pdir.source_writable flag drives this.
func Classify(signal DriftSignal, sourceWritable bool, sourceUpdated, mirrorUpdated string) Outcome {
	switch {
	case !signal.SourceChanged && !signal.MirrorDrifted:
		return Outcome{Action: ActionSkip, Reason: ReasonUnchanged}

	case signal.SourceChanged && !signal.MirrorDrifted:
		return Outcome{Action: ActionPatch, Reason: ReasonSourceUpdated}

	case !signal.SourceChanged && signal.MirrorDrifted:
		if sourceWritable {
			return Outcome{Action: ActionPropagate, Reason: ReasonTargetEdited}
		}
		return Outcome{Action: ActionRevert, Reason: ReasonTargetEdited}

	default: // both true: conflict
		if compareTimestamps(sourceUpdated, mirrorUpdated) >= 0 {
			return Outcome{Action: ActionPatch, Reason: ReasonSourceUpdated, Conflict: ConflictSourceWon}
		}
		if sourceWritable {
			return Outcome{Action: ActionPropagate, Reason: ReasonTargetEdited, Conflict: ConflictTargetWon}
		}
		return Outcome{Action: ActionRevert, Reason: ReasonTargetEdited, Conflict: ConflictTargetWon}
	}
}
