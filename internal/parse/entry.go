package parse

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"
	"github.com/mmcdole/gofeed/atom"
	ext "github.com/mmcdole/gofeed/extensions"
	jsonfeed "github.com/mmcdole/gofeed/json"
	"github.com/mmcdole/gofeed/rss"
)

// Presentation details of entry normalization.
const (
	// titleExcerptRunes is the length of the summary excerpt used as the
	// title of an entry that has none.
	titleExcerptRunes = 80
	// untitled is the title of last resort.
	untitled = "(untitled)"
	// youTubeWatchURL is the canonical link of a YouTube video.
	youTubeWatchURL = "https://www.youtube.com/watch?"
)

// textKind says how a feed text field must be turned into HTML.
type textKind int

const (
	kindNone      textKind = iota // not usable (binary or out-of-line content)
	kindHTML                      // HTML: sanitize
	kindText                      // plain text: escape
	kindMaybeHTML                 // HTML or plain text, decided by content
)

// source is a parsed document: gofeed's universal feed plus the
// format-specific original, which keeps details the universal model drops
// (RSS ttl, Atom content type, JSON content_text vs content_html, …), and
// for XML the structure scan, which keeps what gofeed drops altogether
// (Atom text types and raw ids, xml:base per item). Universal items map
// 1:1, in order, to the original's items.
type source struct {
	format format
	feed   *gofeed.Feed
	rss    *rss.Feed
	atom   *atom.Feed
	json   *jsonfeed.Feed
	scan   *xmlScan // nil for JSON Feed
}

// itemMeta is what entry building needs from the format-specific item.
type itemMeta struct {
	titleKind   textKind // how to read gofeed Item.Title
	summaryKind textKind // how to render gofeed Item.Description
	contentKind textKind // how to render gofeed Item.Content
	// Bases for relative URLs: base for the item's media and images,
	// summaryBase and contentBase for the HTML fields. They honor the
	// xml:base in scope wherever gofeed did not already resolve URLs.
	base, summaryBase, contentBase *url.URL
	// rawID is the Atom <id> as written (rawIDKnown when the scan has it):
	// gofeed's Item.GUID is that id resolved as a URL, which normalizes it.
	rawID       string
	rawIDKnown  bool
	permalink   string // RSS guid usable as the link (isPermaLink != false)
	externalURL string // JSON Feed external_url
}

// buildFeed normalizes a parsed document. base resolves relative URLs that
// xml:base did not already resolve; it may be nil.
func buildFeed(src *source, base *url.URL) *Feed {
	f := src.feed
	out := &Feed{
		Title:   titleText(f.Title, src.feedTitleKind()),
		SiteURL: absHTTP(base, f.Link),
		IconURL: feedIcon(src, base),
		TTL:     feedTTL(src),
	}
	var feedAuthor *gofeed.Person
	if src.format != formatRSS {
		// Atom and JSON Feed entries inherit the feed's authors; RSS has no
		// such rule (managingEditor is a contact address).
		feedAuthor = firstPerson(append([]*gofeed.Person{f.Author}, f.Authors...))
	}
	entries := make([]Entry, 0, len(f.Items))
	for i, it := range f.Items {
		if it == nil {
			continue
		}
		entries = append(entries, buildEntry(it, src.itemMeta(i, base), feedAuthor, base))
	}
	out.Entries = dedupByGUID(entries)
	return out
}

// feedIcon picks the feed's icon: Atom icon then logo, JSON icon then
// favicon, RSS image (or what gofeed derived from iTunes/Media RSS).
func feedIcon(src *source, base *url.URL) string {
	var candidates []string
	switch {
	case src.atom != nil:
		candidates = append(candidates, src.atom.Icon, src.atom.Logo)
	case src.json != nil:
		candidates = append(candidates, src.json.Icon, src.json.Favicon)
	}
	if src.feed.Image != nil {
		candidates = append(candidates, src.feed.Image.URL)
	}
	return firstAbsHTTP(base, candidates...)
}

// feedTTL converts RSS <ttl> (minutes) to a duration; 0 when absent or
// invalid. Absurd values are clamped to avoid overflow; capping them to a
// sane polling bound is the scheduler's job.
func feedTTL(src *source) time.Duration {
	if src.rss == nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(src.rss.TTL), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	if maxMin := int64(math.MaxInt64 / int64(time.Minute)); n > maxMin {
		n = maxMin
	}
	return time.Duration(n) * time.Minute
}

// feedTitleKind says how to read the feed title: JSON Feed titles are plain
// text, Atom titles follow their type attribute, RSS titles are guessed.
func (s *source) feedTitleKind() textKind {
	switch {
	case s.json != nil:
		return kindText
	case s.atom != nil && s.scan != nil:
		return atomTitleKind(s.scan.feedTitleType)
	}
	return kindMaybeHTML
}

// scanItem returns the scan of item i, or nil when the scan does not line
// up with gofeed's items (then nothing from it is used).
func (s *source) scanItem(i int) *xmlItem {
	if s.scan == nil || len(s.scan.items) != len(s.feed.Items) || i >= len(s.scan.items) {
		return nil
	}
	return s.scan.items[i]
}

// itemMeta extracts the format-specific details of item i; base is the
// document base.
func (s *source) itemMeta(i int, base *url.URL) itemMeta {
	m := itemMeta{
		titleKind: kindMaybeHTML, summaryKind: kindMaybeHTML, contentKind: kindMaybeHTML,
		base: base, summaryBase: base, contentBase: base,
	}
	switch {
	case s.rss != nil && i < len(s.rss.Items) && s.rss.Items[i] != nil:
		m.contentKind = kindHTML
		if g := s.rss.Items[i].GUID; g != nil && !strings.EqualFold(strings.TrimSpace(g.IsPermalink), "false") {
			m.permalink = g.Value
		}
		if x := s.scanItem(i); x != nil {
			// gofeed resolves no URL inside RSS description/content.
			m.base = withXMLBase(base, x.base)
			m.summaryBase = withXMLBase(base, x.summaryBase)
			m.contentBase = withXMLBase(base, x.contentBase)
		}
	case s.atom != nil && i < len(s.atom.Entries) && s.atom.Entries[i] != nil:
		c := s.atom.Entries[i].Content
		m.contentKind = atomContentKind(c)
		x := s.scanItem(i)
		if x == nil {
			break
		}
		m.titleKind = atomTitleKind(x.titleType)
		m.summaryKind = atomTextKind(x.summaryType)
		m.rawID, m.rawIDKnown = x.id, true
		m.base = withXMLBase(base, x.base)
		// gofeed already resolved html and xhtml text against xml:base
		// (possibly to a relative URL): resolving it again would apply a
		// relative xml:base twice.
		if !gofeedResolvesHTML(x.summaryType) {
			m.summaryBase = withXMLBase(base, x.summaryBase)
		}
		if c != nil && !gofeedResolvesHTML(c.Type) {
			m.contentBase = withXMLBase(base, x.contentBase)
		}
	case s.json != nil && i < len(s.json.Items) && s.json.Items[i] != nil:
		it := s.json.Items[i]
		// JSON Feed titles and summaries are plain text ("HTML is not
		// allowed"); only content_html is HTML.
		m.titleKind, m.summaryKind, m.contentKind = kindText, kindText, kindHTML
		m.externalURL = it.ExternalURL
		if it.ContentHTML == "" {
			m.contentKind = kindText // gofeed fell back to content_text
		}
	}
	return m
}

// withXMLBase returns the base for URLs under the xml:base xb (nil: none)
// in a document fetched from doc (nil: unknown).
func withXMLBase(doc, xb *url.URL) *url.URL {
	switch {
	case xb == nil:
		return doc
	case doc == nil:
		return xb
	}
	return doc.ResolveReference(xb)
}

// gofeedResolvesHTML reports whether gofeed resolves relative URLs inside an
// Atom text construct of type t (its own test, without trimming).
func gofeedResolvesHTML(t string) bool {
	t = strings.ToLower(t)
	return t == "html" || strings.Contains(t, "xhtml")
}

// atomContentKind maps an Atom content element (RFC 4287 section 4.1.3) to a
// kind.
func atomContentKind(c *atom.Content) textKind {
	if c == nil || strings.TrimSpace(c.Src) != "" {
		return kindNone
	}
	return atomTextKind(c.Type)
}

// atomTextKind maps the type attribute of an Atom summary or content
// (RFC 4287 sections 3.1.1, 4.1.3) to a kind. An explicit "text" is plain
// text; an absent type defaults to "text" but is often mislabelled HTML, so
// the content decides.
func atomTextKind(t string) textKind {
	t = strings.ToLower(strings.TrimSpace(t))
	switch {
	case t == "":
		return kindMaybeHTML
	case t == "html" || t == "text/html" || strings.Contains(t, "xhtml"):
		return kindHTML
	case t == "text" || strings.HasPrefix(t, "text/"):
		return kindText
	}
	return kindNone
}

// atomTitleKind maps the type attribute of an Atom title to a kind: html and
// xhtml titles are markup, everything else (the default "text" included,
// as in every YouTube feed) is plain text.
func atomTitleKind(t string) textKind {
	if atomTextKind(t) == kindHTML {
		return kindHTML
	}
	return kindText
}

// buildEntry normalizes one item.
func buildEntry(it *gofeed.Item, m itemMeta, feedAuthor *gofeed.Person, base *url.URL) Entry {
	e := Entry{
		URL:         entryURL(it, m, base),
		Author:      entryAuthor(it, feedAuthor),
		PublishedAt: utc(it.PublishedParsed),
		UpdatedAt:   utc(it.UpdatedParsed),
	}
	if e.PublishedAt.IsZero() {
		e.PublishedAt = e.UpdatedAt
	}

	text := entryBody(it, m)
	e.SummaryHTML = text.summary
	e.ThumbnailURL = text.thumbnail()

	titleKind := m.titleKind
	if text.isVideo {
		titleKind = kindText // YouTube titles never carry markup
	}
	e.Title = titleText(it.Title, titleKind)
	if e.Title == "" {
		e.Title = excerpt(e.SummaryHTML, titleExcerptRunes)
	}
	if e.Title == "" {
		e.Title = untitled
	}

	e.GUID = entryGUID(it, m, text.rawSummary)
	return e
}

// entryGUID derives the dedup key: the published id/guid exactly as written
// (entities decoded and surrounding whitespace trimmed, nothing else: an
// Atom id is compared character by character, RFC 4287 section 4.2.6), else
// the entry link as published, else hex(sha256(title + summary)) over the
// raw, unsanitized fields so it does not change when the sanitizer policy
// does.
func entryGUID(it *gofeed.Item, m itemMeta, rawSummary string) string {
	switch {
	case m.rawIDKnown:
		// gofeed's Item.GUID is the id resolved as a URL: lower-cased
		// scheme, escaped path, xml:base applied, and a blank id turned
		// into the base URL.
		if strings.TrimSpace(m.rawID) != "" {
			return m.rawID
		}
	case strings.TrimSpace(it.GUID) != "":
		return it.GUID
	}
	if l := strings.TrimSpace(it.Link); l != "" {
		return l
	}
	sum := sha256.Sum256([]byte(it.Title + rawSummary))
	return hex.EncodeToString(sum[:])
}

// entryURL returns the absolute http(s) link to the original: the YouTube
// watch URL for videos, else the item link, else an absolute RSS permalink
// guid or JSON Feed external_url.
func entryURL(it *gofeed.Item, m itemMeta, base *url.URL) string {
	if id := youTubeVideoID(it); id != "" {
		return youTubeWatchURL + url.Values{"v": {id}}.Encode()
	}
	if u := absHTTP(base, it.Link); u != "" {
		return u
	}
	// A permalink guid is only trusted as a link when it is already an
	// absolute URL: "isPermaLink" defaults to true even for opaque ids.
	return firstAbsHTTP(nil, m.permalink, m.externalURL)
}

// entryAuthor returns the item author's name (or email), falling back to the
// inherited feed author, as plain text.
func entryAuthor(it *gofeed.Item, feedAuthor *gofeed.Person) string {
	for _, p := range []*gofeed.Person{it.Author, firstPerson(it.Authors), feedAuthor} {
		if p == nil {
			continue
		}
		if name := plainText(p.Name); name != "" {
			return name
		}
		if email := plainText(p.Email); email != "" {
			return email
		}
	}
	return ""
}

// firstPerson returns the first non-nil person, or nil.
func firstPerson(ps []*gofeed.Person) *gofeed.Person {
	for _, p := range ps {
		if p != nil {
			return p
		}
	}
	return nil
}

// utc dereferences an optional time as UTC; nil is the zero time.
func utc(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.UTC()
}

// entryText is the rendered summary of an entry plus what thumbnail
// selection needs.
type entryText struct {
	summary    string // sanitized summary HTML (SummaryHTML)
	rawSummary string // the unsanitized source of summary, for the hash GUID
	// content is the raw content when the summary came from elsewhere; it
	// is only rendered if the summary has no image.
	content     string
	contentKind textKind
	contentBase *url.URL
	base        *url.URL // resolves image and media URLs
	isVideo     bool
	thumb       string // YouTube media:thumbnail
	image       string // gofeed item image (iTunes, Media RSS, enclosure)
	media       string // Media RSS thumbnail of an article
}

// entryBody renders the summary: YouTube media:description (plain text);
// otherwise the summary/description, falling back to the content.
func entryBody(it *gofeed.Item, m itemMeta) entryText {
	b := entryText{base: m.base}
	if it.Image != nil {
		b.image = it.Image.URL
	}
	if youTubeVideoID(it) != "" {
		b.isVideo = true
		b.thumb = mediaAttr(it.Extensions, "thumbnail", "url", m.base)
		if desc := mediaValue(it.Extensions, "description"); desc != "" {
			b.summary, b.rawSummary = textHTML(desc), desc
			return b
		}
	} else {
		b.media = mediaAttr(it.Extensions, "thumbnail", "url", m.base)
	}

	if summary := render(it.Description, m.summaryKind, m.summaryBase); summary != "" {
		b.summary, b.rawSummary = summary, it.Description
		b.content, b.contentKind, b.contentBase = it.Content, m.contentKind, m.contentBase
		return b
	}
	if content := render(it.Content, m.contentKind, m.contentBase); content != "" {
		b.summary, b.rawSummary = content, it.Content
		return b
	}
	b.rawSummary = it.Description
	return b
}

// thumbnail picks the card image: for videos the media:thumbnail; for
// articles the first image of the summary, then of the content, then the
// item image gofeed found, then a Media RSS thumbnail. Only absolute http(s)
// URLs are returned.
func (b entryText) thumbnail() string {
	if b.isVideo {
		if u := absHTTP(b.base, b.thumb); u != "" {
			return u
		}
	}
	if u := absHTTP(b.base, firstImageSrc(b.summary)); u != "" {
		return u
	}
	// render bounds the work like it does for the summary.
	content := render(b.content, b.contentKind, b.contentBase)
	return firstAbsHTTP(b.base, firstImageSrc(content), b.image, b.media)
}

// render turns a feed text field into sanitized HTML according to kind.
// Only the first maxFieldBytes of s are used.
func render(s string, kind textKind, base *url.URL) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	s = clipUTF8(s, maxFieldBytes)
	switch kind {
	case kindHTML:
		return sanitizeHTML(s, base)
	case kindText:
		return textHTML(s)
	case kindMaybeHTML:
		if looksLikeHTML(s) {
			return sanitizeHTML(s, base)
		}
		return textHTML(s)
	}
	return ""
}

// youTubeVideoID returns the yt:videoId of an item, or "".
func youTubeVideoID(it *gofeed.Item) string {
	return strings.TrimSpace(extValue(it.Extensions, "yt", "videoId"))
}

// extValue returns the text of the first prefix:name extension element.
func extValue(exts ext.Extensions, prefix, name string) string {
	for _, e := range exts[prefix][name] {
		if e.Value != "" {
			return e.Value
		}
	}
	return ""
}

// mediaElements returns the Media RSS elements named name, first those
// inside media:group (where YouTube puts them), then direct children.
func mediaElements(exts ext.Extensions, name string) []ext.Extension {
	media := exts["media"]
	var out []ext.Extension
	for _, g := range media["group"] {
		out = append(out, g.Children[name]...)
	}
	return append(out, media[name]...)
}

// mediaValue returns the text of the first non-empty media:<name>.
func mediaValue(exts ext.Extensions, name string) string {
	for _, e := range mediaElements(exts, name) {
		if v := strings.TrimSpace(e.Value); v != "" {
			return v
		}
	}
	return ""
}

// mediaAttr returns the first media:<name>@<attr> that is an absolute
// http(s) URL once resolved against base.
func mediaAttr(exts ext.Extensions, name, attr string, base *url.URL) string {
	for _, e := range mediaElements(exts, name) {
		if u := absHTTP(base, e.Attrs[attr]); u != "" {
			return u
		}
	}
	return ""
}

// dedupByGUID keeps one entry per GUID, in document order: among entries
// sharing a GUID the one with the latest UpdatedAt wins (the first one on a
// tie), at the position of the first occurrence (RFC 4287 section 4.1.1).
func dedupByGUID(entries []Entry) []Entry {
	seen := make(map[string]int, len(entries))
	out := make([]Entry, 0, len(entries))
	for _, e := range entries {
		if i, ok := seen[e.GUID]; ok {
			if e.UpdatedAt.After(out[i].UpdatedAt) {
				out[i] = e
			}
			continue
		}
		seen[e.GUID] = len(out)
		out = append(out, e)
	}
	return out
}
