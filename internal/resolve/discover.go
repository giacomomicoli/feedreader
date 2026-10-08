package resolve

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html/charset"

	"github.com/giacomomicoli/feedreader/internal/fetch"
)

// feedLinkTypes are the <link type> values that advertise a feed (RSS
// Advisory Board RSS Autodiscovery; application/feed+json from JSON Feed 1.1).
var feedLinkTypes = map[string]bool{
	"application/rss+xml":   true,
	"application/atom+xml":  true,
	"application/feed+json": true,
}

// fallbackPaths are probed in this order, relative to the site origin, when
// a page advertises no feed.
var fallbackPaths = []string{
	"/feed",
	"/feed/",
	"/rss",
	"/rss.xml",
	"/atom.xml",
	"/index.xml",
	"/feed.json",
	"/?feed=rss2",
	"/blog/feed",
}

// Bounds on what a hostile or broken page can make the resolver collect.
const (
	// maxFeedLinks caps how many feed <link> elements are considered.
	maxFeedLinks = 50
	// maxTitleRunes caps a candidate's <link title>.
	maxTitleRunes = 200
)

// feedLink is a feed <link> element as written in the page.
type feedLink struct {
	href, title, typ string
}

// discoverLinks returns the feeds an HTML page advertises with
// <link rel="alternate" type="application/rss+xml|application/atom+xml|
// application/feed+json" href>, in document order and without duplicates.
// Relative hrefs are resolved against <base href> (itself resolved against
// pageURL) or pageURL; hrefs that are not http(s) are dropped.
//
// The whole document is scanned, not only <head>: real pages (YouTube's,
// for one) emit their <link> elements after content that ends the head
// implicitly, which browsers accept.
func discoverLinks(body []byte, contentType string, pageURL *url.URL) []Candidate {
	var (
		links    []feedLink
		baseHref string
		haveBase bool
	)
	_ = scanTags(decodeHTML(body, contentType), []string{"base", "link"}, func(t tag) bool {
		switch t.name {
		case "base":
			// Only the first <base> with an href counts (HTML Living Standard,
			// section 4.2.3).
			if href, ok := t.attrs["href"]; ok && !haveBase {
				baseHref, haveBase = href, true
			}
		case "link":
			if l, ok := asFeedLink(t); ok {
				links = append(links, l)
			}
		}
		return len(links) < maxFeedLinks
	})
	// A scan error only means the page was cut short; what was found so far
	// is still valid.
	return candidates(links, documentBase(pageURL, baseHref, haveBase))
}

// asFeedLink reports whether a <link> element advertises a feed: its rel is
// exactly "alternate" (case-insensitive, no other keyword) and its type is a
// feed type.
func asFeedLink(t tag) (feedLink, bool) {
	rel := relTokens(t.attrs["rel"])
	if len(rel) != 1 || rel[0] != "alternate" {
		return feedLink{}, false
	}
	typ := mediaType(t.attrs["type"])
	if !feedLinkTypes[typ] {
		return feedLink{}, false
	}
	return feedLink{href: t.attrs["href"], title: t.attrs["title"], typ: typ}, true
}

// documentBase is the URL relative hrefs resolve against: the first
// <base href>, resolved against the page URL, when it is a valid http(s)
// URL; the page URL otherwise.
func documentBase(pageURL *url.URL, baseHref string, haveBase bool) *url.URL {
	if !haveBase {
		return pageURL
	}
	b, err := pageURL.Parse(trimHTMLSpace(baseHref))
	if err != nil || !isHTTPURL(b) {
		return pageURL
	}
	return b
}

// candidates resolves links against base, keeping the first occurrence of
// each absolute http(s) URL.
func candidates(links []feedLink, base *url.URL) []Candidate {
	seen := make(map[string]bool, len(links))
	var out []Candidate
	for _, l := range links {
		href := trimHTMLSpace(l.href)
		if href == "" {
			continue
		}
		u, err := base.Parse(href)
		if err != nil || !isHTTPURL(u) {
			continue
		}
		u.Fragment, u.RawFragment = "", ""
		abs := u.String()
		if seen[abs] {
			continue
		}
		seen[abs] = true
		out = append(out, Candidate{URL: abs, Title: cleanTitle(l.title), Type: l.typ})
	}
	return out
}

// cleanTitle collapses whitespace and bounds the length of a <link title>.
func cleanTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= maxTitleRunes {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:maxTitleRunes])) + "…"
}

// decodeHTML returns a UTF-8 reader for an HTML body, honouring the HTTP
// charset, a BOM or a <meta charset>; undecodable input is read as-is.
func decodeHTML(body []byte, contentType string) io.Reader {
	r, err := charset.NewReader(bytes.NewReader(body), contentType)
	if err != nil {
		return bytes.NewReader(body)
	}
	return r
}

// looksLikeHTML reports whether a non-feed response is an HTML page worth
// searching for <link> elements: by Content-Type, or by content sniffing
// when the server sent no or a generic type.
func looksLikeHTML(res *fetch.Result) bool {
	switch mediaType(res.ContentType) {
	case "text/html", "application/xhtml+xml":
		return true
	case "", "text/plain", "application/octet-stream":
		return strings.HasPrefix(http.DetectContentType(res.Body), "text/html")
	}
	return false
}

// mediaType returns the lower-cased type/subtype of a MIME type, without
// parameters; "" when s is empty.
func mediaType(s string) string {
	if mt, _, err := mime.ParseMediaType(s); err == nil {
		return mt
	}
	mt, _, _ := strings.Cut(s, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

// probeFallbacks requests fallbackPaths in order on site's origin
// and returns the first that is a feed. URLs in skip were already fetched
// and are not requested again. An HTTP error or non-feed answer moves on to
// the next path; a network failure or cancellation stops the probing and is
// returned, since the remaining paths live on the same host.
func (r *Resolver) probeFallbacks(ctx context.Context, site *url.URL, skip map[string]bool) (Candidate, bool, error) {
	for _, probe := range fallbackURLs(site) {
		if skip[probe] {
			continue
		}
		res, err := r.fetch(ctx, probe, fetch.FeedAccept)
		if err != nil {
			if isURLLevelError(err) {
				continue
			}
			return Candidate{}, false, err
		}
		if c, ok := directFeed(res, probe); ok {
			return c, true, nil
		}
	}
	return Candidate{}, false, nil
}

// fallbackURLs returns the fallback paths as absolute URLs on site's origin
// (scheme, user info and host).
func fallbackURLs(site *url.URL) []string {
	origin := &url.URL{Scheme: site.Scheme, User: site.User, Host: site.Host}
	out := make([]string, 0, len(fallbackPaths))
	for _, p := range fallbackPaths {
		ref, err := url.Parse(p)
		if err != nil {
			continue // unreachable: the paths are constants
		}
		out = append(out, origin.ResolveReference(ref).String())
	}
	return out
}
