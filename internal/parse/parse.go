// Package parse turns feed documents (RSS 0.9x/1.0/2.0, Atom 1.0, JSON Feed
// 1.0/1.1) into normalized entries, using github.com/mmcdole/gofeed, and
// sanitizes all feed HTML with an allow-list (github.com/microcosm-cc/bluemonday).
package parse

import (
	"bytes"
	"errors"
	"fmt"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/mmcdole/gofeed/atom"
	jsonfeed "github.com/mmcdole/gofeed/json"
	"github.com/mmcdole/gofeed/rss"
)

// Feed is a parsed feed document.
type Feed struct {
	Title   string // plain text, at most 200 runes
	SiteURL string // absolute http(s) URL of the site, or ""
	IconURL string // absolute http(s) URL of the feed image/icon/logo, or ""
	// TTL is RSS <ttl> (minutes in the document); 0 when absent.
	TTL time.Duration
	// Entries in document order, de-duplicated by GUID: when several items
	// share a GUID only the one with the latest UpdatedAt is kept (RFC 4287
	// section 4.1.1).
	Entries []Entry
}

// Entry is a normalized feed item.
type Entry struct {
	// GUID: Atom <id> / RSS <guid> / JSON Feed id (exact string,
	// only surrounding whitespace trimmed; Atom ids are taken as written,
	// never resolved against xml:base) → entry link → hex(sha256(title +
	// summary)).
	GUID string
	// URL is the absolute http(s) link to the original, or "". YouTube
	// entries use https://www.youtube.com/watch?v=<videoId>.
	URL          string
	Title        string // plain text, never empty (falls back to an excerpt or "(untitled)"), at most 200 runes
	SummaryHTML  string // allow-list sanitized, relative URLs made absolute
	Author       string
	ThumbnailURL string    // absolute http(s) URL or ""
	PublishedAt  time.Time // published, else updated, else zero
	UpdatedAt    time.Time // zero when absent
}

// ErrNotAFeed is returned when the document is not a recognizable feed.
var ErrNotAFeed = errors.New("not a recognizable RSS, Atom or JSON feed")

// Translators are stateless, hence shared. Images inside content are found
// by this package after sanitizing, so gofeed's raw HTML scan is disabled.
var (
	rssTranslator  = &gofeed.DefaultRSSTranslator{DisableContentImageScan: true}
	atomTranslator = &gofeed.DefaultAtomTranslator{}
	jsonTranslator = &gofeed.DefaultJSONTranslator{}
)

// Parse parses a feed document. contentType is the HTTP Content-Type (used
// for the charset when the document does not declare one); baseURL is the URL
// the document was fetched from, used to resolve relative URLs when the
// document has no xml:base.
//
// Malformed documents get a second, repaired attempt (invalid UTF-8 and
// characters XML forbids removed, stray '&' and '<' escaped) before Parse
// gives up with an error wrapping the original failure. Unknown XML
// namespaces are ignored. A body that is not RSS, Atom or JSON Feed yields
// ErrNotAFeed; one too deeply nested or with too many elements or items to
// parse in bounded memory yields an error wrapping ErrFeedTooComplex.
func Parse(body []byte, contentType, baseURL string) (*Feed, error) {
	var (
		src *source
		err error
	)
	switch f := sniffFormat(body, contentType); f {
	case formatRSS, formatAtom:
		src, err = parseXML(body, contentType, f)
	case formatJSON:
		src, err = parseJSON(body, contentType)
	default:
		return nil, ErrNotAFeed
	}
	if err != nil {
		return nil, err
	}
	return buildFeed(src, parseBase(baseURL)), nil
}

// parseXML parses an RSS or Atom document, retrying once on a repaired body
// unless it is too complex (repairing does not simplify it).
func parseXML(body []byte, contentType string, f format) (*source, error) {
	src, err := decodeXML(prepareXML(body, contentType), f)
	if err == nil {
		return src, nil
	}
	if errors.Is(err, ErrFeedTooComplex) {
		return nil, fmt.Errorf("parse %s feed: %w", f, err)
	}
	src, rerr := decodeXML(repairXML(body, contentType), f)
	if rerr != nil {
		return nil, fmt.Errorf("parse %s feed: %w (repair attempt: %v)", f, err, rerr)
	}
	return src, nil
}

// parseJSON parses a JSON Feed, retrying once on a repaired body.
func parseJSON(body []byte, contentType string) (*source, error) {
	b := prepareJSON(body, contentType)
	src, err := decodeJSON(b)
	if err == nil {
		return src, nil
	}
	if errors.Is(err, ErrFeedTooComplex) {
		return nil, fmt.Errorf("parse %s: %w", formatJSON, err)
	}
	fixed := repairJSON(b)
	if fixed == nil {
		return nil, fmt.Errorf("parse %s: %w", formatJSON, err)
	}
	src, rerr := decodeJSON(fixed)
	if rerr != nil {
		return nil, fmt.Errorf("parse %s: %w (repair attempt: %v)", formatJSON, err, rerr)
	}
	return src, nil
}

// decodeXML scans the document's structure (scanXML), then runs gofeed's
// RSS or Atom parser and translator. Feed bodies are untrusted, so a panic
// inside the library is turned into an error.
func decodeXML(b []byte, f format) (src *source, err error) {
	defer recoverInto(&err)
	scan, err := scanXML(b, f)
	if err != nil {
		return nil, err
	}
	switch f {
	case formatRSS:
		orig, err := (&rss.Parser{}).Parse(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		feed, err := rssTranslator.Translate(orig)
		if err != nil {
			return nil, err
		}
		return &source{format: f, feed: feed, rss: orig, scan: scan}, nil
	case formatAtom:
		orig, err := (&atom.Parser{}).Parse(bytes.NewReader(b))
		if err != nil {
			return nil, err
		}
		feed, err := atomTranslator.Translate(orig)
		if err != nil {
			return nil, err
		}
		return &source{format: f, feed: feed, atom: orig, scan: scan}, nil
	}
	return nil, ErrNotAFeed
}

// decodeJSON checks the document's size (scanJSON), then runs gofeed's JSON
// Feed parser and translator.
func decodeJSON(b []byte) (src *source, err error) {
	defer recoverInto(&err)
	if err := scanJSON(b); err != nil {
		return nil, err
	}
	orig, err := (&jsonfeed.Parser{}).Parse(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	feed, err := jsonTranslator.Translate(orig)
	if err != nil {
		return nil, err
	}
	return &source{format: formatJSON, feed: feed, json: orig}, nil
}

// recoverInto converts a panic into an error stored in *err.
func recoverInto(err *error) {
	if r := recover(); r != nil {
		*err = fmt.Errorf("feed parser panic: %v", r)
	}
}

// Detect cheaply reports whether body looks like a feed document (RSS, RDF,
// Atom or JSON Feed), without a full parse.
//
// It sniffs the root element (after any BOM, XML declaration, comments,
// processing instructions and doctype) for rss, rdf:RDF or feed, or a JSON
// object whose "version" contains "jsonfeed.org". contentType is only a
// hint: application/feed+json admits a JSON object with "items" but no
// version, and no Content-Type makes an HTML body a feed.
func Detect(body []byte, contentType string) bool {
	return sniffFormat(body, contentType) != formatUnknown
}

// Sanitize applies the allow-list policy to feed HTML, resolving relative
// URLs against base. No script, iframe, object, embed, form, style or event
// attributes survive; img is kept with an absolute src.
//
// Only http, https (and mailto for links) URLs are kept; whatever cannot be
// made absolute is dropped, along with srcset, style and id attributes.
// Links get rel="nofollow noreferrer noopener" and target="_blank". The
// output has balanced tags.
//
// Work and output are bounded: only the first 64 KiB of html are used, and
// markup that would make the HTML tree builder (or the output) grow far
// beyond the input is cut or replaced by its first image and visible text.
func Sanitize(html, base string) string {
	return sanitizeHTML(html, parseBase(base))
}

// Excerpt returns at most n runes of visible text from html, with whitespace
// collapsed and "…" appended when truncated. The result is plain text and
// must still be HTML-escaped when rendered (html/template does this).
func Excerpt(html string, n int) string {
	return excerpt(html, n)
}
