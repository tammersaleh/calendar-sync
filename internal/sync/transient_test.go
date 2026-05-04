package sync

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// ---------- helpers ----------

// captureLogger collects warn calls for assertion. Other levels are
// silently dropped because runClassifyLoop only emits warns for classify
// errors. Implements the local sync.Logger interface; no external
// dependency.
type captureLogger struct {
	mu    sync.Mutex
	warns []map[string]any
}

func (l *captureLogger) Debug(string, ...any) {}
func (l *captureLogger) Info(string, ...any)  {}
func (l *captureLogger) Error(string, ...any) {}

func (l *captureLogger) Warn(msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := map[string]any{"msg": msg}
	for i := 0; i+1 < len(args); i += 2 {
		k, ok := args[i].(string)
		if !ok {
			continue
		}
		entry[k] = args[i+1]
	}
	l.warns = append(l.warns, entry)
}

// makeRecurringParentSource builds a source event with Recurrence set so
// the classifier's step-7 horizon check delegates to events.instances.
// Used by tests that exercise the events.instances flake path.
func makeRecurringParentSource(id, updated string) *gws.Event {
	return &gws.Event{
		ID:           id,
		Status:       gws.EventStatusConfirmed,
		Summary:      "Recurring " + id,
		Start:        &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		End:          &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		Updated:      updated,
		HTMLLink:     "https://www.google.com/calendar/event?eid=ABC",
		Transparency: gws.TransparencyOpaque,
		Recurrence:   []string{"RRULE:FREQ=WEEKLY"},
	}
}

// makeRecurringInstanceSource builds a source event with RecurringEventID
// set so step-2 routes to the recurring handler. Tests use this to
// exercise the resolveMirrorParent EventsGet flake path.
func makeRecurringInstanceSource(id, parentID, updated string) *gws.Event {
	return &gws.Event{
		ID:                id,
		RecurringEventID:  parentID,
		OriginalStartTime: &gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"},
		Status:            gws.EventStatusConfirmed,
		Summary:           "Instance of " + parentID,
		Start:             &gws.EventDateTime{DateTime: "2026-05-01T13:00:00Z"},
		End:               &gws.EventDateTime{DateTime: "2026-05-01T14:00:00Z"},
		Updated:           updated,
		HTMLLink:          "https://www.google.com/calendar/event?eid=ABC",
		Transparency:      gws.TransparencyOpaque,
	}
}

// queueInstancesErr enqueues an error for the next EventsInstances call.
// The dequeue path returns early on the error and does NOT consume the
// response queue, so callers must not push placeholder responses
// alongside an error.
func queueInstancesErr(s *stubAPI, err error) {
	s.instancesErrors = append(s.instancesErrors, err)
}

// ---------- isTransientClassifyReadError unit tests ----------

func TestIsTransientClassifyReadError_NilFalse(t *testing.T) {
	if isTransientClassifyReadError(nil) {
		t.Errorf("nil should not be transient")
	}
}

func TestIsTransientClassifyReadError_EventsInstancesMatrix(t *testing.T) {
	tests := []struct {
		code string
		want bool
	}{
		{gws.CodeBackendError, true},
		{gws.CodeAPIInvalidRequest, true},
		{gws.CodeAPINotFound, true},
		{gws.CodeRateLimited, false},
		{gws.CodeAPIAuthFailed, false},
		{gws.CodeAPIForbidden, false},
		{gws.CodeAPIConflict, false},
		{gws.CodeAPIGone, false},
		{gws.CodeNetworkError, false},
		{gws.CodeGWSAuthFailed, false},
		{gws.CodeGWSNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			err := &gws.Error{Code: tt.code, Op: "events.instances"}
			if got := isTransientClassifyReadError(err); got != tt.want {
				t.Errorf("events.instances + %s: got %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

func TestIsTransientClassifyReadError_EventsGetMatrix(t *testing.T) {
	tests := []struct {
		code string
		want bool
	}{
		{gws.CodeBackendError, true},
		{gws.CodeAPINotFound, true},
		// 400 on events.get is most likely a request-shape bug, not a
		// Google indexing quirk - keep fatal.
		{gws.CodeAPIInvalidRequest, false},
		{gws.CodeRateLimited, false},
		{gws.CodeAPIAuthFailed, false},
		{gws.CodeAPIForbidden, false},
		{gws.CodeAPIConflict, false},
		{gws.CodeAPIGone, false},
		{gws.CodeNetworkError, false},
	}
	for _, tt := range tests {
		t.Run(tt.code, func(t *testing.T) {
			err := &gws.Error{Code: tt.code, Op: "events.get"}
			if got := isTransientClassifyReadError(err); got != tt.want {
				t.Errorf("events.get + %s: got %v, want %v", tt.code, got, tt.want)
			}
		})
	}
}

func TestIsTransientClassifyReadError_WriteOpsAlwaysFatal(t *testing.T) {
	for _, op := range []string{"events.insert", "events.patch", "events.delete"} {
		err := &gws.Error{Code: gws.CodeBackendError, Op: op}
		if isTransientClassifyReadError(err) {
			t.Errorf("write op %q + backend_error must be fatal", op)
		}
	}
}

func TestIsTransientClassifyReadError_EventsListAlwaysFatal(t *testing.T) {
	// events.list errors are handled at source-list time and never reach
	// runClassifyLoop, but the helper must not classify them as transient
	// if they ever were wrapped into a classify error.
	err := &gws.Error{Code: gws.CodeBackendError, Op: "events.list"}
	if isTransientClassifyReadError(err) {
		t.Errorf("events.list errors must not be classified as transient")
	}
}

func TestIsTransientClassifyReadError_ContextErrorsAlwaysFatal(t *testing.T) {
	tests := []error{
		context.Canceled,
		context.DeadlineExceeded,
		fmt.Errorf("gws subprocess: %w", context.Canceled),
		fmt.Errorf("gws subprocess: %w", context.DeadlineExceeded),
	}
	for _, err := range tests {
		if isTransientClassifyReadError(err) {
			t.Errorf("context error %v must be fatal (signals whole-pass shutdown / run budget exhaustion)", err)
		}
	}
}

func TestIsTransientClassifyReadError_PenetratesWraps(t *testing.T) {
	inner := &gws.Error{Code: gws.CodeBackendError, Op: "events.instances"}
	wrapped := fmt.Errorf("horizon check for recurring parent foo: %w", inner)
	deepWrapped := fmt.Errorf("classify src/foo: %w", wrapped)
	if !isTransientClassifyReadError(deepWrapped) {
		t.Errorf("multi-level wrapping must not hide a transient error")
	}
}

func TestIsTransientClassifyReadError_PostInsertCollisionMarkerStaysFatal(t *testing.T) {
	// A backend_error on events.get inside the post-409 insert recovery
	// path looks identical at the gws layer to a transient horizon-check
	// flake, but it's part of a write decision (revive vs reconcile vs
	// fail). The errInsertCollisionRead marker keeps it fatal.
	inner := &gws.Error{Code: gws.CodeBackendError, Op: "events.get"}
	withMarker := fmt.Errorf("post-409 events.get tgt/m1: %w (in insert recovery: %w)",
		inner, errInsertCollisionRead)
	if isTransientClassifyReadError(withMarker) {
		t.Errorf("post-409 collision-recovery read must stay fatal")
	}
}

func TestIsTransientClassifyReadError_NonGWSErrorFatal(t *testing.T) {
	// A plain non-typed error (e.g. a parser or programmer bug) must
	// stay fatal. The helper only relaxes for known Calendar API hiccups.
	if isTransientClassifyReadError(errors.New("synthetic non-gws failure")) {
		t.Errorf("non-gws errors must be fatal")
	}
}

// ---------- Tick: single transient events.instances flake advances token ----------

// TestTick_TransientEventsInstancesError_AdvancesToken pins the live-
// observed scenario from B18: a recurring parent's horizon-check
// events.instances returns HTTP 500. The pdir must succeed, the syncToken
// must advance, and the warn line must surface the transient classification.
func TestTick_TransientEventsInstancesError_AdvancesToken(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	src := makeRecurringParentSource("recurring-1", "2026-04-29T20:00:00Z")
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")
	queueInstancesErr(api, &gws.Error{
		Code:     gws.CodeBackendError,
		ExitCode: 1,
		Op:       "events.instances",
	})

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")
	logger := &captureLogger{}
	r.Log = logger

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("pdir should succeed (transient skip); got %v", pr.Err)
	}
	if r.syncTokens["src-A"] != "tok-new" {
		t.Errorf("token did not advance after transient skip; got %q", r.syncTokens["src-A"])
	}
	if !res.PerSource["src-A"].SyncTokenChanged {
		t.Errorf("SyncTokenChanged should be true")
	}
	// Warn line surfaces the failure with transient=true.
	if len(logger.warns) != 1 {
		t.Fatalf("expected 1 warn, got %d: %+v", len(logger.warns), logger.warns)
	}
	if got := logger.warns[0]["transient"]; got != true {
		t.Errorf("warn[transient] = %v, want true", got)
	}
	if got := logger.warns[0]["source_event"]; got != "recurring-1" {
		t.Errorf("warn[source_event] = %v, want recurring-1", got)
	}
}

// ---------- Tick: events.instances 400 (Google indexing quirk) advances ----------

// TestTick_EventsInstancesInvalidRequest_AdvancesToken pins the
// _R<UTC>-suffix recurring exception parent quirk: events.instances
// returns 400 api_invalid_request even though the request shape is valid.
// SPEC's per-event tolerance lets the daemon skip + advance.
func TestTick_EventsInstancesInvalidRequest_AdvancesToken(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	src := makeRecurringParentSource("recurring-1", "2026-04-29T20:00:00Z")
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")
	queueInstancesErr(api, &gws.Error{
		Code:     gws.CodeAPIInvalidRequest,
		ExitCode: 1,
		Op:       "events.instances",
	})

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("pdir should succeed; got %v", pr.Err)
	}
	if r.syncTokens["src-A"] != "tok-new" {
		t.Errorf("token did not advance; got %q", r.syncTokens["src-A"])
	}
}

// ---------- Tick: events.instances 5xx in recurring-handler locate path ----------

// TestTick_RecurringHandlerLocateInstancesError_AdvancesToken pins the
// recurring handler's locateMirrorInstance path: the mirror parent is in
// inventory (so resolveMirrorParent succeeds via LookupMirrorParent), and
// the first events.instances call (the locate-the-mirror-instance lookup
// against the TARGET calendar) returns 5xx. The horizon-check
// events.instances tested above lives in classify.go's isInHorizon; this
// is a different code path with the same op-shape, and the transient
// classifier must work for both.
func TestTick_RecurringHandlerLocateInstancesError_AdvancesToken(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	// Recurring INSTANCE source (RecurringEventID set). Routes through
	// step 2 to recurring.Handler.Handle.
	src := makeRecurringInstanceSource("inst-1", "src-parent", "2026-04-29T20:00:00Z")
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")

	// Mirror parent is in inventory keyed by (src-A, src-parent). The
	// handler's resolveMirrorParent step-1 finds it via LookupMirrorParent
	// without calling EventsGet, so the failure point isolates to
	// locateMirrorInstance.
	inv := NewInventory("tgt-A")
	parentTuple := mirror.SourceTuple{CalendarID: "src-A", EventID: "src-parent"}
	inv.Set(parentTuple, &gws.Event{
		ID:         "mp-1",
		Status:     gws.EventStatusConfirmed,
		Summary:    "Parent",
		Recurrence: []string{"RRULE:FREQ=WEEKLY"},
		ExtendedProperties: &gws.ExtendedProperties{
			Private: map[string]string{
				mirror.ExtKeySource:  "src-A:src-parent",
				mirror.ExtKeyVersion: mirror.SchemaVersion,
			},
		},
	})

	// First (and only) EventsInstances call is the locate-the-instance
	// lookup against the TARGET calendar. Flake it.
	queueInstancesErr(api, &gws.Error{
		Code:     gws.CodeBackendError,
		ExitCode: 1,
		Op:       "events.instances",
	})

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = inv

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("pdir should succeed (transient skip); got %v", pr.Err)
	}
	if r.syncTokens["src-A"] != "tok-new" {
		t.Errorf("token did not advance; got %q", r.syncTokens["src-A"])
	}
}

// ---------- Tick: events.get 5xx in recurring-handler advances token ----------

// TestTick_RecurringHandlerEventsGet5xx_AdvancesToken covers the recurring
// handler's resolveMirrorParent inventory-miss path: the source-parent
// events.get returns HTTP 500. Per B18 this is a transient read flake;
// the orphan walk on the next FullSync will catch up.
func TestTick_RecurringHandlerEventsGet5xx_AdvancesToken(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	// Recurring instance whose mirror parent is NOT in inventory -
	// triggers handler.resolveMirrorParent's EventsGet on the source.
	src := makeRecurringInstanceSource("inst-1", "src-parent", "2026-04-29T20:00:00Z")
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")
	api.queueGetErr("src-A", "src-parent", &gws.Error{
		Code:     gws.CodeBackendError,
		ExitCode: 1,
		Op:       "events.get",
	})

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("pdir should succeed; got %v", pr.Err)
	}
	if r.syncTokens["src-A"] != "tok-new" {
		t.Errorf("token did not advance; got %q", r.syncTokens["src-A"])
	}
}

// ---------- Tick: events.get 404 (recurring parent vanished) advances ----------

func TestTick_RecurringHandlerEventsGet404_AdvancesToken(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	src := makeRecurringInstanceSource("inst-1", "src-parent", "2026-04-29T20:00:00Z")
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")
	api.queueGetErr("src-A", "src-parent", &gws.Error{
		Code:     gws.CodeAPINotFound,
		ExitCode: 1,
		Op:       "events.get",
	})

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err != nil {
		t.Errorf("pdir should succeed; got %v", pr.Err)
	}
	if r.syncTokens["src-A"] != "tok-new" {
		t.Errorf("token did not advance; got %q", r.syncTokens["src-A"])
	}
}

// ---------- Tick: events.get 400 stays fatal (programmer-bug shape) ----------

// TestTick_RecurringHandlerEventsGet400_PdirFails_TokenStays pins the
// boundary that the events.get matrix is narrower than events.instances:
// 400 on events.get is most likely a request-shape bug and must surface
// to the operator, not silently skip.
func TestTick_RecurringHandlerEventsGet400_PdirFails_TokenStays(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	src := makeRecurringInstanceSource("inst-1", "src-parent", "2026-04-29T20:00:00Z")
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")
	api.queueGetErr("src-A", "src-parent", &gws.Error{
		Code:     gws.CodeAPIInvalidRequest,
		ExitCode: 1,
		Op:       "events.get",
	})

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err == nil {
		t.Errorf("pdir should fail on events.get 400")
	}
	if r.syncTokens["src-A"] != "tok-old" {
		t.Errorf("token must not advance on fatal error; got %q", r.syncTokens["src-A"])
	}
}

// ---------- Tick: write failure stays fatal ----------

func TestTick_WriteError_PdirFails_TokenStays(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	src := makeNonRecurringSource("evt-1", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")
	// EventsInsert (the write) returns 5xx. Even though the underlying
	// gws code is CodeBackendError, the Op is events.insert which is
	// outside the read matrix, so the pdir must fail.
	api.queueInsertErr(&gws.Error{
		Code:     gws.CodeBackendError,
		ExitCode: 1,
		Op:       "events.insert",
	})

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err == nil {
		t.Errorf("pdir should fail when a write errors")
	}
	if r.syncTokens["src-A"] != "tok-old" {
		t.Errorf("token must not advance on write failure")
	}
}

// ---------- Tick: post-409 events.get 5xx stays fatal ----------

// TestTick_Post409EventsGet5xx_PdirFails_TokenStays is the key boundary
// from Codex's review: the post-409 events.get is read-shape but is
// resolving a write decision (revive cancelled vs reconcile alive). A
// flake here cannot be safely skipped because the daemon doesn't know
// what state the colliding mirror is in.
func TestTick_Post409EventsGet5xx_PdirFails_TokenStays(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	src := makeNonRecurringSource("src-evt", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")

	// Insert returns 409, post-409 events.get returns 5xx.
	api.queueInsertErr(&gws.Error{Code: gws.CodeAPIConflict, ExitCode: 1, Op: "events.insert"})
	deterministic := mirror.DeterministicID("src-A", "src-evt")
	api.queueGetErr("tgt-A", deterministic, &gws.Error{
		Code:     gws.CodeBackendError,
		ExitCode: 1,
		Op:       "events.get",
	})

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err == nil {
		t.Errorf("pdir should fail when post-409 events.get errors")
	}
	if r.syncTokens["src-A"] != "tok-old" {
		t.Errorf("token must not advance after post-409 lookup failure")
	}
}

// ---------- Tick: context errors stay fatal ----------

func TestTick_ContextCanceled_PdirFails_TokenStays(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	src := makeRecurringParentSource("recurring-1", "2026-04-29T20:00:00Z")
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")
	// Same shape as gws.Client.execute returns when the context fires.
	queueInstancesErr(api, fmt.Errorf("gws subprocess: %w", context.Canceled))

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err == nil {
		t.Errorf("pdir should fail on context.Canceled")
	}
	if r.syncTokens["src-A"] != "tok-old" {
		t.Errorf("token must not advance on context.Canceled")
	}
}

func TestTick_ContextDeadlineExceeded_PdirFails_TokenStays(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	src := makeRecurringParentSource("recurring-1", "2026-04-29T20:00:00Z")
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")
	queueInstancesErr(api, fmt.Errorf("gws subprocess: %w", context.DeadlineExceeded))

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err == nil {
		t.Errorf("pdir should fail on context.DeadlineExceeded")
	}
	if r.syncTokens["src-A"] != "tok-old" {
		t.Errorf("token must not advance on context.DeadlineExceeded")
	}
}

// ---------- Tick: mixed transient + fatal in same loop -> fatal trumps ----------

// TestTick_MixedTransientAndFatal_FatalWins ensures any genuine failure
// inside a pdir keeps the token pinned, even if other events in the same
// loop only flaked transiently.
func TestTick_MixedTransientAndFatal_FatalWins(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	transient := makeRecurringParentSource("recurring-flaky", "2026-04-29T20:00:00Z")
	fatal := makeNonRecurringSource("write-fail", "2026-04-29T20:00:00Z",
		&gws.EventDateTime{DateTime: "2026-05-01T12:00:00Z"})

	api.queueListIncr("src-A", []gws.Event{*transient, *fatal}, "tok-new")
	queueInstancesErr(api, &gws.Error{
		Code:     gws.CodeBackendError,
		ExitCode: 1,
		Op:       "events.instances",
	})
	api.queueInsertErr(&gws.Error{
		Code:     gws.CodeBackendError,
		ExitCode: 1,
		Op:       "events.insert",
	})

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err == nil {
		t.Errorf("pdir should fail when a write fails alongside a transient skip")
	}
	if r.syncTokens["src-A"] != "tok-old" {
		t.Errorf("token must not advance when the loop has any fatal error")
	}
}

// ---------- Tick: rate_limited stays fatal ----------

// TestTick_RateLimited_PdirFails_TokenStays pins that rate-limit errors
// during a per-event read are NOT in the transient set: skipping defeats
// backoff and risks the daemon getting throttled harder.
func TestTick_RateLimited_PdirFails_TokenStays(t *testing.T) {
	api := newStubAPI()
	pd := makeTestPDir("p1", "src-A", "tgt-A", true)
	canonical := makeCanonical(pd)

	src := makeRecurringParentSource("recurring-1", "2026-04-29T20:00:00Z")
	api.queueListIncr("src-A", []gws.Event{*src}, "tok-new")
	queueInstancesErr(api, &gws.Error{
		Code:     gws.CodeRateLimited,
		ExitCode: 3,
		Op:       "events.instances",
	})

	r := newTestReconciler(api, canonical)
	r.syncTokens["src-A"] = "tok-old"
	r.inventories["tgt-A"] = NewInventory("tgt-A")

	res, err := r.Tick(context.Background())
	if err != nil {
		t.Fatalf("Tick error: %v", err)
	}
	pr, _ := pdirByPair(res.PDirs, "p1")
	if pr.Err == nil {
		t.Errorf("pdir should fail on rate_limited")
	}
	if r.syncTokens["src-A"] != "tok-old" {
		t.Errorf("token must not advance on rate_limited")
	}
}
