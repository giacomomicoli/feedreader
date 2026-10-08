package parse

import (
	"net/url"
	"strings"
)

// parseBase parses s as an absolute http(s) URL usable as a base for
// resolving references. It returns nil when s is empty or unusable, in which
// case relative references stay unresolved (and are later dropped).
func parseBase(s string) *url.URL {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || !isHTTP(u) {
		return nil
	}
	return u
}

// isHTTP reports whether u is an absolute http or https URL with a host.
func isHTTP(u *url.URL) bool {
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// absHTTP resolves ref against base (which may be nil) and returns the result
// only when it is an absolute http(s) URL of at most maxURLBytes bytes;
// javascript:, data:, mailto:, unresolvable relative references and absurdly
// long URLs all yield "".
func absHTTP(base *url.URL, ref string) string {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	if base != nil {
		u = base.ResolveReference(u)
	}
	if !isHTTP(u) {
		return ""
	}
	s := u.String()
	if len(s) > maxURLBytes {
		return ""
	}
	if _, err := url.Parse(s); err != nil {
		return "" // e.g. "//::" resolves to "https://::"
	}
	return s
}

// firstAbsHTTP returns the first candidate that absHTTP accepts, or "".
func firstAbsHTTP(base *url.URL, candidates ...string) string {
	for _, c := range candidates {
		if u := absHTTP(base, c); u != "" {
			return u
		}
	}
	return ""
}

// resolveRef makes a relative reference absolute against base and returns it
// in canonical (escaped) form. Absolute references of any scheme are kept so
// that the sanitizer policy, not this function, decides which schemes
// survive. Unparseable or empty references are returned as "" so the
// sanitizer drops them.
func resolveRef(base *url.URL, ref string) string {
	t := strings.TrimSpace(ref)
	if t == "" {
		return ""
	}
	u, err := url.Parse(t)
	if err != nil {
		return ""
	}
	if !u.IsAbs() && base != nil {
		u = base.ResolveReference(u)
	}
	return u.String()
}
