package output

import (
	"encoding/json"
	"io"
)

// ErrorEnvelope is the single-object JSON shape SPEC §"Output and
// Logging" (line 384) requires for any error that prevents a command
// from running. Hint and Cause are omitempty so they drop out when
// unset; Error and Detail are always present.
//
// Field name choices match SPEC verbatim. Re-ordering or renaming would
// break consumers (and SPEC's example outputs).
type ErrorEnvelope struct {
	Error  string `json:"error"`
	Detail string `json:"detail"`
	Hint   string `json:"hint,omitempty"`
	Cause  string `json:"cause,omitempty"`
}

// EmitError writes one JSON line carrying env to stderr. SPEC line 384:
// this is the wire shape the CLI prints just before exiting non-zero.
//
// Marshal failures are silently dropped (the struct is fixed at compile
// time so a marshal error is a programmer bug; logging it would corrupt
// stderr's own contract). Write failures are dropped for the same
// reason: stderr is a sink, not a transport that can be retried.
func EmitError(stderr io.Writer, env ErrorEnvelope) {
	if stderr == nil {
		return
	}
	buf, err := json.Marshal(env)
	if err != nil {
		return
	}
	_, _ = stderr.Write(buf)
	_, _ = stderr.Write([]byte("\n"))
}
