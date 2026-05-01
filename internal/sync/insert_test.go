package sync

import (
	"context"
	"errors"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// TestClassify_Step8_Insert_HappyPath was already covered in classify_test.go;
// the tests below are the 409-handling branches.

func TestClassify_Step8_Insert_409_AliveExisting_ReconcilesAsHit(t *testing.T) {
	// Insert returns 409. events.get returns an alive existing mirror whose
	// stored source_updated matches the source's updated (SourceChanged=false)
	// and whose live fields match the stored checksum (MirrorDrifted=false).
	// Result: skip(unchanged), no further writes.
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	api.queueInsertErr(&gws.Error{Code: gws.CodeAPIConflict, ExitCode: 1})
	deterministic := mirror.DeterministicID("src-cal", "src-evt")
	existing := makeCleanV2Mirror(deterministic, "src-cal:src-evt",
		source.Updated, "2026-04-29T20:00:00Z",
		source.Summary, source.Start, source.End)
	api.queueGet("tgt-cal", deterministic, existing)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionSkip || got.Reason != mirror.ReasonUnchanged {
		t.Errorf("got %s/%s, want skip/unchanged", got.Action, got.Reason)
	}
	if len(api.callsByOp("EventsPatch")) != 0 {
		t.Errorf("expected no patches on alive-existing-and-unchanged path; got %d", len(api.callsByOp("EventsPatch")))
	}
	// Inventory must reflect the existing mirror.
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
	if got2, ok := inv.Lookup(tuple); !ok || got2.ID != deterministic {
		t.Errorf("inventory not updated post-409; got=%+v ok=%v", got2, ok)
	}
}

func TestClassify_Step8_Insert_409_AliveExisting_RunsDriftDetection(t *testing.T) {
	// Insert 409 -> get returns alive mirror whose summary differs from source
	// (SourceChanged=true). Falls through to source_updated patch.
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-30T11:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	source.Summary = "Updated"

	api.queueInsertErr(&gws.Error{Code: gws.CodeAPIConflict, ExitCode: 1})
	deterministic := mirror.DeterministicID("src-cal", "src-evt")
	// stored source_updated older than source.Updated -> SourceChanged=true.
	// Mirror's live managed fields are clean (match its stored checksum).
	existing := makeCleanV2Mirror(deterministic, "src-cal:src-evt",
		"2026-04-29T20:00:00Z", "2026-04-29T20:00:00Z",
		"Standup", source.Start, source.End)
	api.queueGet("tgt-cal", deterministic, existing)

	postMain := *existing
	postMain.Summary = "Updated"
	api.queuePatch(&postMain)
	api.queuePatch(&postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionPatch || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("got %s/%s, want patch/source_updated", got.Action, got.Reason)
	}
	if len(api.callsByOp("EventsPatch")) != 2 {
		t.Errorf("expected 2 patches (main + checksum); got %d", len(api.callsByOp("EventsPatch")))
	}
}

func TestClassify_Step8_Insert_409_CancelledExisting_Revives(t *testing.T) {
	// Insert 409 -> get returns mirror with status=cancelled. Revive via
	// patch+checksum-followup; outcome is insert/source_updated.
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, captured := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	api.queueInsertErr(&gws.Error{Code: gws.CodeAPIConflict, ExitCode: 1})
	deterministic := mirror.DeterministicID("src-cal", "src-evt")
	cancelled := &gws.Event{
		ID:     deterministic,
		Status: gws.EventStatusCancelled,
	}
	api.queueGet("tgt-cal", deterministic, cancelled)

	postMain := makeCleanV2Mirror(deterministic, "src-cal:src-evt",
		source.Updated, "2026-04-29T20:00:00Z",
		source.Summary, source.Start, source.End)
	api.queuePatch(postMain)
	api.queuePatch(postMain)

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	if err := c.Classify(context.Background(), source); err != nil {
		t.Fatalf("Classify error: %v", err)
	}
	got := firstOutcome(t, *captured)
	if got.Action != mirror.ActionInsert || got.Reason != mirror.ReasonSourceUpdated {
		t.Errorf("got %s/%s, want insert/source_updated", got.Action, got.Reason)
	}
	patches := api.callsByOp("EventsPatch")
	if len(patches) != 2 {
		t.Fatalf("expected 2 patches (main revive + checksum); got %d", len(patches))
	}
	// Main revive body must set status=confirmed and clear ID.
	if patches[0].Body == nil || patches[0].Body.Status != gws.EventStatusConfirmed {
		t.Errorf("revive patch body must set status=confirmed; got %+v", patches[0].Body)
	}
	if patches[0].Body.ID != "" {
		t.Errorf("revive patch body ID must be empty (events.patch carries id in URL); got %q", patches[0].Body.ID)
	}
	// Inventory updated.
	tuple := mirror.SourceTuple{CalendarID: "src-cal", EventID: "src-evt"}
	if _, ok := inv.Lookup(tuple); !ok {
		t.Errorf("inventory not populated after revive")
	}
}

func TestClassify_Step8_Insert_409_GetError_Propagates(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, _ := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	api.queueInsertErr(&gws.Error{Code: gws.CodeAPIConflict, ExitCode: 1})
	deterministic := mirror.DeterministicID("src-cal", "src-evt")
	api.queueGetErr("tgt-cal", deterministic, errors.New("get boom"))

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	err := c.Classify(context.Background(), source)
	if err == nil {
		t.Fatal("expected error from post-409 events.get failure")
	}
}

func TestClassify_Step8_Insert_NonConflictError_Propagates(t *testing.T) {
	api := newStubAPI()
	inv := NewInventory("tgt-cal")
	sink, _ := captureOutputs()

	source := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z", &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	// 500 backend error rather than 409. Should NOT trigger 409 recovery.
	api.queueInsertErr(&gws.Error{Code: gws.CodeBackendError, ExitCode: 1})

	c := newClassifier(t, api, inv, sink, classifyOptions{sourceWritable: true})
	err := c.Classify(context.Background(), source)
	if err == nil {
		t.Fatal("expected error to propagate when insert returns non-409")
	}
	if errors.Is(err, gws.ErrAPIConflict) {
		t.Errorf("error should not match ErrAPIConflict")
	}
	// We must not have called events.get on a non-409 error path.
	if len(api.callsByOp("EventsGet")) != 0 {
		t.Errorf("non-409 insert error must not trigger events.get; got %d", len(api.callsByOp("EventsGet")))
	}
}
