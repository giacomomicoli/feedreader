package resolve

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/giacomomicoli/feedreader/internal/fetch"
)

const ytChannelPage = "https://www.youtube.com/channel/" + ytChannelID

func mustParse(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func ogTitle(content string) string {
	return `<meta property="og:title" content="` + content + `">`
}

func TestChannelFeedID_OnlyChannelFeedsOnYouTube(t *testing.T) {
	for _, tc := range []struct {
		in     string
		wantID string
	}{
		{ytChannelFeed, ytChannelID},
		{"http://m.youtube.com/feeds/videos.xml?channel_id=" + ytChannelID + "&x=1", ytChannelID},
		{"  " + ytChannelFeed + " ", ytChannelID},
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=UULF" + ytChannelID[2:], ""},
		{"https://www.youtube.com/feeds/videos.xml?user=someone", ""},
		{"https://www.youtube.com/feeds/videos.xml?channel_id=UC_short", ""},
		{"https://youtube.com.evil.example/feeds/videos.xml?channel_id=" + ytChannelID, ""},
		{"https://blog.example/feeds/videos.xml?channel_id=" + ytChannelID, ""},
		{"javascript:alert(1)", ""},
		{"", ""},
	} {
		id, ok := ChannelFeedID(tc.in)
		if id != tc.wantID || ok != (tc.wantID != "") {
			t.Errorf("ChannelFeedID(%q) = %q, %v; want %q", tc.in, id, ok, tc.wantID)
		}
	}
	if got := ChannelFeedURL(ytChannelID); got != ytChannelFeed {
		t.Errorf("ChannelFeedURL = %q, want %q", got, ytChannelFeed)
	}
	for _, bad := range []string{"", "UC_short", ytChannelID + "&x=1", "PL" + ytChannelID[2:]} {
		if got := ChannelFeedURL(bad); got != "" {
			t.Errorf("ChannelFeedURL(%q) = %q, want none", bad, got)
		}
	}
}

func TestConfirmChannel_ReadsTitleAndAvatarFromTheChannelPage(t *testing.T) {
	f := newFake()
	f.serve(ytChannelPage, ctHTML, ytHandlePage(canonicalLink(ytChannelID)+ogTitle("Some Channel")+ogImage(ytAvatar900)))
	ch, err := New(f).ConfirmChannel(context.Background(), ytChannelID)
	if err != nil {
		t.Fatalf("ConfirmChannel: %v", err)
	}
	if ch != (Channel{ID: ytChannelID, Title: "Some Channel", IconURL: ytAvatarSmall}) {
		t.Errorf("channel = %+v", ch)
	}
	reqs := f.requests()
	if len(reqs) != 1 || reqs[0].URL != ytChannelPage || reqs[0].Accept != fetch.HTMLAccept {
		t.Errorf("requests = %+v; want one HTML request for %s", reqs, ytChannelPage)
	}
}

func TestConfirmChannel_TitleAndAvatarAreBestEffort(t *testing.T) {
	f := newFake()
	f.serve(ytChannelPage, ctHTML, ytHandlePage(identifierMeta(ytChannelID)))
	ch, err := New(f).ConfirmChannel(context.Background(), ytChannelID)
	if err != nil || ch != (Channel{ID: ytChannelID}) {
		t.Errorf("ConfirmChannel = %+v, %v; want the bare channel", ch, err)
	}
}

func TestConfirmChannel_HostileTitleIsOneBoundedLineOfText(t *testing.T) {
	long := strings.Repeat("é", 4*MaxIconURLBytes)
	for _, tc := range []struct{ name, content, want string }{
		{"markup stays text", "&lt;script&gt;alert(1)&lt;/script&gt; &amp; co", "<script>alert(1)</script> & co"},
		{"control characters and newlines", "a&#10;b\x01c\x1b&#13; d", "a b c d"},
		{"long", long, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			f.serve(ytChannelPage, ctHTML, ytHandlePage(canonicalLink(ytChannelID)+ogTitle(tc.content)))
			ch, err := New(f).ConfirmChannel(context.Background(), ytChannelID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.want != "" && ch.Title != tc.want {
				t.Errorf("title = %q, want %q", ch.Title, tc.want)
			}
			if strings.ContainsAny(ch.Title, "\n\r\x00\x1b") || utf8.RuneCountInString(ch.Title) > maxChannelTitleRunes {
				t.Errorf("title not bounded to one short line: %.40q (%d runes)", ch.Title, utf8.RuneCountInString(ch.Title))
			}
		})
	}
}

func TestConfirmChannel_RefusesWhatDoesNotConfirmTheChannel(t *testing.T) {
	for _, tc := range []struct {
		name     string
		body     string
		finalURL string
	}{
		{"another channel's page", ytHandlePage(canonicalLink(ytOtherID) + ogTitle("Other")), ""},
		{"no channel id", ytHandlePage(ogTitle("Someone")), ""},
		{"consent interstitial", `<html><body><form action="https://consent.youtube.com/save"></form></body></html>`, ""},
		{"served from a consent host", ytHandlePage(canonicalLink(ytChannelID)), "https://consent.youtube.com/m?continue=x"},
		{"served from elsewhere", ytHandlePage(canonicalLink(ytChannelID)), "https://elsewhere.example/channel/" + ytChannelID},
		{"redirected to a watch page", ytHandlePage(rssAlternate(ytChannelID) + ogTitle("Video title")), "https://www.youtube.com/watch?v=abcdefghijk"},
		{"redirected to the home page", ytHandlePage(canonicalLink(ytChannelID)), "https://www.youtube.com/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			r := f.serve(ytChannelPage, ctHTML, tc.body)
			r.FinalURL = tc.finalURL
			if ch, err := New(f).ConfirmChannel(context.Background(), ytChannelID); !errors.Is(err, ErrChannelNotConfirmed) {
				t.Errorf("ConfirmChannel = %+v, %v; want ErrChannelNotConfirmed", ch, err)
			}
		})
	}
	t.Run("page gone", func(t *testing.T) {
		var se *fetch.StatusError
		_, err := New(newFake()).ConfirmChannel(context.Background(), ytChannelID)
		if !errors.Is(err, ErrChannelNotConfirmed) || !errors.As(err, &se) {
			t.Errorf("err = %v; want ErrChannelNotConfirmed wrapping the HTTP error", err)
		}
	})
	t.Run("network failure", func(t *testing.T) {
		f := newFake()
		f.fail(ytChannelPage, errUnreachable)
		if _, err := New(f).ConfirmChannel(context.Background(), ytChannelID); !errors.Is(err, ErrChannelNotConfirmed) || !errors.Is(err, errUnreachable) {
			t.Errorf("err = %v; want ErrChannelNotConfirmed wrapping the network error", err)
		}
	})
}

func TestChannelFromPage_OnlyAChannelsOwnPageOnYouTube(t *testing.T) {
	body := []byte(ytHandlePage(canonicalLink(ytChannelID) + ogTitle("Name") + ogImage(ytAvatar900)))
	want := Channel{ID: ytChannelID, Title: "Name", IconURL: ytAvatarSmall}
	for _, tc := range []struct {
		name, requested, final string
		ok                     bool
	}{
		{"handle page", "https://www.youtube.com/@GoogleDevelopers", "", true},
		{"handle page after consent redirects", "https://www.youtube.com/@GoogleDevelopers", "https://www.youtube.com/@GoogleDevelopers?cbrd=1", true},
		{"/c/ page", "https://www.youtube.com/c/GoogleDevelopers", "", true},
		{"/user/ page", "https://m.youtube.com/user/GoogleDevelopers", "", true},
		{"the channel's own page", ytChannelPage, "", true},
		{"another channel's /channel/ page", "https://www.youtube.com/channel/" + ytOtherID, "", false},
		{"video page", "https://www.youtube.com/watch?v=abcdefghijk", "", false},
		{"served from elsewhere", "https://www.youtube.com/@GoogleDevelopers", "https://elsewhere.example/@GoogleDevelopers", false},
		{"redirected to a watch page", "https://www.youtube.com/@GoogleDevelopers", "https://www.youtube.com/watch?v=abcdefghijk", false},
		{"channel page redirected to a watch page", ytChannelPage, "https://www.youtube.com/watch?v=abcdefghijk", false},
		{"redirected to the home page", "https://www.youtube.com/c/GoogleDevelopers", "https://www.youtube.com/", false},
		{"not YouTube", "https://blog.example/@GoogleDevelopers", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &fetch.Result{StatusCode: http.StatusOK, ContentType: ctHTML, Body: body, FinalURL: tc.final}
			ch, ok := ChannelFromPage(tc.requested, res)
			if ok != tc.ok || (ok && ch != want) {
				t.Errorf("ChannelFromPage = %+v, %v; want %v", ch, ok, tc.ok)
			}
		})
	}
	if _, ok := ChannelFromPage(ytChannelPage, nil); ok {
		t.Error("nil result confirmed a channel")
	}
	if _, ok := ChannelFromPage(ytChannelPage, &fetch.Result{NotModified: true}); ok {
		t.Error("304 confirmed a channel")
	}
}

func TestConfirmChannel_InvalidIDMakesNoRequest(t *testing.T) {
	for _, id := range []string{"", "UC_short", ytChannelID + "/../x", "PL" + ytChannelID[2:], ytChannelID + "?x"} {
		f := newFake()
		if _, err := New(f).ConfirmChannel(context.Background(), id); !errors.Is(err, ErrChannelNotConfirmed) || len(f.urls()) != 0 {
			t.Errorf("ConfirmChannel(%q) = %v after %d requests; want ErrChannelNotConfirmed without a request", id, err, len(f.urls()))
		}
	}
}

func TestChannelPageFromHTML_TitleOnlyFromAPageServedByYouTube(t *testing.T) {
	body := []byte(ytHandlePage(canonicalLink(ytChannelID) + ogTitle("Name")))
	for _, tc := range []struct{ page, want string }{
		{"https://www.youtube.com/channel/" + ytChannelID, "Name"},
		{"https://elsewhere.example/channel/" + ytChannelID, ""},
	} {
		u := mustParse(t, tc.page)
		cp, ok := channelPageFromHTML(body, u)
		if !ok || cp.title != tc.want {
			t.Errorf("page %s: %+v, %v; want title %q", tc.page, cp, ok, tc.want)
		}
	}
}
