package web

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
)

const (
	wantPendingNote = "YouTube's feed isn't answering for this channel right now. You can subscribe anyway, videos will show up once it does."
	allowPending    = `<input type="hidden" name="allow_pending" value="1">`
	waitingStatus   = "Waiting for the first fetch"
)

func (e *testEnv) stubPendingPreview(feedURL, title, icon string) {
	e.sched.previews[feedURL] = &sched.Preview{FeedURL: feedURL, Kind: store.KindYouTube, Title: title,
		IconURL: icon, Pending: true}
}

// addPendingFeed stores a pending subscription with failures failed
// fetches in a row.
func (e *testEnv) addPendingFeed(slug, title string, failures int) store.Feed {
	e.t.Helper()
	ctx := context.Background()
	now := time.Now()
	f, err := e.st.CreateFeed(ctx, store.NewFeed{
		Kind: store.KindYouTube, URL: "https://example.com/" + slug + "/feed.xml", Title: title,
		FetchedAt: now.Add(-time.Hour), NextFetchAt: now.Add(time.Hour), InitialPending: true,
		LastError: "HTTP 404 from upstream",
	}, nil, 0)
	if err != nil {
		e.t.Fatalf("create pending feed: %v", err)
	}
	for range failures - 1 {
		if err := e.st.RecordFetchError(ctx, f.ID, "HTTP 500 from upstream", now, f.NextFetchAt); err != nil {
			e.t.Fatal(err)
		}
	}
	return f
}

func TestAddPendingChannelOffersToSubscribeAnyway(t *testing.T) {
	const avatar = "https://yt3.googleusercontent.com/synthetic-avatar"
	e := newTestEnv(t)
	e.res.results["youtube.com/@down"] = &resolve.Result{Candidates: []resolve.Candidate{{URL: ytFeedURL}}}
	e.stubPendingPreview(ytFeedURL, "Down Channel", avatar)

	w := e.post("/add/resolve", url.Values{"url": {"youtube.com/@down"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body,
		`hx-post="/add/subscribe"`, allowPending, escapeText(wantPendingNote),
		`name="title" type="text" value="Down Channel"`,
		`<input type="hidden" name="icon_url" value="`+avatar+`">`,
		`<input type="hidden" name="feed_url" value="`+ytFeedURL+`">`,
	)
	wantNotContains(t, body, "entries found", "The feed has no entries yet.")
}

// TestAddDirectChannelFeedURLIsCanonical: a channel feed URL entered
// directly is previewed and offered under its canonical spelling, as the
// resolver does, so one channel cannot be subscribed under two URLs.
func TestAddDirectChannelFeedURLIsCanonical(t *testing.T) {
	const id = "UCtestchannel00000000000"
	canonical := resolve.ChannelFeedURL(id)
	e := newTestEnv(t)
	e.stubPreview(canonical, "Chan", store.KindYouTube, 2)
	w := e.post("/add/preview", url.Values{"feed_url": {"m.youtube.com/feeds/videos.xml?channel_id=" + id + "&x=1"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), `<input type="hidden" name="feed_url" value="`+canonical+`">`)
	if len(e.sched.previewCalls) != 1 || e.sched.previewCalls[0] != canonical {
		t.Errorf("preview calls = %v, want the canonical URL", e.sched.previewCalls)
	}
	// Already subscribed under the canonical URL: the other spelling finds it.
	if _, err := e.st.CreateFeed(context.Background(), store.NewFeed{Kind: store.KindYouTube, URL: canonical, Title: "Chan",
		FetchedAt: time.Now(), NextFetchAt: time.Now().Add(time.Hour)}, nil, 0); err != nil {
		t.Fatal(err)
	}
	w = e.post("/add/preview", url.Values{"feed_url": {"http://youtube.com/feeds/videos.xml?channel_id=" + id}}, htmx)
	wantStatus(t, w, http.StatusConflict)
}

// TestAddSubscribeChannelFeedURLIsCanonical: the confirm form's feed URL is
// canonicalized too, so no add path stores another spelling of a channel's
// feed.
func TestAddSubscribeChannelFeedURLIsCanonical(t *testing.T) {
	const id = "UCtestchannel00000000000"
	e := newTestEnv(t)
	form := url.Values{"feed_url": {"http://m.youtube.com/feeds/videos.xml?channel_id=" + id + "&x=1"}, "kind": {"youtube"}, "folder": {"0"}}
	wantStatus(t, e.post("/add/subscribe", form, htmx), http.StatusOK)
	if len(e.sched.subs) != 1 || e.sched.subs[0].FeedURL != resolve.ChannelFeedURL(id) {
		t.Errorf("subscriptions = %+v, want the canonical URL", e.sched.subs)
	}
}

func TestAddNormalPreviewDoesNotAllowPending(t *testing.T) {
	e := newTestEnv(t)
	e.stubPreview(blogFeedURL, "Blog", store.KindRSS, 3)
	w := e.post("/add/preview", url.Values{"feed_url": {blogFeedURL}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantNotContains(t, w.Body.String(), `name="allow_pending"`, escapeText(wantPendingNote))
}

func TestAddSubscribePassesTheConsentOnAndKeepsItOnAFormError(t *testing.T) {
	e := newTestEnv(t)
	e.stubPendingPreview(ytFeedURL, "Down Channel", "")
	form := url.Values{"feed_url": {ytFeedURL}, "kind": {"youtube"}, "allow_pending": {"1"}, "folder": {"new"}}
	w := e.post("/add/subscribe", form, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "Enter a name for the new folder.", allowPending, escapeText(wantPendingNote))

	form.Set("folder", "0")
	wantStatus(t, e.post("/add/subscribe", form, htmx), http.StatusOK)
	form.Del("allow_pending")
	form.Set("feed_url", blogFeedURL)
	wantStatus(t, e.post("/add/subscribe", form, htmx), http.StatusOK)
	if len(e.sched.subs) != 2 || !e.sched.subs[0].AllowPending || e.sched.subs[1].AllowPending {
		t.Errorf("subscriptions = %+v; want consent on the first only", e.sched.subs)
	}
	// Any other value is no consent.
	form.Set("allow_pending", "yes")
	form.Set("feed_url", "https://other.example/feed.xml")
	e.post("/add/subscribe", form, htmx)
	if n := len(e.sched.subs); n != 3 || e.sched.subs[2].AllowPending {
		t.Errorf("allow_pending=yes taken as consent: %+v", e.sched.subs)
	}
}

func TestPendingFeedSettingsSayItIsWaitingNotFailing(t *testing.T) {
	e := newTestEnv(t)
	f := e.addPendingFeed("down", "Down Channel", config.WarnAfterFailures+1)
	body := e.get("/feeds/" + idStr(f.ID)).Body.String()
	wantContains(t, body, waitingStatus, "HTTP 500 from upstream")
	wantNotContains(t, body, `class="feed-warn"`)
}

func TestPendingFeedShowsAWaitingMarkerInsteadOfTheWarning(t *testing.T) {
	e := newTestEnv(t)
	f := e.addPendingFeed("down", "Down Channel", config.WarnAfterFailures+1)
	body := e.get("/").Body.String()
	wantContains(t, body, `<span class="feed-wait" title="`+waitingStatus+`">`)
	wantNotContains(t, body, `class="feed-warn"`)

	// A digest's source list marks it the same way.
	d, err := e.st.CreateDigest(context.Background(), "Daily")
	if err != nil {
		t.Fatal(err)
	}
	body = e.get(digestHref(d.ID)).Body.String()
	wantContains(t, body, `<span class="feed-wait" title="`+waitingStatus+`">`)
	wantNotContains(t, body, `class="feed-warn"`)

	// Once its feed answered, a failing feed warns as usual.
	if _, err := e.st.RecordFetchSuccess(context.Background(), store.FetchSuccess{FeedID: f.ID, FetchedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for range config.WarnAfterFailures {
		if err := e.st.RecordFetchError(context.Background(), f.ID, "HTTP 404 from upstream", now, now); err != nil {
			t.Fatal(err)
		}
	}
	body = e.get("/").Body.String()
	wantContains(t, body, `class="feed-warn"`)
	wantNotContains(t, body, `class="feed-wait"`)
}

func TestPendingFeedEmptyGridSaysWhenTheNextTryIs(t *testing.T) {
	e := newTestEnv(t)
	f := e.addPendingFeed("down", "Down Channel", 1)
	next := f.NextFetchAt.In(e.srv.loc).Format(clockLayout)
	for _, filter := range []string{filterUnread, filterAll} {
		body := e.get(feedHref(f.ID, filter)).Body.String()
		wantContains(t, body, waitingStatus+".", escapeText("Videos will appear when YouTube's feed answers. Next try about "+next+"."))
		wantNotContains(t, body, "You&#39;re all caught up.")
	}
}
