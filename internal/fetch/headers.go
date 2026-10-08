package fetch

import (
	"net/http"
	"strings"
	"time"
)

// maxDeltaSeconds is the saturation value for delta-seconds (RFC 9111
// §1.2.2: larger values are treated as 2^31). It also keeps the conversion
// to time.Duration from overflowing.
const maxDeltaSeconds = 1 << 31

// parseRetryAfter parses a Retry-After value (RFC 9110 §10.2.3): either
// delta-seconds or an HTTP-date, the latter relative to now. Absent,
// negative, past or malformed values yield 0.
func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if d, ok := parseDeltaSeconds(v); ok {
		return d
	}
	t, err := http.ParseTime(v)
	if err != nil {
		return 0
	}
	d := t.Sub(now)
	if d <= 0 {
		return 0
	}
	return min(d, maxDeltaSeconds*time.Second)
}

// parseMaxAge extracts max-age from the Cache-Control field lines (RFC 9111
// §5.2). Directive names are case-insensitive and the argument may be quoted;
// when max-age appears more than once the first occurrence is used (§4.2.1).
//
// An unqualified no-store or no-cache yields 0: the server asks for every
// use to be revalidated, so it gives no freshness hint that could serve as a
// lower bound on the polling interval. Absent or invalid max-age yields 0.
func parseMaxAge(fieldLines []string) time.Duration {
	var (
		maxAge time.Duration
		seen   bool
	)
	for _, line := range fieldLines {
		for _, directive := range strings.Split(line, ",") {
			name, arg, _ := strings.Cut(directive, "=")
			name = strings.ToLower(strings.TrimSpace(name))
			arg = unquote(strings.TrimSpace(arg))
			switch {
			case name == "no-store", name == "no-cache" && arg == "":
				return 0
			case name == "max-age" && !seen:
				seen = true
				if d, ok := parseDeltaSeconds(arg); ok {
					maxAge = d
				}
			}
		}
	}
	return maxAge
}

// parseDeltaSeconds parses a non-negative decimal number of seconds,
// saturating at maxDeltaSeconds. Anything other than ASCII digits (signs,
// spaces, fractions, empty) is rejected.
func parseDeltaSeconds(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	var n int64
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		n = min(n*10+int64(c-'0'), maxDeltaSeconds)
	}
	return time.Duration(n) * time.Second, true
}

// unquote strips one pair of surrounding double quotes.
func unquote(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}
