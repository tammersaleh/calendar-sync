package output

import (
	"encoding/json"
	"io"
	"sync"
)

// Printer writes per-command JSONL output to stdout. SPEC §"Output and
// Logging" (line 357) pins the shape: each line is one JSON object;
// the final line is the `_meta` trailer. Construct one Printer per
// command invocation.
//
// Quiet mirrors the global --quiet / -q flag (SPEC line 406): when
// true, every Emit and Meta call drops the write so the command stays
// silent on stdout; errors continue to flow to stderr through
// EmitError.
//
// A nil Stdout is also treated as "no-op" so callers don't need a
// special-case during testing or in --quiet mode where they may pass
// nil writers.
//
// The mutex protects against interleaved writes if two goroutines ever
// share a Printer. Most commands are single-goroutine, but the daemon's
// per-tick loop can plausibly run concurrent emitters; pinning safety
// once here prevents future regressions.
type Printer struct {
	Stdout io.Writer
	Quiet  bool

	mu sync.Mutex
}

// metaWrapper wraps any payload as `{"_meta":<body>}`. SPEC §"Output and
// Logging" (line 364) requires the trailer to be a single JSON object
// keyed at `_meta`, so any body the caller hands us is wrapped here
// regardless of its own shape.
type metaWrapper struct {
	Meta any `json:"_meta"`
}

// Emit marshals v to JSON and writes one line to Stdout (followed by
// '\n'). Quiet=true and nil Stdout both make Emit a no-op.
//
// Marshal errors are silently dropped: the caller's data shape is
// fixed at compile time, so a marshal failure is a programmer bug.
// Logging it via the same writer would corrupt the JSONL stream.
// Write errors are dropped for the same reason: stdout is the
// transport SPEC defines, and there's no fallback that wouldn't
// confuse the consumer.
func (p *Printer) Emit(v any) {
	if p == nil || p.Quiet || p.Stdout == nil {
		return
	}
	p.writeJSON(v)
}

// Meta writes the `_meta` trailer. Body is wrapped in a single-key
// object as `{"_meta":<body>}`, so callers pass whatever payload they
// want for the body field (struct, map, etc.) and the wrapping is
// applied here.
//
// SPEC §"Output and Logging" (line 365): _meta is always present, even
// on empty results - so callers should always invoke Meta at the end of
// a command. (Quiet=true still suppresses it; the user opted out.)
func (p *Printer) Meta(body any) {
	if p == nil || p.Quiet || p.Stdout == nil {
		return
	}
	p.writeJSON(metaWrapper{Meta: body})
}

// writeJSON marshals v and writes one JSON line followed by '\n'.
// Holding the mutex around the two writes keeps stdout uncorrupted
// even if the caller hands the same Printer to concurrent goroutines.
func (p *Printer) writeJSON(v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, _ = p.Stdout.Write(buf)
	_, _ = p.Stdout.Write([]byte("\n"))
}
