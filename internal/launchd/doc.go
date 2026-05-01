// Package launchd implements `calendar-sync install` / `calendar-sync uninstall`
// per SPEC §"calendar-sync install" (lines 746-799) and §"calendar-sync
// uninstall" (lines 801-822). It generates the launchd plist that runs
// `calendar-sync watch`, drops it under `~/Library/LaunchAgents/`, and shells
// out to `launchctl load -w` / `launchctl unload` to register the agent.
//
// Package layout:
//
//   - plist.go - render the SPEC's plist XML template (lines 766-787) into
//     a byte buffer with the caller's substitutions.
//   - install.go - resolve binary path, write the plist file, run launchctl
//     load.
//   - uninstall.go - run launchctl unload, remove the plist file.
//   - runner.go - the Runner interface (and exec-backed implementation) that
//     install/uninstall use to invoke launchctl. Tests pass a hand-rolled
//     stub.
//   - darwin.go - platform guard. runtime.GOOS is read through a swappable
//     function so tests can simulate non-Darwin without build tags.
//
// All exported errors are sentinel values (ErrNotMacOS, ErrPlistExists, etc.)
// matched via errors.Is. The cmd layer maps each one to SPEC's exit-code
// table.
package launchd
