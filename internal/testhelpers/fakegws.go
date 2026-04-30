package testhelpers

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// Env-var sentinels used to drive the fake gws.
const (
	envFakeMode     = "CALENDAR_SYNC_FAKE_GWS"          // "1" turns the test binary into the fake
	envFakeScenario = "CALENDAR_SYNC_FAKE_GWS_SCENARIO" // path to scenario JSON
	envFakeCalls    = "CALENDAR_SYNC_FAKE_GWS_CALLS"    // path to NDJSON calls log
)

// MaybeRunFakeGWS exits the process as a fake `gws` binary when the test
// binary was re-exec'd by WithFakeGWS (sentinel env var set). When called
// outside that context it returns immediately.
//
// Each test package that uses WithFakeGWS must wire this into TestMain:
//
//	func TestMain(m *testing.M) {
//	    testhelpers.MaybeRunFakeGWS()
//	    os.Exit(m.Run())
//	}
func MaybeRunFakeGWS() {
	if os.Getenv(envFakeMode) != "1" {
		return
	}
	os.Exit(runFakeGWS(os.Args[1:], os.Stdout, os.Stderr))
}

// runFakeGWS is the body of the fake. Split out from MaybeRunFakeGWS so it
// can be reasoned about without the os.Exit side effect.
func runFakeGWS(args []string, stdout, stderr *os.File) int {
	scenarioPath := os.Getenv(envFakeScenario)
	if scenarioPath == "" {
		fmt.Fprintln(stderr, "fakegws: missing CALENDAR_SYNC_FAKE_GWS_SCENARIO")
		return 99
	}
	callsPath := os.Getenv(envFakeCalls)
	if callsPath == "" {
		fmt.Fprintln(stderr, "fakegws: missing CALENDAR_SYNC_FAKE_GWS_CALLS")
		return 99
	}

	scenarioBytes, err := os.ReadFile(scenarioPath)
	if err != nil {
		fmt.Fprintf(stderr, "fakegws: read scenario %q: %v\n", scenarioPath, err)
		return 99
	}
	var scenario Scenario
	if err := json.Unmarshal(scenarioBytes, &scenario); err != nil {
		fmt.Fprintf(stderr, "fakegws: parse scenario: %v\n", err)
		return 99
	}

	callIndex, err := countLines(callsPath)
	if err != nil {
		fmt.Fprintf(stderr, "fakegws: read calls log: %v\n", err)
		return 99
	}
	if callIndex >= len(scenario.Calls) {
		fmt.Fprintf(stderr, "fakegws: scenario exhausted (call %d, only %d responses configured)\n",
			callIndex+1, len(scenario.Calls))
		return 99
	}

	if err := appendCall(callsPath, recordInvocation(args)); err != nil {
		fmt.Fprintf(stderr, "fakegws: append calls log: %v\n", err)
		return 99
	}

	step := scenario.Calls[callIndex]
	if step.Stdout != "" {
		_, _ = stdout.WriteString(step.Stdout)
	}
	if step.Stderr != "" {
		_, _ = stderr.WriteString(step.Stderr)
	}
	return step.Exit
}

// recordInvocation captures argv plus pre-parsed --params and --json so test
// assertions don't have to re-walk the arg list.
func recordInvocation(args []string) RecordedCall {
	rc := RecordedCall{Args: args}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--params":
			if i+1 < len(args) {
				_ = json.Unmarshal([]byte(args[i+1]), &rc.Params)
				i++
			}
		case "--json":
			if i+1 < len(args) {
				_ = json.Unmarshal([]byte(args[i+1]), &rc.Body)
				i++
			}
		}
	}
	return rc
}

func countLines(path string) (int, error) {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return 0, nil
		}
		return 0, err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	n := 0
	for scanner.Scan() {
		n++
	}
	return n, scanner.Err()
}

func appendCall(path string, call RecordedCall) error {
	raw, err := json.Marshal(call)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(raw, '\n'))
	return err
}

// readCalls reads the NDJSON calls log written by the fake. Returns an empty
// slice if the file does not exist (no gws calls happened).
func readCalls(t testing.TB, path string) []RecordedCall {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		t.Fatalf("read calls log: %v", err)
	}
	defer f.Close()

	var calls []RecordedCall
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var c RecordedCall
		if err := json.Unmarshal(scanner.Bytes(), &c); err != nil {
			t.Fatalf("parse calls log line: %v", err)
		}
		calls = append(calls, c)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan calls log: %v", err)
	}
	return calls
}

// WithFakeGWS sets up the test binary as a fake gws on PATH, runs fn, and
// returns the recorded gws invocations in order. Standard usage:
//
//	calls := testhelpers.WithFakeGWS(t, scenario, func() {
//	    client := gws.New()
//	    _ = client.CalendarListGet(ctx, "alice@example.com")
//	})
//	if len(calls) != 1 { ... }
//
// Caller contract:
//   - Tests must NOT call t.Parallel(): the env vars are process-wide and
//     would clash between concurrent fake invocations.
//   - fn MUST NOT return before every gws call it triggers has completed.
//     The calls log is read after fn returns; any goroutine that issues a
//     gws invocation past that point will be missed (and may also race
//     with the test cleanup unlinking t.TempDir()).
func WithFakeGWS(t *testing.T, scenario Scenario, fn func()) []RecordedCall {
	t.Helper()

	tmp := t.TempDir()

	scenarioPath := filepath.Join(tmp, "scenario.json")
	rawScenario, err := json.Marshal(scenario)
	if err != nil {
		t.Fatalf("marshal scenario: %v", err)
	}
	if err := os.WriteFile(scenarioPath, rawScenario, 0o600); err != nil {
		t.Fatalf("write scenario: %v", err)
	}

	fakeBin := filepath.Join(tmp, "gws")
	if err := os.Symlink(os.Args[0], fakeBin); err != nil {
		t.Fatalf("symlink fake gws: %v", err)
	}

	callsPath := filepath.Join(tmp, "calls.jsonl")

	t.Setenv(envFakeMode, "1")
	t.Setenv(envFakeScenario, scenarioPath)
	t.Setenv(envFakeCalls, callsPath)
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))

	fn()

	return readCalls(t, callsPath)
}
