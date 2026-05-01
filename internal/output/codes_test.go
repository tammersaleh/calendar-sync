package output

import "testing"

// TestCodeConstants pins the SPEC string for every error code. Drift
// here would silently change the wire shape: the constant name lives in
// Go, the string lives in SPEC, and consumers (humans, scripts, the
// `gws` package's CodeAPIConflict mirror) all key off the string.
func TestCodeConstants(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"CodeConfigNotFound", CodeConfigNotFound, "config_not_found"},
		{"CodeConfigInvalid", CodeConfigInvalid, "config_invalid"},
		{"CodeConfigExists", CodeConfigExists, "config_exists"},
		{"CodePairNotFound", CodePairNotFound, "pair_not_found"},
		{"CodePDirNotFound", CodePDirNotFound, "pdir_not_found"},
		{"CodeCalendarNotFound", CodeCalendarNotFound, "calendar_not_found"},
		{"CodeCalendarCanonicalizeFailed", CodeCalendarCanonicalizeFailed, "calendar_canonicalize_failed"},
		{"CodeAccessRoleInsufficient", CodeAccessRoleInsufficient, "access_role_insufficient"},
		{"CodeSelectorRequired", CodeSelectorRequired, "selector_required"},
		{"CodeConfirmationRequired", CodeConfirmationRequired, "confirmation_required"},
		{"CodeGWSNotFound", CodeGWSNotFound, "gws_not_found"},
		{"CodeGWSAuthFailed", CodeGWSAuthFailed, "gws_auth_failed"},
		{"CodeAPIAuthFailed", CodeAPIAuthFailed, "api_auth_failed"},
		{"CodeAPIInvalidRequest", CodeAPIInvalidRequest, "api_invalid_request"},
		{"CodeAPINotFound", CodeAPINotFound, "api_not_found"},
		{"CodeAPIConflict", CodeAPIConflict, "api_conflict"},
		{"CodeAPIForbidden", CodeAPIForbidden, "api_forbidden"},
		{"CodeRateLimited", CodeRateLimited, "rate_limited"},
		{"CodeBackendError", CodeBackendError, "backend_error"},
		{"CodeNetworkError", CodeNetworkError, "network_error"},
		{"CodePartialFailure", CodePartialFailure, "partial_failure"},
		{"CodeNotMacOS", CodeNotMacOS, "not_macos"},
		{"CodePlistExists", CodePlistExists, "plist_exists"},
		{"CodePlistNotFound", CodePlistNotFound, "plist_not_found"},
		{"CodeLaunchctlFailed", CodeLaunchctlFailed, "launchctl_failed"},
		{"CodeBinaryNotResolvable", CodeBinaryNotResolvable, "binary_not_resolvable"},
		{"CodeWriteFailed", CodeWriteFailed, "write_failed"},
		{"CodeTimeout", CodeTimeout, "timeout"},
		{"CodeDaemonAlreadyRunning", CodeDaemonAlreadyRunning, "daemon_already_running"},
		{"CodeSocketError", CodeSocketError, "socket_error"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.got != tc.want {
				t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
			}
		})
	}
}

// TestExitCodeFor pins SPEC §"Output and Logging" (lines 388-398). Auth
// is 2, rate is 3, network is 4, daemon-already-running is 5, everything
// else (including unknown strings) is the general 1.
func TestExitCodeFor(t *testing.T) {
	tests := []struct {
		name string
		code string
		want int
	}{
		{"gws_auth_failed -> 2", CodeGWSAuthFailed, 2},
		{"api_auth_failed -> 2", CodeAPIAuthFailed, 2},
		{"rate_limited -> 3", CodeRateLimited, 3},
		{"network_error -> 4", CodeNetworkError, 4},
		{"daemon_already_running -> 5", CodeDaemonAlreadyRunning, 5},
		{"config_invalid -> 1", CodeConfigInvalid, 1},
		{"timeout -> 1", CodeTimeout, 1},
		{"partial_failure -> 1", CodePartialFailure, 1},
		{"unknown code -> 1", "what_is_this", 1},
		{"empty string -> 1", "", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCodeFor(tc.code); got != tc.want {
				t.Errorf("ExitCodeFor(%q) = %d, want %d", tc.code, got, tc.want)
			}
		})
	}
}
