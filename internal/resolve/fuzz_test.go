package resolve

import (
	"context"
	"net/url"
	"testing"
)

// User input and fetched pages are untrusted: none of these may panic, and
// whatever they return must be a usable http(s) URL.

func FuzzResolveInput(f *testing.F) {
	for _, s := range []string{
		"example.com", "https://example.com/feed", "//x", "localhost:8080",
		"youtube.com/@x", "https://www.youtube.com/channel/" + ytChannelID,
		"https://youtube.com/playlist?list=PL1", "mailto:a@b", "http://[::1", "%zz",
		"https://www.youtube.com/feeds/videos.xml?user=", "https://m.youtube.com/c/%",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, raw string) {
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

func FuzzChannelIDFromHTML(f *testing.F) {
	f.Add([]byte(ytHandlePage(canonicalLink(ytChannelID))))
	f.Add([]byte(identifierMeta("UC") + rssAlternate("UC_")))
	page, _ := url.Parse("https://www.youtube.com/@x")
	f.Fuzz(func(t *testing.T, body []byte) {
		if id, ok := channelIDFromHTML(body, page); ok && !channelIDPattern.MatchString(id) {
			t.Fatalf("invalid channel id %q", id)
		}
	})
}
