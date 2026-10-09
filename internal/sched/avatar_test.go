package sched

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// A synthetic YouTube channel: its feed (testdata/youtube.xml, which has no
// icon, like YouTube's) and its page, which advertises its avatar.
const (
	chanID       = "UCtestchannel00000000000"
	chanFeedURL  = "https://www.youtube.com/feeds/videos.xml?channel_id=" + chanID
	chanPageURL  = "https://www.youtube.com/channel/" + chanID
	chanAvatar   = "https://yt3.googleusercontent.com/synthetic-avatar=s900-c-k-c0x00ffffff-no-rj"
	playlistFeed = "https://www.youtube.com/feeds/videos.xml?playlist_id=PLtestplaylist"
)

func chanPage() *fetch.Result {
	return &fetch.Result{StatusCode: 200, ContentType: "text/html; charset=utf-8", FinalURL: chanPageURL,
		Body: []byte(`<!DOCTYPE html><html><head><title>Test Channel</title></head><body>
<link rel="canonical" href="` + chanPageURL + `">
<meta property="og:image" content="` + chanAvatar + `">
</body></html>`)}
}

func chanFeed(t *testing.T, u string) *fetch.Result {
	t.Helper()
	body, err := os.ReadFile("testdata/youtube.xml")
	mustNoErr(t, err)
	return ok(u, body)
}

// wantAvatar is the avatar the resolver reads from chanPage.
func wantAvatar(t *testing.T) string {
	t.Helper()
	a, err := resolve.New(routeFetcher(map[string]*fetch.Result{chanPageURL: chanPage()})).ChannelAvatar(t.Context(), chanFeedURL)
	if err != nil || a == "" {
		t.Fatalf("ChannelAvatar = %q, %v", a, err)
	}
	return a
}

// addYouTubeFeed subscribes a YouTube feed without icon directly in the
// store, due at t0.
func addYouTubeFeed(t *testing.T, st store.Store, u, icon string) store.Feed {
	t.Helper()
	f, err := st.CreateFeed(t.Context(), store.NewFeed{
		Kind: store.KindYouTube, URL: u, Title: "Test Channel", IconURL: icon,
		FetchedAt: t0.Add(-time.Hour), NextFetchAt: t0,
	}, nil, config.InitialUnread)
	mustNoErr(t, err)
	return f
}

func pageRequests(f *fakeFetcher) int {
	n := 0
	for _, c := range f.Calls() {
		if c.URL == chanPageURL {
			n++
		}
	}
	return n
}

func TestSubscribe_FetchesTheChannelAvatarWhenTheAddFlowHasNone(t *testing.T) {
	st := openStore(t)
	clock := newClock(t0)
	fetcher := routeFetcher(map[string]*fetch.Result{chanFeedURL: chanFeed(t, chanFeedURL), chanPageURL: chanPage()})
	s := newClockedScheduler(t, st, fetcher, clock)

	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanFeedURL})
	mustNoErr(t, err)
	if want := wantAvatar(t); feed.IconURL != want {
		t.Errorf("stored icon = %q; want %q", feed.IconURL, want)
	}
	calls := fetcher.Calls()
	if len(calls) != 2 || calls[0].URL != chanFeedURL || calls[1].URL != chanPageURL || calls[1].Accept != fetch.HTMLAccept {
		t.Fatalf("requests = %+v; want the feed, then its channel page", calls)
	}

	clock.Advance(config.DefaultPollInterval)
	pollOnce(t, s, st, feed.ID)
	if got := getFeed(t, st, feed.ID); got.IconURL != feed.IconURL || pageRequests(fetcher) != 1 {
		t.Errorf("after a poll: icon %q after %d page requests; want it kept, no new request", got.IconURL, pageRequests(fetcher))
	}
}

func TestSubscribe_NoChannelPageForOtherFeeds(t *testing.T) {
	for _, tc := range []struct{ name, url, icon string }{
		{"playlist feed", playlistFeed, ""},
		{"avatar found by the add flow", chanFeedURL, "https://yt3.googleusercontent.com/found-by-resolver"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			fetcher := routeFetcher(map[string]*fetch.Result{tc.url: chanFeed(t, tc.url), chanPageURL: chanPage()})
			s := newClockedScheduler(t, st, fetcher, newClock(t0))
			feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: tc.url, IconURL: tc.icon})
			mustNoErr(t, err)
			if feed.IconURL != tc.icon || fetcher.CallCount() != 1 {
				t.Errorf("icon %q after %d requests; want %q after the feed's only", feed.IconURL, fetcher.CallCount(), tc.icon)
			}
		})
	}
}

func TestChannelAvatar_BestEffortAndOncePerRunOnPolls(t *testing.T) {
	st := openStore(t)
	clock := newClock(t0)
	fetcher := routeFetcher(map[string]*fetch.Result{chanFeedURL: chanFeed(t, chanFeedURL)}) // page: 404
	s := newClockedScheduler(t, st, fetcher, clock)

	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanFeedURL})
	mustNoErr(t, err)
	if feed.IconURL != "" || pageRequests(fetcher) != 1 {
		t.Fatalf("icon %q after %d page requests", feed.IconURL, pageRequests(fetcher))
	}
	// The first check after subscribing tries again; later ones in this run
	// do not.
	for range 3 {
		clock.Advance(config.DefaultPollInterval)
		pollOnce(t, s, st, feed.ID)
	}
	if got := getFeed(t, st, feed.ID); got.ErrorCount != 0 || got.IconURL != "" || pageRequests(fetcher) != 2 {
		t.Errorf("after polls: errors %d icon %q, %d page requests; want no error and one try by the polls",
			got.ErrorCount, got.IconURL, pageRequests(fetcher))
	}

	// After a restart the channel page is tried again, and now has one.
	fetcher = routeFetcher(map[string]*fetch.Result{chanFeedURL: chanFeed(t, chanFeedURL), chanPageURL: chanPage()})
	s = newClockedScheduler(t, st, fetcher, clock)
	clock.Advance(config.DefaultPollInterval)
	pollOnce(t, s, st, feed.ID)
	if got := getFeed(t, st, feed.ID); got.IconURL != wantAvatar(t) {
		t.Errorf("icon after a restart = %q; want the avatar", got.IconURL)
	}
}

func TestSubscribe_ResubscribedChannelGetsItsAvatarAgain(t *testing.T) {
	st := openStore(t)
	fetcher := routeFetcher(map[string]*fetch.Result{chanFeedURL: chanFeed(t, chanFeedURL), chanPageURL: chanPage()})
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	for i := range 2 {
		feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanFeedURL})
		mustNoErr(t, err)
		if feed.IconURL != wantAvatar(t) {
			t.Fatalf("subscription %d: icon %q; want the avatar", i+1, feed.IconURL)
		}
		mustNoErr(t, st.DeleteFeed(t.Context(), feed.ID))
	}
	if n := pageRequests(fetcher); n != 2 {
		t.Errorf("%d page requests; want one per subscription", n)
	}
}

// TestSubscribe_AvatarLookupNeverFailsTheSubscription: the lookup runs once
// the feed is stored, so a request that ends during it (the deadline, or
// the user leaving) still leaves the subscription, and the first check
// fetches the avatar.
func TestSubscribe_AvatarLookupNeverFailsTheSubscription(t *testing.T) {
	st := openStore(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		if r.URL == chanPageURL {
			cancel()
			return nil, ctx.Err()
		}
		return chanFeed(t, chanFeedURL), nil
	})
	clock := newClock(t0)
	s := newClockedScheduler(t, st, fetcher, clock)
	feed, err := s.Subscribe(ctx, Subscription{FeedURL: chanFeedURL})
	if err != nil || feed.IconURL != "" {
		t.Fatalf("Subscribe = icon %q, %v; want the feed stored without an icon", feed.IconURL, err)
	}
	if _, err := st.GetFeed(t.Context(), feed.ID); err != nil {
		t.Fatalf("feed not stored: %v", err)
	}

	fetcher.setRespond(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		if r.URL == chanPageURL {
			return chanPage(), nil
		}
		return chanFeed(t, chanFeedURL), nil
	})
	clock.Advance(config.DefaultPollInterval)
	pollOnce(t, s, st, feed.ID)
	if got := getFeed(t, st, feed.ID); got.IconURL != wantAvatar(t) {
		t.Errorf("icon after the first check = %q; want the avatar", got.IconURL)
	}
}

func TestPoll_BackfillsTheAvatarOfAChannelFeedWithoutIcon(t *testing.T) {
	for _, tc := range []struct {
		name string
		res  *fetch.Result
	}{
		{"200", nil},
		{"304", notModified(chanFeedURL)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			f := addYouTubeFeed(t, st, chanFeedURL, "")
			res := tc.res
			if res == nil {
				res = chanFeed(t, chanFeedURL)
			}
			fetcher := routeFetcher(map[string]*fetch.Result{chanFeedURL: res, chanPageURL: chanPage()})
			s := newClockedScheduler(t, st, fetcher, newClock(t0))

			pollOnce(t, s, st, f.ID)
			got := getFeed(t, st, f.ID)
			if got.IconURL != wantAvatar(t) || !got.LastFetchedAt.Equal(t0) {
				t.Errorf("icon %q, last fetch %s; want the avatar and the poll recorded", got.IconURL, got.LastFetchedAt)
			}
			calls := fetcher.Calls()
			if want := []string{chanFeedURL, chanPageURL}; len(calls) != 2 || calls[0].URL != want[0] || calls[1].URL != want[1] {
				t.Errorf("requests = %+v; want %v", calls, want)
			}
		})
	}
}

func TestPoll_NoAvatarLookup(t *testing.T) {
	const icon = "https://images.example/icon.png"
	for _, tc := range []struct {
		name string
		add  func(t *testing.T, st store.Store) store.Feed
		res  *fetch.Result
		err  error
	}{
		{"rss feed", func(t *testing.T, st store.Store) store.Feed { return addFeed(t, st, feedURL, t0) },
			ok(feedURL, atomDoc("A", itemsNewestFirst("a", 1, t0)...)), nil},
		{"playlist feed", func(t *testing.T, st store.Store) store.Feed { return addYouTubeFeed(t, st, playlistFeed, "") },
			chanFeed(t, playlistFeed), nil},
		{"channel feed with an icon", func(t *testing.T, st store.Store) store.Feed { return addYouTubeFeed(t, st, chanFeedURL, icon) },
			chanFeed(t, chanFeedURL), nil},
		{"failed poll", func(t *testing.T, st store.Store) store.Feed { return addYouTubeFeed(t, st, chanFeedURL, "") },
			nil, &fetch.StatusError{URL: chanFeedURL, StatusCode: 503}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			f := tc.add(t, st)
			fetcher := newFetcher(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
				if r.URL == chanPageURL {
					return chanPage(), nil
				}
				return tc.res, tc.err
			})
			s := newClockedScheduler(t, st, fetcher, newClock(t0))
			pollOnce(t, s, st, f.ID)
			if n := fetcher.CallCount(); n != 1 {
				t.Errorf("%d requests; want only the feed's", n)
			}
			if got := getFeed(t, st, f.ID); got.IconURL == wantAvatar(t) {
				t.Errorf("icon = %q; want no channel avatar", got.IconURL)
			}
		})
	}

	t.Run("failed poll does not use up the try", func(t *testing.T) {
		st := openStore(t)
		f := addYouTubeFeed(t, st, chanFeedURL, "")
		fail := true
		fetcher := newFetcher(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
			switch {
			case r.URL == chanPageURL:
				return chanPage(), nil
			case fail:
				return nil, &fetch.StatusError{URL: r.URL, StatusCode: 503}
			}
			return chanFeed(t, chanFeedURL), nil
		})
		s := newClockedScheduler(t, st, fetcher, newClock(t0))
		pollOnce(t, s, st, f.ID)
		fail = false
		pollOnce(t, s, st, f.ID)
		if got := getFeed(t, st, f.ID); got.IconURL != wantAvatar(t) {
			t.Errorf("icon = %q; want the avatar after the first successful poll", got.IconURL)
		}
	})
}

// TestPoll_AvatarIsNotWrittenToAReplacementFeed: the feed is unsubscribed,
// and its id given to a new subscription, while its channel page is being
// fetched.
func TestPoll_AvatarIsNotWrittenToAReplacementFeed(t *testing.T) {
	st := openStore(t)
	a := addYouTubeFeed(t, st, chanFeedURL, "")
	const otherURL = "https://b.example/feed"
	var (
		s *Scheduler
		b store.Feed
	)
	fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		switch r.URL {
		case otherURL:
			return ok(otherURL, atomDoc("B", itemsNewestFirst("b", 1, t0)...)), nil
		case chanPageURL:
			mustNoErr(t, st.DeleteFeed(ctx, a.ID))
			var err error
			b, err = s.Subscribe(ctx, Subscription{FeedURL: otherURL})
			mustNoErr(t, err)
			if b.ID != a.ID {
				t.Fatalf("new subscription got id %d, not the freed %d", b.ID, a.ID)
			}
			return chanPage(), nil
		}
		return chanFeed(t, chanFeedURL), nil
	})
	s = newClockedScheduler(t, st, fetcher, newClock(t0))
	pollOnce(t, s, st, a.ID)
	if got := getFeed(t, st, b.ID); got != b {
		t.Errorf("the avatar of feed %d was written to the new subscription:\n got %+v\nwant %+v", a.ID, got, b)
	}
}

func TestPoll_ShutdownDuringAvatarLookupRecordsNothing(t *testing.T) {
	st := openStore(t)
	f := addYouTubeFeed(t, st, chanFeedURL, "")
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		if r.URL == chanPageURL {
			cancel()
			return nil, ctx.Err()
		}
		return chanFeed(t, chanFeedURL), nil
	})
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	if hold := s.poll(ctx, f); !hold.IsZero() {
		t.Errorf("hold until %s", hold)
	}
	got := getFeed(t, st, f.ID)
	if got.IconURL != "" || got.ErrorCount != 0 || got.LastError != "" || !got.LastFetchedAt.Equal(t0) {
		t.Errorf("feed = icon %q errors %d %q, last fetch %s; want the poll recorded and nothing else",
			got.IconURL, got.ErrorCount, got.LastError, got.LastFetchedAt)
	}
	if !slices.ContainsFunc(fetcher.Calls(), func(r fetch.Request) bool { return r.URL == chanPageURL }) {
		t.Error("channel page was never requested")
	}

	// A cancelled lookup says nothing about the page: the next check tries
	// again in the same run.
	fetcher.setRespond(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		if r.URL == chanPageURL {
			return chanPage(), nil
		}
		return chanFeed(t, chanFeedURL), nil
	})
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID); got.IconURL != wantAvatar(t) {
		t.Errorf("icon after the next check = %q; want the avatar", got.IconURL)
	}
}

// TestSubscribe_AvatarIsNotWrittenToAReplacementFeed: the new subscription
// is removed, and its id given to another one, while its channel page is
// being fetched.
func TestSubscribe_AvatarIsNotWrittenToAReplacementFeed(t *testing.T) {
	st := openStore(t)
	var (
		s *Scheduler
		b store.Feed
	)
	fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		if r.URL != chanPageURL {
			return chanFeed(t, r.URL), nil
		}
		a, err := st.GetFeedByURL(ctx, chanFeedURL)
		mustNoErr(t, err)
		mustNoErr(t, st.DeleteFeed(ctx, a.ID))
		b, err = s.Subscribe(ctx, Subscription{FeedURL: playlistFeed}) // no icon, no lookup
		mustNoErr(t, err)
		if b.ID != a.ID {
			t.Fatalf("new subscription got id %d, not the freed %d", b.ID, a.ID)
		}
		return chanPage(), nil
	})
	s = newClockedScheduler(t, st, fetcher, newClock(t0))
	a, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanFeedURL})
	if err != nil || a.IconURL != "" {
		t.Fatalf("Subscribe = icon %q, %v; want the subscription without its avatar", a.IconURL, err)
	}
	if got := getFeed(t, st, b.ID); got != b {
		t.Errorf("the avatar was written to the new subscription:\n got %+v\nwant %+v", got, b)
	}
}

// iconWriteFails fails the first n SetFeedIcon calls.
type iconWriteFails struct {
	store.Store
	n int
}

func (s *iconWriteFails) SetFeedIcon(ctx context.Context, id int64, iconURL string) error {
	if s.n > 0 {
		s.n--
		return errDiskFull
	}
	return s.Store.SetFeedIcon(ctx, id, iconURL)
}

func TestPoll_AvatarThatCouldNotBeStoredIsTriedAgain(t *testing.T) {
	st := &iconWriteFails{Store: openStore(t), n: 1}
	f := addYouTubeFeed(t, st, chanFeedURL, "")
	fetcher := routeFetcher(map[string]*fetch.Result{chanFeedURL: chanFeed(t, chanFeedURL), chanPageURL: chanPage()})
	s := newClockedScheduler(t, st, fetcher, newClock(t0))

	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID); got.IconURL != "" {
		t.Fatalf("icon = %q after a failed write", got.IconURL)
	}
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID); got.IconURL != wantAvatar(t) || pageRequests(fetcher) != 2 {
		t.Errorf("icon %q after %d page requests; want the avatar stored by the second poll", got.IconURL, pageRequests(fetcher))
	}
}
