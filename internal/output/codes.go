package output

// Error codes are the calendar-sync stable identifiers SPEC §"Error
// Conditions" (lines 1212-1245) catalogues. Every code that can land in
// the `error` field of the JSON envelope on stderr is listed here.
//
// Some codes overlap with the `gws` package's own constants (e.g.
// CodeAPIConflict). Callers in the cmd/ layer translate gws.Error.Code
// strings into these directly - the strings are equal by design - rather
// than crossing package boundaries to share constants.
const (
	CodeConfigNotFound             = "config_not_found"
	CodeConfigInvalid              = "config_invalid"
	CodeConfigExists               = "config_exists"
	CodePairNotFound               = "pair_not_found"
	CodePDirNotFound               = "pdir_not_found"
	CodeCalendarNotFound           = "calendar_not_found"
	CodeCalendarCanonicalizeFailed = "calendar_canonicalize_failed"
	CodeAccessRoleInsufficient     = "access_role_insufficient"
	CodeSelectorRequired           = "selector_required"
	CodeConfirmationRequired       = "confirmation_required"
	CodeGWSNotFound                = "gws_not_found"
	CodeGWSAuthFailed              = "gws_auth_failed"
	CodeAPIAuthFailed              = "api_auth_failed"
	CodeAPIInvalidRequest          = "api_invalid_request"
	CodeAPINotFound                = "api_not_found"
	CodeAPIConflict                = "api_conflict"
	CodeAPIForbidden               = "api_forbidden"
	CodeRateLimited                = "rate_limited"
	CodeBackendError               = "backend_error"
	CodeNetworkError               = "network_error"
	CodePartialFailure             = "partial_failure"
	CodeNotMacOS                   = "not_macos"
	CodePlistExists                = "plist_exists"
	CodePlistNotFound              = "plist_not_found"
	CodeLaunchctlFailed            = "launchctl_failed"
	CodeBinaryNotResolvable        = "binary_not_resolvable"
	CodeWriteFailed                = "write_failed"
	CodeTimeout                    = "timeout"
	CodeDaemonAlreadyRunning       = "daemon_already_running"
	CodeSocketError                = "socket_error"
)

// ExitCodeFor maps an error code to SPEC's CLI exit code (lines 388-398).
// Unrecognized codes return 1, which is SPEC's "general error" default.
//
// SPEC's exit-code table only differentiates four codes from the default:
// auth (2), rate (3), network (4), and a daemon-already-running clash
// (5). Usage flag-validation errors (exit 64) are produced by kong before
// any output-layer code runs and so don't appear in this mapping.
func ExitCodeFor(code string) int {
	switch code {
	case CodeGWSAuthFailed, CodeAPIAuthFailed:
		return 2
	case CodeRateLimited:
		return 3
	case CodeNetworkError:
		return 4
	case CodeDaemonAlreadyRunning:
		return 5
	default:
		return 1
	}
}
