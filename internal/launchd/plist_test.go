package launchd

import (
	"encoding/xml"
	"strings"
	"testing"
)

// TestRenderPlist_HappyPath renders the SPEC template with sample inputs
// and asserts the resulting bytes match SPEC §"calendar-sync install"
// example (lines 766-787) once the substitutions are applied.
func TestRenderPlist_HappyPath(t *testing.T) {
	got, err := renderPlist(plistInputs{
		Label:      "org.calendar-sync.agent",
		BinaryPath: "/usr/local/bin/calendar-sync",
		StdoutPath: "/Users/alice/Library/Logs/calendar-sync/calendar-sync.out.log",
		StderrPath: "/Users/alice/Library/Logs/calendar-sync/calendar-sync.err.log",
		PATH:       DefaultPATH,
	})
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}

	want := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key><string>org.calendar-sync.agent</string>
    <key>ProgramArguments</key>
    <array>
        <string>/usr/local/bin/calendar-sync</string>
        <string>watch</string>
    </array>
    <key>RunAtLoad</key><true/>
    <key>KeepAlive</key><true/>
    <key>ProcessType</key><string>Interactive</string>
    <key>StandardOutPath</key><string>/Users/alice/Library/Logs/calendar-sync/calendar-sync.out.log</string>
    <key>StandardErrorPath</key><string>/Users/alice/Library/Logs/calendar-sync/calendar-sync.err.log</string>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>
</dict>
</plist>
`
	if string(got) != want {
		t.Errorf("renderPlist output mismatch\n--got--\n%s\n--want--\n%s", got, want)
	}
}

// TestRenderPlist_ParsesAsValidXML proves the rendered output is
// well-formed XML. launchctl is fairly forgiving but we're better off
// catching template errors that produce malformed output here than at
// install time.
func TestRenderPlist_ParsesAsValidXML(t *testing.T) {
	got, err := renderPlist(plistInputs{
		Label:      "org.example.agent",
		BinaryPath: "/path/to/bin",
		StdoutPath: "/tmp/out.log",
		StderrPath: "/tmp/err.log",
		PATH:       "/usr/bin",
	})
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}

	dec := xml.NewDecoder(strings.NewReader(string(got)))
	for {
		tok, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("xml decode: %v", err)
		}
		_ = tok
	}
}

// TestRenderPlist_RejectsEmptyFields verifies the validator catches each
// required field. An empty Label etc. would produce a plist that
// launchctl rejects on load - better to fail with a clear Go error.
func TestRenderPlist_RejectsEmptyFields(t *testing.T) {
	base := plistInputs{
		Label:      "L",
		BinaryPath: "B",
		StdoutPath: "O",
		StderrPath: "E",
		PATH:       "P",
	}
	cases := []struct {
		name string
		mut  func(*plistInputs)
	}{
		{"label empty", func(p *plistInputs) { p.Label = "" }},
		{"binary empty", func(p *plistInputs) { p.BinaryPath = "" }},
		{"stdout empty", func(p *plistInputs) { p.StdoutPath = "" }},
		{"stderr empty", func(p *plistInputs) { p.StderrPath = "" }},
		{"path empty", func(p *plistInputs) { p.PATH = "" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base
			tc.mut(&in)
			if _, err := renderPlist(in); err == nil {
				t.Errorf("renderPlist with %s should error, got nil", tc.name)
			}
		})
	}
}

// TestRenderPlist_SubstitutesAllFields renders with distinct sentinel
// values for every field and confirms each one appears verbatim in the
// output. Catches future template typos that misroute a field.
func TestRenderPlist_SubstitutesAllFields(t *testing.T) {
	got, err := renderPlist(plistInputs{
		Label:      "LBL_SENTINEL",
		BinaryPath: "BIN_SENTINEL",
		StdoutPath: "OUT_SENTINEL",
		StderrPath: "ERR_SENTINEL",
		PATH:       "PATH_SENTINEL",
	})
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}
	for _, want := range []string{
		"LBL_SENTINEL",
		"BIN_SENTINEL",
		"OUT_SENTINEL",
		"ERR_SENTINEL",
		"PATH_SENTINEL",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("output missing %q", want)
		}
	}
}
