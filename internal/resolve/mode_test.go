package resolve

import (
	"context"
	"slices"
	"testing"
)

// A synthetic channel for the Shorts modes: its ID and the IDs of its two
// per-type upload playlists (the channel ID without "UC", after "UULF" or
// "UUSH").
const (
	modeChanID     = "UCsynthetic0channel0id00"
	modeChanRest   = "synthetic0channel0id00"
	modeChanFeed   = "https://www.youtube.com/feeds/videos.xml?channel_id=" + modeChanID
	modeVideosFeed = "https://www.youtube.com/feeds/videos.xml?playlist_id=UULF" + modeChanRest
	modeShortsFeed = "https://www.youtube.com/feeds/videos.xml?playlist_id=UUSH" + modeChanRest
)

func TestModeChannelIsWellFormed(t *testing.T) {
	if !channelIDPattern.MatchString(modeChanID) || modeChanID != "UC"+modeChanRest {
		t.Fatalf("test channel %q is not a channel ID", modeChanID)
	}
}

func TestParseMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Mode
		ok   bool
	}{
		{"both", ModeBoth, true},
		{"videos", ModeVideos, true},
		{"shorts", ModeShorts, true},
		{"", "", false},
		{"Both", "", false},
		{" videos", "", false},
		{"long", "", false},
		{"UULF", "", false},
	} {
		got, ok := ParseMode(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ParseMode(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
	if got := Modes(); !slices.Equal(got, []Mode{ModeBoth, ModeVideos, ModeShorts}) {
		t.Errorf("Modes() = %v", got)
	}
}

func TestFeedMode(t *testing.T) {
	for _, tc := range []struct {
		feedURL string
		mode    Mode // "" = no mode
	}{
		{modeChanFeed, ModeBoth},
		{modeVideosFeed, ModeVideos},
		{modeShortsFeed, ModeShorts},
		// Other spellings of the same feeds.
		{"http://m.youtube.com/feeds/videos.xml?channel_id=" + modeChanID + "&x=1", ModeBoth},
		{"https://youtube.com/feeds/videos.xml?playlist_id=UULF" + modeChanRest + "&utm_source=x", ModeVideos},
		{"https://YouTube.com./feeds/videos.xml?playlist_id=UUSH" + modeChanRest, ModeShorts},
		// The channel_id parameter wins, as YouTube's own feed URLs use one
		// or the other.
		{modeChanFeed + "&playlist_id=UUSH" + modeChanRest, ModeBoth},
		// Not a mode feed.
		{"https://www.youtube.com/feeds/videos.xml?channel_id=UC_short", ""},
		{"https://www.youtube.com/feeds/videos.xml?channel_id=UC_short&playlist_id=UULF" + modeChanRest, ""},
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=UU" + modeChanRest, ""},   // all uploads
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=UULV" + modeChanRest, ""}, // live streams
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=UULP" + modeChanRest, ""},
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=uulf" + modeChanRest, ""},
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=UUlf" + modeChanRest, ""},
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=UULF" + modeChanRest[1:], ""},
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=UULF" + modeChanRest + "x", ""},
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=UULF" + modeChanRest[1:] + "%26", ""},
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=UULF" + modeChanRest[1:] + "!", ""},
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=PLOU2XLYxmsIIM9h1Ybw2DuRw6o2fkNMeR", ""},
		{"https://www.youtube.com/feeds/videos.xml?user=SomeUser", ""},
		{"https://www.youtube.com/feeds/other.xml?playlist_id=UULF" + modeChanRest, ""},
		{"https://www.youtube.com/playlist?list=UULF" + modeChanRest, ""},
		{"https://youtube.com.evil.example/feeds/videos.xml?playlist_id=UULF" + modeChanRest, ""},
		{"ftp://www.youtube.com/feeds/videos.xml?playlist_id=UULF" + modeChanRest, ""},
		{"javascript:alert(1)", ""},
		{"", ""},
	} {
		id, mode, ok := FeedMode(tc.feedURL)
		switch {
		case tc.mode == "" && (ok || id != "" || mode != ""):
			t.Errorf("FeedMode(%q) = %q, %q, %v; want no mode", tc.feedURL, id, mode, ok)
		case tc.mode != "" && (!ok || id != modeChanID || mode != tc.mode):
			t.Errorf("FeedMode(%q) = %q, %q, %v; want %q, %q", tc.feedURL, id, mode, ok, modeChanID, tc.mode)
		}
	}
}

func TestModeFeedURL(t *testing.T) {
	for _, tc := range []struct {
		id   string
		mode Mode
		want string
	}{
		{modeChanID, ModeBoth, modeChanFeed},
		{modeChanID, ModeVideos, modeVideosFeed},
		{modeChanID, ModeShorts, modeShortsFeed},
		{modeChanID, "", ""},
		{modeChanID, "long", ""},
		{"UC_short", ModeVideos, ""},
		{"PL" + modeChanRest, ModeVideos, ""},
		{"UC" + modeChanRest[1:] + "&", ModeVideos, ""},
		{"", ModeBoth, ""},
	} {
		got := ModeFeedURL(tc.id, tc.mode)
		if got != tc.want {
			t.Errorf("ModeFeedURL(%q, %q) = %q; want %q", tc.id, tc.mode, got, tc.want)
		}
		if got == "" {
			continue
		}
		if id, mode, ok := FeedMode(got); !ok || id != tc.id || mode != tc.mode {
			t.Errorf("FeedMode(ModeFeedURL(%q, %q)) = %q, %q, %v", tc.id, tc.mode, id, mode, ok)
		}
		if KindForURL(got) != "youtube" {
			t.Errorf("KindForURL(%q) is not youtube", got)
		}
	}
}

// TestResolve_YouTube_ModeFeedURL_Canonical: every spelling of a channel's
// per-type upload feed resolves to its one canonical URL, as a channel feed
// does, so one list is not subscribed twice.
func TestResolve_YouTube_ModeFeedURL_Canonical(t *testing.T) {
	for raw, want := range map[string]string{
		"http://youtube.com/feeds/videos.xml?playlist_id=UULF" + modeChanRest:             modeVideosFeed,
		"https://m.youtube.com/feeds/videos.xml?playlist_id=UUSH" + modeChanRest + "&x=1": modeShortsFeed,
		"youtube.com/feeds/videos.xml?playlist_id=UULF" + modeChanRest:                    modeVideosFeed,
		"https://www.youtube.com/playlist?list=UUSH" + modeChanRest:                       modeShortsFeed,
		modeVideosFeed: modeVideosFeed,
	} {
		f := newFake()
		res := resolveOK(t, f, raw)
		if got := candidateURLs(res); !slices.Equal(got, []string{want}) {
			t.Errorf("Resolve(%s) = %v; want [%s]", raw, got, want)
		}
		if n := len(f.urls()); n != 0 {
			t.Errorf("Resolve(%s) made %d requests; want none", raw, n)
		}
	}
}

// TestChannelAvatar_ModeFeedsReadTheChannelPage: a channel followed as
// "Videos only" or "Shorts only" still gets its channel's avatar.
func TestChannelAvatar_ModeFeedsReadTheChannelPage(t *testing.T) {
	page := "https://www.youtube.com/channel/" + modeChanID
	for _, feedURL := range []string{modeVideosFeed, modeShortsFeed} {
		f := newFake()
		f.serve(page, "text/html; charset=utf-8", htmlPage(canonicalLink(modeChanID)+ogImage(ytAvatar900)))
		avatar, err := New(f).ChannelAvatar(context.Background(), feedURL)
		if err != nil || avatar != ytAvatarSmall {
			t.Errorf("ChannelAvatar(%s) = %q, %v; want %q", feedURL, avatar, err, ytAvatarSmall)
		}
		if got := f.urls(); !slices.Equal(got, []string{page}) {
			t.Errorf("ChannelAvatar(%s) fetched %v; want only %s", feedURL, got, page)
		}
		if !IsChannelOrModeFeed(feedURL) || IsChannelFeed(feedURL) {
			t.Errorf("%s: IsChannelOrModeFeed = %v, IsChannelFeed = %v", feedURL,
				IsChannelOrModeFeed(feedURL), IsChannelFeed(feedURL))
		}
	}
}
