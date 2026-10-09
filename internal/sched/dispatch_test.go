package sched

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// countingStore counts the schedule queries, to detect busy loops.
type countingStore struct {
	store.Store
	dueCalls  atomic.Int64
	nextCalls atomic.Int64
}

func (c *countingStore) DueFeeds(ctx context.Context, now time.Time) ([]store.Feed, error) {
	c.dueCalls.Add(1)
	return c.Store.DueFeeds(ctx, now)
}

func (c *countingStore) NextFetchAfter(ctx context.Context, t time.Time) (time.Time, bool, error) {
	c.nextCalls.Add(1)
	return c.Store.NextFetchAfter(ctx, t)
}

// gatedFetcher answers 304 once the gate is opened (or fails when ctx ends).
func gatedFetcher() (*fakeFetcher, func()) {
	gate := make(chan struct{})
	f := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		select {
		case <-gate:
			return notModified(r.URL), nil
		case <-ctx.Done():
			return nil, fmt.Errorf("fetch %s: %w", r.URL, ctx.Err())
		}
	})
	return f, sync.OnceFunc(func() { close(gate) })
}

func TestRun_ZeroFeedsMakesNoHTTPRequestAndDoesNotBusyLoop(t *testing.T) {
	st := &countingStore{Store: openStore(t)}
	body := atomDoc("First", itemsNewestFirst("e", 2, time.Now())...)
	fetcher := staticFetcher(ok(feedURL, body), nil)
	s := newTestScheduler(t, st, fetcher, config.DefaultFetchWorkers)
	startRun(t, s)

	mustNoErr(t, s.RefreshAll(t.Context())) // a wake-up with nothing to fetch
	time.Sleep(300 * time.Millisecond)
	if n := fetcher.CallCount(); n != 0 {
		t.Fatalf("fetches with zero feeds = %d, want 0", n)
	}
	if n := st.dueCalls.Load() + st.nextCalls.Load(); n > 6 {
		t.Errorf("schedule queried %d times in 300ms with zero feeds: busy loop", n)
	}

	// Adding the first feed makes exactly its initial fetch; the next poll
	// is an interval away.
	_, err := s.Subscribe(t.Context(), Subscription{FeedURL: feedURL})
	mustNoErr(t, err)
	time.Sleep(200 * time.Millisecond)
	if n := fetcher.CallCount(); n != 1 {
		t.Errorf("fetches after the first subscription = %d, want 1", n)
	}
}

func TestRun_LogsBothDefaultIntervalsAtStartup(t *testing.T) {
	cfg := config.Default()
	cfg.PollIntervalYouTube = 2 * config.DefaultYouTubePollInterval
	buf := &syncBuffer{}
	s := New(openStore(t), staticFetcher(nil, errors.New("no fetch expected")), cfg, slog.New(slog.NewTextHandler(buf, nil)))
	startRun(t, s)()
	_, line, found := strings.Cut(buf.String(), "scheduler started")
	if !found {
		t.Fatalf("no startup log line in:\n%s", buf.String())
	}
	line, _, _ = strings.Cut(line, "\n")
	for _, want := range []string{
		"interval=" + config.DefaultPollInterval.String(),
		"youtube_interval=" + cfg.PollIntervalYouTube.String(),
	} {
		if !strings.Contains(line, want) {
			t.Errorf("startup log %q lacks %q", line, want)
		}
	}
}

func TestRun_AtMostFetchWorkersConcurrentFetches(t *testing.T) {
	const workers, feeds = 3, 10
	st := openStore(t)
	later := time.Now().Add(time.Hour) // not due: no startup stagger
	for i := range feeds {
		addFeed(t, st, fmt.Sprintf("https://f%d.example/feed", i), later)
	}
	fetcher, release := gatedFetcher()
	defer release()
	s := newTestScheduler(t, st, fetcher, workers)
	startRun(t, s)

	mustNoErr(t, s.RefreshAll(t.Context()))
	waitFor(t, "the pool to fill", func() bool { return fetcher.Active() == workers })
	time.Sleep(100 * time.Millisecond)
	if n := fetcher.CallCount(); n != workers {
		t.Fatalf("started %d fetches with %d due feeds, want %d", n, feeds, workers)
	}
	release()
	waitFor(t, "every feed to be fetched", func() bool { return fetcher.CallCount() == feeds })
	if got := fetcher.MaxActive(); got != workers {
		t.Errorf("max concurrent fetches = %d, want %d", got, workers)
	}
	time.Sleep(100 * time.Millisecond)
	if n := fetcher.CallCount(); n != feeds {
		t.Errorf("fetches = %d after one refresh of %d feeds", n, feeds)
	}
}

func TestRun_AtMostOneInFlightFetchPerFeedEvenWhenRefreshIsSpammed(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, time.Now().Add(time.Hour))
	fetcher, release := gatedFetcher()
	defer release()
	s := newTestScheduler(t, st, fetcher, config.DefaultFetchWorkers)
	startRun(t, s)

	mustNoErr(t, s.Refresh(t.Context(), f.ID))
	waitFor(t, "the fetch to start", func() bool { return fetcher.Active() == 1 })
	// Stored times have one-second precision: a refresh within the second of
	// the one that started the fetch is indistinguishable from it.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			if err := s.Refresh(context.Background(), f.ID); err != nil {
				t.Errorf("Refresh: %v", err)
			}
		})
	}
	wg.Wait()
	time.Sleep(100 * time.Millisecond)
	if n := fetcher.CallCount(); n != 1 {
		t.Fatalf("fetches while one was in flight = %d, want 1", n)
	}

	// The refreshes that arrived during the fetch are not lost: they add up
	// to exactly one more fetch, after the first one is done.
	release()
	waitFor(t, "the refreshed fetch", func() bool { return fetcher.CallCount() == 2 })
	waitFor(t, "its poll to be recorded", func() bool {
		return getFeed(t, st, f.ID).NextFetchAt.After(time.Now().Add(time.Hour))
	})
	time.Sleep(100 * time.Millisecond)
	if n := fetcher.CallCount(); n != 2 {
		t.Errorf("fetches = %d, want 2: the first, then one for the refreshes it absorbed", n)
	}
	if got := fetcher.MaxPerURL(); got != 1 {
		t.Errorf("max concurrent fetches of one feed = %d, want 1", got)
	}
}

func TestRun_StaggersOverdueFeedsOnStartup(t *testing.T) {
	st := openStore(t)
	start := time.Now()
	var feeds []store.Feed
	for i := range 4 { // f0 is the most overdue
		feeds = append(feeds, addFeed(t, st, fmt.Sprintf("https://f%d.example/feed", i),
			start.Add(-time.Duration(4-i)*time.Hour)))
	}
	fetcher := newFetcher(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		return notModified(r.URL), nil
	})
	s := newTestScheduler(t, st, fetcher, config.DefaultFetchWorkers)
	stop := startRun(t, s)

	waitFor(t, "the first fetch", func() bool { return fetcher.CallCount() == 1 })
	time.Sleep(200 * time.Millisecond)
	stop()
	calls := fetcher.Calls()
	if len(calls) != 1 || calls[0].URL != feeds[0].URL {
		t.Fatalf("fetched %v at startup, want only the most overdue feed", calls)
	}

	step := config.StartupStagger / 4
	for i := 1; i < len(feeds); i++ {
		next := getFeed(t, st, feeds[i].ID).NextFetchAt
		lo := start.Truncate(time.Second).Add(time.Duration(i) * step)
		hi := time.Now().Add(time.Duration(i) * step)
		if next.Before(lo) || next.After(hi) {
			t.Errorf("feed %d next fetch = +%s, want ≈ +%s", i, next.Sub(start), time.Duration(i)*step)
		}
	}
}

func TestStagger_SpreadsOverdueFeedsEvenlyInDueOrder(t *testing.T) {
	st := openStore(t)
	// Created out of order; f3 is the most overdue, the last one is not due.
	overdue := []time.Duration{-1 * time.Hour, -3 * time.Hour, -2 * time.Hour, -4 * time.Hour, -30 * time.Minute}
	var feeds []store.Feed
	for i, d := range overdue {
		feeds = append(feeds, addFeed(t, st, fmt.Sprintf("https://f%d.example/feed", i), t0.Add(d)))
	}
	future := addFeed(t, st, "https://future.example/feed", t0.Add(time.Hour))
	s := newClockedScheduler(t, st, staticFetcher(nil, errors.New("no fetch expected")), newClock(t0))
	mustNoErr(t, s.stagger(t.Context()))

	step := config.StartupStagger / 5
	order := []int{3, 1, 2, 0, 4} // by original next_fetch_at
	for slot, i := range order {
		want := t0.Add(time.Duration(slot) * step)
		if got := getFeed(t, st, feeds[i].ID).NextFetchAt; !got.Equal(want) {
			t.Errorf("feed %d: next fetch +%s, want +%s", i, got.Sub(t0), want.Sub(t0))
		}
	}
	if got := getFeed(t, st, future.ID).NextFetchAt; !got.Equal(t0.Add(time.Hour)) {
		t.Errorf("feed not yet due was moved to %s", got)
	}
}

func TestRun_FetchesAFeedWhenItFallsDue(t *testing.T) {
	st := openStore(t)
	// Due in 0.5–1.5 s (the store keeps whole seconds): not due at startup.
	f := addFeed(t, st, feedURL, time.Now().Add(1500*time.Millisecond))
	fetcher := staticFetcher(notModified(feedURL), nil)
	s := newTestScheduler(t, st, fetcher, config.DefaultFetchWorkers)
	startRun(t, s)

	time.Sleep(100 * time.Millisecond)
	if n := fetcher.CallCount(); n != 0 {
		t.Fatalf("fetched %d times before the feed was due", n)
	}
	waitFor(t, "the timed fetch", func() bool { return fetcher.CallCount() == 1 })
	waitFor(t, "the poll to be recorded", func() bool { return !getFeed(t, st, f.ID).LastFetchedAt.Equal(t0) })
}

func TestRun_ExitsPromptlyOnCancelWithoutRecordingAnError(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, time.Now().Add(-time.Minute))
	fetcher, release := gatedFetcher() // never released: blocks until ctx ends
	defer release()
	s := newTestScheduler(t, st, fetcher, config.DefaultFetchWorkers)
	stop := startRun(t, s)

	waitFor(t, "the fetch to start", func() bool { return fetcher.Active() == 1 })
	if took := stop(); took > 2*time.Second {
		t.Errorf("Run took %s to return after cancel", took)
	}
	if n := fetcher.Active(); n != 0 {
		t.Errorf("%d fetches still running after Run returned", n)
	}
	got := getFeed(t, st, f.ID)
	if got.ErrorCount != 0 || got.LastError != "" {
		t.Errorf("shutdown recorded as a failure: %d %q", got.ErrorCount, got.LastError)
	}
}

func TestRun_UnstorableOutcomeIsNotRefetchedInATightLoop(t *testing.T) {
	st := openStore(t)
	addFeed(t, st, feedURL, time.Now().Add(-time.Minute))
	fetcher := staticFetcher(nil, &fetch.StatusError{URL: feedURL, StatusCode: 503})
	s := newTestScheduler(t, failingWrites{st}, fetcher, config.DefaultFetchWorkers)
	startRun(t, s)

	waitFor(t, "the first fetch", func() bool { return fetcher.CallCount() == 1 })
	time.Sleep(300 * time.Millisecond)
	if n := fetcher.CallCount(); n != 1 {
		t.Errorf("fetches = %d although the feed's backoff could not be stored, want 1", n)
	}
}

func TestRun_SecondConcurrentRunIsRejected(t *testing.T) {
	s := newTestScheduler(t, openStore(t), staticFetcher(nil, nil), config.DefaultFetchWorkers)
	startRun(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := s.Run(ctx); !errors.Is(err, errAlreadyRunning) {
		t.Errorf("second Run: err = %v, want errAlreadyRunning", err)
	}
}

func TestRefresh_UnknownFeedIsNotFound(t *testing.T) {
	s := newTestScheduler(t, openStore(t), staticFetcher(nil, nil), config.DefaultFetchWorkers)
	if err := s.Refresh(t.Context(), 42); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", err)
	}
	if len(s.wake) != 0 {
		t.Error("a failed refresh woke the dispatcher")
	}
}

func TestRefresh_SchedulesTheFeedNow(t *testing.T) {
	st := openStore(t)
	a := addFeed(t, st, "https://a.example/feed", t0.Add(time.Hour))
	b := addFeed(t, st, "https://b.example/feed", t0.Add(2*time.Hour))
	clock := newClock(t0)
	s := newClockedScheduler(t, st, staticFetcher(nil, nil), clock)

	mustNoErr(t, s.Refresh(t.Context(), a.ID))
	if got := getFeed(t, st, a.ID).NextFetchAt; !got.Equal(t0) {
		t.Errorf("refreshed feed next fetch = %s, want now", got)
	}
	if got := getFeed(t, st, b.ID).NextFetchAt; !got.Equal(t0.Add(2 * time.Hour)) {
		t.Errorf("other feed moved to %s", got)
	}
	if len(s.wake) != 1 {
		t.Error("Refresh did not wake the dispatcher")
	}
	mustNoErr(t, s.RefreshAll(t.Context()))
	if got := getFeed(t, st, b.ID).NextFetchAt; !got.Equal(t0) {
		t.Errorf("RefreshAll: next fetch = %s, want now", got)
	}
}

// flakyWrites fails the first RecordFetchError, as during a transient lock.
type flakyWrites struct {
	store.Store
	failed atomic.Bool
}

func (f *flakyWrites) RecordFetchError(ctx context.Context, id int64, msg string, fetchedAt, next time.Time) error {
	if f.failed.CompareAndSwap(false, true) {
		return errDiskFull
	}
	return f.Store.RecordFetchError(ctx, id, msg, fetchedAt, next)
}

func TestRun_RefreshLiftsTheHoldOfAFeedWhoseOutcomeWasNotStored(t *testing.T) {
	for _, all := range []bool{false, true} {
		name := map[bool]string{false: "Refresh", true: "RefreshAll"}[all]
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			f := addFeed(t, st, feedURL, time.Now().Add(-time.Minute))
			fetcher := staticFetcher(nil, &fetch.StatusError{URL: feedURL, StatusCode: 500})
			s := newTestScheduler(t, &flakyWrites{Store: st}, fetcher, config.DefaultFetchWorkers)
			startRun(t, s)

			waitFor(t, "the first fetch", func() bool { return fetcher.CallCount() == 1 })
			time.Sleep(100 * time.Millisecond) // its outcome is not stored: the feed is held
			if fetcher.CallCount() != 1 {
				t.Fatalf("fetches = %d before the refresh, want 1", fetcher.CallCount())
			}
			if all {
				mustNoErr(t, s.RefreshAll(t.Context()))
			} else {
				mustNoErr(t, s.Refresh(t.Context(), f.ID))
			}
			waitFor(t, "the refreshed fetch", func() bool { return fetcher.CallCount() == 2 })
			waitFor(t, "its outcome to be stored", func() bool { return getFeed(t, st, f.ID).ErrorCount == 1 })
			time.Sleep(100 * time.Millisecond)
			if n := fetcher.CallCount(); n != 2 {
				t.Errorf("fetches = %d, want 2: one per refresh, then the stored backoff", n)
			}
		})
	}
}

func TestStagger_ReschedulesFeedsScheduledWhileTheClockWasAhead(t *testing.T) {
	st := openStore(t)
	ahead := addFeed(t, st, "https://ahead.example/feed", t0.Add(365*24*time.Hour))
	atCap := addFeed(t, st, "https://cap.example/feed", t0.Add(config.MaxBackoff))
	long := addFeed(t, st, "https://long.example/feed", t0.Add(3*config.MaxBackoff))
	mustNoErr(t, st.SetFeedInterval(t.Context(), long.ID, int(4*config.MaxBackoff/time.Second)))
	s := newClockedScheduler(t, st, staticFetcher(nil, errors.New("no fetch expected")), newClock(t0))
	mustNoErr(t, s.stagger(t.Context()))

	if got := getFeed(t, st, ahead.ID).NextFetchAt; !got.Equal(t0) {
		t.Errorf("feed scheduled a year ahead: next fetch %s, want now", got)
	}
	if got := getFeed(t, st, atCap.ID).NextFetchAt; !got.Equal(t0.Add(config.MaxBackoff)) {
		t.Errorf("feed at the longest backoff was moved to %s", got)
	}
	if got := getFeed(t, st, long.ID).NextFetchAt; !got.Equal(t0.Add(3 * config.MaxBackoff)) {
		t.Errorf("feed within its own long interval was moved to %s", got)
	}
}

// TestStagger_KeepsAScheduleMadeWithTheOtherKindsDefault: YouTube feeds
// were scheduled with FR_POLL_INTERVAL before they had their own default, so
// a schedule within the longer of the two defaults is not clock skew.
func TestStagger_KeepsAScheduleMadeWithTheOtherKindsDefault(t *testing.T) {
	st := openStore(t)
	cfg := config.Default()
	cfg.PollInterval = 2 * config.MaxBackoff
	addYouTube := func(u string, next time.Time) store.Feed {
		f, err := st.CreateFeed(t.Context(), store.NewFeed{Kind: store.KindYouTube, URL: u, Title: u,
			FetchedAt: t0.Add(-time.Hour), NextFetchAt: next}, nil, config.InitialUnread)
		mustNoErr(t, err)
		return f
	}
	kept := addYouTube("https://www.youtube.com/feeds/videos.xml?channel_id=UCkept", t0.Add(cfg.PollInterval-time.Hour))
	ahead := addYouTube("https://www.youtube.com/feeds/videos.xml?channel_id=UCahead", t0.Add(365*24*time.Hour))
	s := New(st, staticFetcher(nil, errors.New("no fetch expected")), cfg, testLogger(t))
	s.now = newClock(t0).Now
	mustNoErr(t, s.stagger(t.Context()))

	if got := getFeed(t, st, kept.ID).NextFetchAt; !got.Equal(kept.NextFetchAt) {
		t.Errorf("YouTube feed within the blog default was moved from %s to %s", kept.NextFetchAt, got)
	}
	if got := getFeed(t, st, ahead.ID).NextFetchAt; !got.Equal(t0) {
		t.Errorf("YouTube feed scheduled a year ahead: next fetch %s, want now", got)
	}
}

func TestRun_ClockSetBackReschedulesFeedsPolledWhileItWasAhead(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0.Add(-time.Minute))
	fetcher := staticFetcher(notModified(feedURL), nil)
	clock := newClock(t0.Add(365 * 24 * time.Hour)) // a year ahead
	s := newClockedScheduler(t, st, fetcher, clock)
	startRun(t, s)
	waitFor(t, "the poll while the clock is ahead", func() bool {
		return getFeed(t, st, f.ID).LastFetchedAt.Equal(clock.Now())
	})

	clock.Set(t0) // the clock is corrected
	s.signal()
	waitFor(t, "the feed to be polled again", func() bool { return fetcher.CallCount() == 2 })
	waitFor(t, "the new schedule", func() bool {
		return getFeed(t, st, f.ID).NextFetchAt.Equal(t0.Add(config.DefaultPollInterval))
	})
}
