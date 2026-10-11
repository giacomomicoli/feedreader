package sched

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// ytFeedURL is a YouTube channel feed (synthetic channel id).
const ytFeedURL = "https://www.youtube.com/feeds/videos.xml?channel_id=UCtestchannel0000000000a"

// statusErr is the error the fetcher returns for an HTTP error status.
func statusErr(u string, code int) error { return &fetch.StatusError{URL: u, StatusCode: code} }

// TestFailureDelay_CappedAtTheInterval: a YouTube feed whose feed endpoint
// answers 404 or 5xx is retried after config.FirstRetryDelay, then once
// every interval, never doubling; Retry-After stays a lower bound.
func TestFailureDelay_CappedAtTheInterval(t *testing.T) {
	yt := config.DefaultYouTubePollInterval
	tests := []struct {
		name       string
		interval   time.Duration
		errorCount int
		retryAfter time.Duration
		want       time.Duration
	}{
		{"first failure retries soon", yt, 1, 0, config.FirstRetryDelay},
		{"second failure waits one interval", yt, 2, 0, yt},
		{"third failure waits one interval", yt, 3, 0, yt},
		{"sixth failure still waits one interval", yt, 6, 0, yt},
		{"huge error count", yt, 1 << 30, 0, yt},
		{"no failure yet is one interval", yt, 0, 0, yt},
		{"per-feed override is the cap", 3 * yt, 4, 0, 3 * yt},
		{"interval shorter than the first retry", config.MinPollInterval, 1, 0, config.MinPollInterval},
		{"interval shorter than the first retry, later", config.MinPollInterval, 5, 0, config.MinPollInterval},
		{"Retry-After longer than the interval wins", yt, 3, 2 * yt, 2 * yt},
		{"Retry-After shorter than the interval is a lower bound only", yt, 3, yt / 2, yt},
		{"Retry-After is capped", yt, 3, 3 * maxBackoff, maxBackoff},
		{"Retry-After on a first failure", yt, 1, 2 * config.FirstRetryDelay, 2 * config.FirstRetryDelay},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := failureDelay(tt.interval, tt.errorCount, tt.retryAfter, true); got != tt.want {
				t.Errorf("failureDelay(%s, %d, %s, capped) = %s, want %s", tt.interval, tt.errorCount, tt.retryAfter, got, tt.want)
			}
		})
	}
}

// TestCappedFailure_OnlyYouTubeFeedEndpointErrors: the cap applies to a
// YouTube feed answering 404 or 5xx, however the error is wrapped; rate
// limits, other statuses, network and parse failures, and every other feed
// back off as before.
func TestCappedFailure_OnlyYouTubeFeedEndpointErrors(t *testing.T) {
	wrapped := fmt.Errorf("moved permanently to x: %w", statusErr(ytFeedURL, http.StatusNotFound))
	tests := []struct {
		name string
		kind store.Kind
		err  error
		want bool
	}{
		{"404", store.KindYouTube, statusErr(ytFeedURL, http.StatusNotFound), true},
		{"500", store.KindYouTube, statusErr(ytFeedURL, http.StatusInternalServerError), true},
		{"502", store.KindYouTube, statusErr(ytFeedURL, http.StatusBadGateway), true},
		{"503", store.KindYouTube, statusErr(ytFeedURL, http.StatusServiceUnavailable), true},
		{"599", store.KindYouTube, statusErr(ytFeedURL, 599), true},
		{"wrapped 404", store.KindYouTube, wrapped, true},
		{"429", store.KindYouTube, statusErr(ytFeedURL, http.StatusTooManyRequests), false},
		{"403", store.KindYouTube, statusErr(ytFeedURL, http.StatusForbidden), false},
		{"410", store.KindYouTube, statusErr(ytFeedURL, http.StatusGone), false},
		{"400", store.KindYouTube, statusErr(ytFeedURL, http.StatusBadRequest), false},
		{"600", store.KindYouTube, statusErr(ytFeedURL, 600), false},
		{"network", store.KindYouTube, errors.New("dial tcp: connection refused"), false},
		{"timeout", store.KindYouTube, context.DeadlineExceeded, false},
		{"too large", store.KindYouTube, fetch.ErrTooLarge, false},
		{"redirect loop", store.KindYouTube, fetch.ErrRedirects, false},
		{"RSS 404", store.KindRSS, statusErr(feedURL, http.StatusNotFound), false},
		{"RSS 503", store.KindRSS, statusErr(feedURL, http.StatusServiceUnavailable), false},
		{"nil", store.KindYouTube, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cappedFailure(store.Feed{Kind: tt.kind}, tt.err); got != tt.want {
				t.Errorf("cappedFailure(%s, %v) = %v, want %v", tt.kind, tt.err, got, tt.want)
			}
		})
	}
}

// pollAtEachDue polls f at each of its stored next fetches, answering with
// errs in turn, and returns the delays it was scheduled with.
func pollAtEachDue(t *testing.T, st store.Store, clock *testClock, s *Scheduler, fetcher *fakeFetcher, id int64, errs ...error) []time.Duration {
	t.Helper()
	var out []time.Duration
	for _, err := range errs {
		fetcher.setRespond(func(context.Context, fetch.Request) (*fetch.Result, error) { return nil, err })
		clock.Set(getFeed(t, st, id).NextFetchAt)
		pollOnce(t, s, st, id)
		out = append(out, getFeed(t, st, id).NextFetchAt.Sub(clock.Now()))
	}
	return out
}

func TestPoll_YouTubeOutageIsRetriedEveryIntervalWithoutDoubling(t *testing.T) {
	st := openStore(t)
	f := addFeedOfKind(t, st, store.KindYouTube, ytFeedURL, t0, itemsNewestFirst("v", 2, t0)...)
	clock := newClock(t0)
	fetcher := staticFetcher(nil, nil)
	s := newClockedScheduler(t, st, fetcher, clock)

	yt := config.DefaultYouTubePollInterval
	got := pollAtEachDue(t, st, clock, s, fetcher, f.ID,
		statusErr(ytFeedURL, 404), statusErr(ytFeedURL, 404), statusErr(ytFeedURL, 500),
		statusErr(ytFeedURL, 503), statusErr(ytFeedURL, 404))
	want := []time.Duration{config.FirstRetryDelay, yt, yt, yt, yt}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("after failure %d: next fetch in %s, want %s", i+1, got[i], want[i])
		}
	}
	if n := getFeed(t, st, f.ID).ErrorCount; n != len(want) {
		t.Errorf("error_count = %d, want %d: every scheduled failure still counts", n, len(want))
	}
	if len(feedEntries(t, st, f.ID)) != 2 {
		t.Error("failures touched the entries")
	}
}

// TestPoll_YouTubeRateLimitsAndNetworkFailuresStillBackOff: only the feed
// endpoint's own 404/5xx are capped; the latest failure decides.
func TestPoll_YouTubeRateLimitsAndNetworkFailuresStillBackOff(t *testing.T) {
	yt := config.DefaultYouTubePollInterval
	network := errors.New("dial tcp: connection refused")
	tests := []struct {
		name string
		errs []error
		want []time.Duration
	}{
		{"429", []error{statusErr(ytFeedURL, 429), statusErr(ytFeedURL, 429), statusErr(ytFeedURL, 429)},
			[]time.Duration{config.FirstRetryDelay, 2 * yt, 4 * yt}},
		{"network", []error{network, network, network}, []time.Duration{config.FirstRetryDelay, 2 * yt, 4 * yt}},
		{"a network failure in a 404 streak backs off", []error{statusErr(ytFeedURL, 404), statusErr(ytFeedURL, 404), network},
			[]time.Duration{config.FirstRetryDelay, yt, 4 * yt}},
		{"a 404 after network failures is capped again", []error{network, network, statusErr(ytFeedURL, 404)},
			[]time.Duration{config.FirstRetryDelay, 2 * yt, yt}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			f := addFeedOfKind(t, st, store.KindYouTube, ytFeedURL, t0)
			clock := newClock(t0)
			fetcher := staticFetcher(nil, nil)
			s := newClockedScheduler(t, st, fetcher, clock)
			got := pollAtEachDue(t, st, clock, s, fetcher, f.ID, tt.errs...)
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("after failure %d: next fetch in %s, want %s", i+1, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestPoll_RSSFeedsStillBackOffOn404And5xx(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	clock := newClock(t0)
	fetcher := staticFetcher(nil, nil)
	s := newClockedScheduler(t, st, fetcher, clock)
	got := pollAtEachDue(t, st, clock, s, fetcher, f.ID, statusErr(feedURL, 404), statusErr(feedURL, 503))
	if want := []time.Duration{firstBackoff, secondBackoff}; got[0] != want[0] || got[1] != want[1] {
		t.Errorf("RSS delays = %v, want %v", got, want)
	}
}

func TestPoll_YouTubePerFeedIntervalIsTheCap(t *testing.T) {
	st := openStore(t)
	f := addFeedOfKind(t, st, store.KindYouTube, ytFeedURL, t0)
	override := 3 * config.DefaultYouTubePollInterval
	mustNoErr(t, st.SetFeedInterval(t.Context(), f.ID, int(override/time.Second)))
	clock := newClock(t0)
	fetcher := staticFetcher(nil, nil)
	s := newClockedScheduler(t, st, fetcher, clock)
	got := pollAtEachDue(t, st, clock, s, fetcher, f.ID, statusErr(ytFeedURL, 404), statusErr(ytFeedURL, 404), statusErr(ytFeedURL, 404))
	if want := []time.Duration{config.FirstRetryDelay, override, override}; got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Errorf("delays = %v, want %v", got, want)
	}
}

// --- manual refresh (#19) ---

// manualPoll polls f as a refresh asked before it, whose next fetch was then
// prevNext, would.
func manualPoll(t *testing.T, s *Scheduler, st store.Store, id int64, prevNext time.Time) time.Time {
	t.Helper()
	f := getFeed(t, st, id)
	return s.pollWith(t.Context(), f, &manualRefresh{prevNext: prevNext})
}

func TestPoll_ManualFailureKeepsTheErrorCountAndNeverDelaysTheNextFetch(t *testing.T) {
	yt := config.DefaultYouTubePollInterval
	blog := config.DefaultPollInterval
	tests := []struct {
		name     string
		kind     store.Kind
		url      string
		errCount int
		err      error
		prevNext time.Duration // from now; <= 0 = the feed was already due
		want     time.Duration // next fetch from now
	}{
		{"healthy feed: quick retry", store.KindRSS, feedURL, 0, statusErr(feedURL, 404), blog, firstBackoff},
		{"feed in backoff keeps its earlier next fetch", store.KindRSS, feedURL, 3, statusErr(feedURL, 503), time.Hour, time.Hour},
		{"due feed backs off as its failures say", store.KindRSS, feedURL, 2, statusErr(feedURL, 503), 0, secondBackoff},
		{"YouTube outage: one interval at most", store.KindYouTube, ytFeedURL, 4, statusErr(ytFeedURL, 404), 2 * yt, yt},
		{"YouTube outage: an earlier next fetch is kept", store.KindYouTube, ytFeedURL, 4, statusErr(ytFeedURL, 404), yt / 2, yt / 2},
		{"Retry-After wins over the earlier next fetch", store.KindRSS, feedURL, 1,
			&fetch.StatusError{URL: feedURL, StatusCode: 429, RetryAfter: 2 * time.Hour}, time.Minute, 2 * time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			f := addFeedOfKind(t, st, tt.kind, tt.url, t0, itemsNewestFirst("e", 2, t0)...)
			for range tt.errCount {
				mustNoErr(t, st.RecordFetchError(t.Context(), f.ID, "earlier failure", t0, t0))
			}
			clock := newClock(t0.Add(time.Hour))
			s := newClockedScheduler(t, st, staticFetcher(nil, tt.err), clock)
			var prev time.Time
			if tt.prevNext > 0 {
				prev = clock.Now().Add(tt.prevNext)
			}
			if hold := manualPoll(t, s, st, f.ID, prev); !hold.IsZero() {
				t.Fatalf("held until %s", hold)
			}
			got := getFeed(t, st, f.ID)
			if got.ErrorCount != tt.errCount {
				t.Errorf("error_count = %d, want %d (a manual refresh does not count)", got.ErrorCount, tt.errCount)
			}
			if got.LastError == "earlier failure" || got.LastError == "" || !got.LastFetchedAt.Equal(clock.Now()) {
				t.Errorf("last error %q at %s, want this failure at %s", got.LastError, got.LastFetchedAt, clock.Now())
			}
			if d := got.NextFetchAt.Sub(clock.Now()); d != tt.want {
				t.Errorf("next fetch in %s, want %s", d, tt.want)
			}
			if len(feedEntries(t, st, f.ID)) != 2 || got.ETag != `"v1"` {
				t.Error("a manual failure touched entries or validators")
			}
		})
	}
}

// TestPoll_ManualRefreshSpamOnADeadFeedLeavesTheCountAlone: five refreshes
// of a feed that answers 404 count no failure; the scheduled polls still do.
func TestPoll_ManualRefreshSpamOnADeadFeedLeavesTheCountAlone(t *testing.T) {
	st := openStore(t)
	f := addFeedOfKind(t, st, store.KindYouTube, ytFeedURL, t0)
	clock := newClock(t0)
	fetcher := staticFetcher(nil, statusErr(ytFeedURL, 404))
	s := newClockedScheduler(t, st, fetcher, clock)
	pollOnce(t, s, st, f.ID) // a scheduled failure
	scheduled := getFeed(t, st, f.ID).NextFetchAt
	for range 5 {
		clock.Advance(time.Minute)
		manualPoll(t, s, st, f.ID, getFeed(t, st, f.ID).NextFetchAt)
	}
	got := getFeed(t, st, f.ID)
	if got.ErrorCount != 1 {
		t.Errorf("error_count = %d after one scheduled failure and five refreshes, want 1", got.ErrorCount)
	}
	if got.NextFetchAt.After(scheduled) {
		t.Errorf("next fetch %s, after the scheduled retry %s", got.NextFetchAt, scheduled)
	}
	clock.Set(got.NextFetchAt)
	pollOnce(t, s, st, f.ID)
	if n := getFeed(t, st, f.ID).ErrorCount; n != 2 {
		t.Errorf("error_count = %d after the next scheduled failure, want 2", n)
	}
}

func TestPoll_ManualSuccessIsANormalSuccess(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	mustNoErr(t, st.RecordFetchError(t.Context(), f.ID, "HTTP 503", t0, t0))
	clock := newClock(t0.Add(time.Hour))
	s := newClockedScheduler(t, st, staticFetcher(notModified(feedURL), nil), clock)
	manualPoll(t, s, st, f.ID, clock.Now().Add(time.Hour))
	got := getFeed(t, st, f.ID)
	if got.ErrorCount != 0 || got.LastError != "" || got.NextFetchAt.Sub(clock.Now()) != config.DefaultPollInterval {
		t.Errorf("after a manual success: %+v", got)
	}
}

func TestPoll_ManualFailureIsLoggedAsManual(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	s := newClockedScheduler(t, st, staticFetcher(nil, statusErr(feedURL, 404)), newClock(t0.Add(time.Hour)))
	logs := captureLog(t, s)
	manualPoll(t, s, st, f.ID, time.Time{})
	recs := logs.withMsg("feed fetch failed")
	if len(recs) != 1 {
		t.Fatalf("warnings = %d, want 1:\n%s", len(recs), logs)
	}
	if !strings.Contains(logs.String(), `"manual":true`) || !strings.Contains(logs.String(), `"consecutive_failures":0`) {
		t.Errorf("warning does not say the refresh was manual and uncounted:\n%s", logs)
	}
}

// TestReschedule_KeepsTheRetryAfterOfAFailedManualFetch: a manual failure
// leaves error_count at 0, but the feed may be waiting out a Retry-After, so
// a changed interval must not pull its next fetch earlier.
func TestReschedule_KeepsTheRetryAfterOfAFailedManualFetch(t *testing.T) {
	ctx := t.Context()
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	clock := newClock(t0.Add(time.Hour))
	wait := 2 * config.DefaultPollInterval
	s := newClockedScheduler(t, st, staticFetcher(nil, &fetch.StatusError{URL: feedURL, StatusCode: 429, RetryAfter: wait}), clock)
	manualPoll(t, s, st, f.ID, time.Time{})
	got := getFeed(t, st, f.ID)
	if got.ErrorCount != 0 || got.NextFetchAt.Sub(clock.Now()) != wait {
		t.Fatalf("after a rate-limited refresh: %+v", got)
	}
	mustNoErr(t, st.SetFeedInterval(ctx, f.ID, int(config.MinPollInterval/time.Second)))
	mustNoErr(t, s.Reschedule(ctx, f.ID))
	if next := getFeed(t, st, f.ID).NextFetchAt; !next.Equal(got.NextFetchAt) {
		t.Errorf("Reschedule moved the next fetch from %s to %s, inside the Retry-After", got.NextFetchAt, next)
	}
}

// --- manual marks ---

func TestRefresh_MarksTheNextPollOfTheFeedAsManualOnce(t *testing.T) {
	st := openStore(t)
	prev := t0.Add(3 * time.Hour)
	f := addFeed(t, st, feedURL, prev)
	clock := newClock(t0)
	s := newClockedScheduler(t, st, staticFetcher(nil, nil), clock)
	mustNoErr(t, s.Refresh(t.Context(), f.ID))
	mustNoErr(t, s.Refresh(t.Context(), f.ID)) // the first click's schedule is kept
	m := s.takeManual(getFeed(t, st, f.ID))
	if m == nil || !m.prevNext.Equal(prev) {
		t.Fatalf("mark = %+v, want one with the next fetch before the clicks %s", m, prev)
	}
	if m := s.takeManual(getFeed(t, st, f.ID)); m != nil {
		t.Errorf("mark taken twice: %+v", m)
	}
}

// TestManualMark_ReArmedDuringItsFetchKeepsTheFirstSchedule: a click while
// the marked fetch runs re-arms the mark for the following fetch with the
// schedule from before the first click; finishing a fetch drops only a mark
// nobody re-armed.
func TestManualMark_ReArmedDuringItsFetchKeepsTheFirstSchedule(t *testing.T) {
	st := openStore(t)
	prev := t0.Add(time.Hour)
	f := addFeed(t, st, feedURL, prev)
	s := newClockedScheduler(t, st, staticFetcher(nil, nil), newClock(t0))
	mustNoErr(t, s.Refresh(t.Context(), f.ID))
	if m := s.takeManual(getFeed(t, st, f.ID)); m == nil || !m.prevNext.Equal(prev) {
		t.Fatalf("first take = %+v", m)
	}
	mustNoErr(t, s.Refresh(t.Context(), f.ID)) // during the fetch
	s.finishManual(f.ID)                       // the first fetch is done
	m := s.takeManual(getFeed(t, st, f.ID))
	if m == nil || !m.prevNext.Equal(prev) {
		t.Fatalf("re-armed take = %+v, want the schedule from before the first click %s", m, prev)
	}
	s.finishManual(f.ID)
	if n := s.manualCount(); n != 0 {
		t.Errorf("marks after both fetches = %d, want 0", n)
	}
}

// TestManualMark_UndoTouchesOnlyItsOwnMark: a late undo of a click whose
// mark has since been taken, dropped and replaced by an identical one (same
// feed, same clock reading) leaves the new mark alone.
func TestManualMark_UndoTouchesOnlyItsOwnMark(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0.Add(time.Hour))
	s := newClockedScheduler(t, st, staticFetcher(nil, nil), newClock(t0))
	undo := s.markManual(f)
	if s.takeManual(f) == nil {
		t.Fatal("mark not taken")
	}
	s.finishManual(f.ID)
	s.markManual(f) // a new click, indistinguishable by value
	undo()
	if n := s.manualCount(); n != 1 {
		t.Errorf("marks after a stale undo = %d, want the new click's mark kept", n)
	}
	// Undoing a re-arm puts back exactly the mark it replaced.
	if s.takeManual(f) == nil {
		t.Fatal("mark not taken")
	}
	undo = s.markManual(f) // re-armed during the fetch
	undo()
	s.finishManual(f.ID)
	if n := s.manualCount(); n != 0 {
		t.Errorf("marks after undoing a re-arm and finishing = %d, want 0", n)
	}
}

func TestRefreshAll_MarksEveryFeed(t *testing.T) {
	st := openStore(t)
	a := addFeed(t, st, "https://a.example/feed", t0.Add(time.Hour))
	b := addFeedOfKind(t, st, store.KindYouTube, ytFeedURL, t0.Add(2*time.Hour))
	s := newClockedScheduler(t, st, staticFetcher(nil, nil), newClock(t0))
	mustNoErr(t, s.RefreshAll(t.Context()))
	for _, f := range []store.Feed{a, b} {
		if m := s.takeManual(getFeed(t, st, f.ID)); m == nil || !m.prevNext.Equal(f.NextFetchAt) {
			t.Errorf("feed %d mark = %+v, want prevNext %s", f.ID, m, f.NextFetchAt)
		}
	}
}

func TestTakeManual_RefusesMarksOfAnotherSubscriptionOrTooOld(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0.Add(time.Hour))
	clock := newClock(t0)
	s := newClockedScheduler(t, st, staticFetcher(nil, nil), clock)

	mustNoErr(t, s.Refresh(t.Context(), f.ID))
	other := getFeed(t, st, f.ID)
	other.URL = "https://other.example/feed" // a new subscription given the same id
	if m := s.takeManual(other); m != nil {
		t.Errorf("mark of feed %d taken by another subscription: %+v", f.ID, m)
	}

	mustNoErr(t, s.Refresh(t.Context(), f.ID))
	clock.Advance(manualMarkTTL + time.Second)
	if m := s.takeManual(getFeed(t, st, f.ID)); m != nil {
		t.Errorf("stale mark taken: %+v", m)
	}

	// A failed refresh leaves no mark.
	if err := s.Refresh(t.Context(), f.ID+100); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("err = %v", err)
	}
	if n := s.manualCount(); n != 0 {
		t.Errorf("marks = %d, want none", n)
	}
}

// TestRun_RefreshedFailureDoesNotCount: a refresh through the dispatcher
// records the failure without counting it, and a restart forgets the mark.
func TestRun_RefreshedFailureDoesNotCount(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, time.Now().Add(time.Hour))
	mustNoErr(t, st.RecordFetchError(t.Context(), f.ID, "HTTP 503", time.Now(), time.Now().Add(time.Hour)))
	before := getFeed(t, st, f.ID)
	fetcher := staticFetcher(nil, statusErr(feedURL, 404))
	s := newTestScheduler(t, st, fetcher, config.DefaultFetchWorkers)
	startRun(t, s)

	mustNoErr(t, s.Refresh(t.Context(), f.ID))
	waitFor(t, "the refreshed fetch to be stored", func() bool {
		return strings.Contains(getFeed(t, st, f.ID).LastError, "404")
	})
	got := getFeed(t, st, f.ID)
	if got.ErrorCount != before.ErrorCount {
		t.Errorf("error_count = %d, want %d", got.ErrorCount, before.ErrorCount)
	}
	if got.NextFetchAt.After(before.NextFetchAt) {
		t.Errorf("next fetch %s, later than before the refresh %s", got.NextFetchAt, before.NextFetchAt)
	}

	restarted := newTestScheduler(t, st, fetcher, config.DefaultFetchWorkers)
	if restarted.manualCount() != 0 {
		t.Error("a new scheduler starts with marks")
	}
}

// TestRun_SecondClickDuringAManualFetchKeepsTheScheduleFromBeforeTheClicks:
// a refresh that arrives while a refreshed fetch is in flight reads the
// schedule the first click wrote (now); its own fetch must still be bounded
// by the next fetch from before the first click.
func TestRun_SecondClickDuringAManualFetchKeepsTheScheduleFromBeforeTheClicks(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, time.Now().Add(firstBackoff/3))
	before := getFeed(t, st, f.ID)
	started := make(chan struct{}, 1)
	gate := make(chan struct{})
	fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		select {
		case started <- struct{}{}:
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		default:
		}
		return nil, statusErr(feedURL, http.StatusNotFound)
	})
	s := newTestScheduler(t, st, fetcher, config.DefaultFetchWorkers)
	startRun(t, s)

	mustNoErr(t, s.Refresh(t.Context(), f.ID)) // first click
	<-started
	// Stored times have one-second precision: a click within the second the
	// fetch started in would be absorbed by it.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	mustNoErr(t, s.Refresh(t.Context(), f.ID)) // second click, the first fetch in flight
	close(gate)
	waitFor(t, "two fetches", func() bool { return fetcher.CallCount() == 2 })
	waitFor(t, "both outcomes to be stored", func() bool {
		return fetcher.Active() == 0 && getFeed(t, st, f.ID).NextFetchAt.After(time.Now())
	})
	got := getFeed(t, st, f.ID)
	if got.NextFetchAt.After(before.NextFetchAt) || got.ErrorCount != 0 {
		t.Errorf("after two refreshes: next fetch %s, error_count %d; want no later than %s and 0",
			got.NextFetchAt, got.ErrorCount, before.NextFetchAt)
	}
	waitFor(t, "the marks to be dropped", func() bool { return s.manualCount() == 0 })
}

// TestRun_RefreshDuringAFetchMakesTheNextFetchManual: a refresh that arrives
// while the feed is being fetched (a scheduled poll) is served by the fetch
// that follows it, and only that one is uncounted.
func TestRun_RefreshDuringAFetchMakesTheNextFetchManual(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, time.Now().Add(-time.Minute))
	started := make(chan struct{}, 1)
	gate := make(chan struct{})
	fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		select {
		case started <- struct{}{}:
			select {
			case <-gate:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		default:
		}
		return nil, statusErr(feedURL, 404)
	})
	s := newTestScheduler(t, st, fetcher, config.DefaultFetchWorkers)
	startRun(t, s)

	<-started
	// Stored times have one-second precision: a refresh within the second
	// the fetch started in would be absorbed by it.
	time.Sleep(time.Until(time.Now().Truncate(time.Second).Add(time.Second)))
	mustNoErr(t, s.Refresh(t.Context(), f.ID))
	close(gate)
	waitFor(t, "the refreshed fetch", func() bool { return fetcher.CallCount() == 2 })
	waitFor(t, "both outcomes to be stored", func() bool {
		g := getFeed(t, st, f.ID)
		return fetcher.Active() == 0 && g.NextFetchAt.After(time.Now().Add(firstBackoff/2))
	})
	if n := getFeed(t, st, f.ID).ErrorCount; n != 1 {
		t.Errorf("error_count = %d, want 1: the scheduled failure counts, the refresh does not", n)
	}
}
