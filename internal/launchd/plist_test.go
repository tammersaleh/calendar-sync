package launchd

import (
	"encoding/xml"
	"strings"
	"testing"
)

// TestRenderPlist_HappyPath renders the SPEC template with sample inputs
// and asserts the resulting bytes match SPEC §"calendar-sync install"
// example (lines 766-787) plus the WatchPaths directive (B7).
func TestRenderPlist_HappyPath(t *testing.T) {
	got, err := renderPlist(plistInputs{
		Label:      "org.calendar-sync.agent",
		BinaryPath: "/usr/local/bin/calendar-sync",
		StdoutPath: "/Users/alice/Library/Logs/calendar-sync/calendar-sync.out.log",
		StderrPath: "/Users/alice/Library/Logs/calendar-sync/calendar-sync.err.log",
		PATH:       DefaultPATH,
		ConfigPath: "/Users/alice/.config/calendar-sync/config.toml",
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
    <key>WatchPaths</key>
    <array>
        <string>/Users/alice/.config/calendar-sync/config.toml</string>
    </array>
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
		ConfigPath: "/tmp/config.toml",
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
		ConfigPath: "C",
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
		{"config empty", func(p *plistInputs) { p.ConfigPath = "" }},
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
		ConfigPath: "CONFIG_SENTINEL",
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
		"CONFIG_SENTINEL",
	} {
		if !strings.Contains(string(got), want) {
			t.Errorf("output missing %q", want)
		}
	}
}

// TestRenderPlist_EscapesXMLMetacharacters pins the XML-safety guarantee:
// when a user's config path or log directory contains an XML
// metacharacter (`&`, `<`, `>`, `'`, `"`), the rendered plist must still
// be valid XML so launchctl can load it. text/template does no escaping
// by default; the renderer registers an xmlEscape template function on
// every substituted field.
func TestRenderPlist_EscapesXMLMetacharacters(t *testing.T) {
	got, err := renderPlist(plistInputs{
		Label:      "org.example.with&amp;char",
		BinaryPath: "/path/with<bracket>/calendar-sync",
		StdoutPath: "/log/Tom & Jerry/out.log",
		StderrPath: "/log/with\"quote/err.log",
		PATH:       "/usr/bin:/path/with'apos",
		ConfigPath: "/conf/with<>&'\".toml",
	})
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}

	// The rendered output must parse as well-formed XML.
	dec := xml.NewDecoder(strings.NewReader(string(got)))
	for {
		_, err := dec.Token()
		if err != nil {
			if err.Error() == "EOF" {
				break
			}
			t.Fatalf("xml decode failed; rendered plist is not valid XML: %v\nrendered:\n%s", err, got)
		}
	}

	// Sanity-check: the ConfigPath's literal `<` `>` `&` `'` `"` characters
	// must NOT appear unescaped between <string>...</string> tags. We look
	// for the entity forms in their place.
	wantSubstring := "&lt;&gt;&amp;&#39;&#34;.toml"
	if !strings.Contains(string(got), wantSubstring) {
		t.Errorf("expected ConfigPath chars escaped to %q in rendered plist; got:\n%s",
			wantSubstring, got)
	}
}

// TestRenderPlist_RoundTripsLabelEntities pins that an already-encoded
// XML entity in the input survives as a literal character in the parsed
// plist, NOT as the entity itself. ("&amp;" in input → "&amp;amp;" in
// output → parses back to "&amp;" - which is wrong.) The escaper's job
// is to be idempotent in the OPPOSITE direction: input is treated as
// raw text. This pins that contract.
func TestRenderPlist_RoundTripsLabelEntities(t *testing.T) {
	got, err := renderPlist(plistInputs{
		Label:      "org.example & co",
		BinaryPath: "/bin/calendar-sync",
		StdoutPath: "/tmp/out.log",
		StderrPath: "/tmp/err.log",
		PATH:       "/usr/bin",
		ConfigPath: "/tmp/config.toml",
	})
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}
	type plist struct {
		Dict struct {
			Keys    []string `xml:"key"`
			Strings []string `xml:"string"`
		} `xml:"dict"`
	}
	var p plist
	if err := xml.Unmarshal(got, &p); err != nil {
		t.Fatalf("xml unmarshal: %v\nrendered:\n%s", err, got)
	}
	// Find the Label string by walking key/string pairs.
	var label string
	for i, k := range p.Dict.Keys {
		if k == "Label" && i < len(p.Dict.Strings) {
			label = p.Dict.Strings[i]
			break
		}
	}
	if label != "org.example & co" {
		t.Errorf("decoded Label = %q, want %q (escape must be idempotent through XML round-trip)",
			label, "org.example & co")
	}
}

// TestRenderPlist_WatchPathsContainsConfig pins B7: the rendered plist
// includes a launchd `WatchPaths` directive listing the resolved config
// path. launchd watches the listed paths and restarts the daemon when
// any of them is modified - because the daemon's startup re-reads
// config.toml from disk, a launchd-driven restart IS the config reload.
//
// Without this directive, editing config.toml requires
// `calendar-sync uninstall && calendar-sync install` per SPEC line 971.
// With it, the editor-save-and-rename pattern most editors use triggers
// a kqueue event launchd interprets as "file changed" and the daemon
// restarts within seconds.
//
// The XML structure launchd expects is:
//
//	<key>WatchPaths</key>
//	<array>
//	    <string>/path/to/config.toml</string>
//	</array>
//
// We assert on the textual substring rather than parsing the full XML
// because the surrounding plist tests (TestRenderPlist_HappyPath,
// TestRenderPlist_ParsesAsValidXML) already pin XML correctness.
func TestRenderPlist_WatchPathsContainsConfig(t *testing.T) {
	got, err := renderPlist(plistInputs{
		Label:      "org.calendar-sync.agent",
		BinaryPath: "/usr/local/bin/calendar-sync",
		StdoutPath: "/tmp/out.log",
		StderrPath: "/tmp/err.log",
		PATH:       DefaultPATH,
		ConfigPath: "/Users/alice/.config/calendar-sync/config.toml",
	})
	if err != nil {
		t.Fatalf("renderPlist: %v", err)
	}
	if !strings.Contains(string(got), "<key>WatchPaths</key>") {
		t.Errorf("plist missing <key>WatchPaths</key>; got:\n%s", got)
	}
	if !strings.Contains(string(got), "<string>/Users/alice/.config/calendar-sync/config.toml</string>") {
		t.Errorf("plist missing config path inside WatchPaths; got:\n%s", got)
	}
}

