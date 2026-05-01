package daemon

import (
	"encoding/json"
	"io"
	"sync"
	"time"

	syncpkg "github.com/tammersaleh/calendar-sync/internal/sync"
)

// outcomeRow is the JSONL stdout shape SPEC §"calendar-sync run" line 449
// onwards specifies. Tags use omitempty so cells the SPEC doesn't populate
// for a given action drop out of the marshal cleanly:
//
//   - skip outcomes don't carry target_event (no mirror to point at).
//   - non-conflict outcomes don't carry source_updated/mirror_updated.
//   - non-drift outcomes don't carry fields.
//
// Field names match SPEC's lowercase_snake_case verbatim so the output
// renders without further translation.
type outcomeRow struct {
	Action        string   `json:"action"`
	Pair          string   `json:"pair"`
	Direction     string   `json:"direction"`
	SourceEvent   string   `json:"source_event,omitempty"`
	TargetEvent   string   `json:"target_event,omitempty"`
	Summary       string   `json:"summary,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	Fields        []string `json:"fields,omitempty"`
	Conflict      string   `json:"conflict,omitempty"`
	SourceUpdated string   `json:"source_updated,omitempty"`
	MirrorUpdated string   `json:"mirror_updated,omitempty"`
}

// metaRow is the trailing `_meta` line emitted after each FullSync or
// Tick that ran. SPEC §"calendar-sync run" line 459 pins the field names
// (pdirs/events_processed/inserts/...). Wrapped in a one-key object so
// the line round-trips as `{"_meta":{...}}`.
type metaRow struct {
	Meta metaPayload `json:"_meta"`
}

type metaPayload struct {
	Pdirs           int   `json:"pdirs"`
	EventsProcessed int   `json:"events_processed"`
	Inserts         int   `json:"inserts"`
	Patches         int   `json:"patches"`
	Propagates      int   `json:"propagates"`
	Reverts         int   `json:"reverts"`
	Deletes         int   `json:"deletes"`
	Skips           int   `json:"skips"`
	DurationMS      int64 `json:"duration_ms"`
}

// outcomePrinter is the thread-safe JSONL writer the daemon installs as
// Reconciler.Output. Outcomes arrive serially from the main loop's
// classifier, but a sync.Mutex around the writer is cheap insurance
// against future concurrency changes (and against partial writes from
// concurrent goroutines that share the same Stdout).
type outcomePrinter struct {
	mu     sync.Mutex
	writer io.Writer
}

// newOutcomePrinter wraps w in a mutex-protected JSONL emitter. nil w
// produces a no-op printer (used when --quiet is passed; the daemon
// still tracks counts via the Reconciler's wrapper).
func newOutcomePrinter(w io.Writer) *outcomePrinter {
	return &outcomePrinter{writer: w}
}

// emitOutcome writes one outcome JSON line to the underlying writer.
// Failure to encode (writer error, marshal error) is silently dropped:
// SPEC doesn't define behavior on stdout-write failure, and tests that
// care about correctness wire a bytes.Buffer that won't fail.
func (p *outcomePrinter) emitOutcome(o syncpkg.Outcome) {
	if p == nil || p.writer == nil {
		return
	}
	row := outcomeRow{
		Action:        string(o.Action),
		Pair:          o.Pair,
		Direction:     o.Direction,
		SourceEvent:   o.SourceEventID,
		TargetEvent:   o.TargetEventID,
		Summary:       o.Summary,
		Reason:        string(o.Reason),
		Fields:        o.Fields,
		Conflict:      string(o.Conflict),
		SourceUpdated: o.SourceUpdated,
		MirrorUpdated: o.MirrorUpdated,
	}
	p.writeJSON(row)
}

// emitMeta writes a `_meta` trailer summarizing one FullSync or Tick.
// Called once per pass after every outcome has been emitted. Per
// SPEC §"calendar-sync run" line 459, the meta payload includes the
// per-action counters plus the pass duration and the number of pdirs
// processed.
func (p *outcomePrinter) emitMeta(pdirs int, counts syncpkg.Counts, duration time.Duration) {
	if p == nil || p.writer == nil {
		return
	}
	row := metaRow{
		Meta: metaPayload{
			Pdirs:           pdirs,
			EventsProcessed: counts.EventsProcessed,
			Inserts:         counts.Inserts,
			Patches:         counts.Patches,
			Propagates:      counts.Propagates,
			Reverts:         counts.Reverts,
			Deletes:         counts.Deletes,
			Skips:           counts.Skips,
			DurationMS:      duration.Milliseconds(),
		},
	}
	p.writeJSON(row)
}

// writeJSON marshals v and writes one JSON line followed by a newline.
// Marshal errors are dropped (the row types are simple structs that
// can't fail to encode in practice). Holding the mutex across the
// write prevents interleaved bytes when two goroutines share a writer.
func (p *outcomePrinter) writeJSON(v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.writer.Write(buf)
	_, _ = p.writer.Write([]byte("\n"))
}
