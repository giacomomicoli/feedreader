package sched

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/parse"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

func TestSubscribe_StoresAllEntriesNewestUnreadRestRead(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	folder, err := st.CreateFolder(ctx, "Tech")
	mustNoErr(t, err)

	// More entries than config.InitialUnread, not in date order: the newest
	// by published date are n0, n1, …
	all := itemsNewestFirst("n", config.InitialUnread+3, t0)
	shuffled := slices.Clone(all)
	rand.New(rand.NewPCG(1, 2)).Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})
	clock := newClock(t0.Add(time.Hour))
	fetcher := staticFetcher(ok(feedURL, atomDoc("Site feed", shuffled...)), nil)
	s := newClockedScheduler(t, st, fetcher, clock)

	feed, err := s.Subscribe(ctx, Subscription{FeedURL: feedURL, Title: "  My title  ", FolderID: folder.ID})
	mustNoErr(t, err)

	entries := feedEntries(t, st, feed.ID)
	if len(entries) != len(all) {
		t.Fatalf("stored %d entries, want all %d", len(entries), len(all))
	}
	unread := unreadGUIDs(entries)
	if len(unread) != config.InitialUnread {
		t.Fatalf("unread = %v, want the %d newest", unread, config.InitialUnread)
	}
	for _, it := range all[:config.InitialUnread] {
		if !unread[it.id] {
			t.Errorf("%s should be unread; unread = %v", it.id, unread)
		}
	}

	got := getFeed(t, st, feed.ID)
	now := clock.Now()
	switch {
	case got.URL != feedURL, got.Kind != store.KindRSS, got.FolderID != folder.ID:
		t.Errorf("feed = %+v", got)
	case got.Title != "My title", got.OriginalTitle != "Site feed":
		t.Errorf("titles = %q / %q", got.Title, got.OriginalTitle)
	case got.SiteURL != "https://site.example/", got.IconURL != "https://site.example/icon.png":
		t.Errorf("site %q icon %q", got.SiteURL, got.IconURL)
	case got.ETag != `"v2"`, got.LastModified != "Thu, 01 Oct 2026 18:00:00 GMT":
		t.Errorf("validators %q %q not stored from the initial fetch", got.ETag, got.LastModified)
	case !got.LastFetchedAt.Equal(now), !got.NextFetchAt.Equal(now.Add(config.DefaultPollInterval)):
		t.Errorf("last fetch %s next %s, want now and one interval later", got.LastFetchedAt, got.NextFetchAt)
	case got.ErrorCount != 0:
		t.Errorf("error_count = %d", got.ErrorCount)
	}
	if len(s.wake) != 1 {
		t.Error("Subscribe did not wake the dispatcher")
	}
}

func TestSubscribe_FewerEntriesThanInitialUnreadAreAllUnread(t *testing.T) {
	st := openStore(t)
	n := config.InitialUnread - 1
	s := newClockedScheduler(t, st,
		staticFetcher(ok(feedURL, atomDoc("Small", itemsNewestFirst("s", n, t0)...)), nil), newClock(t0))
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: feedURL})
	mustNoErr(t, err)
	entries := feedEntries(t, st, feed.ID)
	if len(entries) != n || len(unreadGUIDs(entries)) != n {
		t.Errorf("entries = %d, unread = %d; want %d and %d", len(entries), len(unreadGUIDs(entries)), n, n)
	}
}

func TestPreviewThenSubscribe_PerformsExactlyOneFetch(t *testing.T) {
	st := openStore(t)
	fetcher := staticFetcher(ok(feedURL, atomDoc("Site feed", itemsNewestFirst("e", 7, t0)...)), nil)
	clock := newClock(t0)
	s := newClockedScheduler(t, st, fetcher, clock)

	p, err := s.Preview(t.Context(), feedURL)
	mustNoErr(t, err)
	want := Preview{FeedURL: feedURL, Kind: store.KindRSS, Title: "Site feed", SiteURL: "https://site.example/", EntryCount: 7}
	if *p != want {
		t.Errorf("preview = %+v, want %+v", *p, want)
	}
	if calls := fetcher.Calls(); len(calls) != 1 || calls[0].Accept != fetch.FeedAccept || calls[0].ETag != "" {
		t.Fatalf("preview requests = %+v, want one unconditional feed request", calls)
	}

	clock.Advance(previewCacheTTL - time.Second)
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: p.FeedURL, Title: p.Title})
	mustNoErr(t, err)
	if n := fetcher.CallCount(); n != 1 {
		t.Errorf("fetches = %d, want exactly 1 for preview + subscribe", n)
	}
	if got := feedEntries(t, st, feed.ID); len(got) != 7 {
		t.Errorf("stored %d entries from the cached preview, want 7", len(got))
	}
	if !feed.LastFetchedAt.Equal(t0) {
		t.Errorf("last fetch = %s, want the preview fetch time %s", feed.LastFetchedAt, t0)
	}
}

func TestSubscribe_RefetchesWhenThePreviewIsOlderThanTenMinutes(t *testing.T) {
	st := openStore(t)
	fetcher := staticFetcher(ok(feedURL, atomDoc("Site feed", itemsNewestFirst("e", 2, t0)...)), nil)
	clock := newClock(t0)
	s := newClockedScheduler(t, st, fetcher, clock)
	_, err := s.Preview(t.Context(), feedURL)
	mustNoErr(t, err)
	clock.Advance(previewCacheTTL)
	_, err = s.Subscribe(t.Context(), Subscription{FeedURL: feedURL})
	mustNoErr(t, err)
	if n := fetcher.CallCount(); n != 2 {
		t.Errorf("fetches = %d, want 2 (stale preview refetched)", n)
	}
}

func TestPreview_PermanentRedirectGivesTheFinalFeedURLAndSubscribeReusesIt(t *testing.T) {
	st := openStore(t)
	const typed, moved = "http://old.example/feed", "https://new.example/feed"
	res := ok(moved, atomDoc("Moved feed", itemsNewestFirst("e", 2, t0)...))
	res.PermanentURL = moved
	fetcher := routeFetcher(map[string]*fetch.Result{typed: res})
	s := newClockedScheduler(t, st, fetcher, newClock(t0))

	p, err := s.Preview(t.Context(), typed)
	mustNoErr(t, err)
	if p.FeedURL != moved {
		t.Fatalf("preview FeedURL = %q, want %q", p.FeedURL, moved)
	}
	// The confirmation form posts the final URL.
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: p.FeedURL})
	mustNoErr(t, err)
	if feed.URL != moved || fetcher.CallCount() != 1 {
		t.Errorf("stored %q after %d fetches, want %q after 1", feed.URL, fetcher.CallCount(), moved)
	}
}

func TestPreview_YouTubeFeedIsKindYouTube(t *testing.T) {
	body, err := os.ReadFile("testdata/youtube.xml")
	mustNoErr(t, err)
	const ytURL = "https://www.youtube.com/feeds/videos.xml?channel_id=UCtestchannel0000000000"
	st := openStore(t)
	s := newClockedScheduler(t, st, routeFetcher(map[string]*fetch.Result{ytURL: ok(ytURL, body)}), newClock(t0))

	p, err := s.Preview(t.Context(), ytURL)
	mustNoErr(t, err)
	if p.Kind != store.KindYouTube || p.Title != "Test Channel" || p.EntryCount != 2 {
		t.Fatalf("preview = %+v", *p)
	}
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: ytURL})
	mustNoErr(t, err)
	if feed.Kind != store.KindYouTube {
		t.Errorf("stored kind = %q", feed.Kind)
	}
	e := feedEntries(t, st, feed.ID)["yt:video:vid00000001"]
	if e.URL != "https://www.youtube.com/watch?v=vid00000001" || e.ThumbnailURL != "https://i1.ytimg.com/vi/vid00000001/hqdefault.jpg" {
		t.Errorf("video entry = url %q thumbnail %q", e.URL, e.ThumbnailURL)
	}
}

// TestSubscribe_ChannelAvatarFoundByTheResolverIsStoredAndKept wires the
// real resolver over ResolveFetcher, as the app does: the avatar comes from
// the channel page fetched for the channel ID, with no request of its own,
// and polls of the feed (which has no icon) keep it.
func TestSubscribe_ChannelAvatarFoundByTheResolverIsStoredAndKept(t *testing.T) {
	const (
		channelID = "UCtestchannel00000000000"
		handle    = "https://www.youtube.com/@testchannel"
		ytURL     = "https://www.youtube.com/feeds/videos.xml?channel_id=" + channelID
	)
	body, err := os.ReadFile("testdata/youtube.xml")
	mustNoErr(t, err)
	page := []byte(`<!DOCTYPE html><html><head><title>Test Channel</title></head><body>
<link rel="canonical" href="https://www.youtube.com/channel/` + channelID + `">
<meta property="og:image" content="https://yt3.googleusercontent.com/synthetic-avatar=s900-c-k-c0x00ffffff-no-rj">
</body></html>`)
	st := openStore(t)
	clock := newClock(t0)
	fetcher := routeFetcher(map[string]*fetch.Result{
		handle: {StatusCode: 200, Body: page, ContentType: "text/html; charset=utf-8", FinalURL: handle},
		ytURL:  ok(ytURL, body),
	})
	s := newClockedScheduler(t, st, fetcher, clock)

	res, err := resolve.New(s.ResolveFetcher()).Resolve(t.Context(), handle)
	mustNoErr(t, err)
	c := res.Candidates[0]
	if c.URL != ytURL || !strings.HasPrefix(c.IconURL, "https://yt3.googleusercontent.com/synthetic-avatar=") {
		t.Fatalf("candidate = %+v", c)
	}
	_, err = s.Preview(t.Context(), c.URL)
	mustNoErr(t, err)
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: c.URL, IconURL: c.IconURL})
	mustNoErr(t, err)
	if feed.IconURL != c.IconURL {
		t.Errorf("stored icon = %q; want the avatar %q", feed.IconURL, c.IconURL)
	}
	if n := fetcher.CallCount(); n != 2 {
		t.Errorf("fetches = %d; want 2 (channel page, feed)", n)
	}

	clock.Advance(config.DefaultPollInterval)
	pollOnce(t, s, st, feed.ID)
	if got := getFeed(t, st, feed.ID); got.IconURL != c.IconURL || got.LastFetchedAt.Equal(feed.LastFetchedAt) {
		t.Errorf("after a poll: icon %q fetched %s; want the avatar kept by a new fetch", got.IconURL, got.LastFetchedAt)
	}
}

func TestSubscribe_IconURLOnlyForAFeedWithoutIconAndOnlyHTTP(t *testing.T) {
	ytBody, err := os.ReadFile("testdata/youtube.xml")
	mustNoErr(t, err)
	const (
		ytURL  = "https://www.youtube.com/feeds/videos.xml?channel_id=UCtestchannel0000000000"
		avatar = "https://yt3.googleusercontent.com/synthetic-avatar"
	)
	rssBody := atomDoc("Site feed", itemsNewestFirst("e", 1, t0)...) // has <icon>
	for _, tc := range []struct {
		name, url string
		body      []byte
		icon      string
		want      string
	}{
		{"feed's own icon wins", feedURL, rssBody, avatar, "https://site.example/icon.png"},
		{"used when the feed has none", ytURL, ytBody, avatar, avatar},
		{"trimmed", ytURL, ytBody, "  " + avatar + "\n", avatar},
		{"none", ytURL, ytBody, "", ""},
		{"javascript", ytURL, ytBody, "javascript:alert(1)", ""},
		{"data", ytURL, ytBody, "data:image/png;base64,AAAA", ""},
		{"ftp", ytURL, ytBody, "ftp://files.example/a.png", ""},
		{"relative", ytURL, ytBody, "/a.png", ""},
		{"no host", ytURL, ytBody, "https:///a.png", ""},
		{"unparsable", ytURL, ytBody, "https://[::1/a.png", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			s := newClockedScheduler(t, st, routeFetcher(map[string]*fetch.Result{tc.url: ok(tc.url, tc.body)}), newClock(t0))
			feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: tc.url, IconURL: tc.icon})
			mustNoErr(t, err)
			if feed.IconURL != tc.want {
				t.Errorf("stored icon = %q; want %q", feed.IconURL, tc.want)
			}
		})
	}
}

func TestPreview_ReturnsFetchAndParseErrorsAndCachesNothing(t *testing.T) {
	st := openStore(t)
	page := ok(feedURL, []byte("<!doctype html><html><head><title>x</title></head></html>"))
	fetcher := staticFetcher(page, nil)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))

	_, err := s.Preview(t.Context(), feedURL)
	if !errors.Is(err, parse.ErrNotAFeed) {
		t.Errorf("HTML page: err = %v, want parse.ErrNotAFeed", err)
	}
	_, err = s.Subscribe(t.Context(), Subscription{FeedURL: feedURL})
	if !errors.Is(err, parse.ErrNotAFeed) || fetcher.CallCount() != 2 {
		t.Errorf("subscribe after a failed preview: err %v after %d fetches", err, fetcher.CallCount())
	}

	fetcher.setRespond(func(context.Context, fetch.Request) (*fetch.Result, error) {
		return nil, &fetch.StatusError{URL: feedURL, StatusCode: 404}
	})
	var se *fetch.StatusError
	if _, err := s.Preview(t.Context(), feedURL); !errors.As(err, &se) || se.StatusCode != 404 {
		t.Errorf("404: err = %v, want a *fetch.StatusError", err)
	}
	if _, err := s.Preview(t.Context(), "  "); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("empty URL: err = %v, want store.ErrInvalid", err)
	}
	if n, _ := st.ListFeeds(t.Context()); len(n) != 0 {
		t.Errorf("failed adds stored feeds: %+v", n)
	}
}

func TestSubscribe_AlreadySubscribedURLIsConflictWithoutFetching(t *testing.T) {
	st := openStore(t)
	addFeed(t, st, feedURL, t0)
	fetcher := staticFetcher(ok(feedURL, atomDoc("x")), nil)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	_, err := s.Subscribe(t.Context(), Subscription{FeedURL: feedURL})
	if !errors.Is(err, store.ErrConflict) {
		t.Fatalf("err = %v, want store.ErrConflict", err)
	}
	if n := fetcher.CallCount(); n != 0 {
		t.Errorf("fetches = %d, want 0", n)
	}
}

func TestSubscribe_TwiceAfterOnePreviewIsConflict(t *testing.T) {
	st := openStore(t)
	fetcher := staticFetcher(ok(feedURL, atomDoc("Site feed", itemsNewestFirst("e", 1, t0)...)), nil)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	_, err := s.Preview(t.Context(), feedURL)
	mustNoErr(t, err)
	_, err = s.Subscribe(t.Context(), Subscription{FeedURL: feedURL})
	mustNoErr(t, err)
	if _, err := s.Subscribe(t.Context(), Subscription{FeedURL: feedURL}); !errors.Is(err, store.ErrConflict) {
		t.Errorf("second subscribe: err = %v, want store.ErrConflict", err)
	}
	if n := fetcher.CallCount(); n != 1 {
		t.Errorf("fetches = %d, want 1", n)
	}
}

func TestSubscribe_TitleFallsBackToFeedTitleThenHost(t *testing.T) {
	tests := []struct {
		name, userTitle, docTitle, want string
	}{
		{"user title", "Mine", "Theirs", "Mine"},
		{"blank user title uses the feed title", "   ", "Theirs", "Theirs"},
		{"no titles at all uses the host", "", "", "site.example"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			s := newClockedScheduler(t, st, staticFetcher(ok(feedURL, atomDoc(tt.docTitle)), nil), newClock(t0))
			feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: feedURL, Title: tt.userTitle})
			mustNoErr(t, err)
			if feed.Title != tt.want || feed.OriginalTitle != tt.docTitle {
				t.Errorf("title %q original %q, want %q and %q", feed.Title, feed.OriginalTitle, tt.want, tt.docTitle)
			}
		})
	}
}

func TestSubscribe_FirstPollHonoursTTLAndMaxAge(t *testing.T) {
	st := openStore(t)
	res := ok(feedURL, rssDoc("Slow", minutes(longHint), itemsNewestFirst("e", 1, t0)...))
	res.MaxAge = (config.DefaultPollInterval + longHint) / 2
	clock := newClock(t0)
	s := newClockedScheduler(t, st, staticFetcher(res, nil), clock)
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: feedURL})
	mustNoErr(t, err)
	if got := feed.NextFetchAt.Sub(clock.Now()); got != longHint {
		t.Errorf("first poll in %s, want %s (ttl)", got, longHint)
	}
}

func TestSubscribe_UnknownFolderFailsAndARetryReusesTheFetch(t *testing.T) {
	st := openStore(t)
	fetcher := staticFetcher(ok(feedURL, atomDoc("Site feed", itemsNewestFirst("e", 1, t0)...)), nil)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	if _, err := s.Subscribe(t.Context(), Subscription{FeedURL: feedURL, FolderID: 999}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v, want store.ErrNotFound", err)
	}
	if _, err := s.Subscribe(t.Context(), Subscription{FeedURL: feedURL}); err != nil {
		t.Fatal(err)
	}
	if n := fetcher.CallCount(); n != 1 {
		t.Errorf("fetches = %d, want 1", n)
	}
}

func TestPreview_ConcurrentAddFetchesAreBounded(t *testing.T) {
	gate := make(chan struct{})
	fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		select {
		case <-gate:
			return ok(r.URL, atomDoc("Feed", itemsNewestFirst("e", 1, t0)...)), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	s := newClockedScheduler(t, openStore(t), fetcher, newClock(t0))
	n := addFetchSlots + 2
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() {
			if _, err := s.Preview(context.Background(), fmt.Sprintf("https://f%d.example/feed", i)); err != nil {
				t.Errorf("Preview: %v", err)
			}
		})
	}
	waitFor(t, "the add-flow fetches to start", func() bool { return fetcher.Active() == addFetchSlots })
	time.Sleep(100 * time.Millisecond)
	if got := fetcher.CallCount(); got != addFetchSlots {
		t.Errorf("concurrent add-flow fetches = %d, want %d", got, addFetchSlots)
	}

	// A request that gives up while waiting for a slot fetches nothing.
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Preview(ctx, "https://late.example/feed"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Preview while all slots are busy: err = %v, want the context's error", err)
	}

	close(gate)
	wg.Wait()
	if got := fetcher.CallCount(); got != n {
		t.Errorf("fetches = %d, want %d", got, n)
	}
	if got := fetcher.MaxActive(); got != addFetchSlots {
		t.Errorf("max concurrent add-flow fetches = %d, want %d", got, addFetchSlots)
	}
}

// TestResolveFetcher_DirectFeedURLIsFetchedOnce: pasting a feed's own URL
// makes the resolver fetch it; Preview and Subscribe reuse that fetch.
func TestResolveFetcher_DirectFeedURLIsFetchedOnce(t *testing.T) {
	fetcher := staticFetcher(ok(feedURL, atomDoc("Direct", itemsNewestFirst("e", 2, t0)...)), nil)
	s := newClockedScheduler(t, openStore(t), fetcher, newClock(t0))
	res, err := resolve.New(s.ResolveFetcher()).Resolve(t.Context(), feedURL)
	mustNoErr(t, err)
	if len(res.Candidates) != 1 || res.Candidates[0].URL != feedURL {
		t.Fatalf("candidates = %+v, want [%s]", res.Candidates, feedURL)
	}
	p, err := s.Preview(t.Context(), res.Candidates[0].URL)
	mustNoErr(t, err)
	if p.Title != "Direct" || p.EntryCount != 2 {
		t.Errorf("preview = %+v", p)
	}
	if _, err := s.Subscribe(t.Context(), Subscription{FeedURL: p.FeedURL}); err != nil {
		t.Fatal(err)
	}
	if n := fetcher.CallCount(); n != 1 {
		t.Errorf("feed fetched %d times by resolve, preview and subscribe; want 1", n)
	}
}

// TestResolveFetcher_UsesTheAddFlowSlots: resolver requests count against
// addFetchSlots like Preview's, so a burst of add-form submissions cannot
// multiply outbound requests.
func TestResolveFetcher_UsesTheAddFlowSlots(t *testing.T) {
	gate := make(chan struct{})
	fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		select {
		case <-gate:
			return ok(r.URL, atomDoc("Feed")), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	s := newClockedScheduler(t, openStore(t), fetcher, newClock(t0))
	var wg sync.WaitGroup
	defer wg.Wait()
	defer close(gate)
	for i := range addFetchSlots {
		wg.Go(func() { _, _ = s.Preview(context.Background(), fmt.Sprintf("https://f%d.example/feed", i)) })
	}
	waitFor(t, "the add-flow slots to fill", func() bool { return fetcher.Active() == addFetchSlots })

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.ResolveFetcher().Fetch(ctx, fetch.Request{URL: "https://page.example/"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("resolver fetch while all add-flow slots are busy: err = %v, want the context's error", err)
	}
	if n := fetcher.CallCount(); n != addFetchSlots {
		t.Errorf("fetches = %d, want %d", n, addFetchSlots)
	}
}

func TestPreview_DoesNotWaitForThePollingPool(t *testing.T) {
	st := openStore(t)
	later := time.Now().Add(time.Hour) // not due: no startup stagger
	for i := range config.DefaultFetchWorkers {
		addFeed(t, st, fmt.Sprintf("https://f%d.example/feed", i), later)
	}
	gate := make(chan struct{})
	defer close(gate)
	fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		if r.URL == feedURL {
			return ok(feedURL, atomDoc("New", itemsNewestFirst("e", 1, t0)...)), nil
		}
		select { // slow polls
		case <-gate:
			return notModified(r.URL), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	})
	s := newTestScheduler(t, st, fetcher, config.DefaultFetchWorkers)
	startRun(t, s)
	mustNoErr(t, s.RefreshAll(t.Context()))
	waitFor(t, "the polling pool to fill", func() bool { return fetcher.Active() == config.DefaultFetchWorkers })

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := s.Preview(ctx, feedURL); err != nil {
		t.Errorf("Preview while every poll slot is busy: %v", err)
	}
}
