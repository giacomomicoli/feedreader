package parse

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

// readFixture returns the bytes of testdata/name.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return b
}

// mustParse parses body and fails the test on error.
func mustParse(t *testing.T, body []byte, contentType, base string) *Feed {
	t.Helper()
	f, err := Parse(body, contentType, base)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return f
}

// mustParseFixture parses testdata/name.
func mustParseFixture(t *testing.T, name, contentType, base string) *Feed {
	t.Helper()
	return mustParse(t, readFixture(t, name), contentType, base)
}

// rfc3339 parses a test timestamp.
func rfc3339(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("bad test time %q: %v", s, err)
	}
	return v.UTC()
}

// checkEqual reports a mismatch of a named field.
func checkEqual[T comparable](t *testing.T, field string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %#v, want %#v", field, got, want)
	}
}

// checkTime compares times as instants and requires UTC.
func checkTime(t *testing.T, field string, got, want time.Time) {
	t.Helper()
	if !got.Equal(want) {
		t.Errorf("%s = %v, want %v", field, got, want)
	}
	if !got.IsZero() && got.Location() != time.UTC {
		t.Errorf("%s location = %v, want UTC", field, got.Location())
	}
}

// guids lists the GUIDs of f's entries in order.
func guids(f *Feed) []string {
	out := make([]string, len(f.Entries))
	for i, e := range f.Entries {
		out[i] = e.GUID
	}
	return out
}

func TestParse_RSS2_ContentEncodedAndRelativeURLs(t *testing.T) {
	f := mustParseFixture(t, "rss2_content.xml", "application/rss+xml", "https://blog.example.com/feed.xml")

	checkEqual(t, "Title", f.Title, "Example & Co. Blog")
	checkEqual(t, "SiteURL", f.SiteURL, "https://blog.example.com/")
	checkEqual(t, "IconURL", f.IconURL, "https://blog.example.com/images/logo.png")
	checkEqual(t, "TTL", f.TTL, time.Duration(0))
	checkEqual(t, "GUIDs", strings.Join(guids(f), " | "),
		"post-1 | https://blog.example.com/?p=2 | https://blog.example.com/2026/10/third")

	first := f.Entries[0]
	checkEqual(t, "first.URL", first.URL, "https://blog.example.com/2026/10/first-post")
	checkEqual(t, "first.Title", first.Title, "First post")
	checkEqual(t, "first.Author", first.Author, "Jane Doe")
	checkTime(t, "first.PublishedAt", first.PublishedAt, rfc3339(t, "2026-10-06T06:00:00Z"))
	checkTime(t, "first.UpdatedAt", first.UpdatedAt, time.Time{})
	checkEqual(t, "first.SummaryHTML (description preferred over content:encoded)", first.SummaryHTML,
		`<p>Short teaser with a <a href="https://blog.example.com/about" rel="nofollow noreferrer noopener" target="_blank">relative link</a>.</p>`)
	checkEqual(t, "first.ThumbnailURL (from content when the summary has no image)", first.ThumbnailURL,
		"https://blog.example.com/img/first.jpg")

	second := f.Entries[1]
	checkEqual(t, "second.SummaryHTML (content:encoded when no description)", second.SummaryHTML,
		`<p>Only content here, <img src="https://blog.example.com/images/second.png"/> with an image.</p>`)
	checkEqual(t, "second.ThumbnailURL", second.ThumbnailURL, "https://blog.example.com/images/second.png")

	third := f.Entries[2]
	checkEqual(t, "third.SummaryHTML (plain-text description escaped)", third.SummaryHTML,
		"Line one<br>Line two &amp; more")
	checkEqual(t, "third.ThumbnailURL", third.ThumbnailURL, "")
}

func TestParse_RSSTTL_ExposedInMinutes(t *testing.T) {
	f := mustParseFixture(t, "rss_ttl.xml", "", "https://slow.example.com/feed")
	checkEqual(t, "TTL", f.TTL, 90*time.Minute)

	cases := map[string]time.Duration{
		"":                     0,
		"abc":                  0,
		"-5":                   0,
		"0":                    0,
		" 15 ":                 15 * time.Minute,
		"99999999999999999999": 0, // does not even fit int64
		"9999999999999999":     time.Duration(1<<63-1) / time.Minute * time.Minute,
	}
	for ttl, want := range cases {
		doc := `<rss version="2.0"><channel><title>t</title><ttl>` + ttl + `</ttl></channel></rss>`
		f := mustParse(t, []byte(doc), "", "")
		if f.TTL != want {
			t.Errorf("ttl %q: TTL = %v, want %v", ttl, f.TTL, want)
		}
	}
}

func TestParse_Atom_XMLBaseResolvesRelativeURLs(t *testing.T) {
	// The document base must not win over xml:base.
	f := mustParseFixture(t, "atom_xmlbase.xml", "application/atom+xml", "https://elsewhere.example.com/atom")

	checkEqual(t, "Title (type=html stripped to text)", f.Title, "Atom Example")
	checkEqual(t, "SiteURL", f.SiteURL, "https://atom.example.org/blog/")
	checkEqual(t, "IconURL (icon preferred over logo)", f.IconURL, "https://atom.example.org/favicon.ico")
	if len(f.Entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(f.Entries))
	}

	e := f.Entries[0]
	checkEqual(t, "GUID", e.GUID, "tag:atom.example.org,2026:1")
	checkEqual(t, "URL (nested xml:base)", e.URL, "https://atom.example.org/blog/posts/2026/relative-everything.html")
	checkEqual(t, "Author (inherited from feed)", e.Author, "Feed Author")
	checkTime(t, "PublishedAt", e.PublishedAt, rfc3339(t, "2026-10-01T07:00:00Z"))
	checkTime(t, "UpdatedAt", e.UpdatedAt, rfc3339(t, "2026-10-01T10:00:00Z"))
	checkEqual(t, "SummaryHTML", e.SummaryHTML,
		`<p>See <a href="https://atom.example.org/blog/posts/2026/related.html" rel="nofollow noreferrer noopener" target="_blank">this</a> and <img src="https://atom.example.org/blog/posts/2026/pic.jpg"/></p>`)
	checkEqual(t, "ThumbnailURL", e.ThumbnailURL, "https://atom.example.org/blog/posts/2026/pic.jpg")

	text := f.Entries[1]
	checkEqual(t, "Title (type=text, entity decoded)", text.Title, "Text content & no summary")
	checkEqual(t, "URL", text.URL, "https://atom.example.org/blog/two.html")
	checkEqual(t, "SummaryHTML (content type=text escaped)", text.SummaryHTML, "Plain &lt;b&gt;text&lt;/b&gt; body")
	checkTime(t, "PublishedAt (missing → updated)", text.PublishedAt, rfc3339(t, "2026-09-30T10:00:00Z"))
}

func TestParse_JSONFeed11(t *testing.T) {
	f := mustParseFixture(t, "jsonfeed11.json", "application/feed+json", "https://json.example.net/feed.json")

	checkEqual(t, "Title", f.Title, "JSON Example")
	checkEqual(t, "SiteURL", f.SiteURL, "https://json.example.net/")
	checkEqual(t, "IconURL", f.IconURL, "https://json.example.net/icon-512.png")
	checkEqual(t, "GUIDs (numeric id coerced)", strings.Join(guids(f), ","), "2,1,3")

	html := f.Entries[0]
	checkEqual(t, "html.Title", html.Title, "HTML item")
	checkEqual(t, "html.Author", html.Author, "Item Author")
	checkEqual(t, "html.SummaryHTML (content_html sanitized)", html.SummaryHTML,
		`<p>Hello <img src="https://json.example.net/img/2.png"/></p>`)
	checkEqual(t, "html.ThumbnailURL", html.ThumbnailURL, "https://json.example.net/img/2.png")
	checkTime(t, "html.PublishedAt", html.PublishedAt, rfc3339(t, "2026-10-02T15:00:00Z"))
	checkTime(t, "html.UpdatedAt", html.UpdatedAt, rfc3339(t, "2026-10-03T00:00:00Z"))

	text := f.Entries[1]
	checkEqual(t, "text.URL (relative url resolved)", text.URL, "https://json.example.net/1")
	checkEqual(t, "text.SummaryHTML (content_text escaped)", text.SummaryHTML,
		"Microblog &lt;b&gt;post&lt;/b&gt; without a title<br>second line")
	checkEqual(t, "text.Title (excerpt of the summary)", text.Title, "Microblog <b>post</b> without a title second line")
	checkEqual(t, "text.ThumbnailURL (item image)", text.ThumbnailURL, "https://cdn.example.net/1.jpg")
	checkEqual(t, "text.Author (inherited)", text.Author, "Feed Author")

	link := f.Entries[2]
	checkEqual(t, "link.URL (external_url fallback)", link.URL, "https://elsewhere.example.com/x")
	checkEqual(t, "link.SummaryHTML (plain-text summary preferred and escaped)", link.SummaryHTML,
		"A plain &lt;summary&gt; &amp; stuff")
	checkTime(t, "link.PublishedAt", link.PublishedAt, time.Time{})
}

func TestParse_YouTube_WatchURLThumbnailAndDescription(t *testing.T) {
	f := mustParseFixture(t, "youtube.xml", "text/xml; charset=UTF-8",
		"https://www.youtube.com/feeds/videos.xml?channel_id=UCaaaaaaaaaaaaaaaaaaaaaa")

	checkEqual(t, "Title", f.Title, "Example Channel")
	checkEqual(t, "SiteURL", f.SiteURL, "https://www.youtube.com/channel/UCaaaaaaaaaaaaaaaaaaaaaa")
	if len(f.Entries) != 3 {
		t.Fatalf("got %d entries, want 3", len(f.Entries))
	}
	want := []struct{ guid, url, thumb string }{
		{"yt:video:AAAAAAAAAA1", "https://www.youtube.com/watch?v=AAAAAAAAAA1", "https://i4.ytimg.com/vi/AAAAAAAAAA1/hqdefault.jpg"},
		{"yt:video:BBBBBBBBBB2", "https://www.youtube.com/watch?v=BBBBBBBBBB2", "https://i1.ytimg.com/vi/BBBBBBBBBB2/hqdefault.jpg"},
		{"yt:video:CCCCCCCCCC3", "https://www.youtube.com/watch?v=CCCCCCCCCC3", "https://i2.ytimg.com/vi/CCCCCCCCCC3/hqdefault.jpg"},
	}
	for i, w := range want {
		e := f.Entries[i]
		checkEqual(t, "GUID", e.GUID, w.guid)
		checkEqual(t, "URL (watch URL even for /shorts/)", e.URL, w.url)
		checkEqual(t, "ThumbnailURL (media:group/media:thumbnail)", e.ThumbnailURL, w.thumb)
		checkEqual(t, "Author", e.Author, "Example Channel")
	}

	short := f.Entries[0]
	checkEqual(t, "Title", short.Title, "A short clip")
	checkEqual(t, "SummaryHTML (media:description escaped, newlines → <br>)", short.SummaryHTML,
		"First line &lt;b&gt;not bold&lt;/b&gt; &amp; &#34;quoted&#34;<br><br>Links → https://example.com/x")
	checkTime(t, "PublishedAt", short.PublishedAt, rfc3339(t, "2026-10-07T23:00:12Z"))
	checkTime(t, "UpdatedAt", short.UpdatedAt, rfc3339(t, "2026-10-07T23:00:16Z"))

	checkEqual(t, "emoji title kept", f.Entries[1].Title, "🧶 A long video")
	checkEqual(t, "empty description", f.Entries[2].SummaryHTML, "")
}

func TestParse_Encoding_HonorsXMLDeclaration(t *testing.T) {
	body := readFixture(t, "iso8859_1.xml")
	// The HTTP charset is wrong on purpose: the declaration wins.
	for _, ct := range []string{"", "text/xml", "text/xml; charset=utf-8"} {
		f := mustParse(t, body, ct, "")
		checkEqual(t, "Title ["+ct+"]", f.Title, "Café à la crème")
		if len(f.Entries) != 1 {
			t.Fatalf("[%s] got %d entries, want 1", ct, len(f.Entries))
		}
		checkEqual(t, "entry.Title ["+ct+"]", f.Entries[0].Title, "Naïve résumé")
		checkEqual(t, "entry.SummaryHTML ["+ct+"]", f.Entries[0].SummaryHTML, "Prix £5 · déjà vu")
	}
}

func TestParse_Encoding_HTTPCharsetWhenNoDeclaration(t *testing.T) {
	f := mustParseFixture(t, "windows1252_nodecl.xml", "application/rss+xml; charset=windows-1252", "")
	checkEqual(t, "Title", f.Title, "“Smart” quotes")
	checkEqual(t, "entry.Title", f.Entries[0].Title, "Price: € 10 – café")

	// Same with a malformed (but recognizable) Content-Type header.
	f = mustParseFixture(t, "windows1252_nodecl.xml", "application/rss+xml; charset=windows-1252; =", "")
	checkEqual(t, "Title (lenient header)", f.Title, "“Smart” quotes")
}

func TestParse_Encoding_FallsBackToUTF8(t *testing.T) {
	t.Run("undeclared non-UTF-8 bytes are replaced", func(t *testing.T) {
		f := mustParseFixture(t, "windows1252_nodecl.xml", "application/rss+xml", "")
		checkEqual(t, "Title", f.Title, "�Smart� quotes")
	})
	t.Run("UTF-8 mislabelled as ISO-8859-1 by HTTP stays UTF-8", func(t *testing.T) {
		doc := `<rss version="2.0"><channel><title>Café ✓</title></channel></rss>`
		f := mustParse(t, []byte(doc), "text/xml; charset=ISO-8859-1", "")
		checkEqual(t, "Title", f.Title, "Café ✓")
	})
	t.Run("ASCII body with a Latin-1 HTTP charset", func(t *testing.T) {
		doc := `<rss version="2.0"><channel><title>Plain</title></channel></rss>`
		f := mustParse(t, []byte(doc), "text/xml; charset=ISO-8859-1", "")
		checkEqual(t, "Title", f.Title, "Plain")
	})
	t.Run("UTF-8 byte-order mark", func(t *testing.T) {
		doc := "\xEF\xBB\xBF<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss version=\"2.0\"><channel><title>Café</title></channel></rss>"
		f := mustParse(t, []byte(doc), "", "")
		checkEqual(t, "Title (BOM wins over declaration)", f.Title, "Café")
	})
	t.Run("UTF-16 with byte-order mark", func(t *testing.T) {
		doc := `<?xml version="1.0" encoding="UTF-16"?><rss version="2.0"><channel><title>Ünïcödé</title></channel></rss>`
		for _, le := range []bool{true, false} {
			f := mustParse(t, utf16Bytes(doc, le), "", "")
			checkEqual(t, "Title", f.Title, "Ünïcödé")
		}
	})
	t.Run("JSON Feed with byte-order mark", func(t *testing.T) {
		doc := "\xEF\xBB\xBF" + `{"version":"https://jsonfeed.org/version/1.1","title":"Bom","items":[]}`
		f := mustParse(t, []byte(doc), "application/feed+json", "")
		checkEqual(t, "Title", f.Title, "Bom")
	})
}

// utf16Bytes encodes s as UTF-16 with a byte-order mark.
func utf16Bytes(s string, littleEndian bool) []byte {
	var out []byte
	put := func(u uint16) {
		if littleEndian {
			out = append(out, byte(u), byte(u>>8))
		} else {
			out = append(out, byte(u>>8), byte(u))
		}
	}
	put(0xFEFF)
	for _, r := range s {
		if r >= 0x10000 {
			r -= 0x10000
			put(uint16(0xD800 + (r >> 10)))
			put(uint16(0xDC00 + (r & 0x3FF)))
			continue
		}
		put(uint16(r))
	}
	return out
}

func TestParse_MalformedXML_TolerantParse(t *testing.T) {
	f := mustParseFixture(t, "malformed.xml", "application/rss+xml", "")
	checkEqual(t, "Title (bare &)", f.Title, "Broken & Co")
	if len(f.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(f.Entries))
	}
	e := f.Entries[0]
	checkEqual(t, "Title (bare & and <)", e.Title, "Fish & Chips < 5 euros")
	checkEqual(t, "URL (bare & in query)", e.URL, "http://broken.example.com/?a=1&b=2")
	if !strings.Contains(e.SummaryHTML, "invalid � bytes") {
		t.Errorf("invalid UTF-8 not replaced: %q", e.SummaryHTML)
	}
	for _, bad := range []string{"\x07", "\x0b", "￾"} {
		if strings.Contains(e.SummaryHTML, bad) {
			t.Errorf("SummaryHTML still contains %q: %q", bad, e.SummaryHTML)
		}
	}

	t.Run("unknown declared encoding", func(t *testing.T) {
		doc := `<?xml version="1.0" encoding="x-no-such-charset"?><rss version="2.0"><channel><title>Fine</title></channel></rss>`
		checkEqual(t, "Title", mustParse(t, []byte(doc), "", "").Title, "Fine")
	})
	t.Run("UTF-16 declared on a UTF-8 document", func(t *testing.T) {
		doc := `<?xml version="1.0" encoding="UTF-16"?><rss version="2.0"><channel><title>Café</title></channel></rss>`
		checkEqual(t, "Title", mustParse(t, []byte(doc), "", "").Title, "Café")
	})
	t.Run("declared UTF-8 but windows-1252 per HTTP", func(t *testing.T) {
		doc := "<?xml version=\"1.0\" encoding=\"UTF-8\"?><rss version=\"2.0\"><channel><title>caf\xe9</title></channel></rss>"
		checkEqual(t, "Title", mustParse(t, []byte(doc), "text/xml; charset=windows-1252", "").Title, "café")
	})
	t.Run("ampersands inside CDATA are untouched", func(t *testing.T) {
		doc := "<rss version=\"2.0\"><channel><title>A & B</title><item><title>x\xff</title><description><![CDATA[<p>Q&amp;A & more</p>]]></description></item></channel></rss>"
		f := mustParse(t, []byte(doc), "", "")
		checkEqual(t, "SummaryHTML", f.Entries[0].SummaryHTML, "<p>Q&amp;A &amp; more</p>")
	})
	t.Run("trailing garbage after a JSON Feed", func(t *testing.T) {
		doc := `{"version":"https://jsonfeed.org/version/1.1","title":"J","items":[{"id":"1","title":"one"}]} <html>oops`
		f := mustParse(t, []byte(doc), "", "")
		checkEqual(t, "entries", len(f.Entries), 1)
	})
}

func TestParse_Unrecoverable_ReturnsWrappedError(t *testing.T) {
	cases := map[string]string{
		"truncated RSS":  `<rss version="2.0"><channel><title>x</title><item><title>unterminated`,
		"truncated Atom": `<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>1</id>`,
		"broken JSON":    `{"version":"https://jsonfeed.org/version/1.1","items":[{"id":`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			f, err := Parse([]byte(doc), "", "")
			if err == nil {
				t.Fatalf("Parse succeeded: %+v", f)
			}
			if errors.Is(err, ErrNotAFeed) {
				t.Errorf("err = %v, want a parse error, not ErrNotAFeed", err)
			}
			if !strings.Contains(err.Error(), "parse ") {
				t.Errorf("err = %q, want context", err)
			}
		})
	}
}

func TestParse_NotAFeed(t *testing.T) {
	cases := map[string]struct {
		body, contentType string
	}{
		"HTML page":                     {string(readFixture(t, "html_page.html")), "text/html"},
		"HTML served as RSS":            {string(readFixture(t, "html_page.html")), "application/rss+xml"},
		"empty":                         {"", "application/rss+xml"},
		"whitespace":                    {" \n\t ", ""},
		"plain text":                    {"hello world", "text/plain"},
		"other XML":                     {`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg"/>`, "image/svg+xml"},
		"JSON object without version":   {`{"title":"x","items":[]}`, "application/json"},
		"JSON with a foreign version":   {`{"version":"2.0","items":[]}`, ""},
		"JSON array":                    {`[{"version":"https://jsonfeed.org/version/1"}]`, "application/feed+json"},
		"binary":                        {"\x00\x01\x02\xff", ""},
		"comment only":                  {"<!-- nothing -->", ""},
		"unterminated doctype":          {"<!DOCTYPE rss", ""},
		"processing instruction only":   {`<?xml version="1.0"?>`, ""},
		"feed media type, HTML doctype": {"<!DOCTYPE html><html><body><rss></rss></body></html>", "application/atom+xml"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Parse([]byte(c.body), c.contentType, "https://example.com/")
			if !errors.Is(err, ErrNotAFeed) {
				t.Errorf("err = %v, want ErrNotAFeed", err)
			}
		})
	}
}

func TestParse_UnknownNamespacesNeverFail(t *testing.T) {
	t.Run("RSS", func(t *testing.T) {
		f := mustParseFixture(t, "unknown_namespaces.xml", "application/rss+xml", "")
		checkEqual(t, "Title", f.Title, "Namespaced")
		if len(f.Entries) != 1 {
			t.Fatalf("got %d entries, want 1 (foreign <foo:item> is not an item)", len(f.Entries))
		}
		e := f.Entries[0]
		checkEqual(t, "GUID", e.GUID, "ns-1")
		checkEqual(t, "Title", e.Title, "Real item")
		checkEqual(t, "URL", e.URL, "https://ns.example.com/1")
		checkEqual(t, "SummaryHTML", e.SummaryHTML, "Body")
	})
	t.Run("Atom", func(t *testing.T) {
		f := mustParseFixture(t, "atom_unknown_namespaces.xml", "application/atom+xml", "")
		checkEqual(t, "Title", f.Title, "Atom namespaced")
		if len(f.Entries) != 1 {
			t.Fatalf("got %d entries, want 1 (foreign <x:entry> is not an entry)", len(f.Entries))
		}
		checkEqual(t, "entry.Title", f.Entries[0].Title, "Only entry")
		checkEqual(t, "entry.URL", f.Entries[0].URL, "https://ns.example.org/1")
	})
}

func TestParse_GUIDDerivation_IDThenLinkThenHash(t *testing.T) {
	f := mustParseFixture(t, "guids.xml", "", "https://g.example.com/feed")
	hash := sha256.Sum256([]byte("Hash me" + "Body"))
	want := []struct{ guid, url string }{
		{"http://g.example.com/Thing", "https://g.example.com/a"}, // guid exactly as published
		{"http://g.example.com/thing", "https://g.example.com/b"}, // case-sensitive: distinct entry
		{"https://g.example.com/c", "https://g.example.com/c"},    // no guid → link
		{hex.EncodeToString(hash[:]), ""},                         // no guid, no link → sha256(title+summary)
		{"https://g.example.com/e", "https://g.example.com/e"},    // permalink guid doubles as URL
		{"https://g.example.com/f-not-a-link", ""},                // isPermaLink="false" is not a URL
		{"https://g.example.com/g", "https://g.example.com/g"},    // blank guid → link
	}
	if len(f.Entries) != len(want) {
		t.Fatalf("got %d entries, want %d: %v", len(f.Entries), len(want), guids(f))
	}
	for i, w := range want {
		checkEqual(t, f.Entries[i].Title+" GUID", f.Entries[i].GUID, w.guid)
		checkEqual(t, f.Entries[i].Title+" URL", f.Entries[i].URL, w.url)
	}
}

func TestParse_HashedGUID_StableAcrossFetchesAndBaseURLs(t *testing.T) {
	doc := []byte(`<rss version="2.0"><channel><title>t</title><item><title>Same</title><description>&lt;p&gt;x&lt;/p&gt;</description></item></channel></rss>`)
	a := mustParse(t, doc, "", "https://one.example.com/feed")
	b := mustParse(t, doc, "", "https://two.example.com/other")
	checkEqual(t, "GUID", a.Entries[0].GUID, b.Entries[0].GUID)
	if len(a.Entries[0].GUID) != sha256.Size*2 {
		t.Errorf("GUID %q is not a hex sha256", a.Entries[0].GUID)
	}
}

func TestParse_DuplicateGUIDs_KeepLatestUpdated(t *testing.T) {
	f := mustParseFixture(t, "duplicates.xml", "", "")
	var titles []string
	for _, e := range f.Entries {
		titles = append(titles, e.Title)
	}
	checkEqual(t, "titles", strings.Join(titles, ","), "newest,other,first tie")
	checkEqual(t, "GUIDs", strings.Join(guids(f), ","), "dup,other,tie")
	checkTime(t, "dup.UpdatedAt", f.Entries[0].UpdatedAt, rfc3339(t, "2026-10-03T00:00:00Z"))
}

func TestParse_Dates(t *testing.T) {
	f := mustParseFixture(t, "dates.xml", "", "")
	want := map[string]struct{ published, updated string }{
		"future":  {"2099-01-01T00:00:00Z", ""}, // future dates kept as-is
		"none":    {"", ""},                     // zero: the caller falls back to fetched_at
		"dc":      {"2026-09-15T06:30:00Z", "2026-09-15T06:30:00Z"},
		"offset":  {"2026-10-03T01:00:00Z", ""}, // normalized to UTC
		"garbage": {"", ""},
	}
	if len(f.Entries) != len(want) {
		t.Fatalf("got %d entries, want %d", len(f.Entries), len(want))
	}
	for _, e := range f.Entries {
		w, ok := want[e.GUID]
		if !ok {
			t.Fatalf("unexpected entry %q", e.GUID)
		}
		var pub, upd time.Time
		if w.published != "" {
			pub = rfc3339(t, w.published)
		}
		if w.updated != "" {
			upd = rfc3339(t, w.updated)
		}
		checkTime(t, e.GUID+" PublishedAt", e.PublishedAt, pub)
		checkTime(t, e.GUID+" UpdatedAt", e.UpdatedAt, upd)
	}
}

func TestParse_Titles_PlainTextWithFallbacks(t *testing.T) {
	long := strings.Repeat("word ", 40)
	doc := `<rss version="2.0"><channel><title>  Spaced
		 &amp;amp; title  </title>
<item><guid>tags</guid><title>&lt;script&gt;alert(1)&lt;/script&gt;Clean &lt;b&gt;bold&lt;/b&gt;   text</title></item>
<item><guid>literal</guid><title>if x&lt;y then</title></item>
<item><guid>excerpt</guid><title>   </title><description>&lt;p&gt;Teaser &lt;em&gt;text&lt;/em&gt;&lt;/p&gt;</description></item>
<item><guid>long</guid><description>` + long + `</description></item>
<item><guid>untitled</guid></item>
</channel></rss>`
	f := mustParse(t, []byte(doc), "", "")
	checkEqual(t, "feed Title", f.Title, "Spaced & title")
	want := map[string]string{
		"tags":     "Clean bold text",
		"literal":  "if x<y then",
		"excerpt":  "Teaser text",
		"long":     strings.TrimSpace(long[:titleExcerptRunes]) + "…",
		"untitled": "(untitled)",
	}
	for _, e := range f.Entries {
		checkEqual(t, e.GUID+" Title", e.Title, want[e.GUID])
	}
}

func TestParse_Thumbnails_SummaryThenContentThenItemImage(t *testing.T) {
	doc := `<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:media="http://search.yahoo.com/mrss/"><channel><title>t</title>
<item><guid>summary</guid><description>&lt;img src="/s.jpg"&gt;</description><content:encoded>&lt;img src="/c.jpg"&gt;</content:encoded></item>
<item><guid>content</guid><description>no image</description><content:encoded>&lt;p&gt;&lt;img src="javascript:alert(1)"&gt;&lt;img src="/c.jpg"&gt;&lt;/p&gt;</content:encoded></item>
<item><guid>enclosure</guid><description>text</description><enclosure url="/e.jpg" type="image/jpeg" length="1"/></item>
<item><guid>media</guid><description>text</description><media:thumbnail url="https://cdn.example.com/m.jpg"/></item>
<item><guid>unsafe</guid><description>&lt;img src="data:image/png;base64,AAAA"&gt;</description><media:thumbnail url="javascript:alert(1)"/></item>
</channel></rss>`
	f := mustParse(t, []byte(doc), "", "https://img.example.com/feed.xml")
	want := map[string]string{
		"summary":   "https://img.example.com/s.jpg",
		"content":   "https://img.example.com/c.jpg",
		"enclosure": "https://img.example.com/e.jpg",
		"media":     "https://cdn.example.com/m.jpg",
		"unsafe":    "",
	}
	for _, e := range f.Entries {
		checkEqual(t, e.GUID+" ThumbnailURL", e.ThumbnailURL, want[e.GUID])
	}
}

func TestParse_HostileFeed_OnlySafeURLsAndHTML(t *testing.T) {
	f := mustParseFixture(t, "xss.xml", "application/rss+xml", "https://evil.example.com/feed.xml")
	checkEqual(t, "Title", f.Title, "Evil feed")
	checkEqual(t, "SiteURL (javascript: dropped)", f.SiteURL, "")
	checkEqual(t, "IconURL (javascript: dropped)", f.IconURL, "")
	if len(f.Entries) != 1 {
		t.Fatalf("got %d entries, want 1", len(f.Entries))
	}
	e := f.Entries[0]
	checkEqual(t, "URL (javascript: dropped)", e.URL, "")
	checkEqual(t, "Title (markup stripped)", e.Title, "Evil title")
	checkEqual(t, "ThumbnailURL (relative src made absolute, data:/javascript: never)", e.ThumbnailURL,
		"https://evil.example.com/x")
	assertSafeHTML(t, e.SummaryHTML)
	for _, s := range []string{"styled", "<b>unclosed", "</b>"} {
		if !strings.Contains(e.SummaryHTML, s) {
			t.Errorf("SummaryHTML lost safe content %q: %s", s, e.SummaryHTML)
		}
	}
}

func TestParse_BaseURLOptional(t *testing.T) {
	doc := `<rss version="2.0"><channel><title>t</title><link>/</link><item><guid>1</guid><link>/rel</link><description>&lt;a href="/x"&gt;x&lt;/a&gt; &lt;img src="/i.png"&gt;</description></item></channel></rss>`
	for _, base := range []string{"", "not a url", "/relative/base", "ftp://example.com/feed"} {
		f := mustParse(t, []byte(doc), "", base)
		e := f.Entries[0]
		checkEqual(t, "SiteURL ["+base+"]", f.SiteURL, "")
		checkEqual(t, "URL ["+base+"]", e.URL, "")
		checkEqual(t, "SummaryHTML ["+base+"] (unresolvable relative URLs dropped)", e.SummaryHTML, "x")
	}
}

func TestParse_ConcurrentUse(t *testing.T) {
	docs := [][]byte{
		readFixture(t, "youtube.xml"),
		readFixture(t, "rss2_content.xml"),
		readFixture(t, "jsonfeed11.json"),
		readFixture(t, "malformed.xml"),
	}
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(doc []byte) {
			defer wg.Done()
			if _, err := Parse(doc, "", "https://example.com/feed"); err != nil {
				t.Errorf("Parse: %v", err)
			}
		}(docs[i%len(docs)])
	}
	wg.Wait()
}

func TestParse_GUIDDerivation_AtomIDsExactlyAsWritten(t *testing.T) {
	// gofeed resolves <id> as a URL against xml:base: scheme lower-cased,
	// path escaped, relative ids made absolute and blank ids replaced by the
	// base, so distinct ids merged and keys moved with xml:base.
	doc := `<feed xmlns="http://www.w3.org/2005/Atom" xml:base="http://example.org/blog/"><title>t</title>
<entry><id>http://x/thing</id><title>lower</title></entry>
<entry><id>HTTP://x/thing</id><title>upper scheme</title></entry>
<entry><id>http://x/a b</id><title>space</title></entry>
<entry><id>http://x/a%20b</id><title>escaped</title></entry>
<entry><id>http://x/café</id><title>non-ASCII</title></entry>
<entry><id>Post:12</id><title>opaque</title></entry>
<entry><id>post-1</id><title>relative</title></entry>
<entry><id>
  tag:x,2026:1  </id><title>padded</title><source><id>urn:source</id></source></entry>
<entry><id></id><link href="https://x/blank"/><title>blank</title></entry>
<entry><id><![CDATA[urn:cdata&1]]></id><title>cdata</title></entry>
<entry><id>urn:a&amp;b</id><title>entity</title></entry>
</feed>`
	f := mustParse(t, []byte(doc), "application/atom+xml", "https://example.org/feed.atom")
	want := []string{"http://x/thing", "HTTP://x/thing", "http://x/a b", "http://x/a%20b", "http://x/café",
		"Post:12", "post-1", "tag:x,2026:1", "https://x/blank", "urn:cdata&1", "urn:a&b"}
	checkEqual(t, "GUIDs", strings.Join(guids(f), " | "), strings.Join(want, " | "))
}

func TestParse_TitleKinds(t *testing.T) {
	t.Run("JSON Feed titles are plain text", func(t *testing.T) {
		doc := `{"version":"https://jsonfeed.org/version/1.1","title":"A <b>bold</b> feed","items":[
			{"id":"1","title":"The <dialog> element"},
			{"id":"2","title":"<script>alert(1)</script>"},
			{"id":"3","title":"if a<b and b>c then"}]}`
		f := mustParse(t, []byte(doc), "application/feed+json", "")
		checkEqual(t, "feed Title", f.Title, "A <b>bold</b> feed")
		for i, want := range []string{"The <dialog> element", "<script>alert(1)</script>", "if a<b and b>c then"} {
			checkEqual(t, "Title", f.Entries[i].Title, want)
		}
	})
	t.Run("Atom titles and summaries follow their type", func(t *testing.T) {
		doc := `<feed xmlns="http://www.w3.org/2005/Atom"><title>Vec&lt;T&gt; news</title>
<entry><id>untyped</id><title>Rust Vec&lt;T&gt; vs Box&lt;T&gt; explained</title></entry>
<entry><id>text</id><title type="text">&lt;b&gt;x&lt;/b&gt; &amp;amp; y</title><summary type="text">Use &lt;div&gt; and &lt;span&gt; tags.</summary></entry>
<entry><id>html</id><title type="html">&lt;b&gt;Bold&lt;/b&gt; &amp;amp; co</title><summary type="html">&lt;p&gt;Para&lt;/p&gt;</summary></entry>
<entry><id>xhtml</id><title type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml">X<b>H</b>TML</div></title></entry>
<entry><id>maybe</id><title>Untyped summary</title><summary>&lt;p&gt;Mislabelled &lt;b&gt;HTML&lt;/b&gt;&lt;/p&gt;</summary></entry>
</feed>`
		f := mustParse(t, []byte(doc), "application/atom+xml", "")
		checkEqual(t, "feed Title", f.Title, "Vec<T> news")
		want := map[string]struct{ title, summary string }{
			"untyped": {"Rust Vec<T> vs Box<T> explained", ""},
			"text":    {"<b>x</b> &amp; y", "Use &lt;div&gt; and &lt;span&gt; tags."},
			"html":    {"Bold & co", "<p>Para</p>"},
			"xhtml":   {"XHTML", ""},
			"maybe":   {"Untyped summary", "<p>Mislabelled <b>HTML</b></p>"},
		}
		for _, e := range f.Entries {
			checkEqual(t, e.GUID+" Title", e.Title, want[e.GUID].title)
			checkEqual(t, e.GUID+" SummaryHTML", e.SummaryHTML, want[e.GUID].summary)
		}
		checkEqual(t, "text excerpt", Excerpt(f.Entries[1].SummaryHTML, 100), "Use <div> and <span> tags.")
	})
	t.Run("YouTube titles are plain text", func(t *testing.T) {
		doc := `<feed xmlns:yt="http://www.youtube.com/xml/schemas/2015" xmlns="http://www.w3.org/2005/Atom"><title>C</title>
<entry><id>yt:video:A</id><yt:videoId>A</yt:videoId><title>&lt;b&gt;Bold&lt;/b&gt; claims about Vec&lt;T&gt;</title></entry></feed>`
		f := mustParse(t, []byte(doc), "", "")
		checkEqual(t, "Title", f.Entries[0].Title, "<b>Bold</b> claims about Vec<T>")
	})
	t.Run("RSS titles: only closed known elements or entities are markup", func(t *testing.T) {
		doc := `<rss version="2.0"><channel><title>t</title>
<item><guid>generic</guid><title>Generic List&lt;T&gt; in C#</title></item>
<item><guid>compare</guid><title>if a&lt;b and b&gt;c then</title></item>
<item><guid>element</guid><title>The &lt;dialog&gt; element</title></item>
<item><guid>markup</guid><title>&lt;b&gt;Bold&lt;/b&gt; move</title></item>
<item><guid>entity</guid><title>AT&amp;amp;T</title></item>
</channel></rss>`
		want := map[string]string{
			"generic": "Generic List<T> in C#",
			"compare": "if a<b and b>c then",
			"element": "The <dialog> element",
			"markup":  "Bold move",
			"entity":  "AT&T",
		}
		for _, e := range mustParse(t, []byte(doc), "", "").Entries {
			checkEqual(t, e.GUID+" Title", e.Title, want[e.GUID])
		}
	})
}

func TestParse_TitlesAndURLsAreBounded(t *testing.T) {
	long := strings.Repeat("word ", 100_000)
	longURL := "https://l.example.com/" + strings.Repeat("a", maxURLBytes)
	exact := strings.Repeat("é", maxTitleRunes)
	doc := `<rss version="2.0" xmlns:media="http://search.yahoo.com/mrss/"><channel><title>` + long + `</title>
<link>` + longURL + `</link><image><url>` + longURL + `</url></image>
<item><guid>long</guid><title>` + long + `</title><author>` + long + `</author><link>` + longURL + `</link>
<description>&lt;img src="` + longURL + `"&gt;</description><media:thumbnail url="` + longURL + `"/></item>
<item><guid>exact</guid><title>` + exact + `</title><link>https://l.example.com/ok</link></item>
<item><guid>one more</guid><title>` + exact + `x</title></item>
</channel></rss>`
	f := mustParse(t, []byte(doc), "", "https://l.example.com/feed")
	for name, s := range map[string]string{"feed Title": f.Title, "Title": f.Entries[0].Title, "Author": f.Entries[0].Author} {
		if n := utf8.RuneCountInString(s); n != maxTitleRunes || !strings.HasSuffix(s, "…") {
			t.Errorf("%s has %d runes (%.20q…), want %d ending in an ellipsis", name, n, s, maxTitleRunes)
		}
	}
	checkEqual(t, "SiteURL", f.SiteURL, "")
	checkEqual(t, "IconURL", f.IconURL, "")
	checkEqual(t, "URL", f.Entries[0].URL, "")
	checkEqual(t, "ThumbnailURL", f.Entries[0].ThumbnailURL, "")
	checkEqual(t, "exact Title", f.Entries[1].Title, exact)
	checkEqual(t, "exact URL", f.Entries[1].URL, "https://l.example.com/ok")
	checkEqual(t, "one more Title", f.Entries[2].Title, strings.Repeat("é", maxTitleRunes-1)+"…")
}

func TestParse_XMLBase_UntypedAtomAndRSSContent(t *testing.T) {
	t.Run("Atom", func(t *testing.T) {
		doc := `<feed xmlns="http://www.w3.org/2005/Atom" xml:base="https://cdn.blog.example/posts/"><title>t</title>
<entry><id>summary</id><link href="one.html"/><summary>&lt;img src="pic.jpg"&gt; text</summary></entry>
<entry><id>cdata content</id><link href="two.html"/><content><![CDATA[<p><img src="c.png"> body</p>]]></content></entry>
<entry xml:base="sub/"><id>typed</id><summary type="html">&lt;img src="h.jpg"&gt;</summary></entry>
<entry><id>own base</id><summary xml:base="/s/">&lt;img src="s.jpg"&gt;</summary></entry>
</feed>`
		f := mustParse(t, []byte(doc), "application/atom+xml", "https://blog.example/feed.atom")
		want := map[string]struct{ url, thumb string }{
			"summary":       {"https://cdn.blog.example/posts/one.html", "https://cdn.blog.example/posts/pic.jpg"},
			"cdata content": {"https://cdn.blog.example/posts/two.html", "https://cdn.blog.example/posts/c.png"},
			"typed":         {"", "https://cdn.blog.example/posts/sub/h.jpg"},
			"own base":      {"", "https://cdn.blog.example/s/s.jpg"},
		}
		for _, e := range f.Entries {
			checkEqual(t, e.GUID+" URL", e.URL, want[e.GUID].url)
			checkEqual(t, e.GUID+" ThumbnailURL", e.ThumbnailURL, want[e.GUID].thumb)
			if !strings.Contains(e.SummaryHTML, want[e.GUID].thumb) {
				t.Errorf("%s SummaryHTML = %q, want the image resolved", e.GUID, e.SummaryHTML)
			}
		}
	})
	t.Run("Atom with a relative xml:base", func(t *testing.T) {
		// gofeed resolves typed html against "posts/" already; resolving it
		// again must not give posts/posts/.
		doc := `<feed xmlns="http://www.w3.org/2005/Atom" xml:base="posts/"><title>t</title>
<entry><id>typed</id><summary type="html">&lt;img src="pic.jpg"&gt;</summary></entry>
<entry><id>untyped</id><summary>&lt;img src="raw.jpg"&gt;</summary></entry>
</feed>`
		f := mustParse(t, []byte(doc), "", "https://blog.example/feed.atom")
		checkEqual(t, "typed", f.Entries[0].ThumbnailURL, "https://blog.example/posts/pic.jpg")
		checkEqual(t, "untyped", f.Entries[1].ThumbnailURL, "https://blog.example/posts/raw.jpg")
	})
	t.Run("RSS", func(t *testing.T) {
		doc := `<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:media="http://search.yahoo.com/mrss/">
<channel xml:base="http://b.example/chan/"><title>t</title>
<item xml:base="http://b.example/dir/"><guid>description</guid><description>&lt;img src="i.png"&gt;</description></item>
<item><guid>content</guid><description>no image</description><content:encoded><![CDATA[<img src="c.png">]]></content:encoded></item>
<item xml:base="/abs/"><guid>media</guid><link>page.html</link><description>&lt;a href="x.html"&gt;x&lt;/a&gt;</description><media:thumbnail url="t.jpg"/></item>
</channel></rss>`
		f := mustParse(t, []byte(doc), "", "https://e.example/feed")
		want := map[string]string{
			"description": "http://b.example/dir/i.png",
			"content":     "http://b.example/chan/c.png",
			"media":       "http://b.example/abs/t.jpg",
		}
		for _, e := range f.Entries {
			checkEqual(t, e.GUID+" ThumbnailURL", e.ThumbnailURL, want[e.GUID])
		}
		checkEqual(t, "media URL", f.Entries[2].URL, "http://b.example/abs/page.html")
		checkEqual(t, "media SummaryHTML", f.Entries[2].SummaryHTML,
			`<a href="http://b.example/abs/x.html" rel="nofollow noreferrer noopener" target="_blank">x</a>`)
	})
	t.Run("RSS 1.0 item order", func(t *testing.T) {
		// gofeed lists channel items first, then those outside the channel.
		doc := `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/">
<item xml:base="http://r.example/a/"><title>A</title><description>&lt;img src="i.png"&gt;</description></item>
<channel><title>R</title><item xml:base="http://r.example/b/"><title>B</title><description>&lt;img src="i.png"&gt;</description></item></channel>
<item xml:base="http://r.example/c/"><title>C</title><description>&lt;img src="i.png"&gt;</description></item>
</rdf:RDF>`
		f := mustParse(t, []byte(doc), "", "https://r.example/rss")
		var got []string
		for _, e := range f.Entries {
			got = append(got, e.Title+"="+e.ThumbnailURL)
		}
		checkEqual(t, "entries", strings.Join(got, " "),
			"B=http://r.example/b/i.png A=http://r.example/a/i.png C=http://r.example/c/i.png")
	})
}
