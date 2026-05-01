package launchd

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// goos returns the current GOOS. It's a function variable so tests can
// stub the value without resorting to build tags - SPEC's exit code for
// `not_macos` is reachable from any test platform that way.
var goos = func() string { return runtime.GOOS }

// isDarwin reports whether the current platform is macOS. SPEC §"calendar-sync
// install" / §"uninstall" both list `not_macos` as exit-code-1 when this
// is false.
func isDarwin() bool {
	return goos() == "darwin"
}

// expandHome replaces a leading "~" or "~/" in path with the user's home
// directory. A path that doesn't start with "~" is returned unchanged.
//
// Returns an error if path begins with "~" but the home directory can't
// be resolved - SPEC §"calendar-sync install" wires LogDir and
// LaunchAgents under ~ by default, so a bad home is fatal for install.
func expandHome(path string) (string, error) {
	if path == "" {
		return path, nil
	}
	if path == "~" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		return home, nil
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(path, "~/")), nil
	}
	return path, nil
}

// launchAgentsDir returns the absolute path to ~/Library/LaunchAgents.
// SPEC line 760 shows the per-user LaunchAgents directory as the install
// target.
func launchAgentsDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents"), nil
}

// defaultLogDir returns the default LogDir per SPEC line 752:
// `~/Library/Logs/calendar-sync/`.
func defaultLogDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return filepath.Join(home, "Library", "Logs", "calendar-sync"), nil
}

// plistFilename returns the per-Label plist filename: "<Label>.plist".
// Used to compute the absolute plist path under launchAgentsDir.
func plistFilename(label string) string {
	return label + ".plist"
}
