package mirror

import (
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// makeMirror builds a gws.Event with calendar-sync extended properties
// representing the recorded post-write state.
func makeMirror(storedSourceUpdated, storedChecksum string, m ManagedFields) *gws.Event {
	e := &gws.Event{
		Summary:      m.Summary,
		Description:  m.Description,
		Start:        managedStartToGWS(m.Start),
		End:          managedStartToGWS(m.End),
		Recurrence:   m.Recurrence,
		Transparency: m.Transparency,
		Visibility:   m.Visibility,
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				ExtKeySourceUpdated: storedSourceUpdated,
				ExtKeyChecksum:      storedChecksum,
				ExtKeyVersion:       SchemaVersion,
			},
		},
	}
	return e
}

func managedStartToGWS(d EventDateTime) *gws.EventDateTime {
	if d == (EventDateTime{}) {
		return nil
	}
	return &gws.EventDateTime{Date: d.Date, DateTime: d.DateTime, TimeZone: d.TimeZone}
}

func TestComputeDriftSignal_NoChange(t *testing.T) {
	m := ManagedFields{Summary: "Lunch"}
	mirror := makeMirror("2026-04-29T23:00:00.000Z", Checksum(m), m)
	source := &gws.Event{
		Summary: "Lunch", // same body
		Updated: "2026-04-29T23:00:00.000Z",
	}
	got := ComputeDriftSignal(source, mirror)
	if got.SourceChanged || got.MirrorDrifted {
		t.Errorf("expected both false; got %#v", got)
	}
}

func TestComputeDriftSignal_SourceUpdatedOnly(t *testing.T) {
	m := ManagedFields{Summary: "Lunch"}
	mirror := makeMirror("2026-04-29T23:00:00.000Z", Checksum(m), m)
	source := &gws.Event{
		Summary: "Lunch",
		Updated: "2026-04-30T10:00:00.000Z", // newer
	}
	got := ComputeDriftSignal(source, mirror)
	if !got.SourceChanged {
		t.Error("expected SourceChanged=true")
	}
	if got.MirrorDrifted {
		t.Errorf("expected MirrorDrifted=false (mirror still matches checksum)")
	}
}

func TestComputeDriftSignal_MirrorDriftedOnly(t *testing.T) {
	originalFields := ManagedFields{Summary: "Lunch"}
	originalChecksum := Checksum(originalFields)
	driftedMirror := makeMirror("2026-04-29T23:00:00.000Z", originalChecksum,
		ManagedFields{Summary: "User edited it"}, // mirror's actual fields differ
	)
	source := &gws.Event{
		Summary: "Lunch",
		Updated: "2026-04-29T23:00:00.000Z",
	}
	got := ComputeDriftSignal(source, driftedMirror)
	if got.SourceChanged {
		t.Error("SourceChanged should be false (timestamps match)")
	}
	if !got.MirrorDrifted {
		t.Error("MirrorDrifted should be true (managed fields no longer hash to stored checksum)")
	}
}

func TestComputeDriftSignal_BothChanged(t *testing.T) {
	originalFields := ManagedFields{Summary: "Lunch"}
	driftedMirror := makeMirror("2026-04-29T23:00:00.000Z", Checksum(originalFields),
		ManagedFields{Summary: "User edited"},
	)
	source := &gws.Event{
		Summary: "Lunch updated",
		Updated: "2026-04-30T10:00:00.000Z", // newer
	}
	got := ComputeDriftSignal(source, driftedMirror)
	if !got.SourceChanged || !got.MirrorDrifted {
		t.Errorf("expected both true; got %#v", got)
	}
}

func TestComputeDriftSignal_PreV2MirrorSetsNeedsMigration(t *testing.T) {
	// A v1 mirror has no calendar-sync:checksum extended property and
	// version="1". ComputeDriftSignal must surface NeedsMigration=true so
	// the sync layer can route to SPEC.md's "Schema version migration"
	// path (which runs a different drift check).
	mirror := &gws.Event{
		Summary: "Lunch",
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				ExtKeySourceUpdated: "2026-04-29T23:00:00.000Z",
				ExtKeyVersion:       "1",
				// no checksum
			},
		},
	}
	source := &gws.Event{
		Summary: "Lunch",
		Updated: "2026-04-29T23:00:00.000Z",
	}
	got := ComputeDriftSignal(source, mirror)
	if !got.NeedsMigration {
		t.Error("expected NeedsMigration=true for v1 mirror")
	}
}

func TestComputeDriftSignal_V2MirrorClearsNeedsMigration(t *testing.T) {
	m := ManagedFields{Summary: "Lunch"}
	mirror := makeMirror("2026-04-29T23:00:00.000Z", Checksum(m), m)
	source := &gws.Event{Summary: "Lunch", Updated: "2026-04-29T23:00:00.000Z"}
	got := ComputeDriftSignal(source, mirror)
	if got.NeedsMigration {
		t.Errorf("v2 mirror should not need migration; got %#v", got)
	}
}

func TestClassify_FourWayMatrix(t *testing.T) {
	tests := []struct {
		name           string
		signal         DriftSignal
		sourceWritable bool
		sourceUpdated  string
		mirrorUpdated  string
		want           Outcome
	}{
		{
			name:   "no change -> skip",
			signal: DriftSignal{SourceChanged: false, MirrorDrifted: false},
			want:   Outcome{Action: ActionSkip, Reason: ReasonUnchanged},
		},
		{
			name:   "source-only -> patch",
			signal: DriftSignal{SourceChanged: true, MirrorDrifted: false},
			want:   Outcome{Action: ActionPatch, Reason: ReasonSourceUpdated},
		},
		{
			name:           "mirror-only writable -> propagate",
			signal:         DriftSignal{SourceChanged: false, MirrorDrifted: true},
			sourceWritable: true,
			want:           Outcome{Action: ActionPropagate, Reason: ReasonTargetEdited},
		},
		{
			name:           "mirror-only read-only -> revert",
			signal:         DriftSignal{SourceChanged: false, MirrorDrifted: true},
			sourceWritable: false,
			want:           Outcome{Action: ActionRevert, Reason: ReasonTargetEdited},
		},
		{
			name:           "conflict source-newer -> patch + conflict_source_won",
			signal:         DriftSignal{SourceChanged: true, MirrorDrifted: true},
			sourceWritable: true,
			sourceUpdated:  "2026-04-30T10:00:00.000Z",
			mirrorUpdated:  "2026-04-30T09:00:00.000Z",
			want: Outcome{
				Action: ActionPatch, Reason: ReasonSourceUpdated, Conflict: ConflictSourceWon,
			},
		},
		{
			name:           "conflict equal timestamps tie to source",
			signal:         DriftSignal{SourceChanged: true, MirrorDrifted: true},
			sourceWritable: true,
			sourceUpdated:  "2026-04-30T10:00:00.000Z",
			mirrorUpdated:  "2026-04-30T10:00:00.000Z",
			want: Outcome{
				Action: ActionPatch, Reason: ReasonSourceUpdated, Conflict: ConflictSourceWon,
			},
		},
		{
			name:           "conflict mirror-newer writable -> propagate + conflict_target_won",
			signal:         DriftSignal{SourceChanged: true, MirrorDrifted: true},
			sourceWritable: true,
			sourceUpdated:  "2026-04-30T09:00:00.000Z",
			mirrorUpdated:  "2026-04-30T10:00:00.000Z",
			want: Outcome{
				Action: ActionPropagate, Reason: ReasonTargetEdited, Conflict: ConflictTargetWon,
			},
		},
		{
			name:           "conflict mirror-newer read-only -> revert + conflict_target_won",
			signal:         DriftSignal{SourceChanged: true, MirrorDrifted: true},
			sourceWritable: false,
			sourceUpdated:  "2026-04-30T09:00:00.000Z",
			mirrorUpdated:  "2026-04-30T10:00:00.000Z",
			want: Outcome{
				Action: ActionRevert, Reason: ReasonTargetEdited, Conflict: ConflictTargetWon,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Classify(tc.signal, tc.sourceWritable, tc.sourceUpdated, tc.mirrorUpdated)
			if got != tc.want {
				t.Errorf("Classify = %#v\nwant %#v", got, tc.want)
			}
		})
	}
}

func TestCompareTimestamps(t *testing.T) {
	tests := []struct {
		name string
		a, b string
		want int
	}{
		{"equal", "2026-04-30T12:00:00Z", "2026-04-30T12:00:00Z", 0},
		{"a earlier", "2026-04-30T12:00:00Z", "2026-04-30T13:00:00Z", -1},
		{"a later", "2026-04-30T13:00:00Z", "2026-04-30T12:00:00Z", 1},
		{"nano vs no-nano equal", "2026-04-30T12:00:00.000Z", "2026-04-30T12:00:00Z", 0},
		{"nano newer", "2026-04-30T12:00:00.500Z", "2026-04-30T12:00:00.000Z", 1},
		{"empty vs valid", "", "2026-04-30T12:00:00Z", -1},
		{"valid vs empty", "2026-04-30T12:00:00Z", "", 1},
		{"unparseable vs valid (parseable wins)", "junk", "2026-04-30T12:00:00Z", -1},
		{"valid vs unparseable", "2026-04-30T12:00:00Z", "junk", 1},
		{"unparseable vs unparseable", "junk-a", "junk-b", -1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := compareTimestamps(tc.a, tc.b)
			if got != tc.want {
				t.Errorf("compareTimestamps(%q,%q) = %d, want %d", tc.a, tc.b, got, tc.want)
			}
		})
	}
}
