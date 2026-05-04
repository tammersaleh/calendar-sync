//go:build e2e

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tammersaleh/calendar-sync/internal/gws"
)

const (
	// envGuard is the safety env var the mise test:e2e task sets. The
	// harness refuses to start without it so a stray
	// `go test -tags=e2e ./...` doesn't accidentally provision and
	// destroy fixtures.
	envGuard = "CALENDAR_SYNC_E2E"

	// defaultPairName is the pair the harness's auto-written config
	// uses unless the test overrides via SetupOptions.
	defaultPairName = "e2e-pair"
)

// Harness is the per-test scaffold. Setup populates every field; tests
// then drive the binary via Run and assert state via GWS.
type Harness struct {
	T *testing.T

	SourceCalID string
	TargetCalID string

	ConfigPath string // per-test config.toml
	Sandbox    string // working dir for subprocesses; stray writes land here
	Binary     string // built calendar-sync binary path

	GWS *gws.Client // for direct API access (assertions, helpers)

	PairName string // auto-written into config; default "e2e-pair"

	uniqueSuffix string // 8-char hex appended to event titles for collision-resistance
}

// SetupOptions tunes the per-test config.toml. Zero value gives a
// minimal one-pair source-to-target setup at default horizon, no
// propagate.
type SetupOptions struct {
	PropagateTargetEdits bool
	Horizon              string // e.g. "30d", "365d"; default "365d"
	DryRun               bool
	PairName             string // default "e2e-pair"
}

// Setup is called from a test to prepare the per-test harness state.
// TestMain must have already run (it sets the package-level
// fixtureSourceID, fixtureTargetID, and binaryPath).
func Setup(t *testing.T, opts SetupOptions) *Harness {
	t.Helper()

	if os.Getenv(envGuard) != "1" {
		t.Skipf("E2E tests require %s=1; use `mise run test:e2e`", envGuard)
	}
	if fixtureSourceID == "" || fixtureTargetID == "" {
		t.Fatal("e2e fixtures not provisioned; TestMain didn't run or failed")
	}

	pairName := opts.PairName
	if pairName == "" {
		pairName = defaultPairName
	}
	horizon := opts.Horizon
	if horizon == "" {
		horizon = "365d"
	}

	sandbox := t.TempDir()

	h := &Harness{
		T:            t,
		SourceCalID:  fixtureSourceID,
		TargetCalID:  fixtureTargetID,
		Sandbox:      sandbox,
		Binary:       binaryPath,
		PairName:     pairName,
		uniqueSuffix: randomHex(t, 8),
		GWS:          gws.New(gws.WithWorkDir(sandbox)),
	}

	configPath := filepath.Join(sandbox, "config.toml")
	cfg := writeConfig(t, configPath, configValues{
		PairName:             pairName,
		Source:               fixtureSourceID,
		Target:               fixtureTargetID,
		Horizon:              horizon,
		PropagateTargetEdits: opts.PropagateTargetEdits,
		DryRun:               opts.DryRun,
	})
	h.ConfigPath = cfg

	// Per-test TMPDIR isolates the IPC-socket detection probe from the
	// production daemon's socket. Without this the run command's
	// daemon_already_running check would trip and exit 5.
	t.Setenv("TMPDIR", sandbox)

	// Pre-test wipe: prior tests in the same run may have left state.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := wipeCalendar(ctx, h.GWS, h.SourceCalID); err != nil {
		t.Fatalf("pre-test wipe of source: %v", err)
	}
	if err := wipeCalendar(ctx, h.GWS, h.TargetCalID); err != nil {
		t.Fatalf("pre-test wipe of target: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := wipeCalendar(ctx, h.GWS, h.SourceCalID); err != nil {
			t.Logf("post-test wipe of source: %v", err)
		}
		if err := wipeCalendar(ctx, h.GWS, h.TargetCalID); err != nil {
			t.Logf("post-test wipe of target: %v", err)
		}
	})

	return h
}

// configValues populates the per-test config.toml template.
type configValues struct {
	PairName             string
	Source               string
	Target               string
	Horizon              string
	PropagateTargetEdits bool
	DryRun               bool
}

// writeConfig produces a minimal config.toml at path. Returns the path
// for use as `--config` on the binary.
func writeConfig(t *testing.T, path string, v configValues) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("[settings]\n")
	b.WriteString("poll_interval = \"15s\"\n") // SPEC minimum; relevant for watch tests
	fmt.Fprintf(&b, "horizon = %q\n", v.Horizon)
	if v.PropagateTargetEdits {
		b.WriteString("propagate_target_edits = true\n")
	}
	if v.DryRun {
		b.WriteString("dry_run = true\n")
	}
	b.WriteString("\n")
	b.WriteString("[[pairs]]\n")
	fmt.Fprintf(&b, "name = %q\n", v.PairName)
	fmt.Fprintf(&b, "source = %q\n", v.Source)
	fmt.Fprintf(&b, "target = %q\n", v.Target)

	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write config %q: %v", path, err)
	}
	return path
}

// Title generates a unique, harness-tagged event title for use in
// scenarios. Pattern: "e2e-<scenario>-<random>". Wholesale wipe at
// teardown handles cleanup; the random suffix is a second line of
// defense if a wipe partially fails.
func (h *Harness) Title(scenario string) string {
	return fmt.Sprintf("e2e-%s-%s", scenario, h.uniqueSuffix)
}

// RunResult is the parsed output of a `calendar-sync run` invocation.
type RunResult struct {
	Outcomes []Outcome
	Meta     *Meta
	Stderr   string
	ExitCode int
}

// Outcome mirrors the SPEC stdout-action wire shape (cmd/run.go's
// outcomeRow.MarshalJSON). We re-declare it here so the harness has
// a stable consumer-side type without importing the cmd package.
type Outcome struct {
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

// Meta mirrors the `_meta` trailer SPEC defines for `calendar-sync run`.
type Meta struct {
	PDirs           int      `json:"pdirs"`
	EventsProcessed int      `json:"events_processed"`
	Inserts         int      `json:"inserts"`
	Patches         int      `json:"patches"`
	Propagates      int      `json:"propagates"`
	Reverts         int      `json:"reverts"`
	Deletes         int      `json:"deletes"`
	Skips           int      `json:"skips"`
	DurationMS      int64    `json:"duration_ms"`
	Failures        []string `json:"failures,omitempty"`
}

// Run invokes `calendar-sync run --config <h.ConfigPath> [extra...]`
// and returns the parsed output.
func (h *Harness) Run(ctx context.Context, extraArgs ...string) RunResult {
	h.T.Helper()
	args := append([]string{"run", "--config", h.ConfigPath}, extraArgs...)
	cmd := exec.CommandContext(ctx, h.Binary, args...)
	cmd.Dir = h.Sandbox

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if asExit(err, &ee) {
			exit = ee.ExitCode()
		} else {
			h.T.Fatalf("run %v: launch failed: %v", args, err)
		}
	}

	res := RunResult{
		Stderr:   stderr.String(),
		ExitCode: exit,
	}
	parseStdoutJSONL(h.T, stdout.Bytes(), &res)
	return res
}

// asExit is errors.As specialized; defined here to avoid an extra
// import line at the call site and to keep the type assertion next to
// the run logic.
func asExit(err error, target **exec.ExitError) bool {
	for e := err; e != nil; {
		if ee, ok := e.(*exec.ExitError); ok {
			*target = ee
			return true
		}
		type unwrapper interface{ Unwrap() error }
		uw, ok := e.(unwrapper)
		if !ok {
			return false
		}
		e = uw.Unwrap()
	}
	return false
}

// parseStdoutJSONL splits the run command's stdout into outcomes plus
// the `_meta` trailer. SPEC §"Output and Logging" pins one JSON object
// per line.
func parseStdoutJSONL(t *testing.T, stdout []byte, res *RunResult) {
	t.Helper()
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		// Distinguish meta from outcomes by the `_meta` envelope key.
		var probe map[string]json.RawMessage
		if err := json.Unmarshal(line, &probe); err != nil {
			t.Fatalf("parse run stdout line %q: %v", string(line), err)
		}
		if raw, ok := probe["_meta"]; ok {
			var m Meta
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatalf("parse _meta body %q: %v", string(raw), err)
			}
			res.Meta = &m
			continue
		}
		var o Outcome
		if err := json.Unmarshal(line, &o); err != nil {
			t.Fatalf("parse outcome %q: %v", string(line), err)
		}
		res.Outcomes = append(res.Outcomes, o)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan run stdout: %v", err)
	}
}

// OutcomeMatch is the predicate for AssertOutcome. Empty fields are
// wildcards. Fields slice matches as exact set equality when set.
type OutcomeMatch struct {
	Action      string
	Reason      string
	SourceEvent string
	TargetEvent string
}

// AssertOutcome asserts that exactly one outcome in res matches m.
// Returns the matched outcome for follow-up assertions.
func (res RunResult) AssertOutcome(t *testing.T, m OutcomeMatch) Outcome {
	t.Helper()
	var matches []Outcome
	for _, o := range res.Outcomes {
		if m.Action != "" && o.Action != m.Action {
			continue
		}
		if m.Reason != "" && o.Reason != m.Reason {
			continue
		}
		if m.SourceEvent != "" && o.SourceEvent != m.SourceEvent {
			continue
		}
		if m.TargetEvent != "" && o.TargetEvent != m.TargetEvent {
			continue
		}
		matches = append(matches, o)
	}
	switch len(matches) {
	case 0:
		t.Fatalf("no outcome matched %+v; got: %s", m, formatOutcomes(res.Outcomes))
	case 1:
		return matches[0]
	default:
		t.Fatalf("expected exactly one outcome matching %+v, got %d: %s", m, len(matches), formatOutcomes(matches))
	}
	return Outcome{}
}

// AssertSuccess fails the test if exit code is non-zero or _meta is
// missing. Use immediately after Run for any scenario where the run
// must complete cleanly.
func (res RunResult) AssertSuccess(t *testing.T) {
	t.Helper()
	if res.ExitCode != 0 {
		t.Fatalf("calendar-sync run exited %d\nstderr: %s", res.ExitCode, res.Stderr)
	}
	if res.Meta == nil {
		t.Fatalf("calendar-sync run produced no _meta trailer (stderr: %s)", res.Stderr)
	}
}

func formatOutcomes(outs []Outcome) string {
	if len(outs) == 0 {
		return "(none)"
	}
	var b strings.Builder
	for _, o := range outs {
		fmt.Fprintf(&b, "\n  %+v", o)
	}
	return b.String()
}

// randomHex returns a 2*n-char lowercase hex string from crypto/rand.
// Used to make per-test event titles collision-resistant.
func randomHex(t *testing.T, n int) string {
	t.Helper()
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("crypto/rand.Read: %v", err)
	}
	return hex.EncodeToString(b)
}
