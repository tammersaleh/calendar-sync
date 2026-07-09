package feed

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// maxAge extracts the max-age directive (in seconds) from a Cache-Control
// header value. It returns ok=false when the header is absent or carries no
// non-negative max-age.
func maxAge(header string) (time.Duration, bool) {
	if header == "" {
		return 0, false
	}
	for _, directive := range strings.Split(header, ",") {
		directive = strings.TrimSpace(directive)
		const key = "max-age="
		if !strings.HasPrefix(strings.ToLower(directive), key) {
			continue
		}
		secs, err := strconv.Atoi(strings.TrimSpace(directive[len(key):]))
		if err != nil || secs < 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	return 0, false
}

// iso8601 matches the time-only ISO-8601 duration shape PT#H#M#S (hours,
// minutes, seconds; no date component). At least one component is required.
var iso8601 = regexp.MustCompile(`^PT(?:(\d+)H)?(?:(\d+)M)?(?:(\d+)S)?$`)

// parseISO8601Duration parses the minimal PT#H#M#S subset of ISO-8601 durations
// used by iCal feeds' X-PUBLISHED-TTL header. It rejects anything with a date
// component (e.g. P1D), the empty string, "PT" with no fields, and any other
// malformed input by returning ok=false so the caller falls back.
func parseISO8601Duration(s string) (time.Duration, bool) {
	m := iso8601.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	if m[1] == "" && m[2] == "" && m[3] == "" {
		return 0, false // bare "PT"
	}
	var d time.Duration
	for i, unit := range []time.Duration{time.Hour, time.Minute, time.Second} {
		if m[i+1] == "" {
			continue
		}
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return 0, false
		}
		d += time.Duration(n) * unit
	}
	return d, true
}
