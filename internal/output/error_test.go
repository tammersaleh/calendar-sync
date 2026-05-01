package output

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestEmitError_FullEnvelope exercises the SPEC §"Output and Logging"
// (line 384) example: all four fields populated render in the right
// order with a trailing newline.
func TestEmitError_FullEnvelope(t *testing.T) {
	var buf bytes.Buffer
	EmitError(&buf, ErrorEnvelope{
		Error:  "config_invalid",
		Detail: "pair 'work-personal' has invalid direction 'left_to_right'",
		Hint:   "direction must be one of source_to_target, target_to_source, bidirectional",
		Cause:  "validation error at pairs[0].direction",
	})

	got := buf.String()
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("output should end with newline; got %q", got)
	}
	if strings.Count(got, "\n") != 1 {
		t.Errorf("expected exactly one newline; got %d (output=%q)",
			strings.Count(got, "\n"), got)
	}

	var env ErrorEnvelope
	if err := json.Unmarshal([]byte(strings.TrimSpace(got)), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Error != "config_invalid" {
		t.Errorf("Error = %q, want config_invalid", env.Error)
	}
	if env.Detail != "pair 'work-personal' has invalid direction 'left_to_right'" {
		t.Errorf("Detail = %q", env.Detail)
	}
	if !strings.Contains(env.Hint, "must be one of") {
		t.Errorf("Hint = %q", env.Hint)
	}
	if env.Cause != "validation error at pairs[0].direction" {
		t.Errorf("Cause = %q", env.Cause)
	}
}

// TestEmitError_OmitsCauseWhenEmpty pins the omitempty contract: with
// an empty Cause field, the `cause` key must not appear in the JSON.
// SPEC line 385 calls cause "wrapped, optional".
func TestEmitError_OmitsCauseWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	EmitError(&buf, ErrorEnvelope{
		Error:  "config_not_found",
		Detail: "no config at any search path",
		Hint:   "run `calendar-sync init` first",
	})
	got := buf.String()
	if strings.Contains(got, `"cause"`) {
		t.Errorf("output should not contain cause key when empty: %q", got)
	}
	// Hint is still there.
	if !strings.Contains(got, `"hint"`) {
		t.Errorf("output should contain hint key when set: %q", got)
	}
}

// TestEmitError_OmitsHintWhenEmpty: hint is omitempty too. Some error
// paths (e.g. internal bugs) won't have actionable remediation.
func TestEmitError_OmitsHintWhenEmpty(t *testing.T) {
	var buf bytes.Buffer
	EmitError(&buf, ErrorEnvelope{
		Error:  "api_invalid_request",
		Detail: "calendar API returned 400",
	})
	got := buf.String()
	if strings.Contains(got, `"hint"`) {
		t.Errorf("output should not contain hint key when empty: %q", got)
	}
	if strings.Contains(got, `"cause"`) {
		t.Errorf("output should not contain cause key when empty: %q", got)
	}
}

// TestEmitError_ErrorAndDetailAlwaysPresent: error and detail have no
// omitempty, so even empty strings show as keys. That's intentional -
// SPEC defines them as required, and a missing key would silently mask
// a buggy callsite that forgot to populate them.
func TestEmitError_ErrorAndDetailAlwaysPresent(t *testing.T) {
	var buf bytes.Buffer
	EmitError(&buf, ErrorEnvelope{})
	got := buf.String()
	if !strings.Contains(got, `"error"`) {
		t.Errorf("error key must always be present: %q", got)
	}
	if !strings.Contains(got, `"detail"`) {
		t.Errorf("detail key must always be present: %q", got)
	}
}

// TestEmitError_NilWriterIsSafe defends against a nil writer. The CLI
// path always passes os.Stderr, but tests and library callers may not.
func TestEmitError_NilWriterIsSafe(t *testing.T) {
	// Reaching this point without panic is the assertion.
	EmitError(nil, ErrorEnvelope{Error: "x", Detail: "y"})
}
