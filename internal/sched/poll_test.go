package sched

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/parse"
	"github.com/giacomomicoli/feedreader/internal/store"
)

const feedURL = "https://site.example/feed.xml"

// pollOnce re-reads the feed (as the dispatcher would via DueFeeds) and
// polls it at the clock's time.
func pollOnce(t *testing.T, s *Scheduler, st store.Store, id int64) time.Time {
	t.Helper()
	return s.poll(t.Context(), getFeed(t, st, id))
}

func TestPoll_304SendsStoredValidatorsResetsErrorCountAndReschedules(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0, itemsNewestFirst("e", 2, t0)...)
	ctx := t.Context()
	mustNoErr(t, st.RecordFetchError(ctx, f.ID, "HTTP 503", t0, t0))
	mustNoErr(t, st.RecordFetchError(ctx, f.ID, "HTTP 503", t0, t0))

	clock := newClock(t0.Add(7 * time.Hour))
	fetcher := staticFetcher(notModified(feedURL), nil)
	s := newClockedScheduler(t, st, fetcher, clock)
	if hold := pollOnce(t, s, st, f.ID); !hold.IsZero() {
		t.Fatalf("poll asked to hold the feed until %s", hold)
	}

	calls := fetcher.Calls()
	if len(calls) != 1 {
		t.Fatalf("fetches = %d, want 1", len(calls))
	}
	want := fetch.Request{URL: feedURL, ETag: `"v1"`, LastModified: "Wed, 30 Sep 2026 12:00:00 GMT", Accept: fetch.FeedAccept}
	if calls[0] != want {
		t.Errorf("request = %+v, want %+v", calls[0], want)
	}
	got := getFeed(t, st, f.ID)
	now := clock.Now()
	if got.ErrorCount != 0 || got.LastError != "" {
		t.Errorf("error state = %d %q, want reset", got.ErrorCount, got.LastError)
	}
	if !got.LastFetchedAt.Equal(now) || !got.NextFetchAt.Equal(now.Add(config.DefaultPollInterval)) {
		t.Errorf("last fetch %s, next %s; want %s and one interval later", got.LastFetchedAt, got.NextFetchAt, now)
	}
	if len(feedEntries(t, st, f.ID)) != 2 {
		t.Error("a 304 changed the entries")
	}
}

func TestPoll_YouTubeFeedIsPolledAtTheYouTubeInterval(t *testing.T) {
	const ytURL = "https://www.youtube.com/feeds/videos.xml?channel_id=UCtestchannel0000000000"
	tests := []struct {
		name string
		res  *fetch.Result
	}{
		{"304", notModified(ytURL)},
		{"200", ok(ytURL, atomDoc("Channel", itemsNewestFirst("v", 1, t0)...))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			f := addFeedOfKind(t, st, store.KindYouTube, ytURL, t0)
			clock := newClock(t0.Add(time.Hour))
			s := newClockedScheduler(t, st, staticFetcher(tt.res, nil), clock)
			pollOnce(t, s, st, f.ID)
			if got := getFeed(t, st, f.ID); got.ErrorCount != 0 || !got.NextFetchAt.Equal(clock.Now().Add(config.DefaultYouTubePollInterval)) {
				t.Errorf("next fetch in %s (errors %d), want %s", got.NextFetchAt.Sub(clock.Now()), got.ErrorCount, config.DefaultYouTubePollInterval)
			}
		})
	}
}

func TestPoll_200InsertsNewEntriesUnreadAndKeepsReadStateOfExistingOnes(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	old := itemsNewestFirst("old", 3, t0)
	f := addFeed(t, st, feedURL, t0, old...) // old0, the newest, is unread
	entries := feedEntries(t, st, f.ID)
	mustNoErr(t, st.SetRead(ctx, entries["old1"].ID, true, t0))
	mustNoErr(t, st.SetLater(ctx, entries["old2"].ID, true))
	mustNoErr(t, st.SetFavourite(ctx, entries["old2"].ID, true))

	// The feed now has two new items and an edited title on an old one.
	edited := append([]item(nil), old...)
	edited[1].title = "Edited title"
	doc := atomDoc("Renamed feed", append(itemsNewestFirst("new", 2, t0.Add(5*time.Hour)), edited...)...)
	clock := newClock(t0.Add(6 * time.Hour))
	s := newClockedScheduler(t, st, staticFetcher(ok(feedURL, doc), nil), clock)
	pollOnce(t, s, st, f.ID)

	got := feedEntries(t, st, f.ID)
	if len(got) != 5 {
		t.Fatalf("entries = %d, want 5", len(got))
	}
	for _, guid := range []string{"new0", "new1"} {
		if got[guid].IsRead {
			t.Errorf("new entry %s stored read; new entries on a poll are always unread", guid)
		}
		if !got[guid].FetchedAt.Equal(clock.Now()) {
			t.Errorf("new entry %s fetched_at = %s, want %s", guid, got[guid].FetchedAt, clock.Now())
		}
	}
	if !got["old1"].IsRead || got["old1"].Title != "Edited title" {
		t.Errorf("old1: read=%v title=%q; want read state kept and title refreshed", got["old1"].IsRead, got["old1"].Title)
	}
	if got["old0"].IsRead || !got["old2"].IsLater || !got["old2"].IsFavourite {
		t.Errorf("user state of existing entries changed: %+v / %+v", got["old0"].Entry, got["old2"].Entry)
	}

	feed := getFeed(t, st, f.ID)
	if feed.ETag != `"v2"` || feed.LastModified != "Thu, 01 Oct 2026 18:00:00 GMT" {
		t.Errorf("validators = %q %q, want the new ones", feed.ETag, feed.LastModified)
	}
	if feed.OriginalTitle != "Renamed feed" || feed.Title != "Feed "+feedURL {
		t.Errorf("titles = %q / %q; want original refreshed, user title kept", feed.OriginalTitle, feed.Title)
	}
	if feed.SiteURL != "https://site.example/" || feed.IconURL != "https://site.example/icon.png" {
		t.Errorf("metadata not refreshed: site %q icon %q", feed.SiteURL, feed.IconURL)
	}
	if !feed.NextFetchAt.Equal(clock.Now().Add(config.DefaultPollInterval)) {
		t.Errorf("next fetch = %s, want one interval later", feed.NextFetchAt)
	}
}

func TestPoll_PublishedFallsBackToUpdatedThenFetchedAt(t *testing.T) {
	published := t0.Add(-48 * time.Hour)
	updated := t0.Add(-24 * time.Hour)
	fetchedAt := t0.Add(time.Hour)
	got := newEntries([]parse.Entry{
		{GUID: "p", PublishedAt: published, UpdatedAt: updated},
		{GUID: "u", UpdatedAt: updated},
		{GUID: "f"},
		{GUID: ""}, // never produced by parse; skipped rather than failing the batch
	}, fetchedAt)
	want := map[string]time.Time{"p": published, "u": updated, "f": fetchedAt}
	if len(got) != len(want) {
		t.Fatalf("entries = %d, want %d", len(got), len(want))
	}
	for _, e := range got {
		if !e.PublishedAt.Equal(want[e.GUID]) {
			t.Errorf("%s: published = %s, want %s", e.GUID, e.PublishedAt, want[e.GUID])
		}
	}

	// End to end: an RSS item without pubDate is stored at fetch time.
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	clock := newClock(fetchedAt)
	doc := rssDoc("RSS", 0, item{id: "nodate", title: "No date"})
	s := newClockedScheduler(t, st, staticFetcher(ok(feedURL, doc), nil), clock)
	pollOnce(t, s, st, f.ID)
	if e := feedEntries(t, st, f.ID)["nodate"]; !e.PublishedAt.Equal(fetchedAt) {
		t.Errorf("stored published_at = %s, want fetched_at %s", e.PublishedAt, fetchedAt)
	}
}

func TestPoll_PermanentRedirectUpdatesStoredFeedURL(t *testing.T) {
	st := openStore(t)
	const oldURL, newURL = "http://old.example/feed", "https://new.example/feed"
	f := addFeed(t, st, oldURL, t0)
	res := ok(newURL, atomDoc("Moved", itemsNewestFirst("m", 1, t0)...))
	res.PermanentURL = newURL
	fetcher := staticFetcher(res, nil)
	s := newClockedScheduler(t, st, fetcher, newClock(t0.Add(time.Hour)))
	pollOnce(t, s, st, f.ID)

	got := getFeed(t, st, f.ID)
	if got.URL != newURL {
		t.Fatalf("feed URL = %q, want %q", got.URL, newURL)
	}
	if got.ErrorCount != 0 || len(feedEntries(t, st, f.ID)) != 1 {
		t.Errorf("redirected 200 not recorded as a success: %+v", got)
	}
	pollOnce(t, s, st, f.ID)
	if calls := fetcher.Calls(); calls[1].URL != newURL {
		t.Errorf("next poll fetched %q, want the new URL", calls[1].URL)
	}
}

func TestPoll_PermanentRedirectToAnAlreadySubscribedURLKeepsTheOldURL(t *testing.T) {
	st := openStore(t)
	const oldURL, newURL = "http://old.example/feed", "https://new.example/feed"
	f := addFeed(t, st, oldURL, t0)
	other := addFeed(t, st, newURL, t0)
	res := ok(newURL, atomDoc("Moved", itemsNewestFirst("m", 1, t0)...))
	res.PermanentURL = newURL
	s := newClockedScheduler(t, st, staticFetcher(res, nil), newClock(t0.Add(time.Hour)))
	pollOnce(t, s, st, f.ID)

	if got := getFeed(t, st, f.ID); got.URL != oldURL || got.ErrorCount != 0 {
		t.Errorf("feed = %q (errors %d), want the old URL kept and the fetch recorded", got.URL, got.ErrorCount)
	}
	if len(feedEntries(t, st, f.ID)) != 1 || len(feedEntries(t, st, other.ID)) != 0 {
		t.Error("entries were not stored on the polled feed only")
	}
}

func TestPoll_ConsecutiveFailuresAreCountedKeepEntriesAndBackOff(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	f := addFeed(t, st, feedURL, t0, itemsNewestFirst("e", 3, t0)...)
	mustNoErr(t, st.SetRead(ctx, feedEntries(t, st, f.ID)["e1"].ID, true, t0))
	before := feedEntries(t, st, f.ID)

	clock := newClock(t0)
	fetcher := staticFetcher(nil, &fetch.StatusError{URL: feedURL, StatusCode: 503})
	s := newClockedScheduler(t, st, fetcher, clock)
	// One failure past the warning, so that the backoff reaches its cap.
	for n := 1; n <= config.WarnAfterFailures+1; n++ {
		clock.Set(getFeed(t, st, f.ID).NextFetchAt) // poll when due
		pollOnce(t, s, st, f.ID)
		got := getFeed(t, st, f.ID)
		if got.ErrorCount != n {
			t.Fatalf("after failure %d: error_count = %d", n, got.ErrorCount)
		}
		if got.LastError != "HTTP 503 from "+feedURL {
			t.Errorf("last_error = %q", got.LastError)
		}
		delay := firstBackoff // a single failure is retried soon
		if n > 1 {
			delay = config.DefaultPollInterval // then × 2^(n-1), capped
			for range n - 1 {
				delay = min(2*delay, config.MaxBackoff)
			}
		}
		if !got.LastFetchedAt.Equal(clock.Now()) || !got.NextFetchAt.Equal(clock.Now().Add(delay)) {
			t.Errorf("after failure %d: next fetch %s, want +%s", n, got.NextFetchAt.Sub(clock.Now()), delay)
		}
		if got.ETag != `"v1"` {
			t.Errorf("a failure changed the stored ETag to %q", got.ETag)
		}
	}
	if got := getFeed(t, st, f.ID); got.ErrorCount < config.WarnAfterFailures {
		t.Errorf("error_count = %d, below the warning threshold %d", got.ErrorCount, config.WarnAfterFailures)
	}
	after := feedEntries(t, st, f.ID)
	if len(after) != len(before) {
		t.Fatalf("entries = %d after failures, want %d (never deleted)", len(after), len(before))
	}
	for guid, e := range before {
		if after[guid].IsRead != e.IsRead {
			t.Errorf("%s: read state changed by a failure", guid)
		}
	}

	// The next success resets the counter.
	fetcher.setRespond(func(context.Context, fetch.Request) (*fetch.Result, error) {
		return ok(feedURL, atomDoc("Back", itemsNewestFirst("e", 3, t0)...)), nil
	})
	clock.Set(getFeed(t, st, f.ID).NextFetchAt)
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID); got.ErrorCount != 0 || got.LastError != "" {
		t.Errorf("after success: error_count %d, last_error %q", got.ErrorCount, got.LastError)
	}
}

// TestPoll_OneOffFailureIsRetriedSoonWithoutBackoff: a single bogus error
// (YouTube answers the odd 404 for a working channel) costs one quick retry,
// not a doubled interval; the feed then returns to its normal schedule.
func TestPoll_OneOffFailureIsRetriedSoonWithoutBackoff(t *testing.T) {
	const ytURL = "https://www.youtube.com/feeds/videos.xml?channel_id=UCtestchannel0000000000"
	st := openStore(t)
	f := addFeedOfKind(t, st, store.KindYouTube, ytURL, t0)
	clock := newClock(t0)
	fetcher := staticFetcher(nil, &fetch.StatusError{URL: ytURL, StatusCode: 404})
	s := newClockedScheduler(t, st, fetcher, clock)

	pollOnce(t, s, st, f.ID)
	got := getFeed(t, st, f.ID)
	if want := clock.Now().Add(config.FirstRetryDelay); got.ErrorCount != 1 || !got.NextFetchAt.Equal(want) {
		t.Fatalf("after one 404: error_count %d, next fetch in %s; want 1 and %s",
			got.ErrorCount, got.NextFetchAt.Sub(clock.Now()), config.FirstRetryDelay)
	}

	fetcher.setRespond(func(context.Context, fetch.Request) (*fetch.Result, error) { return notModified(ytURL), nil })
	clock.Set(got.NextFetchAt)
	pollOnce(t, s, st, f.ID)
	got = getFeed(t, st, f.ID)
	if want := clock.Now().Add(config.DefaultYouTubePollInterval); got.ErrorCount != 0 || !got.NextFetchAt.Equal(want) {
		t.Errorf("after the retry succeeded: error_count %d, next fetch in %s; want 0 and %s",
			got.ErrorCount, got.NextFetchAt.Sub(clock.Now()), config.DefaultYouTubePollInterval)
	}
}

func TestPoll_EveryKindOfFailureIsRecordedWithBackoff(t *testing.T) {
	htmlPage := []byte("<!doctype html><html><head><title>Blog</title></head><body>hi</body></html>")
	longRetry := (firstBackoff + config.MaxBackoff) / 2
	tests := []struct {
		name      string
		res       *fetch.Result
		err       error
		wantMsg   string // substring of last_error
		wantDelay time.Duration
	}{
		{"404", nil, &fetch.StatusError{URL: feedURL, StatusCode: 404}, "HTTP 404", firstBackoff},
		{"410", nil, &fetch.StatusError{URL: feedURL, StatusCode: 410}, "HTTP 410", firstBackoff},
		{"429 honours Retry-After", nil, &fetch.StatusError{URL: feedURL, StatusCode: 429, RetryAfter: longRetry}, "HTTP 429", longRetry},
		{"503 Retry-After is capped", nil, &fetch.StatusError{URL: feedURL, StatusCode: 503, RetryAfter: 30 * config.MaxBackoff}, "HTTP 503", config.MaxBackoff},
		{"500 short Retry-After keeps the backoff", nil, &fetch.StatusError{URL: feedURL, StatusCode: 500, RetryAfter: time.Minute}, "HTTP 500", firstBackoff},
		{"network", nil, fmt.Errorf("fetch %s: dial tcp: connection refused", feedURL), "connection refused", firstBackoff},
		{"timeout", nil, fmt.Errorf("fetch %s: %w", feedURL, context.DeadlineExceeded), "deadline exceeded", firstBackoff},
		{"body over the size cap", nil, fmt.Errorf("fetch %s: %w", feedURL, fetch.ErrTooLarge), "size limit", firstBackoff},
		{"redirect loop", nil, fmt.Errorf("fetch %s: %w", feedURL, fetch.ErrRedirects), "redirect", firstBackoff},
		{"malformed XML", ok(feedURL, []byte("<?xml version=\"1.0\"?><rss version=\"2.0\"><channel><item><title>x</ti")), nil, "parse", firstBackoff},
		{"HTML instead of a feed", ok(feedURL, htmlPage), nil, parse.ErrNotAFeed.Error(), firstBackoff},
		{"fetcher returned nothing", nil, nil, errNoResult.Error(), firstBackoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			f := addFeed(t, st, feedURL, t0, itemsNewestFirst("e", 2, t0)...)
			clock := newClock(t0.Add(time.Hour))
			s := newClockedScheduler(t, st, staticFetcher(tt.res, tt.err), clock)
			pollOnce(t, s, st, f.ID)

			got := getFeed(t, st, f.ID)
			if got.ErrorCount != 1 || !strings.Contains(got.LastError, tt.wantMsg) {
				t.Errorf("error_count %d, last_error %q; want 1 and %q", got.ErrorCount, got.LastError, tt.wantMsg)
			}
			if want := clock.Now().Add(tt.wantDelay); !got.NextFetchAt.Equal(want) {
				t.Errorf("next fetch in %s, want %s", got.NextFetchAt.Sub(clock.Now()), tt.wantDelay)
			}
			if got.ETag != `"v1"` || len(feedEntries(t, st, f.ID)) != 2 {
				t.Errorf("a failure touched validators (%q) or entries", got.ETag)
			}
		})
	}
}

func TestPoll_ShutdownDuringFetchRecordsNothing(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	ctx, cancel := context.WithCancel(t.Context())
	fetcher := newFetcher(func(ctx context.Context, _ fetch.Request) (*fetch.Result, error) {
		cancel()
		<-ctx.Done()
		return nil, fmt.Errorf("fetch %s: %w", feedURL, ctx.Err())
	})
	s := newClockedScheduler(t, st, fetcher, newClock(t0.Add(time.Hour)))
	s.poll(ctx, f)

	got := getFeed(t, st, f.ID)
	if got.ErrorCount != 0 || got.LastError != "" || !got.NextFetchAt.Equal(f.NextFetchAt) {
		t.Errorf("shutdown was recorded: %+v", got)
	}
}

func TestPoll_TTLAndMaxAgeRaiseTheInterval(t *testing.T) {
	tests := []struct {
		name   string
		body   []byte
		maxAge time.Duration
		want   time.Duration
	}{
		{"short ttl keeps the interval", rssDoc("r", minutes(config.DefaultPollInterval/4)), 0, config.DefaultPollInterval},
		{"long ttl is a lower bound", rssDoc("r", minutes(longHint)), 0, longHint},
		{"absurd ttl is capped", rssDoc("r", minutes(30*config.MaxBackoff)), 0, config.MaxBackoff},
		{"max-age is a lower bound", atomDoc("a"), longHint, longHint},
		{"short max-age keeps the interval", atomDoc("a"), time.Minute, config.DefaultPollInterval},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			f := addFeed(t, st, feedURL, t0)
			res := ok(feedURL, tt.body)
			res.MaxAge = tt.maxAge
			clock := newClock(t0.Add(time.Hour))
			s := newClockedScheduler(t, st, staticFetcher(res, nil), clock)
			pollOnce(t, s, st, f.ID)
			if got := getFeed(t, st, f.ID).NextFetchAt.Sub(clock.Now()); got != tt.want {
				t.Errorf("next fetch in %s, want %s", got, tt.want)
			}
		})
	}

	// 304 responses carry max-age too.
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	res := notModified(feedURL)
	res.MaxAge = longHint
	clock := newClock(t0.Add(time.Hour))
	s := newClockedScheduler(t, st, staticFetcher(res, nil), clock)
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID).NextFetchAt.Sub(clock.Now()); got != longHint {
		t.Errorf("304 with max-age: next fetch in %s, want %s", got, longHint)
	}
}

// TestPoll_TTLStillAppliesAfter304: a 304 has no document to read <ttl>
// from; the one stored from the last document stays the lower bound.
func TestPoll_TTLStillAppliesAfter304(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	clock := newClock(t0.Add(time.Hour))
	fetcher := staticFetcher(ok(feedURL, rssDoc("r", minutes(longHint))), nil)
	s := newClockedScheduler(t, st, fetcher, clock)
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID).TTLSec; got != int(longHint/time.Second) {
		t.Fatalf("stored ttl = %ds, want %s", got, longHint)
	}

	for i := range 2 {
		clock.Advance(longHint)
		s.fetcher = staticFetcher(notModified(feedURL), nil)
		pollOnce(t, s, st, f.ID)
		if got := getFeed(t, st, f.ID).NextFetchAt.Sub(clock.Now()); got != longHint {
			t.Errorf("304 #%d: next fetch in %s, want the stored ttl %s", i+1, got, longHint)
		}
	}

	// A document without <ttl> clears it.
	clock.Advance(longHint)
	s.fetcher = staticFetcher(ok(feedURL, atomDoc("a")), nil)
	pollOnce(t, s, st, f.ID)
	clock.Advance(config.DefaultPollInterval)
	s.fetcher = staticFetcher(notModified(feedURL), nil)
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID).NextFetchAt.Sub(clock.Now()); got != config.DefaultPollInterval {
		t.Errorf("304 after a document without ttl: next fetch in %s, want %s", got, config.DefaultPollInterval)
	}
}

// TestPoll_RefreshDuringTheFetchIsKept: a Refresh that arrives while the feed
// is being fetched moves next_fetch_at; the poll must not overwrite it with
// its own schedule, or the refresh would be lost.
func TestPoll_RefreshDuringTheFetchIsKept(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	clock := newClock(t0.Add(time.Hour))
	refreshedAt := clock.Now().Add(time.Second)
	fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		mustNoErr(t, st.SetNextFetch(ctx, f.ID, refreshedAt)) // what Refresh writes
		return notModified(feedURL), nil
	})
	s := newClockedScheduler(t, st, fetcher, clock)
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID).NextFetchAt; !got.Equal(refreshedAt) {
		t.Errorf("next fetch = %s, want the refresh time %s", got, refreshedAt)
	}

	// Without a refresh the poll's own schedule is written.
	s.fetcher = staticFetcher(notModified(feedURL), nil)
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID).NextFetchAt.Sub(clock.Now()); got != config.DefaultPollInterval {
		t.Errorf("next fetch in %s, want %s", got, config.DefaultPollInterval)
	}
}

func TestPoll_PerFeedIntervalOverride(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	mustNoErr(t, st.SetFeedInterval(t.Context(), f.ID, 3600))
	clock := newClock(t0.Add(time.Hour))
	s := newClockedScheduler(t, st, staticFetcher(notModified(feedURL), nil), clock)
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID).NextFetchAt.Sub(clock.Now()); got != time.Hour {
		t.Errorf("next fetch in %s, want the per-feed 1h", got)
	}

	clock.Advance(time.Hour)
	s.fetcher = staticFetcher(nil, &fetch.StatusError{URL: feedURL, StatusCode: 500})
	pollOnce(t, s, st, f.ID)
	if got, want := getFeed(t, st, f.ID).NextFetchAt.Sub(clock.Now()), min(time.Hour, config.FirstRetryDelay); got != want {
		t.Errorf("first failure: next fetch in %s, want %s", got, want)
	}
	clock.Set(getFeed(t, st, f.ID).NextFetchAt)
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID).NextFetchAt.Sub(clock.Now()); got != 2*time.Hour {
		t.Errorf("backoff from the per-feed interval: next fetch in %s, want 2h", got)
	}
}

// TestReschedule_ShorterIntervalAppliesNow: changing the per-feed interval
// moves the next fetch to the last fetch plus the new interval (at least
// now, at least the stored <ttl>), never later, and keeps a failure backoff.
func TestReschedule_ShorterIntervalAppliesNow(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	clock := newClock(t0)
	s := newClockedScheduler(t, st, staticFetcher(notModified(feedURL), nil), clock)
	pollOnce(t, s, st, f.ID) // next fetch at t0 + the default interval
	next := func() time.Time { return getFeed(t, st, f.ID).NextFetchAt }

	clock.Advance(time.Hour)
	mustNoErr(t, st.SetFeedInterval(ctx, f.ID, int(2*time.Hour/time.Second)))
	mustNoErr(t, s.Reschedule(ctx, f.ID))
	if got, want := next(), t0.Add(2*time.Hour); !got.Equal(want) {
		t.Errorf("2h interval: next fetch %s, want last fetch + 2h = %s", got, want)
	}

	mustNoErr(t, st.SetFeedInterval(ctx, f.ID, int(config.MinPollInterval/time.Second)))
	mustNoErr(t, s.Reschedule(ctx, f.ID))
	if got := next(); !got.Equal(clock.Now()) {
		t.Errorf("interval already elapsed: next fetch %s, want now %s", got, clock.Now())
	}

	before := next()
	mustNoErr(t, st.SetFeedInterval(ctx, f.ID, int(config.MaxBackoff/time.Second)))
	mustNoErr(t, s.Reschedule(ctx, f.ID))
	if got := next(); !got.Equal(before) {
		t.Errorf("longer interval moved the next fetch from %s to %s", before, got)
	}

	// The stored <ttl> stays a lower bound.
	mustNoErr(t, st.SetNextFetch(ctx, f.ID, t0.Add(config.MaxBackoff)))
	_, err := st.RecordFetchSuccess(ctx, store.FetchSuccess{FeedID: f.ID, FetchedAt: t0,
		NextFetchAt: t0.Add(config.MaxBackoff), TTLSec: int(longHint / time.Second)})
	mustNoErr(t, err)
	mustNoErr(t, st.SetFeedInterval(ctx, f.ID, int(2*time.Hour/time.Second)))
	mustNoErr(t, s.Reschedule(ctx, f.ID))
	if got, want := next(), t0.Add(longHint); !got.Equal(want) {
		t.Errorf("with ttl %s: next fetch %s, want %s", longHint, got, want)
	}

	// A failing feed keeps its backoff.
	backoff := clock.Now().Add(config.MaxBackoff)
	mustNoErr(t, st.RecordFetchError(ctx, f.ID, "HTTP 500", clock.Now(), backoff))
	mustNoErr(t, s.Reschedule(ctx, f.ID))
	if got := next(); !got.Equal(backoff) {
		t.Errorf("failing feed: next fetch %s, want its backoff %s", got, backoff)
	}

	if err := s.Reschedule(ctx, f.ID+1); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("unknown feed: err = %v, want ErrNotFound", err)
	}
}

// failingWrites is a store whose fetch-outcome writes fail, as on a full
// disk.
type failingWrites struct {
	store.Store
}

var errDiskFull = errors.New("database or disk is full")

func (failingWrites) RecordFetchSuccess(context.Context, store.FetchSuccess) (int, error) {
	return 0, errDiskFull
}

func (failingWrites) RecordNotModified(context.Context, int64, time.Time, time.Time) error {
	return errDiskFull
}

func (failingWrites) RecordFetchError(context.Context, int64, string, time.Time, time.Time) error {
	return errDiskFull
}

func TestPoll_UnstorableOutcomeHoldsTheFeedUntilItsNextFetch(t *testing.T) {
	tests := []struct {
		name string
		res  *fetch.Result
		err  error
		want time.Duration
	}{
		{"304", notModified(feedURL), nil, config.DefaultPollInterval},
		// The entries cannot be saved: recorded (and held) as a failure.
		// error_count cannot be raised either, so the feed is held as long
		// as a second failure in a row would delay it, not just for a first
		// failure's quick retry.
		{"200", ok(feedURL, atomDoc("a", itemsNewestFirst("e", 1, t0)...)), nil, secondBackoff},
		{"failure", nil, &fetch.StatusError{URL: feedURL, StatusCode: 500}, secondBackoff},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			f := addFeed(t, st, feedURL, t0)
			clock := newClock(t0.Add(time.Hour))
			s := newClockedScheduler(t, failingWrites{st}, staticFetcher(tt.res, tt.err), clock)
			if hold := s.poll(t.Context(), f); !hold.Equal(clock.Now().Add(tt.want)) {
				t.Errorf("hold until %s, want +%s", hold, tt.want)
			}
		})
	}
}

// TestPoll_UnstorableOutcomeIsNeverRefetchedWithinAnInterval: while the
// database cannot be written, error_count stays where it was and the feed
// stays due, so every poll looks like the first failure again. Polling it
// each time its hold expires must still never fetch it sooner than one
// interval after the previous fetch, for either kind's default interval and
// whatever the server answers: a quick first retry repeated for as long as
// the disk stays full would hammer the feed's server.
func TestPoll_UnstorableOutcomeIsNeverRefetchedWithinAnInterval(t *testing.T) {
	const ytURL = "https://www.youtube.com/feeds/videos.xml?channel_id=UCtestchannel0000000000"
	kinds := []struct {
		kind     store.Kind
		url      string
		interval time.Duration
	}{
		{store.KindRSS, feedURL, config.DefaultPollInterval},
		{store.KindYouTube, ytURL, config.DefaultYouTubePollInterval},
	}
	for _, k := range kinds {
		answers := []struct {
			name string
			res  *fetch.Result
			err  error
		}{
			{"304", notModified(k.url), nil},
			{"200 with new entries", ok(k.url, atomDoc("a", itemsNewestFirst("e", 1, t0)...)), nil},
			{"500", nil, &fetch.StatusError{URL: k.url, StatusCode: 500}},
		}
		for _, a := range answers {
			t.Run(string(k.kind)+"/"+a.name, func(t *testing.T) {
				st := openStore(t)
				f := addFeedOfKind(t, st, k.kind, k.url, t0)
				clock := newClock(t0)
				fetcher := staticFetcher(a.res, a.err)
				s := newClockedScheduler(t, failingWrites{st}, fetcher, clock)
				// Poll for as long as the longest backoff, during which a
				// working feed is fetched at least once.
				for end := t0.Add(config.MaxBackoff); clock.Now().Before(end); {
					hold := s.poll(t.Context(), getFeed(t, st, f.ID))
					if gap := hold.Sub(clock.Now()); gap < k.interval {
						t.Fatalf("fetch %d: held for %s, so the feed would be fetched again within its %s interval",
							fetcher.CallCount(), gap, k.interval)
					}
					clock.Set(hold)
				}
			})
		}
	}
}

func TestPoll_FeedDeletedWhileFetchingIsNotAnError(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	fetcher := newFetcher(func(ctx context.Context, _ fetch.Request) (*fetch.Result, error) {
		if err := st.DeleteFeed(ctx, f.ID); err != nil {
			return nil, err
		}
		return ok(feedURL, atomDoc("a", itemsNewestFirst("e", 1, t0)...)), nil
	})
	s := newClockedScheduler(t, st, fetcher, newClock(t0.Add(time.Hour)))
	if hold := s.poll(t.Context(), f); !hold.IsZero() {
		t.Errorf("deleted feed held until %s", hold)
	}
}

func TestFailureMessage_IsOneShortLine(t *testing.T) {
	long := errors.New("parse rss feed: XML syntax error\non line 3:\t" + strings.Repeat("x", 500))
	msg := failureMessage(long)
	if strings.ContainsAny(msg, "\n\t") {
		t.Errorf("message spans lines: %q", msg)
	}
	if n := len([]rune(msg)); n > maxErrorRunes || !strings.HasSuffix(msg, "…") {
		t.Errorf("message has %d runes (max %d) or lacks an ellipsis: %q", n, maxErrorRunes, msg)
	}
	if got := failureMessage(&fetch.StatusError{URL: feedURL, StatusCode: 404}); got != "HTTP 404 from "+feedURL {
		t.Errorf("short message altered: %q", got)
	}
}

func TestPoll_PermanentRedirectToANonFeedKeepsTheStoredURL(t *testing.T) {
	st := openStore(t)
	const parked = "https://parked.example/"
	f := addFeed(t, st, feedURL, t0, itemsNewestFirst("e", 2, t0)...)
	page := ok(parked, []byte("<!doctype html><html><head><title>For sale</title></head><body>This domain is for sale</body></html>"))
	page.ContentType = "text/html; charset=utf-8"
	page.PermanentURL = parked
	fetcher := staticFetcher(page, nil)
	clock := newClock(t0.Add(time.Hour))
	s := newClockedScheduler(t, st, fetcher, clock)
	pollOnce(t, s, st, f.ID)

	got := getFeed(t, st, f.ID)
	if got.URL != feedURL {
		t.Errorf("feed URL = %q after a redirect to a page that is not a feed, want %q kept", got.URL, feedURL)
	}
	if got.ErrorCount != 1 || !strings.Contains(got.LastError, parse.ErrNotAFeed.Error()) {
		t.Errorf("error_count %d, last_error %q; want 1 and the parse error", got.ErrorCount, got.LastError)
	}
	clock.Set(got.NextFetchAt)
	pollOnce(t, s, st, f.ID)
	if calls := fetcher.Calls(); len(calls) != 2 || calls[1].URL != feedURL {
		t.Errorf("requests = %+v, want the next poll to fetch the stored URL again", calls)
	}
}

func TestPoll_PermanentRedirectAnswered304UpdatesStoredFeedURL(t *testing.T) {
	st := openStore(t)
	const oldURL, newURL = "http://old.example/feed", "https://new.example/feed"
	f := addFeed(t, st, oldURL, t0)
	res := notModified(newURL)
	res.PermanentURL = newURL
	s := newClockedScheduler(t, st, staticFetcher(res, nil), newClock(t0.Add(time.Hour)))
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID); got.URL != newURL || got.ErrorCount != 0 {
		t.Errorf("feed = %q (errors %d), want %q: the new location accepted the stored validators",
			got.URL, got.ErrorCount, newURL)
	}
}

func TestPoll_FeedReplacedWhileFetchingIsLeftUntouched(t *testing.T) {
	moved := ok("https://moved.example/feed", atomDoc("A moved", itemsNewestFirst("a", 3, t0)...))
	moved.PermanentURL = "https://moved.example/feed"
	tests := []struct {
		name string
		res  *fetch.Result
		err  error
	}{
		{"200", ok(feedURL, atomDoc("A", itemsNewestFirst("a", 3, t0)...)), nil},
		{"304", notModified(feedURL), nil},
		{"failure", nil, &fetch.StatusError{URL: feedURL, StatusCode: 503}},
		{"permanent redirect", moved, nil},
	}
	const otherURL = "https://b.example/feed"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			a := addFeed(t, st, feedURL, t0)
			var (
				s     *Scheduler
				b     store.Feed
				clock = newClock(t0.Add(time.Hour))
			)
			fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
				if r.URL == otherURL {
					return ok(otherURL, atomDoc("B", itemsNewestFirst("b", 1, t0)...)), nil
				}
				// While A is being fetched the user unsubscribes it and
				// subscribes to B, which SQLite gives A's freed id.
				mustNoErr(t, st.DeleteFeed(ctx, a.ID))
				var err error
				b, err = s.Subscribe(ctx, Subscription{FeedURL: otherURL, Title: "My B"})
				mustNoErr(t, err)
				clock.Advance(time.Minute) // so that any write by A's poll shows
				return tt.res, tt.err
			})
			s = newClockedScheduler(t, st, fetcher, clock)
			if hold := s.poll(t.Context(), a); !hold.IsZero() {
				t.Errorf("poll of the removed feed asked for a hold until %s", hold)
			}

			got := getFeed(t, st, b.ID)
			if got != b {
				t.Errorf("the stale poll of feed %d changed the new subscription:\n got %+v\nwant %+v", a.ID, got, b)
			}
			if entries := feedEntries(t, st, b.ID); len(entries) != 1 {
				t.Errorf("new subscription has %d entries, want its own 1", len(entries))
			}
		})
	}
}
