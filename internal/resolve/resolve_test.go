package resolve

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/store"
)

func TestResolve_InvalidInput_ErrInvalidURL(t *testing.T) {
	for _, raw := range []string{
		"",
		"   \t\n",
		"ftp://example.com/feed",
		"file:///etc/passwd",
		"feed://example.com/rss",
		"mailto:someone@example.com",
		"javascript:alert(1)",
		"data:text/html,<p>x</p>",
		"https:example.com",
		"https://",
		"http://:8080/feed",
		"https:///path-only",
		"http://exa mple.com/",
		"https://example.com/\x00feed",
		"//",
	} {
		t.Run(raw, func(t *testing.T) {
			f := newFake()
			resolveErr(t, f, raw, ErrInvalidURL)
			if n := len(f.urls()); n != 0 {
				t.Errorf("invalid input made %d requests", n)
			}
		})
	}
}

func TestResolve_InputWithoutScheme_TreatedAsHTTPS(t *testing.T) {
	for _, tc := range []struct{ raw, want string }{
		{"example.com/feed.xml", "https://example.com/feed.xml"},
		{"  example.com/feed.xml \n", "https://example.com/feed.xml"},
		{"localhost:8080/rss", "https://localhost:8080/rss"},
		{"example.com:8443", "https://example.com:8443"},
		{"//example.com/feed.xml", "https://example.com/feed.xml"},
		{"[::1]:8080/feed.xml", "https://[::1]:8080/feed.xml"},
		{"HTTP://Example.com/feed.xml", "http://Example.com/feed.xml"},
		{"https://example.com/feed.xml#section", "https://example.com/feed.xml"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			f := newFake()
			f.serve(tc.want, ctRSS, rssDoc)
			res := resolveOK(t, f, tc.raw)
			if got := f.urls(); !slices.Equal(got, []string{tc.want}) {
				t.Errorf("requests = %v; want [%s]", got, tc.want)
			}
			if got := res.Candidates[0].URL; got != tc.want {
				t.Errorf("candidate = %q; want %q", got, tc.want)
			}
		})
	}
}

func TestResolve_Step1_URLAlreadyAFeed_UsedAsIs(t *testing.T) {
	for _, tc := range []struct{ name, ct, body string }{
		{"RSS", ctRSS, rssDoc},
		{"Atom", ctAtom, atomDoc},
		{"JSON Feed", ctJSON, jsonFeedDoc},
		{"RSS served as text/html", ctHTML, rssDoc},
		{"Atom without content type", "", atomDoc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const feedURL = "https://blog.example/feed?format=x"
			f := newFake()
			f.serve(feedURL, tc.ct, tc.body)
			res := resolveOK(t, f, feedURL)
			want := []Candidate{{URL: feedURL}}
			if !slices.Equal(res.Candidates, want) {
				t.Errorf("candidates = %+v; want %+v", res.Candidates, want)
			}
			reqs := f.requests()
			if len(reqs) != 1 || reqs[0].Accept != fetch.FeedAccept {
				t.Errorf("requests = %+v; want one request with Accept FeedAccept", reqs)
			}
		})
	}
}

func TestResolve_Step1_PermanentRedirect_UsesNewLocation(t *testing.T) {
	f := newFake()
	r := f.serve("https://old.example/rss", ctRSS, rssDoc)
	r.FinalURL = "https://new.example/feed.xml"
	r.PermanentURL = "https://new.example/feed.xml"
	res := resolveOK(t, f, "https://old.example/rss")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://new.example/feed.xml"}) {
		t.Errorf("candidates = %v; want the permanent location", got)
	}
}

func TestResolve_Step1_TemporaryRedirect_KeepsRequestedURL(t *testing.T) {
	f := newFake()
	r := f.serve("https://blog.example/rss", ctRSS, rssDoc)
	r.FinalURL = "https://cdn.example/tmp/rss.xml" // 302, PermanentURL empty
	res := resolveOK(t, f, "https://blog.example/rss")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://blog.example/rss"}) {
		t.Errorf("candidates = %v; want the requested URL", got)
	}
}

func TestResolve_Step3_Autodiscovery_RelativeHrefAgainstPageURL(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/posts/hello", ctHTML, htmlPage(
		`<link rel="alternate" type="application/rss+xml" title="Posts" href="../feed.xml">`))
	res := resolveOK(t, f, "https://blog.example/posts/hello")
	want := []Candidate{{URL: "https://blog.example/feed.xml", Title: "Posts", Type: "application/rss+xml"}}
	if !slices.Equal(res.Candidates, want) {
		t.Errorf("candidates = %+v; want %+v", res.Candidates, want)
	}
	if n := len(f.urls()); n != 1 {
		t.Errorf("made %d requests; autodiscovery must not probe fallbacks", n)
	}
}

func TestResolve_Step3_Autodiscovery_RelativeHrefAgainstBaseHref(t *testing.T) {
	f := newFake()
	// <base href> is itself relative to the page; only the first one counts,
	// and it applies to links that precede it too.
	f.serve("https://blog.example/a/b/page.html", ctHTML, htmlPage(`
		<link rel="alternate" type="application/atom+xml" href="atom.xml">
		<base href="/site/">
		<base href="https://ignored.example/">
		<link rel="alternate" type="application/rss+xml" href="rss/">`))
	res := resolveOK(t, f, "https://blog.example/a/b/page.html")
	want := []string{"https://blog.example/site/atom.xml", "https://blog.example/site/rss/"}
	if got := candidateURLs(res); !slices.Equal(got, want) {
		t.Errorf("candidates = %v; want %v", got, want)
	}
}

func TestResolve_Step3_Autodiscovery_InvalidBaseFallsBackToPageURL(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/x/", ctHTML, htmlPage(`
		<base href="javascript:void(0)">
		<link rel="alternate" type="application/rss+xml" href="feed">`))
	res := resolveOK(t, f, "https://blog.example/x/")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://blog.example/x/feed"}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestResolve_Step3_Autodiscovery_ResolvesAgainstFinalURLAfterRedirect(t *testing.T) {
	f := newFake()
	r := f.serve("http://blog.example", ctHTML, htmlPage(
		`<link rel="alternate" type="application/rss+xml" href="feed/">`))
	r.FinalURL = "https://www.blog.example/en/"
	res := resolveOK(t, f, "http://blog.example")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://www.blog.example/en/feed/"}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestResolve_Step3_SeveralFeeds_AllListedInDocumentOrder_FirstPreselected(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/", ctHTML, htmlPage(`
		<link rel="alternate" type="application/rss+xml" title="Blog &raquo; Feed" href="/feed/">
		<link rel="alternate" type="application/rss+xml" title="Blog &raquo; Comments Feed" href="/comments/feed/">
		<link rel="alternate" type="application/atom+xml" title="  Atom
			feed " href="https://blog.example/atom.xml">
		<link rel="alternate" type="application/feed+json" title="JSON" href="/feed.json">`))
	res := resolveOK(t, f, "https://blog.example/")
	want := []Candidate{
		{URL: "https://blog.example/feed/", Title: "Blog » Feed", Type: "application/rss+xml"},
		{URL: "https://blog.example/comments/feed/", Title: "Blog » Comments Feed", Type: "application/rss+xml"},
		{URL: "https://blog.example/atom.xml", Title: "Atom feed", Type: "application/atom+xml"},
		{URL: "https://blog.example/feed.json", Title: "JSON", Type: "application/feed+json"},
	}
	if !slices.Equal(res.Candidates, want) {
		t.Errorf("candidates =\n%+v\nwant\n%+v", res.Candidates, want)
	}
}

func TestResolve_Step3_OnlyAlternateFeedLinksCount(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/", ctHTML, htmlPage(`
		<link rel="stylesheet" type="text/css" href="/style.css">
		<link rel="alternate stylesheet" type="application/rss+xml" href="/not-1">
		<link rel="alternate" hreflang="de" type="text/html" href="/de/">
		<link rel="alternate" type="application/json" href="/not-2.json">
		<link rel="alternate" media="handheld" href="/m/">
		<link rel="alternate" type="application/rss+xml" href="javascript:alert(1)">
		<link rel="alternate" type="application/rss+xml" href="android-app://com.example/feed">
		<link rel="alternate" type="application/rss+xml" href="">
		<link rel="alternate" type="application/rss+xml">
		<link rel="home alternate" type="application/rss+xml" href="/not-3">
		<link rel=" ALTERNATE " type="Application/RSS+XML; charset=utf-8" href=" /feed.xml#top ">
		<link rel="alternate" type="application/rss+xml" href="https://blog.example/feed.xml">
		<link rel="alternate" type="application/atom+xml" href="http://blog.example/atom.xml">`))
	res := resolveOK(t, f, "https://blog.example/")
	want := []Candidate{
		{URL: "https://blog.example/feed.xml", Type: "application/rss+xml"},
		{URL: "http://blog.example/atom.xml", Type: "application/atom+xml"},
	}
	if !slices.Equal(res.Candidates, want) {
		t.Errorf("candidates =\n%+v\nwant\n%+v", res.Candidates, want)
	}
}

func TestResolve_Step3_LinksInsideScriptsAreIgnored(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/", ctHTML, htmlPage(`
		<script>document.write('<link rel="alternate" type="application/rss+xml" href="/js">');</script>
		<!-- <link rel="alternate" type="application/rss+xml" href="/comment"> -->
		<link rel="alternate" type="application/rss+xml" href="/real">`))
	res := resolveOK(t, f, "https://blog.example/")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://blog.example/real"}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestResolve_Step3_LegacyCharsetTitlesDecoded(t *testing.T) {
	f := newFake()
	page := htmlPage(`<link rel="alternate" type="application/rss+xml" title="Caf` + "\xe9" + `" href="/rss">`)
	f.serve("https://cafe.example/", "text/html; charset=iso-8859-1", page)
	res := resolveOK(t, f, "https://cafe.example/")
	if got := res.Candidates[0].Title; got != "Café" {
		t.Errorf("title = %q; want %q", got, "Café")
	}
}

func TestResolve_Step3_HostilePage_CandidatesAndTitlesBounded(t *testing.T) {
	var head []byte
	for i := range maxFeedLinks * 3 {
		head = append(head, `<link rel="alternate" type="application/rss+xml" href="/f`...)
		head = append(head, byte('a'+i%26), byte('a'+i/26%26))
		head = append(head, `" title="`...)
		for range 1000 {
			head = append(head, 'x')
		}
		head = append(head, `">`...)
	}
	f := newFake()
	f.serve("https://spam.example/", ctHTML, htmlPage(string(head)))
	res := resolveOK(t, f, "https://spam.example/")
	if n := len(res.Candidates); n > maxFeedLinks {
		t.Errorf("got %d candidates; want at most %d", n, maxFeedLinks)
	}
	if n := len([]rune(res.Candidates[0].Title)); n > maxTitleRunes+1 {
		t.Errorf("title has %d runes; want at most %d", n, maxTitleRunes+1)
	}
}

func TestResolve_Step3_MislabelledHTMLIsSniffed(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/", "text/plain", htmlPage(
		`<link rel="alternate" type="application/rss+xml" href="/rss">`))
	res := resolveOK(t, f, "https://blog.example/")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://blog.example/rss"}) {
		t.Errorf("candidates = %v", got)
	}
}

// allFallbacks is fallbackPaths on https://blog.example, in probe order.
var allFallbacks = []string{
	"https://blog.example/feed",
	"https://blog.example/feed/",
	"https://blog.example/rss",
	"https://blog.example/rss.xml",
	"https://blog.example/atom.xml",
	"https://blog.example/index.xml",
	"https://blog.example/feed.json",
	"https://blog.example/?feed=rss2",
	"https://blog.example/blog/feed",
}

func TestResolve_Step4_FallbackPathsProbedInOrderOnSiteOrigin(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/2024/01/post?ref=x", ctHTML, htmlPage(""))
	resolveErr(t, f, "https://blog.example/2024/01/post?ref=x", ErrNoFeed)
	want := append([]string{"https://blog.example/2024/01/post?ref=x"}, allFallbacks...)
	if got := f.urls(); !slices.Equal(got, want) {
		t.Errorf("requests =\n%v\nwant\n%v", got, want)
	}
	for _, r := range f.requests() {
		if r.Accept != fetch.FeedAccept {
			t.Errorf("request %s Accept = %q; want FeedAccept", r.URL, r.Accept)
		}
	}
}

func TestResolve_Step4_FirstFallbackThatIsAFeedWins(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/", ctHTML, htmlPage(""))
	f.serve("https://blog.example/feed", ctHTML, htmlPage(""))              // an HTML page, not a feed
	f.fail("https://blog.example/feed/", fetch.ErrTooLarge)                 // unusable
	f.serve("https://blog.example/rss.xml", ctRSS, rssDoc)                  // the winner
	f.serve("https://blog.example/atom.xml", ctAtom, atomDoc)               // never reached
	f.fail("https://blog.example/rss", &fetch.StatusError{StatusCode: 500}) // keeps going
	res := resolveOK(t, f, "https://blog.example/")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://blog.example/rss.xml"}) {
		t.Errorf("candidates = %v", got)
	}
	want := append([]string{"https://blog.example/"}, allFallbacks[:4]...)
	if got := f.urls(); !slices.Equal(got, want) {
		t.Errorf("requests =\n%v\nwant\n%v", got, want)
	}
}

func TestResolve_Step4_WordPressQueryFallback(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/", ctHTML, "<html><body>no head links</body></html>")
	f.serve("https://blog.example/?feed=rss2", ctRSS, rssDoc)
	res := resolveOK(t, f, "https://blog.example/")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://blog.example/?feed=rss2"}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestResolve_Step4_FallbackPermanentRedirect_UsesNewLocation(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/", ctHTML, htmlPage(""))
	r := f.serve("https://blog.example/feed", ctRSS, rssDoc)
	r.FinalURL, r.PermanentURL = "https://blog.example/feed/", "https://blog.example/feed/"
	res := resolveOK(t, f, "https://blog.example/")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://blog.example/feed/"}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestResolve_Step4_FallbackUsesOriginOfFinalPageURL(t *testing.T) {
	f := newFake()
	r := f.serve("http://blog.example/", ctHTML, htmlPage(""))
	r.FinalURL = "https://www.blog.example/"
	f.serve("https://www.blog.example/feed", ctRSS, rssDoc)
	res := resolveOK(t, f, "http://blog.example/")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://www.blog.example/feed"}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestResolve_Step4_AlreadyFetchedURLNotProbedAgain(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/feed", ctHTML, htmlPage("")) // the input itself
	f.serve("https://blog.example/feed/", ctRSS, rssDoc)
	resolveOK(t, f, "https://blog.example/feed")
	want := []string{"https://blog.example/feed", "https://blog.example/feed/"}
	if got := f.urls(); !slices.Equal(got, want) {
		t.Errorf("requests = %v; want %v", got, want)
	}
}

func TestResolve_Step4_NonHTMLNonFeedGoesStraightToFallbacks(t *testing.T) {
	f := newFake()
	// A JSON document that is not a JSON Feed, and contains link-like text.
	f.serve("https://api.example/v1", "application/json",
		`{"html":"<link rel=\"alternate\" type=\"application/rss+xml\" href=\"/x\">"}`)
	f.serve("https://api.example/feed.json", ctJSON, jsonFeedDoc)
	res := resolveOK(t, f, "https://api.example/v1")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://api.example/feed.json"}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestResolve_Step4_PageHTTPErrorStillProbesFallbacks(t *testing.T) {
	f := newFake() // the page itself is a 404
	f.fail("https://blog.example/missing", &fetch.StatusError{URL: "https://blog.example/missing", StatusCode: http.StatusForbidden})
	f.serve("https://blog.example/index.xml", ctRSS, rssDoc)
	res := resolveOK(t, f, "https://blog.example/missing")
	if got := candidateURLs(res); !slices.Equal(got, []string{"https://blog.example/index.xml"}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestResolve_Step5_NothingFound_ErrNoFeed(t *testing.T) {
	t.Run("html page without feeds", func(t *testing.T) {
		f := newFake()
		f.serve("https://plain.example/", ctHTML, htmlPage(`<link rel="icon" href="/favicon.ico">`))
		resolveErr(t, f, "https://plain.example/", ErrNoFeed)
	})
	t.Run("page answers with an HTTP error", func(t *testing.T) {
		f := newFake()
		err := resolveErr(t, f, "https://plain.example/nope", ErrNoFeed)
		var se *fetch.StatusError
		if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound {
			t.Errorf("error %v should keep the page's HTTP status", err)
		}
	})
	t.Run("empty body", func(t *testing.T) {
		f := newFake()
		f.serve("https://plain.example/", "", "")
		resolveErr(t, f, "https://plain.example/", ErrNoFeed)
	})
}

func TestResolve_UnreachableHost_ReturnsFetchErrorWithoutProbing(t *testing.T) {
	f := newFake()
	f.fail("https://down.example/", errUnreachable)
	_, err := New(f).Resolve(context.Background(), "down.example/")
	if !errors.Is(err, errUnreachable) || errors.Is(err, ErrNoFeed) {
		t.Fatalf("err = %v; want the transport error, not ErrNoFeed", err)
	}
	if n := len(f.urls()); n != 1 {
		t.Errorf("made %d requests; want 1", n)
	}
}

func TestResolve_FallbackNetworkFailure_StopsProbing(t *testing.T) {
	f := newFake()
	f.serve("https://blog.example/", ctHTML, htmlPage(""))
	f.fail("https://blog.example/feed/", context.DeadlineExceeded)
	_, err := New(f).Resolve(context.Background(), "https://blog.example/")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v; want the timeout", err)
	}
	if got := f.urls(); len(got) != 3 {
		t.Errorf("requests = %v; probing should stop at the failing one", got)
	}
}

func TestResolve_ContextCancelled_StopsEarly(t *testing.T) {
	t.Run("before the first request", func(t *testing.T) {
		f := newFake()
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, err := New(f).Resolve(ctx, "https://blog.example/")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v; want context.Canceled", err)
		}
		if n := len(f.urls()); n != 0 {
			t.Errorf("made %d requests after cancellation", n)
		}
	})
	t.Run("while probing fallbacks", func(t *testing.T) {
		f := newFake()
		f.serve("https://blog.example/", ctHTML, htmlPage(""))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		f.onFetch = func(n int) {
			if n == 3 {
				cancel()
			}
		}
		_, err := New(f).Resolve(ctx, "https://blog.example/")
		if !errors.Is(err, context.Canceled) || errors.Is(err, ErrNoFeed) {
			t.Fatalf("err = %v; want context.Canceled", err)
		}
		if n := len(f.urls()); n != 3 {
			t.Errorf("made %d requests; want none after cancellation", n)
		}
	})
}

func TestResolve_FetcherReturningNilResult_NoPanic(t *testing.T) {
	_, err := New(nilFetcher{}).Resolve(context.Background(), "https://blog.example/")
	if err == nil {
		t.Fatal("want an error")
	}
}

type nilFetcher struct{}

func (nilFetcher) Fetch(context.Context, fetch.Request) (*fetch.Result, error) { return nil, nil }

func TestResolve_NilFetcher_NoPanic(t *testing.T) {
	if _, err := New(nil).Resolve(context.Background(), "https://blog.example/"); err == nil {
		t.Error("generic URL with no fetcher: want an error")
	}
	var r *Resolver
	if _, err := r.Resolve(context.Background(), "https://blog.example/"); err == nil {
		t.Error("nil resolver: want an error")
	}
}

func TestKindForURL(t *testing.T) {
	for _, tc := range []struct {
		url  string
		want store.Kind
	}{
		{"https://www.youtube.com/feeds/videos.xml?channel_id=UC_x5XG1OV2P6uZZ5FSM9Ttw", store.KindYouTube},
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=PLx", store.KindYouTube},
		{"http://youtube.com/feeds/videos.xml?user=google", store.KindYouTube},
		{"https://m.youtube.com/feeds/videos.xml?channel_id=UC_x5XG1OV2P6uZZ5FSM9Ttw", store.KindYouTube},
		{"https://WWW.YouTube.com/feeds/videos.xml?channel_id=x", store.KindYouTube},
		{"https://www.youtube.com/channel/UC_x5XG1OV2P6uZZ5FSM9Ttw", store.KindRSS},
		{"https://www.youtube.com/feeds/videos.xml.evil", store.KindRSS},
		{"https://notyoutube.com/feeds/videos.xml?channel_id=x", store.KindRSS},
		{"https://youtube.com.evil.example/feeds/videos.xml", store.KindRSS},
		{"https://music.youtube.com/feeds/videos.xml?channel_id=x", store.KindRSS},
		{"ftp://www.youtube.com/feeds/videos.xml", store.KindRSS},
		{"https://blog.example/feed.xml", store.KindRSS},
		{"", store.KindRSS},
		{"::not a url", store.KindRSS},
	} {
		if got := KindForURL(tc.url); got != tc.want {
			t.Errorf("KindForURL(%q) = %q; want %q", tc.url, got, tc.want)
		}
	}
}
