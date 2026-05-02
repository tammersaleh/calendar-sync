package gws

import (
	"errors"
	"strings"
	"testing"
)

// Tests in this file are package-internal (not gws_test) because they
// exercise classifyError directly. CLAUDE.md "Testing" section permits
// this for functions only accessible within the package.

func TestClassifyError_ZeroExitReturnsNil(t *testing.T) {
	if got := classifyError(nil, nil, 0, "events.list"); got != nil {
		t.Fatalf("classifyError(exit=0) = %v, want nil", got)
	}
}

func TestClassifyError_GWSExitCodeMapping(t *testing.T) {
	tests := []struct {
		name         string
		exit         int
		wantCode     string
		wantExitCode int
		wantSentinel *Error
	}{
		{"gws exit 2 -> gws_auth_failed", 2, CodeGWSAuthFailed, 2, ErrGWSAuthFailed},
		{"gws exit 3 -> network_error (cli exit 4)", 3, CodeNetworkError, 4, ErrNetworkError},
		{"gws exit 4 -> rate_limited (cli exit 3)", 4, CodeRateLimited, 3, ErrRateLimited},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyError(nil, []byte(`{"error":{"code":401}}`), tc.exit, "events.list")
			if err == nil {
				t.Fatal("classifyError returned nil; want typed Error")
			}
			var gws *Error
			if !errors.As(err, &gws) {
				t.Fatalf("error not *Error: %v", err)
			}
			if gws.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", gws.Code, tc.wantCode)
			}
			if gws.ExitCode != tc.wantExitCode {
				t.Errorf("ExitCode = %d, want %d", gws.ExitCode, tc.wantExitCode)
			}
			if !errors.Is(err, tc.wantSentinel) {
				t.Errorf("errors.Is(err, %v) = false; want true", tc.wantSentinel.Code)
			}
			if gws.Op != "events.list" {
				t.Errorf("Op = %q, want events.list", gws.Op)
			}
		})
	}
}

func TestClassifyError_GWSExitTakesPrecedenceOverStderrJSON(t *testing.T) {
	// gws exit 2 means "auth failed" regardless of what stderr says.
	// Don't let a stale 404 envelope on stderr mask a fresh auth failure.
	stderr := []byte(`{"error":{"code":404,"message":"old error"}}`)
	err := classifyError(nil, stderr, 2, "events.list")

	if !errors.Is(err, ErrGWSAuthFailed) {
		t.Fatalf("expected gws_auth_failed (exit-code wins); got %v", err)
	}
	var gws *Error
	if errors.As(err, &gws) && gws.HTTPStatus != 0 {
		t.Errorf("HTTPStatus should be 0 on gws-level error; got %d", gws.HTTPStatus)
	}
}

func TestClassifyError_APIErrorMappingByHTTPStatus(t *testing.T) {
	tests := []struct {
		name         string
		stderr       string
		wantCode     string
		wantExitCode int
		wantSentinel *Error
	}{
		{
			"400 bad request",
			`{"error":{"code":400,"message":"bad request"}}`,
			CodeAPIInvalidRequest, 1, ErrAPIInvalidRequest,
		},
		{
			"401 unauthorized",
			`{"error":{"code":401,"message":"unauthorized"}}`,
			CodeAPIAuthFailed, 2, ErrAPIAuthFailed,
		},
		{
			"403 rateLimitExceeded -> rate_limited",
			`{"error":{"code":403,"errors":[{"reason":"rateLimitExceeded"}]}}`,
			CodeRateLimited, 3, ErrRateLimited,
		},
		{
			"403 userRateLimitExceeded -> rate_limited",
			`{"error":{"code":403,"errors":[{"reason":"userRateLimitExceeded"}]}}`,
			CodeRateLimited, 3, ErrRateLimited,
		},
		{
			"403 generic forbidden -> api_forbidden",
			`{"error":{"code":403,"errors":[{"reason":"forbidden"}]}}`,
			CodeAPIForbidden, 1, ErrAPIForbidden,
		},
		{
			"404 not found",
			`{"error":{"code":404,"message":"not found"}}`,
			CodeAPINotFound, 1, ErrAPINotFound,
		},
		{
			"409 conflict (duplicate insert)",
			`{"error":{"code":409,"errors":[{"reason":"duplicate"}]}}`,
			CodeAPIConflict, 1, ErrAPIConflict,
		},
		{
			"410 gone fullSyncRequired -> api_gone",
			`{"error":{"code":410,"errors":[{"reason":"fullSyncRequired"}]}}`,
			CodeAPIGone, 1, ErrAPIGone,
		},
		{
			"410 gone updatedMinTooLongAgo -> api_gone",
			`{"error":{"code":410,"errors":[{"reason":"updatedMinTooLongAgo"}]}}`,
			CodeAPIGone, 1, ErrAPIGone,
		},
		{
			"429 too many requests",
			`{"error":{"code":429,"message":"slow down"}}`,
			CodeRateLimited, 3, ErrRateLimited,
		},
		{
			"500 internal server error",
			`{"error":{"code":500,"message":"oops"}}`,
			CodeBackendError, 1, ErrBackendError,
		},
		{
			"503 service unavailable",
			`{"error":{"code":503,"message":"down"}}`,
			CodeBackendError, 1, ErrBackendError,
		},
		{
			"unknown HTTP code falls back to api_invalid_request",
			`{"error":{"code":418,"message":"teapot"}}`,
			CodeAPIInvalidRequest, 1, ErrAPIInvalidRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyError(nil, []byte(tc.stderr), 1, "events.insert")
			if err == nil {
				t.Fatal("classifyError returned nil; want typed Error")
			}
			var gws *Error
			if !errors.As(err, &gws) {
				t.Fatalf("error not *Error: %v", err)
			}
			if gws.Code != tc.wantCode {
				t.Errorf("Code = %q, want %q", gws.Code, tc.wantCode)
			}
			if gws.ExitCode != tc.wantExitCode {
				t.Errorf("ExitCode = %d, want %d", gws.ExitCode, tc.wantExitCode)
			}
			if !errors.Is(err, tc.wantSentinel) {
				t.Errorf("errors.Is(err, %s) = false; want true", tc.wantSentinel.Code)
			}
		})
	}
}

func TestClassifyError_UnparseableStderrFallsBackToInvalidRequest(t *testing.T) {
	tests := []struct {
		name   string
		stderr string
	}{
		{"empty stderr", ""},
		{"plain text stderr", "something went wrong"},
		{"valid JSON without error envelope", `{"thing":"stuff"}`},
		{"truncated JSON", `{"error":{"code"`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyError(nil, []byte(tc.stderr), 1, "op")
			if !errors.Is(err, ErrAPIInvalidRequest) {
				t.Errorf("expected api_invalid_request fallback; got %v", err)
			}
		})
	}
}

// TestClassifyError_ParsesAPIErrorFromStdout pins B13: gws emits the
// Calendar API error envelope on STDOUT (not stderr), with stderr getting
// only the human-readable summary like "Using keyring backend: keyring\n
// error[api]: <message>\n". Pre-fix, classifyError parsed stderr and
// found no JSON, falling through to api_invalid_request - which masked
// every 409/410/etc. The masked 409s broke SPEC's cancelled-and-revived
// flow (it triggers on errors.Is(err, ErrAPIConflict)).
//
// Verify: feed both streams as gws would emit them, and check the
// resulting error is the right HTTP-status-derived sentinel.
func TestClassifyError_ParsesAPIErrorFromStdout(t *testing.T) {
	tests := []struct {
		name         string
		stdout       string
		stderr       string
		wantSentinel *Error
	}{
		{
			name:         "409 duplicate from events.insert (cancelled-and-revived trigger)",
			stdout:       `{"error":{"code":409,"message":"The requested identifier already exists.","reason":"duplicate"}}`,
			stderr:       "Using keyring backend: keyring\nerror[api]: The requested identifier already exists.\n",
			wantSentinel: ErrAPIConflict,
		},
		{
			name:         "400 Resource has been deleted from events.delete on tombstone",
			stdout:       `{"error":{"code":400,"message":"Resource has been deleted","reason":"deleted"}}`,
			stderr:       "Using keyring backend: keyring\nerror[api]: Resource has been deleted\n",
			wantSentinel: ErrAPIInvalidRequest,
		},
		{
			name:         "410 syncToken expiry",
			stdout:       `{"error":{"code":410,"message":"Sync token is no longer valid","errors":[{"reason":"fullSyncRequired"}]}}`,
			stderr:       "Using keyring backend: keyring\nerror[api]: Sync token is no longer valid\n",
			wantSentinel: ErrAPIGone,
		},
		{
			name:         "404 not found",
			stdout:       `{"error":{"code":404,"message":"Not Found"}}`,
			stderr:       "Using keyring backend: keyring\nerror[api]: Not Found\n",
			wantSentinel: ErrAPINotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyError([]byte(tc.stdout), []byte(tc.stderr), 1, "events.insert")
			if !errors.Is(err, tc.wantSentinel) {
				t.Errorf("errors.Is(err, %s) = false; got err=%v", tc.wantSentinel.Code, err)
			}
		})
	}
}

func TestClassifyError_PreservesHTTPStatusAndReason(t *testing.T) {
	stderr := []byte(`{"error":{"code":403,"message":"daily limit","errors":[{"reason":"rateLimitExceeded"}]}}`)
	err := classifyError(nil, stderr, 1, "events.list")

	var gws *Error
	if !errors.As(err, &gws) {
		t.Fatalf("error not *Error: %v", err)
	}
	if gws.HTTPStatus != 403 {
		t.Errorf("HTTPStatus = %d, want 403", gws.HTTPStatus)
	}
	if gws.Reason != "rateLimitExceeded" {
		t.Errorf("Reason = %q, want rateLimitExceeded", gws.Reason)
	}
	if gws.Cause != "daily limit" {
		t.Errorf("Cause = %q, want 'daily limit'", gws.Cause)
	}
}

func TestClassifyError_GWSErrorsCarryStderrCause(t *testing.T) {
	// Even on auth/network/rate exits, the cause from stderr should be
	// preserved so the user-facing error JSON has a useful 'cause' field.
	stderr := []byte(`{"error":{"code":401,"message":"token revoked"}}`)
	err := classifyError(nil, stderr, 2, "events.list")

	var gws *Error
	if !errors.As(err, &gws) {
		t.Fatalf("not a typed error: %v", err)
	}
	if !strings.Contains(gws.Cause, "token revoked") {
		t.Errorf("Cause = %q, want it to contain 'token revoked'", gws.Cause)
	}
}

func TestError_IsMatchesByCodeOnly(t *testing.T) {
	a := &Error{Code: CodeAPIConflict, HTTPStatus: 409, Op: "events.insert"}
	b := &Error{Code: CodeAPIConflict, HTTPStatus: 0, Op: ""}
	if !errors.Is(a, b) {
		t.Errorf("errors.Is matched only by code; expected true")
	}
	if errors.Is(a, &Error{Code: CodeAPINotFound}) {
		t.Errorf("errors.Is should not match different codes")
	}
	// Non-Error target
	if errors.Is(a, errors.New("plain error")) {
		t.Errorf("errors.Is should not match a plain error")
	}
}

func TestError_ErrorStringIncludesContext(t *testing.T) {
	e := &Error{
		Code:       CodeAPIConflict,
		HTTPStatus: 409,
		Reason:     "duplicate",
		Op:         "events.insert",
		Cause:      "An event with the same ID already exists",
	}
	got := e.Error()
	for _, want := range []string{"api_conflict", "events.insert", "409", "duplicate", "already exists"} {
		if !strings.Contains(got, want) {
			t.Errorf("Error() = %q; want substring %q", got, want)
		}
	}
}
