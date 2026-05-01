package launchd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrPlistNotFound is returned by Uninstall when the plist file isn't
// present on disk. SPEC §"calendar-sync uninstall" "Errors" table line 820
// lists this as `plist_not_found` (exit 1).
var ErrPlistNotFound = errors.New("launchd: plist file not found")

// UninstallConfig is Uninstall's input.
type UninstallConfig struct {
	// Label identifies which plist to remove. Default: DefaultLabel
	// ("org.calendar-sync.agent").
	Label string

	// KeepPlist runs `launchctl unload` but leaves the plist file on
	// disk. SPEC line 805 documents this as `--keep-plist`.
	KeepPlist bool
}

// UninstallResult is what the cmd layer formats for SPEC line 811's JSONL:
// `{"plist":"...","unloaded":true,"removed":true}`.
type UninstallResult struct {
	// PlistPath is the absolute path the plist lived at (whether or not
	// it was removed).
	PlistPath string

	// Unloaded is true if `launchctl unload` returned 0 OR returned a
	// "not loaded" error that we treated as success.
	Unloaded bool

	// Removed is true if Uninstall removed the plist file. False when
	// Config.KeepPlist was set.
	Removed bool
}

// Uninstall carries out SPEC §"calendar-sync uninstall" (lines 801-822):
//
//  1. Refuse on non-Darwin (ErrNotMacOS).
//  2. Compute plist path from Label.
//  3. Run `launchctl unload <plist>`. If the plist isn't loaded, launchctl
//     exits non-zero with a recognizable stderr; that's not a real
//     failure - we proceed to file removal. Other non-zero exits return
//     ErrLaunchctlFailed.
//  4. If the plist file isn't on disk, return ErrPlistNotFound.
//  5. Unless KeepPlist, os.Remove the file.
//
// Step ordering matters: we run `launchctl unload` BEFORE checking if the
// plist exists. SPEC's example output shows `unloaded:true,removed:true`
// for the standard case; the unload step refers to launchd's loaded
// services list (managed via the plist *path* but independent of the
// file's existence), so it's still meaningful even if a previous run
// already removed the file.
func Uninstall(ctx context.Context, cfg UninstallConfig, runner Runner) (UninstallResult, error) {
	if !isDarwin() {
		return UninstallResult{}, fmt.Errorf("%w: GOOS=%s", ErrNotMacOS, goos())
	}

	if cfg.Label == "" {
		cfg.Label = DefaultLabel
	}

	agentsDir, err := launchAgentsDir()
	if err != nil {
		return UninstallResult{}, err
	}
	plistPath := filepath.Join(agentsDir, plistFilename(cfg.Label))
	result := UninstallResult{PlistPath: plistPath}

	if runner == nil {
		return result, fmt.Errorf("launchd: runner is nil")
	}
	_, stderr, code, runErr := runner.Run(ctx, "unload", plistPath)
	switch {
	case runErr != nil:
		return result, fmt.Errorf("%w: launchctl unload: %w", ErrLaunchctlFailed, runErr)
	case code == 0:
		result.Unloaded = true
	case isLaunchctlNotLoaded(stderr):
		// SPEC's flow assumes idempotent uninstall: if the agent is
		// already unloaded (or was never loaded), launchctl exits
		// non-zero. We treat that as success, surface unloaded=true so
		// the JSONL output is consistent, and continue to plist removal.
		result.Unloaded = true
	default:
		return result, fmt.Errorf("%w: launchctl unload exit %d: %s", ErrLaunchctlFailed, code, trimStderr(stderr))
	}

	if _, err := os.Stat(plistPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, fmt.Errorf("%w: %s", ErrPlistNotFound, plistPath)
		}
		return result, fmt.Errorf("stat plist %q: %w", plistPath, err)
	}

	if cfg.KeepPlist {
		return result, nil
	}

	if err := os.Remove(plistPath); err != nil {
		return result, fmt.Errorf("remove plist %q: %w", plistPath, err)
	}
	result.Removed = true
	return result, nil
}

// notLoadedMarkers are stderr substrings that indicate launchctl couldn't
// find an active service to unload - effectively "already unloaded". We
// treat those as success so `calendar-sync uninstall` is idempotent.
//
// macOS has shipped two distinct legacy launchctl error wordings over the
// years. "Could not find specified service" is the bsd-launchctl phrasing;
// "no such file or directory" appears when the launchctl(1) `unload`
// subcommand is given a plist whose Label has no current submission.
// Both are matched case-insensitively.
var notLoadedMarkers = [][]byte{
	[]byte("could not find specified service"),
	[]byte("no such file or directory"),
}

// isLaunchctlNotLoaded reports whether stderr from a non-zero `launchctl
// unload` exit indicates the agent simply wasn't loaded - in which case
// we treat the unload step as a no-op success.
func isLaunchctlNotLoaded(stderr []byte) bool {
	low := bytes.ToLower(stderr)
	for _, m := range notLoadedMarkers {
		if bytes.Contains(low, m) {
			return true
		}
	}
	return false
}
