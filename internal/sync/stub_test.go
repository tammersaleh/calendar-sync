package sync

import (
	"context"
	"errors"
	"sync"

	"github.com/tammersaleh/calendar-sync/internal/gws"
	"github.com/tammersaleh/calendar-sync/internal/mirror"
)

// recordedCall captures one method invocation on stubAPI. Fields are unioned
// across all six operations so a single []recordedCall preserves call order
// across method boundaries; tests assert on (Op, ...) tuples per call.
type recordedCall struct {
	Op            string // "EventsList", "EventsGet", "EventsInstances", "EventsInsert", "EventsPatch", "EventsDelete"
	CalendarID    string
	EventID       string
	ListParams    gws.EventsListParams
	InstanceParams gws.EventsInstancesParams
	Body          *gws.Event
}

// stubAPI is a hand-rolled in-process API implementation used by the layer-6
// tests. CLAUDE.md "Testing" prefers this over the fake-gws harness for
// unit-level work where the gws argv shape isn't what's under test.
//
// Each Op's response is a queue: tests enqueue the expected response(s) in
// FIFO order. errors are interleaved via per-op error queues; when an error
// is non-nil the response queue is not consumed.
//
// stubAPI is safe for concurrent use. Layer 6.B's orphan walk fans out
// events.get calls; protecting the queues + recorded-call log with a
// mutex lets the same stub serve both serial and concurrent tests
// without a parallel thread-safe variant. The lock is not held across
// queue dequeues' channel sends or anything else that could deadlock.
type stubAPI struct {
	mu              sync.Mutex
	listResponses   []listResponse
	listErrors      []error
	listByLabel     map[string][]listResponse // optional per-label routing
	listErrByLabel  map[string][]error
	getResponses    map[[2]string][]*gws.Event
	getErrors       map[[2]string][]error
	instancesResp   [][]gws.Event
	instancesErrors []error
	insertResp      []*gws.Event
	insertErrors    []error
	patchResp       []*gws.Event
	patchErrors     []error
	deleteErrors    []error
	calls           []recordedCall
}

// listResponse mirrors what gws.Client.EventsList returns: events plus the
// next sync token.
type listResponse struct {
	events []gws.Event
	token  string
}

func newStubAPI() *stubAPI {
	return &stubAPI{
		getResponses:   make(map[[2]string][]*gws.Event),
		getErrors:      make(map[[2]string][]error),
		listByLabel:    make(map[string][]listResponse),
		listErrByLabel: make(map[string][]error),
	}
}

// listLabel computes a routing label for an EventsListParams. Tests that
// need to enqueue per-(source, shape) responses use queueListLabeled with
// the matching label; tests that don't care use the FIFO queueList.
//
// Labels:
//
//   - "list:<calendarID>:full"        - full source-list (no syncToken,
//                                       no PrivateExtendedProperty filter)
//   - "list:<calendarID>:incr"        - incremental tick (syncToken set)
//   - "list:<calendarID>:inv:<v>"     - inventory rebuild (per version)
func listLabel(p gws.EventsListParams) string {
	if p.SyncToken != "" {
		return "list:" + p.CalendarID + ":incr"
	}
	if len(p.PrivateExtendedProperty) > 0 {
		// inventory rebuild uses calendar-sync:version=<n>; the label
		// captures the version so v2 and v1 queues stay separate.
		return "list:" + p.CalendarID + ":inv:" + p.PrivateExtendedProperty[0]
	}
	return "list:" + p.CalendarID + ":full"
}

func (s *stubAPI) EventsList(_ context.Context, params gws.EventsListParams) ([]gws.Event, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedCall{Op: "EventsList", CalendarID: params.CalendarID, ListParams: params})

	// Per-label routing wins when configured. Errors for the label are
	// dequeued first; on a non-nil error the response queue isn't consumed.
	label := listLabel(params)
	if errs := s.listErrByLabel[label]; len(errs) > 0 {
		head := errs[0]
		s.listErrByLabel[label] = errs[1:]
		if head != nil {
			return nil, "", head
		}
	}
	if resps := s.listByLabel[label]; len(resps) > 0 {
		head := resps[0]
		s.listByLabel[label] = resps[1:]
		return head.events, head.token, nil
	}

	// Fall back to FIFO queues (used by older tests).
	if len(s.listErrors) > 0 {
		head := s.listErrors[0]
		s.listErrors = s.listErrors[1:]
		if head != nil {
			return nil, "", head
		}
	}
	if len(s.listResponses) == 0 {
		return nil, "", errors.New("stubAPI: no EventsList response queued for " + label)
	}
	head := s.listResponses[0]
	s.listResponses = s.listResponses[1:]
	return head.events, head.token, nil
}

func (s *stubAPI) EventsGet(_ context.Context, calendarID, eventID string) (*gws.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedCall{Op: "EventsGet", CalendarID: calendarID, EventID: eventID})
	key := [2]string{calendarID, eventID}
	if errs := s.getErrors[key]; len(errs) > 0 {
		head := errs[0]
		s.getErrors[key] = errs[1:]
		if head != nil {
			return nil, head
		}
	}
	resps := s.getResponses[key]
	if len(resps) == 0 {
		return nil, errors.New("stubAPI: no EventsGet response queued for " + calendarID + "/" + eventID)
	}
	head := resps[0]
	s.getResponses[key] = resps[1:]
	return head, nil
}

func (s *stubAPI) EventsInstances(_ context.Context, params gws.EventsInstancesParams) ([]gws.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedCall{
		Op:             "EventsInstances",
		CalendarID:     params.CalendarID,
		EventID:        params.EventID,
		InstanceParams: params,
	})
	if len(s.instancesErrors) > 0 {
		head := s.instancesErrors[0]
		s.instancesErrors = s.instancesErrors[1:]
		if head != nil {
			return nil, head
		}
	}
	if len(s.instancesResp) == 0 {
		return nil, errors.New("stubAPI: no EventsInstances response queued")
	}
	head := s.instancesResp[0]
	s.instancesResp = s.instancesResp[1:]
	return head, nil
}

func (s *stubAPI) EventsInsert(_ context.Context, calendarID string, body *gws.Event) (*gws.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedCall{Op: "EventsInsert", CalendarID: calendarID, Body: body})
	if len(s.insertErrors) > 0 {
		head := s.insertErrors[0]
		s.insertErrors = s.insertErrors[1:]
		if head != nil {
			return nil, head
		}
	}
	if len(s.insertResp) == 0 {
		return nil, errors.New("stubAPI: no EventsInsert response queued")
	}
	head := s.insertResp[0]
	s.insertResp = s.insertResp[1:]
	return head, nil
}

func (s *stubAPI) EventsPatch(_ context.Context, calendarID, eventID string, body *gws.Event) (*gws.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedCall{
		Op:         "EventsPatch",
		CalendarID: calendarID,
		EventID:    eventID,
		Body:       body,
	})
	if len(s.patchErrors) > 0 {
		head := s.patchErrors[0]
		s.patchErrors = s.patchErrors[1:]
		if head != nil {
			return nil, head
		}
	}
	if len(s.patchResp) == 0 {
		return nil, errors.New("stubAPI: no EventsPatch response queued")
	}
	head := s.patchResp[0]
	s.patchResp = s.patchResp[1:]
	return head, nil
}

func (s *stubAPI) EventsDelete(_ context.Context, calendarID, eventID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, recordedCall{Op: "EventsDelete", CalendarID: calendarID, EventID: eventID})
	if len(s.deleteErrors) > 0 {
		head := s.deleteErrors[0]
		s.deleteErrors = s.deleteErrors[1:]
		if head != nil {
			return head
		}
	}
	return nil
}

// ---------- queueing helpers ----------

func (s *stubAPI) queueList(events []gws.Event, token string) {
	s.listResponses = append(s.listResponses, listResponse{events: events, token: token})
}

// queueListErr enqueues an error for the next EventsList call. Pair with
// queueList only for tests that chain a successful list AFTER a failed
// one - the dequeue path returns early on the error and does NOT consume
// the response queue, so callers must not push placeholder responses
// alongside an error (doing so would pollute later successful calls).
func (s *stubAPI) queueListErr(err error) {
	s.listErrors = append(s.listErrors, err)
}

// queueListFull enqueues a response for the full source-list call against
// the given calendar (no syncToken, no PrivateExtendedProperty filter).
// Used by FullSync tests; pairs with the matching helpers below for tick /
// inventory shapes so a single test can feed every shape distinctly.
func (s *stubAPI) queueListFull(calendarID string, events []gws.Event, token string) {
	s.listByLabel["list:"+calendarID+":full"] = append(
		s.listByLabel["list:"+calendarID+":full"],
		listResponse{events: events, token: token})
}

// queueListFullErr enqueues an error for the next full source-list call
// against the given calendar.
func (s *stubAPI) queueListFullErr(calendarID string, err error) {
	s.listErrByLabel["list:"+calendarID+":full"] = append(
		s.listErrByLabel["list:"+calendarID+":full"], err)
}

// queueListIncr enqueues a response for the per-tick incremental list
// (with syncToken).
func (s *stubAPI) queueListIncr(calendarID string, events []gws.Event, token string) {
	s.listByLabel["list:"+calendarID+":incr"] = append(
		s.listByLabel["list:"+calendarID+":incr"],
		listResponse{events: events, token: token})
}

// queueListIncrErr enqueues an error for the next incremental list call.
func (s *stubAPI) queueListIncrErr(calendarID string, err error) {
	s.listErrByLabel["list:"+calendarID+":incr"] = append(
		s.listErrByLabel["list:"+calendarID+":incr"], err)
}

// queueListInventory enqueues a response for one of the inventory rebuild
// list calls. version is "1" or "2" (mirror.SchemaVersion).
func (s *stubAPI) queueListInventory(calendarID, version string, events []gws.Event) {
	label := "list:" + calendarID + ":inv:" + mirror.ExtKeyVersion + "=" + version
	s.listByLabel[label] = append(s.listByLabel[label],
		listResponse{events: events, token: ""})
}

// queueListInventoryErr enqueues an error for the next inventory rebuild
// list call against the given calendar+version.
func (s *stubAPI) queueListInventoryErr(calendarID, version string, err error) {
	label := "list:" + calendarID + ":inv:" + mirror.ExtKeyVersion + "=" + version
	s.listErrByLabel[label] = append(s.listErrByLabel[label], err)
}

func (s *stubAPI) queueGet(calendarID, eventID string, e *gws.Event) {
	key := [2]string{calendarID, eventID}
	s.getResponses[key] = append(s.getResponses[key], e)
}

func (s *stubAPI) queueGetErr(calendarID, eventID string, err error) {
	key := [2]string{calendarID, eventID}
	s.getErrors[key] = append(s.getErrors[key], err)
}

func (s *stubAPI) queueInstances(events []gws.Event) {
	s.instancesResp = append(s.instancesResp, events)
}

func (s *stubAPI) queueInsert(e *gws.Event) {
	s.insertResp = append(s.insertResp, e)
}

// queueInsertErr enqueues an error for the next EventsInsert call. The
// dequeue path returns early on the error and does NOT consume the
// response queue, so callers must not push placeholder responses
// alongside an error (doing so would pollute later successful calls).
// Pair with queueInsert only for tests that chain a successful insert
// AFTER a failed one.
func (s *stubAPI) queueInsertErr(err error) {
	s.insertErrors = append(s.insertErrors, err)
}

func (s *stubAPI) queuePatch(e *gws.Event) {
	s.patchResp = append(s.patchResp, e)
}

func (s *stubAPI) callsByOp(op string) []recordedCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []recordedCall
	for _, c := range s.calls {
		if c.Op == op {
			out = append(out, c)
		}
	}
	return out
}
