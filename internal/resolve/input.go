package resolve

import (
	"net/url"
	"regexp"
	"strings"
)

// schemePrefix matches an RFC 3986 scheme followed by ':'.
var schemePrefix = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.\-]*:`)

// normalizeInput turns what the user typed into an absolute http(s) URL:
// surrounding whitespace is trimmed, https:// is added when no scheme is
// given, and the fragment (never sent to servers) is dropped. Anything that
// is not an http(s) URL with a host yields ErrInvalidURL.
func normalizeInput(raw string) (*url.URL, error) {
	s, ok := withScheme(strings.TrimSpace(raw))
	if !ok {
		return nil, ErrInvalidURL
	}
	u, err := url.Parse(s)
	if err != nil || !isHTTPURL(u) || u.Opaque != "" {
		return nil, ErrInvalidURL
	}
	u.Fragment, u.RawFragment = "", ""
	return u, nil
}

// withScheme adds "https:" to scheme-less input. It reports false for input
// that carries some other kind of scheme without an authority ("mailto:…",
// "javascript:…", "https:example.com"), which cannot be repaired by adding
// one. "host:port/…" is recognised as scheme-less.
func withScheme(s string) (string, bool) {
	if s == "" {
		return "", false
	}
	if strings.HasPrefix(s, "//") {
		return "https:" + s, true
	}
	scheme := schemePrefix.FindString(s)
	if scheme == "" {
		return "https://" + s, true
	}
	rest := s[len(scheme):]
	switch {
	case strings.HasPrefix(rest, "//"):
		return s, true
	case startsWithPort(rest):
		return "https://" + s, true
	default:
		return "", false
	}
}

// startsWithPort reports whether s is a port number, optionally followed by
// a path, query or fragment: the "8080/feed" in "localhost:8080/feed".
func startsWithPort(s string) bool {
	i := 0
	for i < len(s) && s[i] >= '0' && s[i] <= '9' {
		i++
	}
	return i > 0 && (i == len(s) || strings.ContainsRune("/?#", rune(s[i])))
}

// isHTTPURL reports whether u is an absolute http or https URL with a host.
func isHTTPURL(u *url.URL) bool {
	return (u.Scheme == "http" || u.Scheme == "https") && u.Hostname() != ""
}
