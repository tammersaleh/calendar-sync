// Package testhelpers builds a fake `gws` binary out of the Go test binary
// itself, so tests in any other package can exercise calendar-sync's real
// gws-subprocess code paths without ever invoking the real CLI.
//
// The pattern: each consuming test package adds a TestMain that calls
// MaybeRunFakeGWS first; when invoked as a fake gws (env var sentinel set)
// the test binary emits the canned scenario response and exits, otherwise
// it runs tests normally. WithFakeGWS sets up a temp dir with a "gws"
// symlink to the test binary and prepends it to PATH, so the wrapper code
// under test sees a normal-looking gws on the path.
package testhelpers

// Scenario is the ordered list of fake-gws responses for one test. Each
// invocation of `gws ...` consumes the next ScenarioCall in order. Tests that
// expect N gws calls supply N entries; an N+1th invocation is a test bug
// and the fake exits non-zero.
type Scenario struct {
	Calls []ScenarioCall `json:"calls"`
}

// ScenarioCall is the response the fake-gws emits for a single invocation:
// the bytes to write to stdout, the bytes to write to stderr, and the exit
// code. Stdout is typically the JSON body of a Calendar API response.
type ScenarioCall struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Exit   int    `json:"exit"`
}

// RecordedCall is one captured fake-gws invocation for after-the-fact
// assertions in tests. Args is the full argv minus argv[0]; Params and Body
// are pre-parsed convenience views of --params and --json (left nil if
// either flag was absent or unparseable).
type RecordedCall struct {
	Args   []string       `json:"args"`
	Params map[string]any `json:"params,omitempty"`
	Body   any            `json:"body,omitempty"`
}
