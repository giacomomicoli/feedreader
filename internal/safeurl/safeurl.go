// Package safeurl renders URLs for logs, error messages and stored fetch
// errors without their credentials.
package safeurl

import "net/url"

// Placeholder replaces the whole userinfo of a URL.
const Placeholder = "xxxxx"

// Invalid stands for a string that does not parse as a URL.
const Invalid = "(invalid URL)"

// Redacted returns u as a string with its userinfo, user name and password
// alike, replaced by Placeholder: a feed URL's user name can be a token on
// its own (https://token@host/feed, sent as Basic auth), which
// url.URL.Redacted would keep. An opaque URL ("https:token@host/feed") is
// shown as its scheme and Placeholder, since its opaque part may hold
// credentials. Only the userinfo is recognised, not, say, a token in the
// query. u itself is not changed; nil gives "".
func Redacted(u *url.URL) string {
	if u == nil {
		return ""
	}
	if u.Opaque != "" {
		return u.Scheme + ":" + Placeholder
	}
	if u.User == nil {
		return u.String()
	}
	c := *u
	c.User = url.User(Placeholder)
	return c.String()
}

// String parses raw and returns it Redacted, or Invalid when it does not
// parse (the string itself may then hold credentials).
func String(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return Invalid
	}
	return Redacted(u)
}
