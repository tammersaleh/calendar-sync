package launchd

import (
	"bytes"
	"fmt"
	"text/template"
)

// DefaultLabel is SPEC line 753's default launchd Label for the agent.
const DefaultLabel = "org.calendar-sync.agent"

// DefaultPATH is the PATH SPEC line 785 hard-codes into the plist's
// EnvironmentVariables. It covers Apple Silicon (/opt/homebrew/bin) and
// Intel (/usr/local/bin) Homebrew prefixes plus the system bin directories.
// `gws` typically lives in one of those, and the daemon spawns it as a
// subprocess - so without a sane PATH the watch command would die at first
// reconciliation tick.
const DefaultPATH = "/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"

// stdoutLogName / stderrLogName are the basenames SPEC §"calendar-sync
// install" example shows under <LogDir>: "calendar-sync.out.log" and
// "calendar-sync.err.log" (lines 780-781).
const (
	stdoutLogName = "calendar-sync.out.log"
	stderrLogName = "calendar-sync.err.log"
)

// plistInputs is the data the plist XML template substitutes into
// SPEC.md's template (lines 766-787). All fields are required; renderPlist
// returns an error if any are empty.
//
// ConfigPath is added for B7 (config.toml auto-reload). The field
// declaration lands ahead of the template + validator wiring so the
// regression test for the WatchPaths directive can compile against the
// final shape; the green-pass commit lights up the rendering and the
// require-non-empty check.
type plistInputs struct {
	Label      string
	BinaryPath string
	StdoutPath string
	StderrPath string
	PATH       string
	ConfigPath string
}

// plistTemplate is SPEC's plist XML verbatim, with `{{.Field}}` placeholders
// for the per-install substitutions. Whitespace (4-space indent inside
// <dict>) matches SPEC's example so a `diff` against the SPEC stays empty.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>{{.Label}}</string>
    <key>ProgramArguments</key>
    <array>
        <string>{{.BinaryPath}}</string>
        <string>watch</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>ProcessType</key><string>Interactive</string>
    <key>StandardOutPath</key><string>{{.StdoutPath}}</string>
    <key>StandardErrorPath</key><string>{{.StderrPath}}</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key><string>{{.PATH}}</string>
    </dict>
</dict>
</plist>
`

// compiledPlistTemplate is parsed once at package init. Parse errors here
// would be a developer bug (the literal above isn't user-supplied), so we
// panic rather than defer the failure to the first install call.
var compiledPlistTemplate = template.Must(template.New("plist").Parse(plistTemplate))

// renderPlist substitutes p into SPEC's plist template and returns the
// rendered XML. Returns an error if any required field is empty - the
// resulting plist would be invalid (e.g. an empty Label produces an
// `<key>Label</key><string></string>` which launchctl rejects).
func renderPlist(p plistInputs) ([]byte, error) {
	if p.Label == "" {
		return nil, fmt.Errorf("plist: Label is empty")
	}
	if p.BinaryPath == "" {
		return nil, fmt.Errorf("plist: BinaryPath is empty")
	}
	if p.StdoutPath == "" {
		return nil, fmt.Errorf("plist: StdoutPath is empty")
	}
	if p.StderrPath == "" {
		return nil, fmt.Errorf("plist: StderrPath is empty")
	}
	if p.PATH == "" {
		return nil, fmt.Errorf("plist: PATH is empty")
	}
	var buf bytes.Buffer
	if err := compiledPlistTemplate.Execute(&buf, p); err != nil {
		return nil, fmt.Errorf("plist: render template: %w", err)
	}
	return buf.Bytes(), nil
}
