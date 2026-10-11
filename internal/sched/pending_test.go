package sched

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Pending subscriptions (#20): a YouTube channel whose feed endpoint does
// not answer can be subscribed to anyway once YouTube's page of the channel
// confirms it.

const chanTitle = "Test Channel Name"

// namedChanPage is chanPage with an og:title.
func namedChanPage() *fetch.Result {
	return &fetch.Result{StatusCode: http.StatusOK, ContentType: "text/html; charset=utf-8", FinalURL: chanPageURL,
		Body: []byte(`<!DOCTYPE html><html><head><title>x</title></head><body>
<link rel="canonical" href="` + chanPageURL + `">
<meta property="og:title" content="` + chanTitle + `">
<meta property="og:image" content="` + chanAvatar + `">
</body></html>`)}
}

// downFetcher answers the channel feed with err and the channel page with
// page (404 when nil); other URLs are 404.
func downFetcher(err error, page *fetch.Result) *fakeFetcher {
	return newFetcher(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		switch {
		case r.URL == chanFeedURL && err != nil:
			return nil, err
		case r.URL == chanPageURL && page != nil:
			return page, nil
		}
		return nil, statusErr(r.URL, http.StatusNotFound)
	})
}

func callURLs(f *fakeFetcher) []string {
	var out []string
	for _, c := range f.Calls() {
		out = append(out, c.URL)
	}
	return out
}

func TestPreview_ChannelFeedDownIsPendingWhenThePageConfirmsIt(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			fetcher := downFetcher(statusErr(chanFeedURL, code), namedChanPage())
			s := newClockedScheduler(t, openStore(t), fetcher, newClock(t0))
			p, err := s.Preview(t.Context(), chanFeedURL)
			mustNoErr(t, err)
			want := Preview{FeedURL: chanFeedURL, Kind: store.KindYouTube, Title: chanTitle, IconURL: wantAvatar(t), Pending: true}
			if *p != want {
				t.Errorf("preview = %+v, want %+v", *p, want)
			}
			calls := fetcher.Calls()
			if len(calls) != 2 || calls[0].URL != chanFeedURL || calls[1].URL != chanPageURL || calls[1].Accept != fetch.HTMLAccept {
				t.Errorf("requests = %v, want the feed then the channel page", callURLs(fetcher))
			}
		})
	}
}

// TestPreview_PendingOffersTheCanonicalFeedURL: a channel feed pasted in
// another spelling is offered under the URL a pending subscription stores.
func TestPreview_PendingOffersTheCanonicalFeedURL(t *testing.T) {
	variant := "https://youtube.com/feeds/videos.xml?channel_id=" + chanID + "&utm_source=x"
	fetcher := newFetcher(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		if r.URL == chanPageURL {
			return namedChanPage(), nil
		}
		return nil, statusErr(r.URL, http.StatusNotFound)
	})
	s := newClockedScheduler(t, openStore(t), fetcher, newClock(t0))
	p, err := s.Preview(t.Context(), variant)
	mustNoErr(t, err)
	if !p.Pending || p.FeedURL != chanFeedURL {
		t.Errorf("preview = %+v, want pending at %s", p, chanFeedURL)
	}
}

func TestPreview_NotPendingUnlessAConfirmedChannelFeedIsDown(t *testing.T) {
	network := errors.New("dial tcp: connection refused")
	htmlPage := &fetch.Result{StatusCode: http.StatusOK, ContentType: "text/html", Body: []byte("<html><body>consent</body></html>"), FinalURL: chanFeedURL}
	otherPage := namedChanPage()
	otherPage.Body = []byte(strings.ReplaceAll(string(otherPage.Body), chanID, "UCanotherchannel00000000"))
	tests := []struct {
		name      string
		url       string
		fetcher   *fakeFetcher
		wantCalls int
	}{
		{"rate limited", chanFeedURL, downFetcher(statusErr(chanFeedURL, http.StatusTooManyRequests), namedChanPage()), 1},
		{"forbidden", chanFeedURL, downFetcher(statusErr(chanFeedURL, http.StatusForbidden), namedChanPage()), 1},
		{"gone", chanFeedURL, downFetcher(statusErr(chanFeedURL, http.StatusGone), namedChanPage()), 1},
		{"network failure", chanFeedURL, downFetcher(network, namedChanPage()), 1},
		{"not a feed", chanFeedURL, routeFetcher(map[string]*fetch.Result{chanFeedURL: htmlPage, chanPageURL: namedChanPage()}), 1},
		{"playlist feed", playlistFeed, downFetcher(nil, namedChanPage()), 1},
		{"other site", feedURL, downFetcher(nil, namedChanPage()), 1},
		{"page names another channel", chanFeedURL, downFetcher(statusErr(chanFeedURL, http.StatusNotFound), otherPage), 2},
		{"page down too", chanFeedURL, downFetcher(statusErr(chanFeedURL, http.StatusNotFound), nil), 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newClockedScheduler(t, openStore(t), tt.fetcher, newClock(t0))
			p, err := s.Preview(t.Context(), tt.url)
			if err == nil {
				t.Fatalf("preview = %+v, want the fetch error", p)
			}
			if n := tt.fetcher.CallCount(); n != tt.wantCalls {
				t.Errorf("requests = %v, want %d", callURLs(tt.fetcher), tt.wantCalls)
			}
		})
	}
	t.Run("the error is the feed's", func(t *testing.T) {
		s := newClockedScheduler(t, openStore(t), downFetcher(statusErr(chanFeedURL, http.StatusNotFound), nil), newClock(t0))
		_, err := s.Preview(t.Context(), chanFeedURL)
		var se *fetch.StatusError
		if !errors.As(err, &se) || se.URL != chanFeedURL {
			t.Errorf("err = %v, want the feed's HTTP 404", err)
		}
	})
}

// TestPreview_HandlePageResolvedMomentsAgoConfirmsTheChannel: during an
// outage, adding an @handle fetches the channel's page once: the page the
// resolver read confirms the channel for the pending preview.
func TestPreview_HandlePageResolvedMomentsAgoConfirmsTheChannel(t *testing.T) {
	const handle = "https://www.youtube.com/@testchannel"
	fetcher := newFetcher(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		switch r.URL {
		case handle, chanPageURL:
			page := namedChanPage()
			page.FinalURL = r.URL
			return page, nil
		}
		return nil, statusErr(r.URL, http.StatusNotFound)
	})
	st := openStore(t)
	clock := newClock(t0)
	s := newClockedScheduler(t, st, fetcher, clock)
	res, err := resolve.New(s.ResolveFetcher()).Resolve(t.Context(), handle)
	mustNoErr(t, err)
	p, err := s.Preview(t.Context(), res.Candidates[0].URL)
	mustNoErr(t, err)
	if !p.Pending || p.Title != chanTitle || p.IconURL != wantAvatar(t) {
		t.Errorf("preview = %+v, want pending with the name and avatar from the handle page", p)
	}
	if got := callURLs(fetcher); len(got) != 2 || got[0] != handle || got[1] != chanFeedURL {
		t.Errorf("requests = %v, want the handle page then the feed only", got)
	}
	// The confirmation is as short-lived as a cached preview.
	clock.Advance(previewCacheTTL)
	if _, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanFeedURL, AllowPending: true}); err != nil {
		t.Fatal(err)
	}
	if got := callURLs(fetcher); len(got) != 4 || got[3] != chanPageURL {
		t.Errorf("requests = %v, want the channel page fetched again once the confirmation expired", got)
	}
}

// TestResolveFetcher_OnlyChannelPagesConfirmChannels: a YouTube page that
// is not a channel's own (a video page naming its channel's feed) or a page
// served elsewhere confirms nothing.
func TestResolveFetcher_OnlyChannelPagesConfirmChannels(t *testing.T) {
	for _, tc := range []struct{ name, url, finalURL string }{
		{"video page", "https://www.youtube.com/watch?v=abcdefghijk", ""},
		{"handle page served elsewhere", "https://www.youtube.com/@testchannel", "https://elsewhere.example/@testchannel"},
		{"handle redirected to a watch page", "https://www.youtube.com/@testchannel", "https://www.youtube.com/watch?v=abcdefghijk"},
		{"another channel's page", "https://www.youtube.com/channel/UCanotherchannel00000000", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := namedChanPage()
			page.FinalURL = cmp.Or(tc.finalURL, tc.url)
			s := newClockedScheduler(t, openStore(t), routeFetcher(map[string]*fetch.Result{tc.url: page}), newClock(t0))
			if _, err := s.ResolveFetcher().Fetch(t.Context(), fetch.Request{URL: tc.url, Accept: fetch.HTMLAccept}); err != nil {
				t.Fatal(err)
			}
			if ch, ok := s.channels.get(chanID, t0); ok {
				t.Errorf("channel confirmed by %s: %+v", tc.url, ch)
			}
		})
	}
}

func TestSubscribe_PendingAfterPreviewStoresTheChannelWithoutEntries(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	clock := newClock(t0)
	fetcher := downFetcher(statusErr(chanFeedURL, http.StatusNotFound), namedChanPage())
	s := newClockedScheduler(t, st, fetcher, clock)
	p, err := s.Preview(ctx, chanFeedURL)
	mustNoErr(t, err)

	clock.Advance(time.Minute)
	feed, err := s.Subscribe(ctx, Subscription{FeedURL: p.FeedURL, IconURL: p.IconURL, AllowPending: true})
	mustNoErr(t, err)
	got := getFeed(t, st, feed.ID)
	now := clock.Now()
	switch {
	case !got.InitialPending, got.Kind != store.KindYouTube, got.URL != chanFeedURL:
		t.Errorf("feed = %+v, want a pending YouTube subscription", got)
	case got.ErrorCount != 1, !strings.Contains(got.LastError, "404"):
		t.Errorf("failure = %d %q, want 1 and the 404", got.ErrorCount, got.LastError)
	case got.Title != chanTitle, got.IconURL != wantAvatar(t):
		t.Errorf("title %q icon %q, want the channel page's", got.Title, got.IconURL)
	case got.ETag != "", got.LastModified != "":
		t.Errorf("validators %q %q stored without a document", got.ETag, got.LastModified)
	case !got.LastFetchedAt.Equal(now), !got.NextFetchAt.Equal(now.Add(config.FirstRetryDelay)):
		t.Errorf("last fetch %s next %s, want now and a first retry after %s", got.LastFetchedAt, got.NextFetchAt, config.FirstRetryDelay)
	}
	if n := len(feedEntries(t, st, feed.ID)); n != 0 {
		t.Errorf("entries = %d, want none", n)
	}
	// Preview: feed, page. Subscribe: the feed again, never the page (the
	// confirmation is cached) nor the avatar lookup.
	if got := callURLs(fetcher); len(got) != 3 || got[2] != chanFeedURL {
		t.Errorf("requests = %v, want feed, page, feed", got)
	}
	if len(s.wake) != 1 {
		t.Error("Subscribe did not wake the dispatcher")
	}
}

func TestSubscribe_PendingTitleIsTheUsers(t *testing.T) {
	st := openStore(t)
	s := newClockedScheduler(t, st, downFetcher(statusErr(chanFeedURL, http.StatusNotFound), namedChanPage()), newClock(t0))
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanFeedURL, Title: " Mine ", AllowPending: true})
	mustNoErr(t, err)
	if feed.Title != "Mine" {
		t.Errorf("title = %q, want the user's", feed.Title)
	}
}

// TestSubscribe_PendingWithoutAPreviewConfirmsTheChannelItself: the
// confirmation is never taken from the form.
func TestSubscribe_PendingWithoutAPreviewConfirmsTheChannelItself(t *testing.T) {
	st := openStore(t)
	fetcher := downFetcher(statusErr(chanFeedURL, http.StatusBadGateway), namedChanPage())
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanFeedURL, AllowPending: true})
	mustNoErr(t, err)
	if !feed.InitialPending {
		t.Errorf("feed = %+v, want pending", feed)
	}
	if got := callURLs(fetcher); len(got) != 2 || got[0] != chanFeedURL || got[1] != chanPageURL {
		t.Errorf("requests = %v, want the feed then the channel page", got)
	}
}

func TestSubscribe_FeedBackUpSubscribesNormally(t *testing.T) {
	st := openStore(t)
	fetcher := downFetcher(statusErr(chanFeedURL, http.StatusNotFound), namedChanPage())
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	p, err := s.Preview(t.Context(), chanFeedURL)
	mustNoErr(t, err)
	items := itemsNewestFirst("v", 2*config.InitialUnread, t0)
	fetcher.setRespond(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		return ok(r.URL, atomDoc("Channel feed", items...)), nil
	})
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: p.FeedURL, AllowPending: true})
	mustNoErr(t, err)
	if feed.InitialPending || feed.ErrorCount != 0 {
		t.Errorf("feed = %+v, want a normal subscription", feed)
	}
	if n := len(unreadGUIDs(feedEntries(t, st, feed.ID))); n != config.InitialUnread {
		t.Errorf("unread = %d, want %d", n, config.InitialUnread)
	}
}

// TestSubscribe_AllowPendingIsOnlyConsent: whatever the form says, a
// pending subscription needs a channel feed whose endpoint answered 404 or
// 5xx and whose page confirmed the channel.
func TestSubscribe_AllowPendingIsOnlyConsent(t *testing.T) {
	variant := "https://youtube.com/feeds/videos.xml?channel_id=" + chanID
	otherPage := namedChanPage()
	otherPage.Body = []byte(strings.ReplaceAll(string(otherPage.Body), chanID, "UCanotherchannel00000000"))
	tests := []struct {
		name    string
		url     string
		fetcher *fakeFetcher
	}{
		{"other site", feedURL, downFetcher(nil, namedChanPage())},
		{"playlist feed", playlistFeed, downFetcher(nil, namedChanPage())},
		{"another spelling of the channel feed", variant, downFetcher(nil, namedChanPage())},
		{"rate limited", chanFeedURL, downFetcher(statusErr(chanFeedURL, http.StatusTooManyRequests), namedChanPage())},
		{"network failure", chanFeedURL, downFetcher(errors.New("connection reset"), namedChanPage())},
		{"page names another channel", chanFeedURL, downFetcher(statusErr(chanFeedURL, http.StatusNotFound), otherPage)},
		{"page down", chanFeedURL, downFetcher(statusErr(chanFeedURL, http.StatusNotFound), nil)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			s := newClockedScheduler(t, st, tt.fetcher, newClock(t0))
			if feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: tt.url, AllowPending: true}); err == nil {
				t.Fatalf("subscribed: %+v", feed)
			}
			if feeds, err := st.ListFeeds(t.Context()); err != nil || len(feeds) != 0 {
				t.Errorf("feeds = %+v, %v; want none", feeds, err)
			}
		})
	}
	t.Run("without consent", func(t *testing.T) {
		fetcher := downFetcher(statusErr(chanFeedURL, http.StatusNotFound), namedChanPage())
		s := newClockedScheduler(t, openStore(t), fetcher, newClock(t0))
		if _, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanFeedURL}); err == nil {
			t.Fatal("subscribed without consent")
		}
		if got := callURLs(fetcher); len(got) != 1 {
			t.Errorf("requests = %v, want the feed only", got)
		}
	})
}

func TestSubscribe_PendingTwiceIsConflict(t *testing.T) {
	st := openStore(t)
	fetcher := downFetcher(statusErr(chanFeedURL, http.StatusNotFound), namedChanPage())
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	sub := Subscription{FeedURL: chanFeedURL, AllowPending: true}
	_, err := s.Subscribe(t.Context(), sub)
	mustNoErr(t, err)
	before := fetcher.CallCount()
	if _, err := s.Subscribe(t.Context(), sub); !errors.Is(err, store.ErrConflict) {
		t.Errorf("second subscribe err = %v, want ErrConflict", err)
	}
	if fetcher.CallCount() != before {
		t.Error("the second subscribe made requests")
	}
}

func TestSubscribe_ConcurrentPendingSubscribesStoreOneFeed(t *testing.T) {
	st := openStore(t)
	s := newClockedScheduler(t, st, downFetcher(statusErr(chanFeedURL, http.StatusNotFound), namedChanPage()), newClock(t0))
	const tries = 4
	var wg sync.WaitGroup
	errs := make([]error, tries)
	for i := range tries {
		wg.Go(func() {
			_, errs[i] = s.Subscribe(context.Background(), Subscription{FeedURL: chanFeedURL, AllowPending: true})
		})
	}
	wg.Wait()
	okCount := 0
	for _, err := range errs {
		switch {
		case err == nil:
			okCount++
		case !errors.Is(err, store.ErrConflict):
			t.Errorf("err = %v, want ErrConflict", err)
		}
	}
	if okCount != 1 {
		t.Errorf("successful subscribes = %d, want 1", okCount)
	}
}

// pendingSubscription stores a pending subscription of the channel at t0
// through the scheduler and returns it with a fetcher to drive its polls.
func pendingSubscription(t *testing.T, st store.Store, clock *testClock) (*Scheduler, *fakeFetcher, store.Feed) {
	t.Helper()
	fetcher := downFetcher(statusErr(chanFeedURL, http.StatusNotFound), namedChanPage())
	s := newClockedScheduler(t, st, fetcher, clock)
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanFeedURL, AllowPending: true})
	mustNoErr(t, err)
	return s, fetcher, feed
}

func TestPoll_FirstSuccessOfAPendingFeedIsAnInitialAdd(t *testing.T) {
	const total = 3 * config.InitialUnread
	st := openStore(t)
	clock := newClock(t0)
	s, fetcher, feed := pendingSubscription(t, st, clock)
	items := itemsNewestFirst("v", total, t0)
	fetcher.setRespond(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		return ok(r.URL, atomDoc(chanTitle, items...)), nil
	})
	clock.Set(getFeed(t, st, feed.ID).NextFetchAt)
	pollOnce(t, s, st, feed.ID)

	entries := feedEntries(t, st, feed.ID)
	if len(entries) != total {
		t.Fatalf("entries = %d, want %d", len(entries), total)
	}
	unread := unreadGUIDs(entries)
	if len(unread) != config.InitialUnread {
		t.Errorf("unread = %v, want the %d newest", unread, config.InitialUnread)
	}
	for _, it := range items[:config.InitialUnread] {
		if !unread[it.id] {
			t.Errorf("%s should be unread", it.id)
		}
	}
	got := getFeed(t, st, feed.ID)
	if got.InitialPending || got.ErrorCount != 0 || got.LastError != "" || got.ETag == "" {
		t.Errorf("after the first success: %+v", got)
	}

	// Later polls store new videos unread as usual.
	more := append(itemsNewestFirst("w", 2, t0.Add(time.Hour)), items...)
	fetcher.setRespond(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		return ok(r.URL, atomDoc(chanTitle, more...)), nil
	})
	clock.Set(getFeed(t, st, feed.ID).NextFetchAt)
	pollOnce(t, s, st, feed.ID)
	if n := len(unreadGUIDs(feedEntries(t, st, feed.ID))); n != config.InitialUnread+2 {
		t.Errorf("unread after the next poll = %d, want %d", n, config.InitialUnread+2)
	}
}

// TestPoll_PendingFeedWaitsOnFailures: a pending subscription is retried
// like any YouTube feed in an outage, stays pending, and a manual refresh
// does not count.
func TestPoll_PendingFeedWaitsOnFailures(t *testing.T) {
	st := openStore(t)
	clock := newClock(t0)
	s, _, feed := pendingSubscription(t, st, clock)
	for n := 2; n <= config.WarnAfterFailures+1; n++ {
		clock.Set(getFeed(t, st, feed.ID).NextFetchAt)
		pollOnce(t, s, st, feed.ID)
		got := getFeed(t, st, feed.ID)
		if !got.InitialPending || got.ErrorCount != n {
			t.Fatalf("after failure %d: %+v", n, got)
		}
		if d := got.NextFetchAt.Sub(clock.Now()); d != config.DefaultYouTubePollInterval {
			t.Errorf("after failure %d: next fetch in %s, want %s", n, d, config.DefaultYouTubePollInterval)
		}
	}
	before := getFeed(t, st, feed.ID)
	clock.Advance(time.Minute)
	manualPoll(t, s, st, feed.ID, before.NextFetchAt)
	if got := getFeed(t, st, feed.ID); !got.InitialPending || got.ErrorCount != before.ErrorCount {
		t.Errorf("after a manual failure: %+v", got)
	}
}
