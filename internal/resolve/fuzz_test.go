package resolve

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/giacomomicoli/feedreader/internal/parse"
)

// maxChannelTitleRunes is the bound parse.PlainTitle keeps a title to,
// measured on a title far longer than any.
var maxChannelTitleRunes = utf8.RuneCountInString(parse.PlainTitle(strings.Repeat("x", 1<<16)))

// User input and fetched pages are untrusted: none of these may panic, and
// whatever they return must be a usable http(s) URL.

func FuzzResolveInput(f *testing.F) {
	for _, s := range []string{
		"example.com", "https://example.com/feed", "//x", "localhost:8080",
		"youtube.com/@x", "https://www.youtube.com/channel/" + ytChannelID,
		"https://youtube.com/playlist?list=PL1", "mailto:a@b", "http://[::1", "%zz",
		"https://www.youtube.com/feeds/videos.xml?user=", "https://m.youtube.com/c/%",
		"http://m.youtube.com/feeds/videos.xml?playlist_id=UULF" + modeChanRest + "&x=1",
		"https://www.youtube.com/feeds/videos.xml?playlist_id=UUSH" + modeChanRest,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if IsChannelFeed(raw) {
			if u, err := url.Parse(strings.TrimSpace(raw)); err != nil || !isHTTPURL(u) || !isYouTubeHost(u) {
				t.Fatalf("IsChannelFeed(%q) for a URL that is not on YouTube", raw)
			}
		}
		if id, m, ok := FeedMode(raw); ok {
			// A mode feed names a well-formed channel, and its canonical URL
			// is built from that channel alone.
			canonical := ModeFeedURL(id, m)
			if canonical == "" {
				t.Fatalf("FeedMode(%q) = %q, %q: no canonical URL", raw, id, m)
			}
			if id2, m2, ok2 := FeedMode(canonical); !ok2 || id2 != id || m2 != m {
				t.Fatalf("FeedMode(%q) = %q, %q; its canonical URL %q gives %q, %q", raw, id, m, canonical, id2, m2)
			}
			if !strings.HasPrefix(canonical, ytFeedBase+"?") || strings.ContainsAny(canonical[len(ytFeedBase)+1:], "&#/%") {
				t.Fatalf("ModeFeedURL(%q, %q) = %q", id, m, canonical)
			}
		}
		res, err := New(newFake()).Resolve(context.Background(), raw)
		if err != nil {
			return
		}
		for _, c := range res.Candidates {
			u, perr := url.Parse(c.URL)
			if perr != nil || !isHTTPURL(u) {
				t.Fatalf("Resolve(%q) produced unusable candidate %q", raw, c.URL)
			}
		}
	})
}

func FuzzDiscoverLinks(f *testing.F) {
	f.Add([]byte(htmlPage(`<base href="/b/"><link rel="alternate" type="application/rss+xml" href="f">`)), "text/html")
	f.Add([]byte(`<link rel=alternate type=application/atom+xml href=//x/y title="&amp;`), "")
	f.Add([]byte("\xff\xfe<\x00b\x00a\x00s\x00e"), "text/html; charset=utf-16")
	page, _ := url.Parse("https://site.example/a/b")
	f.Fuzz(func(t *testing.T, body []byte, contentType string) {
		cands := discoverLinks(body, contentType, page)
		if len(cands) > maxFeedLinks {
			t.Fatalf("%d candidates", len(cands))
		}
		for _, c := range cands {
			u, err := url.Parse(c.URL)
			if err != nil || !isHTTPURL(u) || !feedLinkTypes[c.Type] {
				t.Fatalf("bad candidate %+v", c)
			}
		}
	})
}

func FuzzChannelFromHTML(f *testing.F) {
	f.Add([]byte(ytHandlePage(canonicalLink(ytChannelID))))
	f.Add([]byte(identifierMeta("UC") + rssAlternate("UC_")))
	f.Add([]byte(ytHandlePage(canonicalLink(ytChannelID) + ogImage(ytAvatar900))))
	f.Add([]byte(ogImage("https://yt3.ggpht.com/a=s-s1x-=s") + ogImage("javascript:x") + identifierMeta(ytChannelID)))
	f.Add([]byte(canonicalLink(ytChannelID) + `<meta property="og:title" content="a&#10;&lt;b&gt;` + strings.Repeat("x", 300) + `">`))
	page, _ := url.Parse("https://www.youtube.com/@x")
	f.Fuzz(func(t *testing.T, body []byte) {
		cp, cpOK := channelPageFromHTML(body, page)
		if !cpOK && cp != (channelPage{}) {
			t.Fatalf("channel page %+v without a channel", cp)
		}
		if utf8.RuneCountInString(cp.title) > maxChannelTitleRunes || strings.ContainsFunc(cp.title, unicode.IsControl) {
			t.Fatalf("unbounded or multi-line title %.40q", cp.title)
		}
		id, avatar, ok := channelFromHTML(body, page)
		if ok != cpOK || id != cp.id || avatar != cp.avatar {
			t.Fatalf("channelFromHTML disagrees with channelPageFromHTML")
		}
		if ok && !channelIDPattern.MatchString(id) {
			t.Fatalf("invalid channel id %q", id)
		}
		if !ok && avatar != "" {
			t.Fatalf("avatar %q without a channel", avatar)
		}
		if avatar == "" {
			return
		}
		if u, err := url.Parse(avatar); err != nil || !isHTTPURL(u) || len(avatar) > MaxIconURLBytes {
			t.Fatalf("unusable avatar %q", avatar)
		}
	})
}
