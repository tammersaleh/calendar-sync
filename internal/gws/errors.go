package gws

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Calendar-sync error codes. Each maps to a SPEC.md "Error Conditions" row;
// callers check via errors.Is(err, gws.ErrFoo). The CLI exit code each one
// surfaces as is on Error.ExitCode.
//
// CodeAPIGone is the sentinel the sync layer uses to drive the SPEC's
// "410 GONE recovery" path: when a syncToken expires (typical after a
// laptop sleeps past Google's ~7-day token TTL), events.list returns 410
// and the daemon must run an immediate full re-sync to mint a fresh
// token. SPEC.md "Error Conditions" doesn't enumerate 410 because it's a
// sync-layer recovery rather than a user-visible exit code; we still
// expose it as a typed error so the recovery path keys off a sentinel
// instead of pattern-matching error strings.
const (
	CodeGWSAuthFailed     = "gws_auth_failed"
	CodeNetworkError      = "network_error"
	CodeRateLimited       = "rate_limited"
	CodeAPIInvalidRequest = "api_invalid_request"
	CodeAPIAuthFailed     = "api_auth_failed"
	CodeAPINotFound       = "api_not_found"
	CodeAPIConflict       = "api_conflict"
	CodeAPIForbidden      = "api_forbidden"
	CodeAPIGone           = "api_gone"
	CodeBackendError      = "backend_error"
)

// Sentinel errors for errors.Is matching. Each is comparison-only (the
// classifier returns NEW *Error values with full context); equality is by
// the Code field via the (e *Error).Is method.
var (
	ErrGWSAuthFailed     = &Error{Code: CodeGWSAuthFailed}
	ErrNetworkError      = &Error{Code: CodeNetworkError}
	ErrRateLimited       = &Error{Code: CodeRateLimited}
	ErrAPIInvalidRequest = &Error{Code: CodeAPIInvalidRequest}
	ErrAPIAuthFailed     = &Error{Code: CodeAPIAuthFailed}
	ErrAPINotFound       = &Error{Code: CodeAPINotFound}
	ErrAPIConflict       = &Error{Code: CodeAPIConflict}
	ErrAPIForbidden      = &Error{Code: CodeAPIForbidden}
	ErrAPIGone           = &Error{Code: CodeAPIGone}
	ErrBackendError      = &Error{Code: CodeBackendError}
)

// Error is the typed failure returned by every gws.* method. Code is the
// calendar-sync error name (a string constant above); ExitCode is the CLI
// exit code SPEC.md tells us to surface to the user. HTTPStatus and Reason
// are populated when stderr carried a parseable Google API error envelope.
// Cause is the human-readable lower-level message gws emitted, preserved
// for surfacing to the user as `cause` in the SPEC's error JSON shape.
type Error struct {
	Code       string
	ExitCode   int
	HTTPStatus int
	Reason     string
	Op         string
	Cause      string
}

// Error implements the error interface. Format optimized for log lines and
// pre-classification debug output; the user-facing `cause` and `hint`
// fields are emitted by the output layer using Code/Cause directly.
func (e *Error) Error() string {
	parts := e.Code
	if e.Op != "" {
		parts = fmt.Sprintf("%s during %s", parts, e.Op)
	}
	if e.HTTPStatus != 0 {
		parts = fmt.Sprintf("%s (HTTP %d", parts, e.HTTPStatus)
		if e.Reason != "" {
			parts = fmt.Sprintf("%s %s", parts, e.Reason)
		}
		parts = parts + ")"
	}
	if e.Cause != "" {
		parts = fmt.Sprintf("%s: %s", parts, e.Cause)
	}
	return parts
}

// Is implements errors.Is matching by Code. Only the Code field is
// compared so callers can errors.Is(err, ErrAPIConflict) regardless of
// the wrapped HTTP/reason context.
func (e *Error) Is(target error) bool {
	var t *Error
	if !errors.As(target, &t) {
		return false
	}
	return e.Code == t.Code
}

// classifyError converts a non-zero gws subprocess result into a typed
// *Error per SPEC.md "Error Conditions" / "gws subprocess error mapping".
// Returns nil if exitCode is 0.
//
// Mapping order:
//  1. The gws-subprocess exit codes 2/3/4 are unambiguous (auth, network,
//     rate). They map directly without consulting stderr.
//  2. Any other non-zero exit (typically 1) is an API-layer failure. Try
//     to parse stderr as a Google Calendar API error envelope; if found,
//     map by HTTPStatus and Reason. Otherwise fall back to
//     api_invalid_request.
func classifyError(stdout, stderr []byte, exitCode int, op string) error {
	if exitCode == 0 {
		return nil
	}

	switch exitCode {
	case 2:
		return &Error{
			Code:     CodeGWSAuthFailed,
			ExitCode: 2,
			Op:       op,
			Cause:    parseStderrCause(stderr),
		}
	case 3:
		return &Error{
			Code:     CodeNetworkError,
			ExitCode: 4,
			Op:       op,
			Cause:    parseStderrCause(stderr),
		}
	case 4:
		return &Error{
			Code:     CodeRateLimited,
			ExitCode: 3,
			Op:       op,
			Cause:    parseStderrCause(stderr),
		}
	}

	apiErr, ok := parseAPIError(stderr)
	if !ok {
		return &Error{
			Code:     CodeAPIInvalidRequest,
			ExitCode: 1,
			Op:       op,
			Cause:    string(stderr),
		}
	}
	return mapAPIError(apiErr, op)
}

// gwsAPIErrorEnvelope mirrors the Calendar API's standard error JSON shape
// that gws forwards on stderr:
//
//	{"error":{"code":404,"message":"Not Found","errors":[{"reason":"notFound"}]}}
//
// The fields the wrapper consults are the top-level code and the first
// errors[].reason. Extra fields are ignored.
type gwsAPIErrorEnvelope struct {
	Error struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Errors  []struct {
			Reason string `json:"reason"`
		} `json:"errors"`
	} `json:"error"`
}

// parseAPIError tries to decode stderr as a Google API error envelope.
// Returns (nil, false) on any decode failure or if the envelope is empty
// (zero HTTP code).
func parseAPIError(stderr []byte) (*gwsAPIErrorEnvelope, bool) {
	var env gwsAPIErrorEnvelope
	if err := json.Unmarshal(stderr, &env); err != nil {
		return nil, false
	}
	if env.Error.Code == 0 {
		return nil, false
	}
	return &env, true
}

// parseStderrCause returns the message field from a parseable Calendar API
// error envelope, or the raw stderr if unparseable. Used as the Cause on
// non-API exits (auth/network/rate) where we still want to surface
// whatever gws said.
func parseStderrCause(stderr []byte) string {
	if env, ok := parseAPIError(stderr); ok && env.Error.Message != "" {
		return env.Error.Message
	}
	return string(stderr)
}

// mapAPIError applies SPEC.md's API-layer error mapping. The 403 branch
// distinguishes rate-limit reasons (which retry) from generic forbidden
// (which doesn't); the 410 branch fires the sync layer's full-re-sync
// recovery; everything else maps by HTTP status alone.
func mapAPIError(env *gwsAPIErrorEnvelope, op string) *Error {
	reason := ""
	if len(env.Error.Errors) > 0 {
		reason = env.Error.Errors[0].Reason
	}
	e := &Error{
		HTTPStatus: env.Error.Code,
		Reason:     reason,
		Op:         op,
		Cause:      env.Error.Message,
	}

	switch env.Error.Code {
	case 400:
		e.Code = CodeAPIInvalidRequest
		e.ExitCode = 1
	case 401:
		// Per Google Calendar API docs, all 401 responses carry
		// reason=authError; we don't need to inspect it.
		e.Code = CodeAPIAuthFailed
		e.ExitCode = 2
	case 403:
		// Per Google Calendar API docs, 403 reasons are limited to
		// rateLimitExceeded, userRateLimitExceeded, quotaExceeded, and
		// forbiddenForNonOrganizer. The first two map to rate-limit
		// retries; everything else is plain api_forbidden. (SPEC's
		// "403 with auth-related reason" path is documented but Google
		// doesn't actually emit auth reasons on 403; if that ever
		// changes a future case can be added.)
		switch reason {
		case "rateLimitExceeded", "userRateLimitExceeded":
			e.Code = CodeRateLimited
			e.ExitCode = 3
		default:
			e.Code = CodeAPIForbidden
			e.ExitCode = 1
		}
	case 404:
		e.Code = CodeAPINotFound
		e.ExitCode = 1
	case 409:
		e.Code = CodeAPIConflict
		e.ExitCode = 1
	case 410:
		// SPEC.md "Daemon lifecycle: per-tick reconciliation" → "410
		// GONE recovery": the in-memory syncToken is invalid (typical
		// after a long laptop sleep crossing Google's ~7-day TTL).
		// The sync layer detects this sentinel and triggers an
		// immediate full re-sync for the affected source. Both
		// documented 410 reasons (fullSyncRequired and
		// updatedMinTooLongAgo) map here.
		e.Code = CodeAPIGone
		e.ExitCode = 1
	case 429:
		e.Code = CodeRateLimited
		e.ExitCode = 3
	case 500, 503:
		e.Code = CodeBackendError
		e.ExitCode = 1
	default:
		e.Code = CodeAPIInvalidRequest
		e.ExitCode = 1
	}

	return e
}
