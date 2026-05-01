package launchd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Sentinel errors returned by Install. Each maps to a row in SPEC §"calendar-sync
// install" "Errors" table (lines 794-799) - the cmd layer wraps these to
// emit the matching error code + exit code.
//
//   - ErrNotMacOS         → not_macos             (exit 1)
//   - ErrPlistExists      → plist_exists          (exit 1)
//   - ErrLaunchctlFailed  → launchctl_failed      (exit 1)
//   - ErrBinaryNotResolvable → binary_not_resolvable (exit 1)
var (
	ErrNotMacOS            = errors.New("launchd: not running on macOS")
	ErrPlistExists         = errors.New("launchd: plist file already exists")
	ErrLaunchctlFailed     = errors.New("launchd: launchctl command failed")
	ErrBinaryNotResolvable = errors.New("launchd: cannot resolve calendar-sync binary path")
)

// Config is Install's input. Zero-valued fields are filled in from SPEC's
// defaults.
type Config struct {
	// Label is the launchd Label written into the plist filename + body.
	// Default: DefaultLabel ("org.calendar-sync.agent").
	Label string

	// LogDir is the directory where launchd will redirect the daemon's
	// stdout / stderr. Created (mkdir -p) by Install if it doesn't exist.
	// Default: ~/Library/Logs/calendar-sync. Leading "~" is expanded to
	// the user's home.
	LogDir string

	// Force overwrites an existing plist at the target path. Without it,
	// Install returns ErrPlistExists.
	Force bool

	// NoLoad skips the `launchctl load -w` step. The plist is still
	// written. Useful for first-time installs where the user wants to
	// inspect the file before activation.
	NoLoad bool

	// PATH overrides the EnvironmentVariables.PATH baked into the plist.
	// Default: DefaultPATH. Exposed mainly for tests; production callers
	// can leave it blank.
	PATH string

	// BinaryPath overrides the calendar-sync binary path written into
	// ProgramArguments. Empty resolves via os.Executable. Tests pass an
	// explicit value to avoid depending on os.Executable's behavior.
	BinaryPath string
}

// InstallResult is the data Install hands back to the cmd layer for the
// JSONL output line shown in SPEC line 760:
// `{"plist":"...","loaded":true}`.
type InstallResult struct {
	// PlistPath is the absolute path to the written plist file.
	PlistPath string

	// Loaded reports whether `launchctl load -w` was run successfully.
	// False when Config.NoLoad was set.
	Loaded bool
}

// Install carries out SPEC §"calendar-sync install" (lines 746-799):
//
//  1. Refuse on non-Darwin (ErrNotMacOS).
//  2. Resolve the calendar-sync binary path via os.Executable, unless
//     Config.BinaryPath is set (tests).
//  3. Default + expand LogDir; mkdir -p so launchd doesn't fail at
//     redirect.
//  4. Compute plist path; refuse if it exists and Force is false.
//  5. Render the plist with the resolved inputs and write 0644.
//  6. Unless NoLoad, run `launchctl load -w <plist>` via runner. A non-zero
//     exit returns ErrLaunchctlFailed wrapping stderr.
//
// runner must be non-nil unless cfg.NoLoad is true. Production callers
// pass ExecRunner{}. Tests pass a hand-rolled stub.
func Install(ctx context.Context, cfg Config, runner Runner) (InstallResult, error) {
	if !isDarwin() {
		return InstallResult{}, fmt.Errorf("%w: GOOS=%s", ErrNotMacOS, goos())
	}

	if cfg.Label == "" {
		cfg.Label = DefaultLabel
	}
	if cfg.PATH == "" {
		cfg.PATH = DefaultPATH
	}

	binaryPath := cfg.BinaryPath
	if binaryPath == "" {
		resolved, err := os.Executable()
		if err != nil {
			return InstallResult{}, fmt.Errorf("%w: %w", ErrBinaryNotResolvable, err)
		}
		binaryPath = resolved
	}

	logDir := cfg.LogDir
	if logDir == "" {
		def, err := defaultLogDir()
		if err != nil {
			return InstallResult{}, err
		}
		logDir = def
	} else {
		expanded, err := expandHome(logDir)
		if err != nil {
			return InstallResult{}, err
		}
		logDir = expanded
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("create log dir %q: %w", logDir, err)
	}

	agentsDir, err := launchAgentsDir()
	if err != nil {
		return InstallResult{}, err
	}
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		return InstallResult{}, fmt.Errorf("create LaunchAgents dir %q: %w", agentsDir, err)
	}
	plistPath := filepath.Join(agentsDir, plistFilename(cfg.Label))

	if !cfg.Force {
		if _, err := os.Stat(plistPath); err == nil {
			return InstallResult{}, fmt.Errorf("%w: %s", ErrPlistExists, plistPath)
		} else if !errors.Is(err, os.ErrNotExist) {
			return InstallResult{}, fmt.Errorf("stat plist %q: %w", plistPath, err)
		}
	}

	rendered, err := renderPlist(plistInputs{
		Label:      cfg.Label,
		BinaryPath: binaryPath,
		StdoutPath: filepath.Join(logDir, stdoutLogName),
		StderrPath: filepath.Join(logDir, stderrLogName),
		PATH:       cfg.PATH,
	})
	if err != nil {
		return InstallResult{}, err
	}

	if err := os.WriteFile(plistPath, rendered, 0o644); err != nil {
		return InstallResult{}, fmt.Errorf("write plist %q: %w", plistPath, err)
	}
	// os.WriteFile honors the process umask, which can clamp 0644 down
	// to 0640 or stricter on systems with non-default umasks. Force
	// 0644 explicitly so the file is reliably world-readable for any
	// debugger / launchd inspector who inspects ~/Library/LaunchAgents.
	if err := os.Chmod(plistPath, 0o644); err != nil {
		return InstallResult{}, fmt.Errorf("chmod plist %q: %w", plistPath, err)
	}

	if cfg.NoLoad {
		return InstallResult{PlistPath: plistPath, Loaded: false}, nil
	}

	if runner == nil {
		return InstallResult{}, fmt.Errorf("launchd: runner is nil and NoLoad is false")
	}

	_, stderr, code, runErr := runner.Run(ctx, "load", "-w", plistPath)
	if runErr != nil {
		return InstallResult{}, fmt.Errorf("%w: launchctl load: %w", ErrLaunchctlFailed, runErr)
	}
	if code != 0 {
		return InstallResult{}, fmt.Errorf("%w: launchctl load exit %d: %s", ErrLaunchctlFailed, code, trimStderr(stderr))
	}
	return InstallResult{PlistPath: plistPath, Loaded: true}, nil
}

// trimStderr returns stderr with trailing whitespace stripped. Used in
// error messages so multi-line launchctl output doesn't end with an
// awkward trailing newline.
func trimStderr(b []byte) string {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r' || b[len(b)-1] == ' ' || b[len(b)-1] == '\t') {
		b = b[:len(b)-1]
	}
	return string(b)
}
