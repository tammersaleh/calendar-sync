package mirror

import "regexp"

// trailerPattern matches the strict description trailer that calendar-sync
// appends to every mirror: a blank line, "---", and a "Source: <htmlLink>"
// line whose URL is one of Google's auto-populated `htmlLink` forms.
//
// The regex is anchored to end-of-string with optional trailing whitespace.
// Anything inserted after the eid breaks the match - per SPEC.md "Description
// trailer handling", a partially edited trailer is left alone (and the caller
// logs trailer_unrecognized) rather than being heuristically repaired.
var trailerPattern = regexp.MustCompile(
	`\n\n---\nSource: https://(?:www\.google\.com|calendar\.google\.com)/calendar/event\?eid=[A-Za-z0-9_\-=]+\s*$`,
)

// StripTrailer removes the calendar-sync trailer from a mirror description.
// Returns the description with the trailer (and any trailing whitespace)
// removed, plus a bool indicating whether the trailer was present.
//
// The returned bool is the signal `propagate` uses to decide between "strip
// and forward" (true) and "forward verbatim" (false). The "user mangled the
// trailer" case falls into the false branch.
func StripTrailer(description string) (string, bool) {
	loc := trailerPattern.FindStringIndex(description)
	if loc == nil {
		return description, false
	}
	return description[:loc[0]], true
}
