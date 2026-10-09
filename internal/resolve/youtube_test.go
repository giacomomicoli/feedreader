package resolve

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/fetch"
)

const (
	ytChannelID   = "UC_x5XG1OV2P6uZZ5FSM9Ttw"
	ytOtherID     = "UCB5ofLXaAv_a1QopZ801K7A"
	ytThirdID     = "UCK8sQmJBp8GCxrOtXWBpyEA"
	ytChannelFeed = "https://www.youtube.com/feeds/videos.xml?channel_id=" + ytChannelID
)

// realHandlePageEnv names an optional real YouTube /@handle page saved to
// disk (about 1.6 MB, not committed) for TestResolve_YouTube_RealHandlePage.
const realHandlePageEnv = "FEEDREADER_YT_HANDLE_PAGE"

func TestResolve_YouTube_FeedURL_UsedAsIsWithoutRequest(t *testing.T) {
	for _, raw := range []string{
		"https://www.youtube.com/feeds/videos.xml?channel_id=" + ytChannelID,
		"https://www.youtube.com/feeds/videos.xml?playlist_id=PLOU2XLYxmsIIM9h1Ybw2DuRw6o2fkNMeR",
		"https://www.youtube.com/feeds/videos.xml?user=GoogleDevelopers",
		"http://youtube.com/feeds/videos.xml?channel_id=" + ytChannelID,
		"https://m.youtube.com/feeds/videos.xml?channel_id=" + ytChannelID,
	} {
		t.Run(raw, func(t *testing.T) {
			f := newFake()
			res := resolveOK(t, f, raw)
			if got := candidateURLs(res); !slices.Equal(got, []string{raw}) {
				t.Errorf("candidates = %v; want the URL as-is", got)
			}
			if n := len(f.urls()); n != 0 {
				t.Errorf("made %d requests; want none", n)
			}
		})
	}
	t.Run("without scheme", func(t *testing.T) {
		f := newFake()
		res := resolveOK(t, f, "youtube.com/feeds/videos.xml?channel_id="+ytChannelID)
		want := "https://youtube.com/feeds/videos.xml?channel_id=" + ytChannelID
		if got := candidateURLs(res); !slices.Equal(got, []string{want}) {
			t.Errorf("candidates = %v; want [%s]", got, want)
		}
	})
}

func TestResolve_YouTube_ChannelURL_FeedURLWithoutRequest(t *testing.T) {
	for _, raw := range []string{
		"https://www.youtube.com/channel/" + ytChannelID,
		"youtube.com/channel/" + ytChannelID,
		"http://m.youtube.com/channel/" + ytChannelID + "?si=abc",
		"https://www.youtube.com/channel/" + ytChannelID + "/videos",
		"https://www.youtube.com/channel/" + ytChannelID + "/",
		"WWW.YOUTUBE.COM/channel/" + ytChannelID,
	} {
		t.Run(raw, func(t *testing.T) {
			f := newFake()
			res := resolveOK(t, f, raw)
			want := []Candidate{{URL: ytChannelFeed}}
			if !slices.Equal(res.Candidates, want) {
				t.Errorf("candidates = %+v; want %+v", res.Candidates, want)
			}
			if n := len(f.urls()); n != 0 {
				t.Errorf("made %d requests; want none", n)
			}
		})
	}
	t.Run("no fetcher needed", func(t *testing.T) {
		res, err := New(nil).Resolve(context.Background(), "https://www.youtube.com/channel/"+ytChannelID)
		if err != nil || res.Candidates[0].URL != ytChannelFeed {
			t.Errorf("Resolve = %+v, %v", res, err)
		}
	})
}

func TestResolve_YouTube_PlaylistURL_FeedURLWithoutRequest(t *testing.T) {
	for _, tc := range []struct{ raw, id string }{
		{"https://www.youtube.com/playlist?list=PLOU2XLYxmsIIM9h1Ybw2DuRw6o2fkNMeR", "PLOU2XLYxmsIIM9h1Ybw2DuRw6o2fkNMeR"},
		{"youtube.com/playlist?list=UU_x5XG1OV2P6uZZ5FSM9Ttw&si=xyz", "UU_x5XG1OV2P6uZZ5FSM9Ttw"},
		{"https://m.youtube.com/playlist?feature=share&list=OLAK5uy_kXy-abc_DEF", "OLAK5uy_kXy-abc_DEF"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			f := newFake()
			res := resolveOK(t, f, tc.raw)
			want := "https://www.youtube.com/feeds/videos.xml?playlist_id=" + tc.id
			if got := candidateURLs(res); !slices.Equal(got, []string{want}) {
				t.Errorf("candidates = %v; want [%s]", got, want)
			}
			if n := len(f.urls()); n != 0 {
				t.Errorf("made %d requests; want none", n)
			}
		})
	}
}

func TestResolve_YouTube_MalformedID_ErrYouTubeIDWithoutRequest(t *testing.T) {
	for _, raw := range []string{
		"https://www.youtube.com/channel/UC_x5XG1OV2P6uZZ5FSM9Tt",   // 21 chars
		"https://www.youtube.com/channel/UC_x5XG1OV2P6uZZ5FSM9Ttw0", // 23 chars
		"https://www.youtube.com/channel/XX_x5XG1OV2P6uZZ5FSM9Ttw",  // wrong prefix
		"https://www.youtube.com/channel/UC_x5XG1OV2P6uZZ5FSM9T%21w",
		"https://www.youtube.com/channel/",
		"https://www.youtube.com/playlist",
		"https://www.youtube.com/playlist?list=",
		"https://www.youtube.com/playlist?list=PL%3Cscript%3E",
		"https://www.youtube.com/@",
		"https://www.youtube.com/c/",
		"https://www.youtube.com/user",
		"https://www.youtube.com/feeds/videos.xml",
		"https://www.youtube.com/feeds/videos.xml?channel_id=",
	} {
		t.Run(raw, func(t *testing.T) {
			f := newFake()
			resolveErr(t, f, raw, ErrYouTubeID)
			if n := len(f.urls()); n != 0 {
				t.Errorf("made %d requests; want none", n)
			}
		})
	}
}

// ytHandlePage is a trimmed synthetic channel page with the markup traits
// of the real one: huge inline scripts mentioning other channels, the
// identifying elements after </head>, links to featured channels, and
// app/mobile alternates.
func ytHandlePage(identifying string) string {
	return `<!DOCTYPE html><html lang="it-IT"><head>
<script nonce="x">var ytInitialData = {"canonicalBaseUrl":"/@GoogleDevelopers",
"html":"<link rel=\"canonical\" href=\"https://www.youtube.com/channel/` + ytThirdID + `\">",
"channelId":"` + ytOtherID + `"};</script>
<title>Google for Developers - YouTube</title>
</head><body dir="ltr"><div id="watch7-content">
<link rel="alternate" media="handheld" href="https://m.youtube.com/@GoogleDevelopers">
<link rel="alternate" href="android-app://com.google.android.youtube/http/www.youtube.com/channel/` + ytOtherID + `">
<a href="/channel/` + ytOtherID + `">Featured channel</a>
<span itemscope itemtype="http://schema.org/Person"><meta itemprop="name" content="Featured"></span>
` + identifying + `
</div><script>window.ytcfg = {"x": "</div>"};</script></body></html>`
}

func canonicalLink(id string) string {
	return `<link rel="canonical" href="https://www.youtube.com/channel/` + id + `">`
}

func identifierMeta(id string) string {
	return `<meta itemprop="identifier" content="` + id + `">`
}

func rssAlternate(id string) string {
	return `<link rel="alternate" type="application/rss+xml" title="RSS" href="https://www.youtube.com/feeds/videos.xml?channel_id=` + id + `">`
}

// Synthetic channel avatar on YouTube's image server, as advertised by the
// page (900 px) and as the resolver asks for it (ytAvatarPx).
const (
	ytAvatarOpts = "-c-k-c0x00ffffff-no-rj"
	ytAvatarBase = "https://yt3.googleusercontent.com/synthetic_Avatar-Id0123456789"
	ytAvatar900  = ytAvatarBase + "=s900" + ytAvatarOpts
)

var ytAvatarSmall = ytAvatarBase + "=s" + strconv.Itoa(ytAvatarPx) + ytAvatarOpts

func ogImage(content string) string {
	return `<meta property="og:image" content="` + content + `">`
}

func TestResolve_YouTube_HandleChannelUserURLs_FetchPageWithHTMLAccept(t *testing.T) {
	for _, tc := range []struct{ raw, page string }{
		{"https://www.youtube.com/@GoogleDevelopers", "https://www.youtube.com/@GoogleDevelopers"},
		{"youtube.com/@GoogleDevelopers/videos?si=tracking", "https://www.youtube.com/@GoogleDevelopers"},
		{"https://m.youtube.com/@GoogleDevelopers", "https://www.youtube.com/@GoogleDevelopers"},
		{"https://www.youtube.com/c/GoogleDevelopers", "https://www.youtube.com/c/GoogleDevelopers"},
		{"https://www.youtube.com/C/GoogleDevelopers/featured", "https://www.youtube.com/c/GoogleDevelopers"},
		{"https://www.youtube.com/user/GoogleDevelopers", "https://www.youtube.com/user/GoogleDevelopers"},
		{"https://www.youtube.com/@caf%C3%A9", "https://www.youtube.com/@caf%C3%A9"},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			f := newFake()
			f.serve(tc.page, ctHTML, ytHandlePage(canonicalLink(ytChannelID)))
			res := resolveOK(t, f, tc.raw)
			if got := candidateURLs(res); !slices.Equal(got, []string{ytChannelFeed}) {
				t.Errorf("candidates = %v; want [%s]", got, ytChannelFeed)
			}
			reqs := f.requests()
			if len(reqs) != 1 || reqs[0].URL != tc.page || reqs[0].Accept != fetch.HTMLAccept {
				t.Errorf("requests = %+v; want one HTML request for %s", reqs, tc.page)
			}
		})
	}
}

func TestResolve_YouTube_HandlePage_ChannelIDSources(t *testing.T) {
	for _, tc := range []struct {
		name, markup, want string
	}{
		{"canonical link", canonicalLink(ytChannelID), ytChannelID},
		{"itemprop identifier", identifierMeta(ytChannelID), ytChannelID},
		{"rss alternate link", rssAlternate(ytChannelID), ytChannelID},
		{"relative canonical", `<link rel="canonical" href="/channel/` + ytChannelID + `">`, ytChannelID},
		{"canonical preferred over itemprop and alternate",
			rssAlternate(ytThirdID) + identifierMeta(ytOtherID) + canonicalLink(ytChannelID), ytChannelID},
		{"itemprop preferred over alternate",
			rssAlternate(ytOtherID) + identifierMeta(ytChannelID), ytChannelID},
		{"canonical to a handle falls back to itemprop",
			`<link rel="canonical" href="https://www.youtube.com/@GoogleDevelopers">` + identifierMeta(ytChannelID), ytChannelID},
		{"invalid canonical id falls back to alternate",
			canonicalLink("UC_short") + rssAlternate(ytChannelID), ytChannelID},
		{"canonical on another host ignored",
			`<link rel="canonical" href="https://evil.example/channel/` + ytOtherID + `">` + identifierMeta(ytChannelID), ytChannelID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.serve("https://www.youtube.com/@GoogleDevelopers", ctHTML, ytHandlePage(tc.markup))
			res := resolveOK(t, f, "https://www.youtube.com/@GoogleDevelopers")
			want := "https://www.youtube.com/feeds/videos.xml?channel_id=" + tc.want
			if got := candidateURLs(res); !slices.Equal(got, []string{want}) {
				t.Errorf("candidates = %v; want [%s]", got, want)
			}
		})
	}
}

func TestResolve_YouTube_HandlePage_AvatarFromTheSamePage(t *testing.T) {
	// As on the real page, the og:image comes after the canonical link.
	f := newFake()
	f.serve("https://www.youtube.com/@GoogleDevelopers", ctHTML,
		ytHandlePage(canonicalLink(ytChannelID)+`<meta property="og:title" content="x">`+ogImage(ytAvatar900)))
	res := resolveOK(t, f, "https://www.youtube.com/@GoogleDevelopers")
	want := []Candidate{{URL: ytChannelFeed, IconURL: ytAvatarSmall}}
	if !slices.Equal(res.Candidates, want) {
		t.Errorf("candidates = %+v; want %+v", res.Candidates, want)
	}
	if n := len(f.urls()); n != 1 {
		t.Errorf("made %d requests; want only the page fetched for the channel ID", n)
	}
}

func TestResolve_YouTube_HandlePage_AvatarIsBestEffort(t *testing.T) {
	long := "https://yt3.googleusercontent.com/" + strings.Repeat("a", MaxIconURLBytes)
	for _, tc := range []struct {
		name, markup, want string
	}{
		{"before the canonical link", ogImage(ytAvatar900) + canonicalLink(ytChannelID), ytAvatarSmall},
		{"with itemprop identifier only", identifierMeta(ytChannelID) + ogImage(ytAvatar900), ytAvatarSmall},
		{"surrounding whitespace", canonicalLink(ytChannelID) + ogImage("\n "+ytAvatar900+"\t"), ytAvatarSmall},
		{"property name case", canonicalLink(ytChannelID) + `<meta property="OG:Image" content="` + ytAvatar900 + `">`, ytAvatarSmall},
		{"first og:image wins", canonicalLink(ytChannelID) + ogImage(ytAvatar900) + ogImage("https://yt3.ggpht.com/other=s900"), ytAvatarSmall},
		{"invalid one skipped", canonicalLink(ytChannelID) + ogImage("javascript:alert(1)") + ogImage(ytAvatar900), ytAvatarSmall},
		{"missing", canonicalLink(ytChannelID), ""},
		{"empty", canonicalLink(ytChannelID) + ogImage(""), ""},
		{"relative", canonicalLink(ytChannelID) + ogImage("/img/avatar.jpg"), ""},
		{"protocol-relative", canonicalLink(ytChannelID) + ogImage("//yt3.googleusercontent.com/a=s900"), ""},
		{"not http", canonicalLink(ytChannelID) + ogImage("data:image/png;base64,AAAA"), ""},
		{"too long", canonicalLink(ytChannelID) + ogImage(long), ""},
		{"other meta attributes", canonicalLink(ytChannelID) +
			`<meta name="og:image" content="` + ytAvatar900 + `"><meta itemprop="image" content="` + ytAvatar900 + `">`, ""},
		{"inside a script", canonicalLink(ytChannelID) + `<script>"` + ogImage(ytAvatar900) + `"</script>`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.serve("https://www.youtube.com/@GoogleDevelopers", ctHTML, ytHandlePage(tc.markup))
			res := resolveOK(t, f, "https://www.youtube.com/@GoogleDevelopers")
			want := []Candidate{{URL: ytChannelFeed, IconURL: tc.want}}
			if !slices.Equal(res.Candidates, want) {
				t.Errorf("candidates = %+v; want %+v", res.Candidates, want)
			}
		})
	}
}

func TestResolve_YouTube_AvatarOnlyFromAPageServedByYouTube(t *testing.T) {
	f := newFake()
	r := f.serve("https://www.youtube.com/@GoogleDevelopers", ctHTML,
		ytHandlePage(canonicalLink(ytChannelID)+ogImage(ytAvatar900)))
	r.FinalURL = "https://elsewhere.example/@GoogleDevelopers"
	res := resolveOK(t, f, "https://www.youtube.com/@GoogleDevelopers")
	want := []Candidate{{URL: ytChannelFeed}}
	if !slices.Equal(res.Candidates, want) {
		t.Errorf("candidates = %+v; want %+v", res.Candidates, want)
	}
}

func TestChannelAvatar_FetchesTheChannelPageOfAChannelFeed(t *testing.T) {
	const page = "https://www.youtube.com/channel/" + ytChannelID
	f := newFake()
	f.serve(page, ctHTML, ytHandlePage(canonicalLink(ytChannelID)+ogImage(ytAvatar900)))
	avatar, err := New(f).ChannelAvatar(context.Background(), ytChannelFeed)
	if err != nil || avatar != ytAvatarSmall {
		t.Errorf("ChannelAvatar = %q, %v; want %q", avatar, err, ytAvatarSmall)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].URL != page || reqs[0].Accept != fetch.HTMLAccept {
		t.Errorf("requests = %+v; want one HTML request for %s", reqs, page)
	}
}

func TestChannelAvatar_BestEffort(t *testing.T) {
	const page = "https://www.youtube.com/channel/" + ytChannelID
	for _, tc := range []struct{ name, body string }{
		{"no avatar", ytHandlePage(canonicalLink(ytChannelID))},
		{"another channel's page", ytHandlePage(canonicalLink(ytOtherID) + ogImage(ytAvatar900))},
		{"no channel ID", ytHandlePage(ogImage(ytAvatar900))},
		{"consent interstitial", `<html><body><form action="https://consent.youtube.com/save"></form></body></html>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.serve(page, ctHTML, tc.body)
			if avatar, err := New(f).ChannelAvatar(context.Background(), ytChannelFeed); avatar != "" || err != nil {
				t.Errorf("ChannelAvatar = %q, %v; want none", avatar, err)
			}
		})
	}
	t.Run("page gone", func(t *testing.T) {
		var se *fetch.StatusError
		if avatar, err := New(newFake()).ChannelAvatar(context.Background(), ytChannelFeed); avatar != "" || !errors.As(err, &se) {
			t.Errorf("ChannelAvatar = %q, %v; want the HTTP error", avatar, err)
		}
	})
	t.Run("network failure", func(t *testing.T) {
		f := newFake()
		f.fail(page, errUnreachable)
		if avatar, err := New(f).ChannelAvatar(context.Background(), ytChannelFeed); avatar != "" || !errors.Is(err, errUnreachable) {
			t.Errorf("ChannelAvatar = %q, %v; want the network error", avatar, err)
		}
	})
}

func TestChannelAvatar_OtherFeedsWithoutRequest(t *testing.T) {
	for _, feedURL := range []string{
		"https://www.youtube.com/feeds/videos.xml?playlist_id=PLOU2XLYxmsIIM9h1Ybw2DuRw6o2fkNMeR",
		"https://www.youtube.com/feeds/videos.xml?user=GoogleDevelopers",
		"https://www.youtube.com/feeds/videos.xml?channel_id=UC_short",
		"https://youtube.com.evil.example/feeds/videos.xml?channel_id=" + ytChannelID,
		"https://blog.example.com/feed.xml",
		"javascript:alert(1)",
		"",
	} {
		if IsChannelFeed(feedURL) {
			t.Errorf("IsChannelFeed(%q) = true", feedURL)
		}
		f := newFake()
		if avatar, err := New(f).ChannelAvatar(context.Background(), feedURL); avatar != "" || err != nil || len(f.urls()) != 0 {
			t.Errorf("ChannelAvatar(%q) = %q, %v after %d requests; want none", feedURL, avatar, err, len(f.urls()))
		}
	}
	for _, feedURL := range []string{ytChannelFeed, "http://m.youtube.com/feeds/videos.xml?channel_id=" + ytChannelID + "&x=1"} {
		if !IsChannelFeed(feedURL) {
			t.Errorf("IsChannelFeed(%q) = false", feedURL)
		}
	}
}

func TestSmallAvatar(t *testing.T) {
	px := "=s" + strconv.Itoa(ytAvatarPx)
	for _, tc := range []struct{ in, want string }{
		{ytAvatar900, ytAvatarSmall},
		{"https://yt3.googleusercontent.com/ytc/AIdro_x-Y=s900-c-k-c0x00ffffff-no-rj", "https://yt3.googleusercontent.com/ytc/AIdro_x-Y" + px + "-c-k-c0x00ffffff-no-rj"},
		{"https://yt3.ggpht.com/abc=s240", "https://yt3.ggpht.com/abc" + px},
		{"https://YT3.GOOGLEUSERCONTENT.COM./abc=c-k-s900-no-rj", "https://YT3.GOOGLEUSERCONTENT.COM./abc=c-k-s" + strconv.Itoa(ytAvatarPx) + "-no-rj"},
		{"https://yt3.googleusercontent.com/a=b/c=s900?x=1", "https://yt3.googleusercontent.com/a=b/c" + px + "?x=1"},
		// Left alone: no size option, or not one of Google's image servers.
		{"https://yt3.googleusercontent.com/abc", "https://yt3.googleusercontent.com/abc"},
		{"https://yt3.googleusercontent.com/abc=w900-h900", "https://yt3.googleusercontent.com/abc=w900-h900"},
		{"https://yt3.googleusercontent.com/abc=s-sx", "https://yt3.googleusercontent.com/abc=s-sx"},
		{"https://img.example/abc=s900-c", "https://img.example/abc=s900-c"},
		{"https://googleusercontent.com.evil.example/abc=s900", "https://googleusercontent.com.evil.example/abc=s900"},
	} {
		u, err := url.Parse(tc.in)
		if err != nil {
			t.Fatal(err)
		}
		smallAvatar(u)
		if got := u.String(); got != tc.want {
			t.Errorf("smallAvatar(%s) = %s; want %s", tc.in, got, tc.want)
		}
	}
}

func TestResolve_YouTube_HandlePageAfterConsentRedirects(t *testing.T) {
	// From the EU the page is served after redirects adding query flags.
	f := newFake()
	r := f.serve("https://www.youtube.com/@GoogleDevelopers", ctHTML, ytHandlePage(canonicalLink(ytChannelID)+ogImage(ytAvatar900)))
	r.FinalURL = "https://www.youtube.com/@GoogleDevelopers?cbrd=1&ucbcb=1"
	res := resolveOK(t, f, "https://www.youtube.com/@GoogleDevelopers")
	want := []Candidate{{URL: ytChannelFeed, IconURL: ytAvatarSmall}}
	if !slices.Equal(res.Candidates, want) {
		t.Errorf("candidates = %+v; want %+v", res.Candidates, want)
	}
}

func TestResolve_YouTube_HandleCannotBeResolved_ErrYouTubeID(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"markup changed", ytHandlePage(`<meta itemprop="name" content="Google for Developers">`)},
		{"consent interstitial", `<html><body><form action="https://consent.youtube.com/save"><button>Accept all</button></form></body></html>`},
		{"malformed ids only", ytHandlePage(canonicalLink("UC123") + identifierMeta("not-an-id") + rssAlternate("UC!"))},
		{"empty body", ""},
		{"binary garbage", "\x00\xff\xfe<\x00l\x00i\x00n\x00k"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.serve("https://www.youtube.com/@gone", ctHTML, tc.body)
			resolveErr(t, f, "https://www.youtube.com/@gone", ErrYouTubeID)
		})
	}
}

func TestResolve_YouTube_HandlePageHTTPError_ErrYouTubeID(t *testing.T) {
	f := newFake() // 404: the handle does not exist
	err := resolveErr(t, f, "https://www.youtube.com/@doesnotexist", ErrYouTubeID)
	var se *fetch.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound {
		t.Errorf("err = %v; want the HTTP status kept too", err)
	}
}

func TestResolve_YouTube_HandlePageNetworkFailure_NotErrYouTubeID(t *testing.T) {
	f := newFake()
	f.fail("https://www.youtube.com/@GoogleDevelopers", errUnreachable)
	_, err := New(f).Resolve(context.Background(), "https://www.youtube.com/@GoogleDevelopers")
	if !errors.Is(err, errUnreachable) || errors.Is(err, ErrYouTubeID) {
		t.Errorf("err = %v; want the network error, not ErrYouTubeID", err)
	}
}

func TestResolve_YouTube_LargeHandlePage_StreamedToTheEnd(t *testing.T) {
	// ~3 MB of inline script before the identifying elements.
	padding := `<script>var x = "` + strings.Repeat(`<link rel=\"canonical\" href=\"/channel/`+ytOtherID+`\">`, 40000) + `";</script>`
	body := ytHandlePage(padding + identifierMeta(ytChannelID))
	if len(body) < 2<<20 {
		t.Fatalf("test page is only %d bytes", len(body))
	}
	f := newFake()
	f.serve("https://www.youtube.com/@big", ctHTML, body)
	res := resolveOK(t, f, "https://www.youtube.com/@big")
	if got := candidateURLs(res); !slices.Equal(got, []string{ytChannelFeed}) {
		t.Errorf("candidates = %v", got)
	}
}

func TestResolve_YouTube_RealHandlePage(t *testing.T) {
	path := os.Getenv(realHandlePageEnv)
	if path == "" {
		t.Skipf("set %s to a saved YouTube /@handle page to run this test", realHandlePageEnv)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("real page sample not available: %v", err)
	}
	pageURL, _ := url.Parse("https://www.youtube.com/@GoogleDevelopers?cbrd=1&ucbcb=1")
	id, avatar, ok := channelFromHTML(body, pageURL)
	if !ok || id != ytChannelID {
		t.Fatalf("channelFromHTML = %q, %v; want %q", id, ok, ytChannelID)
	}
	if px := "=s" + strconv.Itoa(ytAvatarPx) + "-"; !strings.HasPrefix(avatar, "https://") || !strings.Contains(avatar, px) {
		t.Errorf("avatar = %q; want an https URL asking for %s", avatar, px)
	}
	// Each source on its own must also yield the channel's ID.
	for _, tc := range []struct {
		name string
		find func(*url.URL, tag) string
	}{
		{"canonical", func(b *url.URL, t tag) string {
			if t.name == "link" && hasRel(t.attrs["rel"], "canonical") {
				return channelIDFromChannelURL(t.attrs["href"], b)
			}
			return ""
		}},
		{"itemprop identifier", func(_ *url.URL, t tag) string {
			if t.name == "meta" && t.attrs["itemprop"] == "identifier" {
				return t.attrs["content"]
			}
			return ""
		}},
		{"rss alternate", func(b *url.URL, t tag) string {
			if t.name == "link" && hasRel(t.attrs["rel"], "alternate") {
				return channelIDFromFeedURL(t.attrs["href"], b)
			}
			return ""
		}},
	} {
		var found string
		_ = scanTags(bytes.NewReader(body), []string{"link", "meta"}, func(tg tag) bool {
			found = tc.find(pageURL, tg)
			return found == ""
		})
		if found != ytChannelID {
			t.Errorf("%s: found %q; want %q", tc.name, found, ytChannelID)
		}
	}
}

func TestResolve_YouTube_OtherPaths_UseGenericResolution(t *testing.T) {
	// A video page is none of the channel/playlist/feed forms: it goes through
	// autodiscovery, which finds the channel feed YouTube advertises.
	const watch = "https://www.youtube.com/watch?v=7Ol-0Iq0JMA"
	f := newFake()
	f.serve(watch, ctHTML, ytHandlePage(rssAlternate(ytChannelID)))
	res := resolveOK(t, f, watch)
	if got := candidateURLs(res); !slices.Equal(got, []string{ytChannelFeed}) {
		t.Errorf("candidates = %v", got)
	}
	if reqs := f.requests(); len(reqs) != 1 || reqs[0].Accept != fetch.FeedAccept {
		t.Errorf("requests = %+v; want one generic request", reqs)
	}
}

func TestResolve_YouTube_LookalikeHostsAreNotYouTube(t *testing.T) {
	for _, raw := range []string{
		"https://youtube.com.evil.example/channel/" + ytChannelID,
		"https://notyoutube.com/channel/" + ytChannelID,
		"https://www.youtube.com@evil.example/channel/" + ytChannelID,
	} {
		t.Run(raw, func(t *testing.T) {
			f := newFake()
			resolveErr(t, f, raw, ErrNoFeed) // generic flow: every request 404s
			if n := len(f.urls()); n == 0 {
				t.Error("lookalike host was answered without a request")
			}
		})
	}
}
