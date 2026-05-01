package daemon

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/mirror"
	syncpkg "github.com/tammersaleh/calendar-sync/internal/sync"
)

// TestOutcomePrinter_OneOutcomeOneLine: one Outcome → one JSON line.
func TestOutcomePrinter_OneOutcomeOneLine(t *testing.T) {
	var buf bytes.Buffer
	p := newOutcomePrinter(&buf)
	p.emitOutcome(syncpkg.Outcome{
		Action:        mirror.ActionInsert,
		Reason:        mirror.ReasonSourceUpdated,
		Pair:          "work-personal",
		Direction:     "a_to_b",
		SourceEventID: "abc123",
		TargetEventID: "def456",
		Summary:       "Standup",
	})
	got := buf.String()
	if strings.Count(got, "\n") != 1 {
		t.Errorf("expected exactly one newline; got %d (output=%q)",
			strings.Count(got, "\n"), got)
	}

	var row outcomeRow
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &row); err != nil {
		t.Fatalf("decode line: %v", err)
	}
	if row.Action != "insert" {
		t.Errorf("action = %q, want insert", row.Action)
	}
	if row.Pair != "work-personal" {
		t.Errorf("pair = %q, want work-personal", row.Pair)
	}
	if row.Reason != "source_updated" {
		t.Errorf("reason = %q, want source_updated", row.Reason)
	}
}

// TestOutcomePrinter_OmitsEmptyConflictAndFields: conflict and fields
// drop out when not set.
func TestOutcomePrinter_OmitsEmptyConflictAndFields(t *testing.T) {
	var buf bytes.Buffer
	p := newOutcomePrinter(&buf)
	p.emitOutcome(syncpkg.Outcome{
		Action:    mirror.ActionSkip,
		Reason:    mirror.Reason("unchanged"),
		Pair:      "p1",
		Direction: "a_to_b",
	})
	got := buf.String()
	if strings.Contains(got, "\"conflict\"") {
		t.Errorf("output should not contain conflict key for non-conflict outcome: %q", got)
	}
	if strings.Contains(got, "\"fields\"") {
		t.Errorf("output should not contain fields key when empty: %q", got)
	}
	// Timestamp fields drop out too on a no-conflict outcome.
	if strings.Contains(got, "\"source_updated\"") {
		t.Errorf("output should not contain source_updated when empty: %q", got)
	}
	if strings.Contains(got, "\"mirror_updated\"") {
		t.Errorf("output should not contain mirror_updated when empty: %q", got)
	}
}

// TestOutcomePrinter_ConflictAndFieldsRendered: both fields appear when
// non-empty.
func TestOutcomePrinter_ConflictAndFieldsRendered(t *testing.T) {
	var buf bytes.Buffer
	p := newOutcomePrinter(&buf)
	p.emitOutcome(syncpkg.Outcome{
		Action:        mirror.ActionPropagate,
		Reason:        mirror.ReasonTargetEdited,
		Conflict:      mirror.ConflictTargetWon,
		Fields:        []string{"summary", "start"},
		Pair:          "p1",
		Direction:     "a_to_b",
		SourceUpdated: "2026-04-30T10:00:00Z",
		MirrorUpdated: "2026-04-30T10:01:00Z",
	})
	var row outcomeRow
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &row); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if row.Conflict != "conflict_target_won" {
		t.Errorf("conflict = %q, want conflict_target_won", row.Conflict)
	}
	if len(row.Fields) != 2 || row.Fields[0] != "summary" || row.Fields[1] != "start" {
		t.Errorf("fields = %v, want [summary start]", row.Fields)
	}
	if row.SourceUpdated != "2026-04-30T10:00:00Z" {
		t.Errorf("source_updated = %q, want timestamp", row.SourceUpdated)
	}
}

// TestOutcomePrinter_EmitMetaShape: SPEC's `_meta` line has the canonical
// summary fields.
func TestOutcomePrinter_EmitMetaShape(t *testing.T) {
	var buf bytes.Buffer
	p := newOutcomePrinter(&buf)
	p.emitMeta(2, syncpkg.Counts{
		EventsProcessed: 18,
		Inserts:         1,
		Patches:         1,
		Propagates:      1,
		Reverts:         1,
		Deletes:         1,
		Skips:           13,
	}, 1842*time.Millisecond)

	var meta metaRow
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &meta); err != nil {
		t.Fatalf("decode meta: %v (line=%q)", err, buf.String())
	}
	if meta.Meta.Pdirs != 2 {
		t.Errorf("pdirs = %d, want 2", meta.Meta.Pdirs)
	}
	if meta.Meta.EventsProcessed != 18 {
		t.Errorf("events_processed = %d, want 18", meta.Meta.EventsProcessed)
	}
	if meta.Meta.DurationMS != 1842 {
		t.Errorf("duration_ms = %d, want 1842", meta.Meta.DurationMS)
	}
	if meta.Meta.Inserts != 1 || meta.Meta.Skips != 13 {
		t.Errorf("counts mis-rendered: %+v", meta.Meta)
	}
}

// TestOutcomePrinter_NilWriterIsNoOp: a printer with nil writer (used for
// --quiet / tests) silently drops output.
func TestOutcomePrinter_NilWriterIsNoOp(t *testing.T) {
	p := newOutcomePrinter(nil)
	p.emitOutcome(syncpkg.Outcome{Action: mirror.ActionSkip})
	p.emitMeta(1, syncpkg.Counts{}, 0)
	// No assertion needed - reaching this point without panic is success.
}

// TestOutcomePrinter_NilPrinterIsNoOp: defensive guard against a nil
// receiver.
func TestOutcomePrinter_NilPrinterIsNoOp(t *testing.T) {
	var p *outcomePrinter
	p.emitOutcome(syncpkg.Outcome{Action: mirror.ActionSkip})
	p.emitMeta(1, syncpkg.Counts{}, 0)
}
