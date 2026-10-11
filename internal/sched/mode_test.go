package sched

import (
	"context"
	"errors"
	"net/http"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Shorts modes (#18): a YouTube channel subscription follows all its
// uploads, only its regular videos or only its Shorts; the mode is the feed
// URL (see resolve.Mode).

// The synthetic channel of avatar_test.go, followed in each mode.
var (
	chanVideosFeed = resolve.ModeFeedURL(chanID, resolve.ModeVideos)
	chanShortsFeed = resolve.ModeFeedURL(chanID, resolve.ModeShorts)
)

func TestModeFeedsOfTheTestChannel(t *testing.T) {
	if resolve.ModeFeedURL(chanID, resolve.ModeBoth) != chanFeedURL || chanVideosFeed == "" || chanShortsFeed == "" {
		t.Fatalf("mode feeds of %s: %q, %q", chanID, chanVideosFeed, chanShortsFeed)
	}
}

func TestSetMode_SwitchesTheURLAndFetchesItAtOnceAsANewSubscription(t *testing.T) {
	st := openStore(t)
	clock := newClock(t0)
	f := addFeedOfKind(t, st, store.KindYouTube, chanFeedURL, t0.Add(time.Hour), itemsNewestFirst("v", 2, t0)...)
	fetcher := staticFetcher(nil, errors.New("no request expected"))
	s := newClockedScheduler(t, st, fetcher, clock)
	clock.Advance(time.Minute)

	got, err := s.SetMode(t.Context(), f.ID, resolve.ModeShorts)
	mustNoErr(t, err)
	stored := getFeed(t, st, f.ID)
	if got != stored {
		t.Errorf("SetMode returned %+v; stored %+v", got, stored)
	}
	if stored.URL != chanShortsFeed || !stored.InitialPending || stored.ETag != "" || stored.LastModified != "" ||
		!stored.NextFetchAt.Equal(clock.Now()) || stored.Title != f.Title {
		t.Errorf("after switching to Shorts: %+v", stored)
	}
	if n := len(feedEntries(t, st, f.ID)); n != 2 {
		t.Errorf("%d entries after the switch, want the 2 stored before", n)
	}
	if len(s.wake) != 1 {
		t.Error("SetMode did not wake the dispatcher")
	}
	if n := fetcher.CallCount(); n != 0 {
		t.Errorf("SetMode made %d requests; want none (the dispatcher fetches)", n)
	}

	// The first document of the Shorts feed is an initial add.
	total := 3 * config.InitialUnread
	items := itemsNewestFirst("s", total, t0.Add(-time.Hour))
	fetcher.setRespond(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		if r.URL != chanShortsFeed || r.ETag != "" || r.LastModified != "" {
			t.Errorf("fetched %+v; want the Shorts feed without validators", r)
		}
		return ok(r.URL, atomDoc(chanTitle, items...)), nil
	})
	pollOnce(t, s, st, f.ID)
	if n := len(unreadGUIDs(feedEntries(t, st, f.ID))); n != 2+config.InitialUnread {
		t.Errorf("%d unread after the first fetch; want the 2 kept and the %d newest new ones", n, config.InitialUnread)
	}
	if after := getFeed(t, st, f.ID); after.InitialPending || after.URL != chanShortsFeed {
		t.Errorf("after the first fetch: %+v", after)
	}

	// And back: all uploads again.
	_, err = s.SetMode(t.Context(), f.ID, resolve.ModeBoth)
	mustNoErr(t, err)
	if back := getFeed(t, st, f.ID); back.URL != chanFeedURL || !back.InitialPending {
		t.Errorf("after switching back: %+v", back)
	}
}

func TestSetMode_SameModeChangesNothing(t *testing.T) {
	st := openStore(t)
	// A channel feed stored by an older version in another spelling.
	const spelled = "http://youtube.com/feeds/videos.xml?channel_id=" + chanID
	f := addFeedOfKind(t, st, store.KindYouTube, spelled, t0.Add(time.Hour))
	s := newClockedScheduler(t, st, staticFetcher(nil, nil), newClock(t0))
	got, err := s.SetMode(t.Context(), f.ID, resolve.ModeBoth)
	mustNoErr(t, err)
	if got != f || getFeed(t, st, f.ID) != f {
		t.Errorf("SetMode to the current mode changed the feed: %+v, want %+v", getFeed(t, st, f.ID), f)
	}
	if len(s.wake) != 0 {
		t.Error("SetMode to the current mode woke the dispatcher")
	}
}

func TestSetMode_Refusals(t *testing.T) {
	st := openStore(t)
	ch := addFeedOfKind(t, st, store.KindYouTube, chanFeedURL, t0.Add(time.Hour))
	other := addFeedOfKind(t, st, store.KindYouTube, chanVideosFeed, t0.Add(time.Hour))
	rss := addFeed(t, st, feedURL, t0.Add(time.Hour))
	pl := addFeedOfKind(t, st, store.KindYouTube, playlistFeed, t0.Add(time.Hour))
	s := newClockedScheduler(t, st, staticFetcher(nil, nil), newClock(t0))
	for _, tc := range []struct {
		name string
		id   int64
		mode resolve.Mode
		want error
	}{
		{"mode already subscribed as another feed", ch.ID, resolve.ModeVideos, store.ErrConflict},
		{"website feed", rss.ID, resolve.ModeVideos, store.ErrInvalid},
		{"other playlist", pl.ID, resolve.ModeShorts, store.ErrInvalid},
		{"unknown mode", ch.ID, "long", store.ErrInvalid},
		{"empty mode", ch.ID, "", store.ErrInvalid},
		{"missing feed", pl.ID + 100, resolve.ModeShorts, store.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.SetMode(t.Context(), tc.id, tc.mode); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	for _, f := range []store.Feed{ch, other, rss, pl} {
		if got := getFeed(t, st, f.ID); got != f {
			t.Errorf("feed %d changed: %+v, want %+v", f.ID, got, f)
		}
	}
	if len(s.wake) != 0 {
		t.Error("a refused switch woke the dispatcher")
	}
}

// TestSetMode_PollInFlightWritesNothingToTheSwitchedFeed: a fetch of the old
// URL that answers after the switch is not stored as the new URL's.
func TestSetMode_PollInFlightWritesNothingToTheSwitchedFeed(t *testing.T) {
	st := openStore(t)
	clock := newClock(t0)
	f := addFeedOfKind(t, st, store.KindYouTube, chanFeedURL, t0)
	started, release := make(chan struct{}), make(chan struct{})
	fetcher := newFetcher(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		close(started)
		<-release
		return ok(r.URL, atomDoc(chanTitle, itemsNewestFirst("old", 3, t0)...)), nil
	})
	s := newClockedScheduler(t, st, fetcher, clock)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.poll(context.Background(), f)
	}()
	<-started
	_, err := s.SetMode(t.Context(), f.ID, resolve.ModeVideos)
	mustNoErr(t, err)
	close(release)
	<-done

	got := getFeed(t, st, f.ID)
	if got.URL != chanVideosFeed || !got.InitialPending || got.ETag != "" || !got.NextFetchAt.Equal(t0) {
		t.Errorf("after the old URL's fetch finished: %+v", got)
	}
	if n := len(feedEntries(t, st, f.ID)); n != 0 {
		t.Errorf("%d entries of the old URL stored in the switched feed", n)
	}
}

// TestPoll_ModeFeedFailureNeverFallsBackToTheChannelFeed: a "Videos only"
// or "Shorts only" feed that fails is a failure of that feed, under the
// YouTube outage rule; the channel feed, which carries what the mode leaves
// out, is never fetched instead.
func TestPoll_ModeFeedFailureNeverFallsBackToTheChannelFeed(t *testing.T) {
	for _, u := range []string{chanVideosFeed, chanShortsFeed} {
		t.Run(u, func(t *testing.T) {
			st := openStore(t)
			clock := newClock(t0)
			f := addFeedOfKind(t, st, store.KindYouTube, u, t0, itemsNewestFirst("v", 2, t0)...)
			fetcher := newFetcher(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
				if r.URL == chanFeedURL {
					return chanFeed(t, r.URL), nil
				}
				return nil, statusErr(r.URL, http.StatusNotFound)
			})
			s := newClockedScheduler(t, st, fetcher, clock)
			yt := config.DefaultYouTubePollInterval
			got := pollAtEachDue(t, st, clock, s, fetcher, f.ID,
				statusErr(u, http.StatusNotFound), statusErr(u, http.StatusServiceUnavailable), statusErr(u, http.StatusNotFound))
			if want := []time.Duration{config.FirstRetryDelay, yt, yt}; !slices.Equal(got, want) {
				t.Errorf("next fetches in %v, want %v", got, want)
			}
			after := getFeed(t, st, f.ID)
			if after.URL != u || after.ErrorCount != 3 || len(feedEntries(t, st, f.ID)) != 2 {
				t.Errorf("after 3 failures: %+v", after)
			}
			// The YouTube fallback reads the mode's own tab (fallback_test.go).
			for _, c := range feedRequests(fetcher.Calls()) {
				if c.URL != u {
					t.Errorf("fetched %s; want only the mode feed", c.URL)
				}
			}
		})
	}
}

// TestPoll_ModeFeedGetsTheChannelAvatar: a channel followed in a mode still
// gets its channel's avatar.
func TestPoll_ModeFeedGetsTheChannelAvatar(t *testing.T) {
	st := openStore(t)
	f := addYouTubeFeed(t, st, chanShortsFeed, "")
	fetcher := newFetcher(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		if r.URL == chanPageURL {
			return chanPage(), nil
		}
		return chanFeed(t, r.URL), nil
	})
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	pollOnce(t, s, st, f.ID)
	if got := getFeed(t, st, f.ID); got.IconURL != wantAvatar(t) {
		t.Errorf("icon = %q; want the channel's avatar", got.IconURL)
	}
	if n := pageRequests(fetcher); n != 1 {
		t.Errorf("%d channel page requests, want 1", n)
	}
}

// TestSubscribe_ModeFeedIsNeverPending: only a channel's own feed may be
// stored before it answers. A mode feed answering 404 may be a channel
// without Shorts (or without regular videos), which the channel's page
// cannot tell from an outage, so it is an error like any failed fetch.
func TestSubscribe_ModeFeedIsNeverPending(t *testing.T) {
	for _, u := range []string{chanVideosFeed, chanShortsFeed} {
		t.Run(u, func(t *testing.T) {
			st := openStore(t)
			fetcher := downFetcher(statusErr(chanFeedURL, http.StatusNotFound), namedChanPage())
			s := newClockedScheduler(t, st, fetcher, newClock(t0))
			if p, err := s.Preview(t.Context(), u); err == nil || p != nil {
				t.Errorf("Preview = %+v, %v; want the fetch error", p, err)
			}
			_, err := s.Subscribe(t.Context(), Subscription{FeedURL: u, AllowPending: true})
			var se *fetch.StatusError
			if !errors.As(err, &se) || se.StatusCode != http.StatusNotFound {
				t.Errorf("Subscribe err = %v; want the 404", err)
			}
			feeds, lerr := st.ListFeeds(t.Context())
			mustNoErr(t, lerr)
			if len(feeds) != 0 {
				t.Errorf("stored %+v", feeds)
			}
			for _, c := range fetcher.Calls() {
				if strings.Contains(c.URL, "/channel/") {
					t.Errorf("requested the channel page %s for a mode feed", c.URL)
				}
			}
		})
	}
}

// TestSubscribe_ModeFeedURL: subscribing in a mode stores that mode's feed
// as a normal subscription, its avatar from the channel's page.
func TestSubscribe_ModeFeedURL(t *testing.T) {
	st := openStore(t)
	fetcher := newFetcher(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		if r.URL == chanPageURL {
			return chanPage(), nil
		}
		return chanFeed(t, r.URL), nil
	})
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanVideosFeed})
	mustNoErr(t, err)
	if feed.URL != chanVideosFeed || feed.Kind != store.KindYouTube || feed.InitialPending || feed.IconURL != wantAvatar(t) {
		t.Errorf("stored %+v", feed)
	}
}

// TestSetMode_KeepsARateLimitWait: while YouTube is rate-limiting the feed
// (its last failure was a 429, or came with a Retry-After), a switch keeps
// the next fetch where the backoff put it instead of fetching at once.
// After any other failure, or none, the new URL is fetched at once.
func TestSetMode_KeepsARateLimitWait(t *testing.T) {
	const wait = 2 * time.Hour
	for _, tc := range []struct {
		name string
		err  error // the failure of the poll before the switch; nil = success
		keep bool
	}{
		{"429", statusErr(chanFeedURL, http.StatusTooManyRequests), true},
		{"429 with Retry-After", &fetch.StatusError{URL: chanFeedURL, StatusCode: http.StatusTooManyRequests, RetryAfter: wait}, true},
		{"503 with Retry-After", &fetch.StatusError{URL: chanFeedURL, StatusCode: http.StatusServiceUnavailable, RetryAfter: wait}, true},
		{"404", statusErr(chanFeedURL, http.StatusNotFound), false},
		{"503", statusErr(chanFeedURL, http.StatusServiceUnavailable), false},
		// The final URL after a redirect is the server's; its text must
		// not pass for a stored 429.
		{"503 from a URL quoting a 429", statusErr(chanFeedURL+"&q=a: HTTP 429 from x", http.StatusServiceUnavailable), false},
		{"network error", errors.New("connection refused"), false},
		{"success", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			clock := newClock(t0)
			f := addFeedOfKind(t, st, store.KindYouTube, chanFeedURL, t0)
			fetcher := staticFetcher(chanFeed(t, chanFeedURL), tc.err)
			if tc.err != nil {
				fetcher = staticFetcher(nil, tc.err)
			}
			s := newClockedScheduler(t, st, fetcher, clock)
			pollOnce(t, s, st, f.ID)
			before := getFeed(t, st, f.ID)
			clock.Advance(time.Minute)
			got, err := s.SetMode(t.Context(), f.ID, resolve.ModeShorts)
			mustNoErr(t, err)
			want := clock.Now()
			if tc.keep {
				want = before.NextFetchAt
				if !want.After(clock.Now()) {
					t.Fatalf("no wait after the rate limit: next fetch %s", want)
				}
			}
			if got.URL != chanShortsFeed || !got.NextFetchAt.Equal(want) {
				t.Errorf("after the switch: %s next at %s; want %s next at %s", got.URL, got.NextFetchAt, chanShortsFeed, want)
			}
		})
	}
}

// TestSetMode_KeepsARateLimitWaitAcrossARestart: a 429 recorded before the
// process restarted still counts (it is read from the stored last error).
func TestSetMode_KeepsARateLimitWaitAcrossARestart(t *testing.T) {
	st := openStore(t)
	clock := newClock(t0)
	f := addFeedOfKind(t, st, store.KindYouTube, chanFeedURL, t0)
	s := newClockedScheduler(t, st, staticFetcher(nil, statusErr(chanFeedURL, http.StatusTooManyRequests)), clock)
	pollOnce(t, s, st, f.ID)
	before := getFeed(t, st, f.ID)
	restarted := newClockedScheduler(t, st, staticFetcher(nil, nil), clock)
	got, err := restarted.SetMode(t.Context(), f.ID, resolve.ModeVideos)
	mustNoErr(t, err)
	if !got.NextFetchAt.Equal(before.NextFetchAt) || !got.NextFetchAt.After(clock.Now()) {
		t.Errorf("next fetch %s after the switch; want the rate limit's %s", got.NextFetchAt, before.NextFetchAt)
	}
}

// TestSetMode_RateLimitOfAnotherFeedDoesNotCount: a rate-limit mark is
// tied to the subscription it was set for.
func TestSetMode_RateLimitOfAnotherFeedDoesNotCount(t *testing.T) {
	st := openStore(t)
	clock := newClock(t0)
	f := addFeedOfKind(t, st, store.KindYouTube, chanFeedURL, t0)
	s := newClockedScheduler(t, st, staticFetcher(nil, &fetch.StatusError{URL: chanFeedURL,
		StatusCode: http.StatusServiceUnavailable, RetryAfter: time.Hour}), clock)
	pollOnce(t, s, st, f.ID)
	// Unsubscribed, and its id given to a new subscription of the channel
	// that failed with a plain 404.
	mustNoErr(t, st.DeleteFeed(t.Context(), f.ID))
	// (Another spelling: within the same second only the URL tells two
	// subscriptions apart, as in writeFeed.)
	g := addFeedOfKind(t, st, store.KindYouTube, "http://youtube.com/feeds/videos.xml?channel_id="+chanID, t0.Add(time.Hour))
	if g.ID != f.ID {
		t.Fatalf("new subscription got id %d, not the deleted %d", g.ID, f.ID)
	}
	mustNoErr(t, st.RecordFetchError(t.Context(), g.ID, "HTTP 404 from upstream", t0, t0.Add(time.Hour)))
	got, err := s.SetMode(t.Context(), g.ID, resolve.ModeVideos)
	mustNoErr(t, err)
	if !got.NextFetchAt.Equal(clock.Now()) {
		t.Errorf("next fetch %s; want now: the rate limit was another subscription's", got.NextFetchAt)
	}
}

// TestSetMode_KeptRateLimitWaitCannotBeShortenedLater: a switch that keeps
// a rate-limit wait also keeps the failure that makes it one, so a shorter
// interval or a digest time set afterwards does not move the fetch before
// the server's Retry-After. A switch that fetches at once starts afresh.
func TestSetMode_KeptRateLimitWaitCannotBeShortenedLater(t *testing.T) {
	const wait = 6 * time.Hour
	for _, tc := range []struct {
		name   string
		change func(t *testing.T, s *Scheduler, st store.Store, f store.Feed)
	}{
		{"shorter interval", func(t *testing.T, s *Scheduler, st store.Store, f store.Feed) {
			mustNoErr(t, st.SetFeedInterval(t.Context(), f.ID, int(config.MinPollInterval/time.Second)))
			mustNoErr(t, s.Reschedule(t.Context(), f.ID))
		}},
		{"digest time", func(t *testing.T, s *Scheduler, st store.Store, f store.Feed) {
			// An hour from now in the server's time zone.
			local := s.now().In(s.loc).Add(time.Hour)
			timedDigest(t, st, "Morning", local.Hour()*minutesPerHour+local.Minute(), store.DigestSources{FeedIDs: []int64{f.ID}})
			mustNoErr(t, s.ScheduleIngest(t.Context(), []int64{f.ID}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			clock := newClock(t0)
			f := addFeedOfKind(t, st, store.KindYouTube, chanFeedURL, t0)
			s := newZonedScheduler(t, st, staticFetcher(nil, &fetch.StatusError{URL: chanFeedURL,
				StatusCode: http.StatusTooManyRequests, RetryAfter: wait}), clock)
			pollOnce(t, s, st, f.ID)
			before := getFeed(t, st, f.ID)
			clock.Advance(time.Minute)
			switched, err := s.SetMode(t.Context(), f.ID, resolve.ModeVideos)
			mustNoErr(t, err)
			if !switched.NextFetchAt.Equal(before.NextFetchAt) || switched.LastError == "" ||
				switched.ErrorCount != before.ErrorCount {
				t.Fatalf("after the switch: %+v; want the wait and the failure of %+v kept", switched, before)
			}
			tc.change(t, s, st, switched)
			if got := getFeed(t, st, f.ID); !got.NextFetchAt.Equal(before.NextFetchAt) {
				t.Errorf("next fetch moved to %s; want the Retry-After's %s", got.NextFetchAt, before.NextFetchAt)
			}
		})
	}

	t.Run("fetched at once", func(t *testing.T) {
		st := openStore(t)
		clock := newClock(t0)
		f := addFeedOfKind(t, st, store.KindYouTube, chanFeedURL, t0)
		s := newClockedScheduler(t, st, staticFetcher(nil, statusErr(chanFeedURL, http.StatusNotFound)), clock)
		pollOnce(t, s, st, f.ID)
		switched, err := s.SetMode(t.Context(), f.ID, resolve.ModeVideos)
		mustNoErr(t, err)
		if !switched.NextFetchAt.Equal(clock.Now()) || switched.LastError != "" || switched.ErrorCount != 0 {
			t.Errorf("after the switch: %+v; want a fresh start, due now", switched)
		}
	})
}

// TestSetMode_FirstFetchAfterASwitchQueuesNoTranscripts: the new list's
// first document is an initial add, which never queues transcripts, even
// with every switch on; the source's own switch survives the switch, and
// later polls queue new videos as usual.
func TestSetMode_FirstFetchAfterASwitchQueuesNoTranscripts(t *testing.T) {
	body, err := os.ReadFile("testdata/youtube.xml")
	mustNoErr(t, err)
	st := openStore(t)
	f := addFeedOfKind(t, st, store.KindYouTube, chanFeedURL, t0)
	mustNoErr(t, st.SetTranscriptsSetting(t.Context(), true, t0))
	mustNoErr(t, st.SetFeedTranscripts(t.Context(), f.ID, true, t0))
	cfg := config.Default()
	cfg.Transcripts = true
	fetcher := staticFetcher(ok(chanShortsFeed, body), nil)
	s := New(st, fetcher, cfg, testLogger(t))
	clock := newClock(t0.Add(time.Hour))
	s.now = clock.Now

	_, err = s.SetMode(t.Context(), f.ID, resolve.ModeShorts)
	mustNoErr(t, err)
	if on, err := st.FeedTranscripts(t.Context(), f.ID); err != nil || !on {
		t.Fatalf("source's transcript switch after the switch = %v, %v; want on", on, err)
	}
	pollOnce(t, s, st, f.ID)
	if n := len(feedEntries(t, st, f.ID)); n == 0 {
		t.Fatal("the first fetch after the switch stored nothing")
	}
	if job, ok, err := st.NextTranscript(t.Context()); ok || err != nil {
		t.Errorf("after the first fetch of the new list: queued %+v, %v; want nothing", job, err)
	}
	if len(s.transcriptWake) != 0 {
		t.Error("the first fetch of the new list woke the transcript worker")
	}

	// A video that arrives later is queued as usual.
	newer := strings.Replace(string(body), "<entry>", `<entry>
  <id>yt:video:vid00000009</id>
  <yt:videoId>vid00000009</yt:videoId>
  <title>Newer short</title>
  <link rel="alternate" href="https://www.youtube.com/shorts/vid00000009"/>
  <published>2026-10-01T10:00:00+00:00</published>
 </entry>
 <entry>`, 1)
	fetcher.setRespond(func(context.Context, fetch.Request) (*fetch.Result, error) {
		return ok(chanShortsFeed, []byte(newer)), nil
	})
	clock.Advance(2 * time.Hour)
	pollOnce(t, s, st, f.ID)
	job, queued, err := st.NextTranscript(t.Context())
	mustNoErr(t, err)
	if !queued || !strings.Contains(job.URL, "vid00000009") {
		t.Errorf("a later new video: queued %+v (%v); want it", job, queued)
	}
}
