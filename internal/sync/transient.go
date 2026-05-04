package sync

import (
	"context"
	"errors"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

// errInsertCollisionRead marks an events.get failure that happened inside
// the post-409 insert-recovery path (insert.go's collision lookup). Read
// errors from standalone read calls (recurring horizon checks, recurring-
// instance parent lookups) are safe to skip per B18 because the source
// event will be re-evaluated on the next tick or FullSync. The post-409
// events.get is read-shape but not standalone: its result drives a write
// decision (revive cancelled vs reconcile alive vs reattempt), and a
// flake here leaves the daemon unable to know the colliding mirror's
// state. The classify loop's transient-read detection looks for this
// marker via errors.Is and keeps the surrounding error fatal even though
// the underlying *gws.Error has Op="events.get".
//
// Wrapped at insert.go's post-409 events.get site only.
var errInsertCollisionRead = errors.New("post-409 events.get is part of an insert write decision")

// isTransientClassifyReadError reports whether err can be safely logged
// and skipped from runClassifyLoop without invalidating the per-source
// syncToken. SPEC §"Conditional advancement" / §"Partial failure
// semantics" carve out a narrow set of well-understood transient read
// flakes that don't represent state mutation and don't represent a
// programmer bug. Everything else stays fatal so the syncToken doesn't
// move past unprocessed events.
//
// Decision matrix (op + code):
//
//	events.instances:
//	  CodeBackendError      - 500/503; the live-observed TARS Office
//	                          Hours flake on the recurring-parent
//	                          horizon-eligibility lookup.
//	  CodeAPIInvalidRequest - 400; a Google indexing quirk on recurring
//	                          exception parents whose recurringEventId
//	                          carries an _R<UTC> suffix.
//	  CodeAPINotFound       - 404; the recurring parent disappeared
//	                          between source-list and the eligibility
//	                          lookup (orphan walk handles cleanup).
//
//	events.get:
//	  CodeBackendError      - 500/503 during recurring-handler parent
//	                          fetch.
//	  CodeAPINotFound       - 404 during recurring-handler parent fetch.
//
// 400 on events.get is intentionally NOT in the matrix: a request-shape
// rejection there is much more likely to indicate a programmer bug than
// a transient Google quirk, and silencing it would hide real issues.
//
// Excluded from "transient" (always fatal):
//
//   - Any write op (events.insert/events.patch/events.delete): a
//     partially applied write must keep the syncToken pinned until the
//     next clean attempt.
//   - Rate-limit / auth / forbidden / 410-gone / network errors:
//     skipping defeats backoff or hides config drift the operator
//     must fix.
//   - Context cancellation/deadline-exceeded: signals whole-pass
//     shutdown (SIGTERM) or run-budget exhaustion (calendar-sync run
//     --timeout), never per-event flake. Advancing tokens after these
//     would commit partial work.
//   - Post-409 events.get inside insert recovery (errInsertCollisionRead):
//     the read directly gates a write decision.
func isTransientClassifyReadError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, errInsertCollisionRead) {
		return false
	}
	var e *gws.Error
	if !errors.As(err, &e) {
		return false
	}
	switch e.Op {
	case "events.instances":
		switch e.Code {
		case gws.CodeBackendError, gws.CodeAPIInvalidRequest, gws.CodeAPINotFound:
			return true
		}
	case "events.get":
		switch e.Code {
		case gws.CodeBackendError, gws.CodeAPINotFound:
			return true
		}
	}
	return false
}
