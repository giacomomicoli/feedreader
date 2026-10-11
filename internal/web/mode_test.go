package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Shorts modes (#18): a YouTube channel is followed with all its uploads,
// only its regular videos or only its Shorts, chosen when adding it and in
// its settings.

// A synthetic channel and its feed in each mode.
const modeChanID = "UCtestchannel00000000000"

var (
	modeChanFeed   = resolve.ModeFeedURL(modeChanID, resolve.ModeBoth)
	modeVideosFeed = resolve.ModeFeedURL(modeChanID, resolve.ModeVideos)
	modeShortsFeed = resolve.ModeFeedURL(modeChanID, resolve.ModeShorts)
)

// modeSelect is the start of the mode <select> of the add form (sub-mode)
// or of the settings page (feed-mode).
func modeSelect(id string) string { return `<select id="` + id + `" name="mode"` }

// optionHTML is one option of a mode <select>.
func optionHTML(m resolve.Mode, selected bool) string {
	s := `<option value="` + string(m) + `"`
	if selected {
		s += ` selected`
	}
	return s + `>`
}

// addFeedAt subscribes a feed at u directly through the store.
func (e *testEnv) addFeedAt(kind store.Kind, u, title string) store.Feed {
	e.t.Helper()
	now := time.Now()
	f, err := e.st.CreateFeed(context.Background(), store.NewFeed{
		Kind: kind, URL: u, Title: title, ETag: `"v1"`,
		FetchedAt: now.Add(-time.Hour), NextFetchAt: now.Add(time.Hour),
	}, makeEntries(title, 2), config.InitialUnread)
	if err != nil {
		e.t.Fatalf("create feed %s: %v", u, err)
	}
	return f
}

func TestModeFeedsOfTheTestChannel(t *testing.T) {
	if modeChanFeed == "" || modeVideosFeed == "" || modeShortsFeed == "" {
		t.Fatalf("no mode feeds for %s", modeChanID)
	}
}

func TestAddConfirmOffersTheModesOfAYouTubeChannel(t *testing.T) {
	e := newTestEnv(t)
	e.stubPreview(modeChanFeed, "Chan", store.KindYouTube, 3)
	w := e.post("/add/preview", url.Values{"feed_url": {modeChanFeed}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), modeSelect("sub-mode"),
		optionHTML(resolve.ModeBoth, true)+"Videos and Shorts</option>",
		optionHTML(resolve.ModeVideos, false)+"Videos only, no Shorts</option>",
		optionHTML(resolve.ModeShorts, false)+"Shorts only</option>")

	// A mode feed entered directly is offered in its own mode.
	e.stubPreview(modeShortsFeed, "Chan", store.KindYouTube, 3)
	w = e.post("/add/preview", url.Values{"feed_url": {modeShortsFeed}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), modeSelect("sub-mode"),
		optionHTML(resolve.ModeBoth, false), optionHTML(resolve.ModeShorts, true))
}

// TestAddDirectModeFeedURLIsCanonical: a mode feed URL entered directly is
// previewed under its canonical spelling, as a channel feed URL is.
func TestAddDirectModeFeedURLIsCanonical(t *testing.T) {
	e := newTestEnv(t)
	e.stubPreview(modeVideosFeed, "Chan", store.KindYouTube, 2)
	w := e.post("/add/preview", url.Values{"feed_url": {"m.youtube.com/feeds/videos.xml?playlist_id=UULF" +
		strings.TrimPrefix(modeChanID, "UC") + "&x=1"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), `<input type="hidden" name="feed_url" value="`+modeVideosFeed+`">`)
	if len(e.sched.previewCalls) != 1 || e.sched.previewCalls[0] != modeVideosFeed {
		t.Errorf("preview calls = %v, want the canonical URL", e.sched.previewCalls)
	}
}

func TestAddConfirmHasNoModesForOtherFeeds(t *testing.T) {
	const playlist = "https://www.youtube.com/feeds/videos.xml?playlist_id=PLsyntheticplaylist"
	e := newTestEnv(t)
	e.stubPreview(blogFeedURL, "Blog", store.KindRSS, 3)
	e.stubPreview(playlist, "List", store.KindYouTube, 3)
	for _, u := range []string{blogFeedURL, playlist} {
		w := e.post("/add/preview", url.Values{"feed_url": {u}}, htmx)
		wantStatus(t, w, http.StatusOK)
		wantNotContains(t, w.Body.String(), `name="mode"`)
	}
}

// TestAddPendingChannelHasNoModes: only a channel's own feed is stored
// before it answers; its mode can be changed in the settings afterwards.
func TestAddPendingChannelHasNoModes(t *testing.T) {
	e := newTestEnv(t)
	e.stubPendingPreview(modeChanFeed, "Down Channel", "")
	w := e.post("/add/preview", url.Values{"feed_url": {modeChanFeed}}, htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantNotContains(t, body, `name="mode"`)
	wantContains(t, body, allowPending, "in its settings once subscribed")
}

func TestAddSubscribeInAMode(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
	}{
		{"", modeChanFeed},
		{"both", modeChanFeed},
		{"videos", modeVideosFeed},
		{"shorts", modeShortsFeed},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			e := newTestEnv(t)
			form := url.Values{"feed_url": {modeChanFeed}, "kind": {"youtube"}, "folder": {"0"}}
			if tc.mode != "" {
				form.Set("mode", tc.mode)
			}
			wantStatus(t, e.post("/add/subscribe", form, htmx), http.StatusOK)
			if len(e.sched.subs) != 1 || e.sched.subs[0].FeedURL != tc.want {
				t.Errorf("subscriptions = %+v, want %s", e.sched.subs, tc.want)
			}
		})
	}
}

func TestAddSubscribeRefusesAModeItCannotApply(t *testing.T) {
	for _, tc := range []struct{ feedURL, mode string }{
		{modeChanFeed, "long"},
		{modeChanFeed, "Videos"},
		{blogFeedURL, "videos"},
		{"https://www.youtube.com/feeds/videos.xml?playlist_id=PLsyntheticplaylist", "shorts"},
	} {
		e := newTestEnv(t)
		form := url.Values{"feed_url": {tc.feedURL}, "kind": {"youtube"}, "folder": {"0"}, "mode": {tc.mode}}
		w := e.post("/add/subscribe", form, htmx)
		wantStatus(t, w, http.StatusUnprocessableEntity)
		wantContains(t, w.Body.String(), "Pick one of the options")
		if len(e.sched.subs) != 0 {
			t.Errorf("%s in mode %q subscribed: %+v", tc.feedURL, tc.mode, e.sched.subs)
		}
	}
}

// TestAddSubscribeModeKeptOnAFormError: the confirm form shown again keeps
// the channel's feed URL and the mode picked.
func TestAddSubscribeModeKeptOnAFormError(t *testing.T) {
	e := newTestEnv(t)
	form := url.Values{"feed_url": {modeChanFeed}, "kind": {"youtube"}, "folder": {"new"}, "mode": {"shorts"}}
	w := e.post("/add/subscribe", form, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "Enter a name for the new folder.",
		`<input type="hidden" name="feed_url" value="`+modeChanFeed+`">`,
		modeSelect("sub-mode"), optionHTML(resolve.ModeShorts, true), optionHTML(resolve.ModeBoth, false))
}

// TestAddSubscribeModeFeedFailureSaysWhy: a mode feed that does not answer
// may be a channel without videos of that type.
func TestAddSubscribeModeFeedFailureSaysWhy(t *testing.T) {
	e := newTestEnv(t)
	e.sched.subscribeErr = fmt.Errorf("sched: subscribe: %w", &fetch.StatusError{URL: modeShortsFeed, StatusCode: http.StatusNotFound})
	form := url.Values{"feed_url": {modeChanFeed}, "kind": {"youtube"}, "folder": {"0"}, "mode": {"shorts"}}
	w := e.post("/add/subscribe", form, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), escapeText("YouTube's “Shorts only” list of this channel answered with HTTP 404"),
		optionHTML(resolve.ModeShorts, true))
}

func TestAddSubscribeModeAlreadySubscribedLinksToIt(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeedAt(store.KindYouTube, modeVideosFeed, "Chan videos")
	e.sched.subscribeErr = fmt.Errorf("sched: subscribe: %w", store.ErrConflict)
	form := url.Values{"feed_url": {modeChanFeed}, "kind": {"youtube"}, "folder": {"0"}, "mode": {"videos"}}
	w := e.post("/add/subscribe", form, htmx)
	wantStatus(t, w, http.StatusConflict)
	wantContains(t, w.Body.String(), "Chan videos", strings.ReplaceAll(feedHref(f.ID, ""), "&", "&amp;"))
}

func TestFeedSettingsShowTheModeOfAYouTubeChannel(t *testing.T) {
	e := newTestEnv(t)
	for u, want := range map[string]resolve.Mode{
		modeChanFeed: resolve.ModeBoth, modeVideosFeed: resolve.ModeVideos, modeShortsFeed: resolve.ModeShorts,
	} {
		f := e.addFeedAt(store.KindYouTube, u, "Chan "+string(want))
		w := e.get("/feeds/" + idStr(f.ID))
		wantStatus(t, w, http.StatusOK)
		body := w.Body.String()
		wantContains(t, body, `hx-post="/feeds/`+idStr(f.ID)+`/mode"`, modeSelect("feed-mode"))
		for _, m := range resolve.Modes() {
			wantContains(t, body, optionHTML(m, m == want))
		}
	}
	for _, f := range []store.Feed{
		e.addFeed(store.KindRSS, "blog", "Blog", 0, nil),
		e.addFeedAt(store.KindYouTube, "https://www.youtube.com/feeds/videos.xml?playlist_id=PLsyntheticplaylist", "List"),
	} {
		w := e.get("/feeds/" + idStr(f.ID))
		wantStatus(t, w, http.StatusOK)
		wantNotContains(t, w.Body.String(), `name="mode"`, "/mode\"")
	}
}

func TestFeedModeSwitch(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeedAt(store.KindYouTube, modeChanFeed, "Chan")
	base := "/feeds/" + idStr(f.ID)

	w := e.post(base+"/mode", url.Values{"mode": {"videos"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	if w.Header().Get("HX-Redirect") != base {
		t.Errorf("HX-Redirect = %q", w.Header().Get("HX-Redirect"))
	}
	got, err := e.st.GetFeed(context.Background(), f.ID)
	if err != nil || got.URL != modeVideosFeed || !got.InitialPending || len(e.entries(f.ID)) != 2 {
		t.Errorf("after switching: %+v, %v (%d entries)", got, err, len(e.entries(f.ID)))
	}
	if len(e.sched.modes) != 1 || e.sched.modes[0] != (modeCall{f.ID, resolve.ModeVideos}) {
		t.Errorf("SetMode calls = %+v", e.sched.modes)
	}
	// Without htmx: a redirect back to the settings.
	w = e.post(base+"/mode", url.Values{"mode": {"shorts"}})
	wantStatus(t, w, http.StatusSeeOther)
	if got, _ := e.st.GetFeed(context.Background(), f.ID); got.URL != modeShortsFeed {
		t.Errorf("URL = %s, want %s", got.URL, modeShortsFeed)
	}
	for _, bad := range []string{"", "long", "BOTH"} {
		w := e.post(base+"/mode", url.Values{"mode": {bad}}, htmx)
		wantStatus(t, w, http.StatusUnprocessableEntity)
		wantContains(t, w.Body.String(), "Pick one of the options")
	}
	if n := len(e.sched.modes); n != 2 {
		t.Errorf("%d SetMode calls; want none for invalid modes", n)
	}
}

func TestFeedModeSwitchOntoAnotherSubscription(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeedAt(store.KindYouTube, modeChanFeed, "Chan")
	other := e.addFeedAt(store.KindYouTube, modeShortsFeed, "Chan <Shorts>")
	w := e.post("/feeds/"+idStr(f.ID)+"/mode", url.Values{"mode": {"shorts"}}, htmx)
	wantStatus(t, w, http.StatusConflict)
	body := w.Body.String()
	wantContains(t, body, "You already follow this channel&#39;s “Shorts only” as “Chan &lt;Shorts&gt;”.")
	wantNotContains(t, body, "Chan <Shorts>")
	if got, _ := e.st.GetFeed(context.Background(), f.ID); got.URL != modeChanFeed {
		t.Errorf("URL changed to %s", got.URL)
	}
	if got, _ := e.st.GetFeed(context.Background(), other.ID); got.URL != modeShortsFeed {
		t.Errorf("other feed's URL changed to %s", got.URL)
	}
}

func TestFeedModeSwitchOfAFeedWithoutModes(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeed(store.KindRSS, "blog", "Blog", 0, nil)
	w := e.post("/feeds/"+idStr(f.ID)+"/mode", url.Values{"mode": {"videos"}}, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "only for YouTube channels")
	wantStatus(t, e.post("/feeds/999999/mode", url.Values{"mode": {"videos"}}, htmx), http.StatusNotFound)
}

func TestFeedModeSwitchFailureIsAServerError(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeedAt(store.KindYouTube, modeChanFeed, "Chan")
	e.sched.setModeErr = errors.New("disk on fire")
	w := e.post("/feeds/"+idStr(f.ID)+"/mode", url.Values{"mode": {"videos"}}, htmx)
	wantStatus(t, w, http.StatusInternalServerError)
	wantNotContains(t, w.Body.String(), "disk on fire")
}

func TestFeedModeSwitchIsCrossOriginProtected(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeedAt(store.KindYouTube, modeChanFeed, "Chan")
	w := e.post("/feeds/"+idStr(f.ID)+"/mode", url.Values{"mode": {"videos"}},
		withHeader("Sec-Fetch-Site", "cross-site"))
	wantStatus(t, w, http.StatusForbidden)
	if got, _ := e.st.GetFeed(context.Background(), f.ID); !strings.HasSuffix(got.URL, modeChanID) {
		t.Errorf("cross-site POST switched the feed to %s", got.URL)
	}
}

// failFeed records n failed fetches of feed id in a row.
func (e *testEnv) failFeed(id int64, n int) {
	e.t.Helper()
	now := time.Now()
	for range n {
		if err := e.st.RecordFetchError(context.Background(), id, "HTTP 404 from upstream", now, now.Add(time.Hour)); err != nil {
			e.t.Fatal(err)
		}
	}
}

// wantFailingNotWaiting checks that every page showing feed f flags it as
// failing, never as waiting for its first fetch.
func (e *testEnv) wantFailingNotWaiting(f store.Feed) {
	e.t.Helper()
	d, err := e.st.CreateDigest(context.Background(), "Digest "+idStr(f.ID))
	if err != nil {
		e.t.Fatal(err)
	}
	for _, page := range []string{"/", "/feeds/" + idStr(f.ID), digestHref(d.ID)} {
		body := e.get(page).Body.String()
		wantContains(e.t, body, `class="feed-warn"`)
		wantNotContains(e.t, body, `class="feed-wait"`, waitingStatus)
	}
	for _, filter := range []string{filterUnread, filterAll} {
		wantNotContains(e.t, e.get(feedHref(f.ID, filter)).Body.String(), waitingStatus, "when YouTube&#39;s feed answers")
	}
}

// TestSwitchedModeFeedFailingShowsTheWarning: a channel switched to a mode
// whose list keeps failing (a channel without Shorts) is a failing source,
// not one waiting for its first fetch.
func TestSwitchedModeFeedFailingShowsTheWarning(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeedAt(store.KindYouTube, modeChanFeed, "Chan")
	wantStatus(t, e.post("/feeds/"+idStr(f.ID)+"/mode", url.Values{"mode": {"shorts"}}, htmx), http.StatusOK)
	page := e.get("/feeds/" + idStr(f.ID)).Body.String()
	wantNotContains(t, page, waitingStatus, `class="feed-warn"`)
	e.failFeed(f.ID, config.WarnAfterFailures)
	e.wantFailingNotWaiting(f)
}

// TestSwitchedBackToTheChannelFeedIsNotWaiting: a source switched back to
// its channel's feed has been answering all along; refetching it is not
// "waiting for the first fetch".
func TestSwitchedBackToTheChannelFeedIsNotWaiting(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeedAt(store.KindYouTube, modeShortsFeed, "Chan")
	wantStatus(t, e.post("/feeds/"+idStr(f.ID)+"/mode", url.Values{"mode": {"both"}}, htmx), http.StatusOK)
	got, err := e.st.GetFeed(context.Background(), f.ID)
	if err != nil || got.URL != modeChanFeed || !got.InitialPending {
		t.Fatalf("after switching back: %+v, %v", got, err)
	}
	wantNotContains(t, e.get("/").Body.String(), `class="feed-wait"`)
	wantNotContains(t, e.get("/feeds/"+idStr(f.ID)).Body.String(), waitingStatus)
	e.failFeed(f.ID, config.WarnAfterFailures)
	e.wantFailingNotWaiting(f)
}

// TestPendingChannelSwitchedToAModeWarnsWhenFailing: a subscription made
// while the channel's feed was down, switched to "Shorts only", follows a
// list that may never exist: its failures are shown as such.
func TestPendingChannelSwitchedToAModeWarnsWhenFailing(t *testing.T) {
	e := newTestEnv(t)
	f := e.addPendingFeed("down", "Down Channel", 1)
	wantStatus(t, e.post("/feeds/"+idStr(f.ID)+"/mode", url.Values{"mode": {"shorts"}}, htmx), http.StatusOK)
	e.failFeed(f.ID, config.WarnAfterFailures)
	e.wantFailingNotWaiting(f)
}

// TestSwitchKeepingARateLimitWaitShowsItsFailures: a switch made while
// YouTube was rate-limiting the source keeps those failures, which are
// shown as usual (⚠ after config.WarnAfterFailures), never as waiting.
func TestSwitchKeepingARateLimitWaitShowsItsFailures(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeedAt(store.KindYouTube, modeChanFeed, "Chan")
	ctx := context.Background()
	next := time.Now().Add(6 * time.Hour)
	for range config.WarnAfterFailures {
		if err := e.st.RecordFetchError(ctx, f.ID, "HTTP 429 from upstream", time.Now(), next); err != nil {
			t.Fatal(err)
		}
	}
	if err := e.st.ChangeFeedURL(ctx, f.ID, modeChanFeed, modeShortsFeed, next, true); err != nil {
		t.Fatal(err)
	}
	e.wantFailingNotWaiting(f)
	wantContains(t, e.get("/feeds/"+idStr(f.ID)).Body.String(), "HTTP 429 from upstream")
}

// TestFeedTranscriptsPanel_SwitchedFeed: after a switch between videos and
// Shorts, the new list's first answer is an initial add and gets no
// transcripts; the panel says so in terms of the switch, not as if the
// source had never answered. Once that answer is stored, the note goes.
func TestFeedTranscriptsPanel_SwitchedFeed(t *testing.T) {
	const (
		waitingNote  = "first answer arrive as when you add a source: they get no transcript."
		switchedNote = "The videos of the new list" + "&#39;" + "s first answer arrive as when you add a source: they get no transcript. Videos already stored are not affected."
	)
	e := newTestEnv(t)
	e.transcriptsOn()
	f := e.addFeedAt(store.KindYouTube, modeChanFeed, "Chan")
	page := "/feeds/" + idStr(f.ID)
	wantNotContains(t, e.get(page).Body.String(), waitingNote, switchedNote)
	wantStatus(t, e.post(page+"/mode", url.Values{"mode": {"shorts"}}, htmx), http.StatusOK)
	body := e.get(page).Body.String()
	wantContains(t, body, switchedNote)
	wantNotContains(t, body, "its feed&#39;s "+waitingNote)
	if _, err := e.st.RecordFetchSuccess(context.Background(), store.FetchSuccess{FeedID: f.ID, FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	wantNotContains(t, e.get(page).Body.String(), waitingNote, switchedNote)
}
