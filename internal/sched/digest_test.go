package sched

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
	_ "time/tzdata" // the DST tests need real time zones wherever they run

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// testZone is the server's time zone in the digest tests. It is not UTC, so
// an ingestion time read in the wrong zone fails the tests.
var testZone = time.FixedZone("UTC+2", int(2*time.Hour/time.Second))

// localTime is hh:mm on the given day of October 2026 in testZone.
func localTime(day, hour, minute int) time.Time {
	return time.Date(2026, time.October, day, hour, minute, 0, 0, testZone)
}

// Ingestion times used by the tests, in minutes of the day.
const (
	sixAM   = 6 * minutesPerHour
	sevenAM = 7 * minutesPerHour
	tenAM   = 10 * minutesPerHour
	noon    = 12 * minutesPerHour
)

// dayAndAnHour is the longest wait for the next ingestion time: a day, plus
// the hour that a fall-back day adds.
const dayAndAnHour = 24*time.Hour + time.Hour

// loadZone loads an IANA time zone (embedded by time/tzdata).
func loadZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%s): %v", name, err)
	}
	return loc
}

// timedDigest creates a digest made of src, with the ingestion time minute
// (a negative minute = none).
func timedDigest(t *testing.T, st store.Store, name string, minute int, src store.DigestSources) store.Digest {
	t.Helper()
	ctx := t.Context()
	d, err := st.CreateDigest(ctx, name)
	mustNoErr(t, err)
	folders, err := st.ListFolders(ctx)
	mustNoErr(t, err)
	feeds, err := st.ListFeeds(ctx)
	mustNoErr(t, err)
	tags, err := st.ListTags(ctx)
	mustNoErr(t, err)
	mustNoErr(t, st.SetDigestSources(ctx, d.ID, src, store.SourcesFingerprint(folders, feeds, tags)))
	if minute >= 0 {
		mustNoErr(t, st.SetDigestIngest(ctx, d.ID, minute, true))
	}
	return d
}

// newZonedScheduler is newClockedScheduler with testZone as the server's
// time zone.
func newZonedScheduler(t *testing.T, st store.Store, f Fetcher, c *testClock) *Scheduler {
	t.Helper()
	s := newClockedScheduler(t, st, f, c)
	s.loc = testZone
	return s
}

// drainWake empties the scheduler's wake-up channel.
func drainWake(s *Scheduler) {
	select {
	case <-s.wake:
	default:
	}
}

func TestNextIngest_FirstOccurrenceStrictlyAfterNow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		now    time.Time
		minute int
		want   time.Time
	}{
		{"later today", localTime(1, 6, 59), sevenAM, localTime(1, 7, 0)},
		{"right at the time: tomorrow", localTime(1, 7, 0), sevenAM, localTime(2, 7, 0)},
		{"just after: tomorrow", localTime(1, 7, 0).Add(time.Second), sevenAM, localTime(2, 7, 0)},
		{"midnight", localTime(1, 23, 59), 0, localTime(2, 0, 0)},
		{"last minute of the day", localTime(1, 0, 0), store.MinutesPerDay - 1, localTime(1, 23, 59)},
		{"end of the month", localTime(31, 8, 0), sevenAM, time.Date(2026, time.November, 1, 7, 0, 0, 0, testZone)},
		{"end of the year", time.Date(2026, time.December, 31, 23, 30, 0, 0, testZone), 0,
			time.Date(2027, time.January, 1, 0, 0, 0, 0, testZone)},
		// The clock of now does not matter, only the instant: 04:59 UTC is
		// 06:59 in testZone.
		{"now in another zone", localTime(1, 6, 59).UTC(), sevenAM, localTime(1, 7, 0)},
	} {
		if got := NextIngest(tc.now, tc.minute, testZone); !got.Equal(tc.want) {
			t.Errorf("%s: NextIngest(%s, %d) = %s, want %s", tc.name, tc.now, tc.minute, got, tc.want)
		}
	}
}

// utcTime is hh:mm on the given day of 2026 in UTC.
func utcTime(month time.Month, day, hour, minute int) time.Time {
	return time.Date(2026, month, day, hour, minute, 0, 0, time.UTC)
}

// Clock times around the night-time DST changes of the tests.
const (
	halfPastOne = 1*minutesPerHour + minutesPerHour/2
	halfPastTwo = 2*minutesPerHour + minutesPerHour/2
)

// TestNextIngest_DaylightSavingTimeChanges pins what time.Date does with
// the clock times that a DST change skips or repeats. Which way a skipped
// time moves and which occurrence of a repeated time counts depend on the
// zone's offset from UTC:
//
//   - America/New_York in 2026 (west of UTC): 02:00 EST jumps to 03:00
//     EDT on 8 March, 02:00 EDT falls back to 01:00 EST on 1 November;
//   - Europe/Rome in 2026 (east of UTC): 02:00 CET jumps to 03:00 CEST on
//     29 March, 03:00 CEST falls back to 02:00 CET on 25 October.
//
// Instants are given in UTC.
func TestNextIngest_DaylightSavingTimeChanges(t *testing.T) {
	ny, rome := loadZone(t, "America/New_York"), loadZone(t, "Europe/Rome")
	for _, tc := range []struct {
		name   string
		loc    *time.Location
		now    time.Time
		minute int
		want   time.Time
	}{
		// Spring forward: 02:30 does not exist on 8 March. time.Date
		// normalizes it to 01:30 EST (06:30 UTC): that day the digest is
		// fetched an hour early, once.
		{"skipped time, before", ny, utcTime(time.March, 8, 5, 0), halfPastTwo, utcTime(time.March, 8, 6, 30)},
		{"skipped time, after", ny, utcTime(time.March, 8, 6, 30), halfPastTwo, utcTime(time.March, 9, 6, 30)},
		// A day of 23 hours: 07:00 EST to 07:00 EDT.
		{"across spring forward", ny, utcTime(time.March, 7, 12, 0), sevenAM, utcTime(time.March, 8, 11, 0)},
		// Fall back: 01:30 happens twice on 1 November. time.Date picks the
		// first, 01:30 EDT (05:30 UTC); the repeat at 01:30 EST (06:30 UTC)
		// is not a second ingestion that day.
		{"repeated time, before", ny, utcTime(time.November, 1, 4, 0), halfPastOne, utcTime(time.November, 1, 5, 30)},
		{"repeated time, after the first", ny, utcTime(time.November, 1, 5, 30), halfPastOne, utcTime(time.November, 2, 6, 30)},
		// A day of 25 hours: 07:00 EDT to 07:00 EST.
		{"across fall back", ny, utcTime(time.October, 31, 11, 0), sevenAM, utcTime(time.November, 1, 12, 0)},

		// Spring forward: 02:30 does not exist on 29 March. time.Date
		// normalizes it to 03:30 CEST (01:30 UTC): that day the digest is
		// fetched an hour late, once.
		{"Rome: skipped time, before", rome, utcTime(time.March, 29, 0, 0), halfPastTwo, utcTime(time.March, 29, 1, 30)},
		{"Rome: skipped time, after", rome, utcTime(time.March, 29, 1, 30), halfPastTwo, utcTime(time.March, 30, 0, 30)},
		// A day of 23 hours: 07:00 CET to 07:00 CEST.
		{"Rome: across spring forward", rome, utcTime(time.March, 28, 6, 0), sevenAM, utcTime(time.March, 29, 5, 0)},
		// Fall back: 02:30 happens twice on 25 October. time.Date picks the
		// second, 02:30 CET (01:30 UTC); the first, 02:30 CEST (00:30 UTC),
		// is not an ingestion, so there is still one that day.
		{"Rome: repeated time, before", rome, utcTime(time.October, 25, 0, 0), halfPastTwo, utcTime(time.October, 25, 1, 30)},
		{"Rome: repeated time, at the first", rome, utcTime(time.October, 25, 0, 30), halfPastTwo, utcTime(time.October, 25, 1, 30)},
		{"Rome: repeated time, after the second", rome, utcTime(time.October, 25, 1, 30), halfPastTwo, utcTime(time.October, 26, 1, 30)},
		// A day of 25 hours: 07:00 CEST to 07:00 CET.
		{"Rome: across fall back", rome, utcTime(time.October, 24, 5, 0), sevenAM, utcTime(time.October, 25, 6, 0)},
	} {
		if got := NextIngest(tc.now, tc.minute, tc.loc); !got.Equal(tc.want) {
			t.Errorf("%s: NextIngest(%s, %d) = %s, want %s", tc.name, tc.now.In(tc.loc), tc.minute, got.In(tc.loc), tc.want.In(tc.loc))
		}
	}
}

// TestNextIngest_NeverInThePast: where a DST change skips the hour after
// midnight (America/Havana, 8 March 2026: 00:00 CST jumps to 01:00 CDT),
// time.Date normalizes 00:30 to 23:30 the day before. Late that evening the
// next 00:30 must still lie ahead, or the feed would be fetched in a loop.
func TestNextIngest_NeverInThePast(t *testing.T) {
	havana := loadZone(t, "America/Havana")
	now := time.Date(2026, time.March, 7, 23, 45, 0, 0, havana)
	got := NextIngest(now, minutesPerHour/2, havana)
	if want := time.Date(2026, time.March, 9, 0, 30, 0, 0, havana); !got.Equal(want) {
		t.Fatalf("NextIngest(%s, 00:30) = %s, want %s", now, got, want)
	}

	// Every quarter of an hour of a year, in zones with DST changes at
	// night and at midnight, west and east of UTC: always ahead, at most a
	// day and an hour away.
	minutes := []int{0, minutesPerHour / 2, halfPastOne, halfPastTwo, sevenAM, store.MinutesPerDay - 1}
	zones := []*time.Location{havana, loadZone(t, "America/New_York"), loadZone(t, "Europe/Rome"), testZone}
	for _, loc := range zones {
		start := time.Date(2026, time.January, 1, 0, 0, 0, 0, loc)
		for now := start; now.Before(start.AddDate(1, 0, 0)); now = now.Add(15 * time.Minute) {
			for _, m := range minutes {
				got := NextIngest(now, m, loc)
				if !got.After(now) || got.Sub(now) > dayAndAnHour {
					t.Fatalf("%s: NextIngest(%s, %d) = %s", loc, now, m, got)
				}
			}
		}
	}
}

// TestPoll_NextFetchIsTheEarlierOfTheIntervalAndTheDigestTime: a feed in a
// digest with an ingestion time is fetched at that time on top of its
// regular schedule.
func TestPoll_NextFetchIsTheEarlierOfTheIntervalAndTheDigestTime(t *testing.T) {
	st := openStore(t)
	f := addFeed(t, st, feedURL, t0)
	timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FeedIDs: []int64{f.ID}})
	clock := newClock(localTime(1, 4, 0))
	body := atomDoc("Feed", itemsNewestFirst("e", 1, t0)...)
	s := newZonedScheduler(t, st, staticFetcher(notModified(feedURL), nil), clock)
	interval := config.DefaultPollInterval

	for _, tc := range []struct {
		name     string
		now      time.Time
		response *fetch.Result
		want     time.Time
	}{
		{"digest time first", localTime(1, 4, 0), notModified(feedURL), localTime(1, 7, 0)},
		{"polled at the digest time", localTime(1, 7, 0), ok(feedURL, body), localTime(1, 7, 0).Add(interval)},
		{"interval first", localTime(1, 20, 0), ok(feedURL, body), localTime(1, 20, 0).Add(interval)},
		{"digest time first the next day", localTime(2, 4, 30), notModified(feedURL), localTime(2, 7, 0)},
	} {
		clock.Set(tc.now)
		s.fetcher = staticFetcher(tc.response, nil)
		if hold := pollOnce(t, s, st, f.ID); !hold.IsZero() {
			t.Fatalf("%s: hold until %s", tc.name, hold)
		}
		if got := getFeed(t, st, f.ID).NextFetchAt; !got.Equal(tc.want) {
			t.Errorf("%s: next fetch %s, want %s", tc.name, got.In(testZone), tc.want)
		}
	}
}

// TestPoll_EarliestOfEveryTimedDigestOfTheFeedOrItsFolder: the digests that
// name the feed and those that name its folder count; digests without a
// time, and digests that only reach the feed's entries through tags, do not.
func TestPoll_EarliestOfEveryTimedDigestOfTheFeedOrItsFolder(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	folder, err := st.CreateFolder(ctx, "Blogs")
	mustNoErr(t, err)
	f := addFeed(t, st, feedURL, t0, itemsNewestFirst("e", 1, t0)...)
	mustNoErr(t, st.MoveFeed(ctx, f.ID, folder.ID))
	entry := feedEntries(t, st, f.ID)["e0"]
	tag, err := st.AddEntryTag(ctx, entry.ID, "go")
	mustNoErr(t, err)
	timedDigest(t, st, "Lunch", noon, store.DigestSources{FolderIDs: []int64{folder.ID}})
	timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FeedIDs: []int64{f.ID}})
	timedDigest(t, st, "Untimed", -1, store.DigestSources{FeedIDs: []int64{f.ID}, FolderIDs: []int64{folder.ID}})
	timedDigest(t, st, "Tagged", sixAM, store.DigestSources{TagIDs: []int64{tag.ID}})

	clock := newClock(localTime(1, 8, 0))
	s := newZonedScheduler(t, st, staticFetcher(notModified(feedURL), nil), clock)
	check := func(now, want time.Time) {
		t.Helper()
		clock.Set(now)
		pollOnce(t, s, st, f.ID)
		if got := getFeed(t, st, f.ID).NextFetchAt; !got.Equal(want) {
			t.Errorf("polled at %s: next fetch %s, want %s", now, got.In(testZone), want)
		}
	}
	check(localTime(1, 8, 0), localTime(1, 12, 0))                                  // the folder's digest
	check(localTime(1, 12, 0), localTime(1, 12, 0).Add(config.DefaultPollInterval)) // interval first
	check(localTime(2, 3, 0), localTime(2, 7, 0))                                   // the feed's digest

	// Out of the folder, the folder's digest no longer applies.
	mustNoErr(t, st.MoveFeed(ctx, f.ID, 0))
	check(localTime(2, 8, 0), localTime(2, 8, 0).Add(config.DefaultPollInterval))
	check(localTime(2, 11, 0), localTime(2, 11, 0).Add(config.DefaultPollInterval))
}

// TestPoll_FailureBackoffIsCappedAtTheDigestTime: a failing feed in a timed
// digest is still tried once a day at the digest's time.
func TestPoll_FailureBackoffIsCappedAtTheDigestTime(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	f := addFeed(t, st, feedURL, t0)
	timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FeedIDs: []int64{f.ID}})
	clock := newClock(localTime(1, 8, 0))
	s := newZonedScheduler(t, st, staticFetcher(nil, &fetch.StatusError{URL: feedURL, StatusCode: 503}), clock)

	// A first failure is retried soon, before the digest time anyway.
	pollOnce(t, s, st, f.ID)
	if got, want := getFeed(t, st, f.ID).NextFetchAt, clock.Now().Add(firstBackoff); !got.Equal(want) {
		t.Errorf("first failure: next fetch %s, want %s", got, want)
	}
	// Failures in a row back off up to a day, but not past the digest time.
	for range config.WarnAfterFailures {
		mustNoErr(t, st.RecordFetchError(ctx, f.ID, "HTTP 503", clock.Now(), clock.Now()))
	}
	pollOnce(t, s, st, f.ID)
	if got, want := getFeed(t, st, f.ID).NextFetchAt, localTime(2, 7, 0); !got.Equal(want) {
		t.Errorf("backing off: next fetch %s, want the digest time %s", got.In(testZone), want)
	}

	// When the failure cannot be stored, the feed is held no longer than
	// that either.
	s.st = failingWrites{st}
	if hold, want := pollOnce(t, s, st, f.ID), localTime(2, 7, 0); !hold.Equal(want) {
		t.Errorf("unstored failure: held until %s, want %s", hold.In(testZone), want)
	}
}

// TestPoll_RetryAfterStaysALowerBoundBeforeTheDigestTime: the digest time
// caps the backoff, but never shortens the wait a server asked for with
// Retry-After (itself capped at MaxBackoff), whether or not the failure can
// be stored.
func TestPoll_RetryAfterStaysALowerBoundBeforeTheDigestTime(t *testing.T) {
	const (
		pastTheDigestTime = 3 * time.Hour // from 06:45, past 07:00
		shortWait         = time.Minute
	)
	polledAt := localTime(1, 6, 45)
	for _, tc := range []struct {
		name       string
		failures   int // failures in a row before this poll
		retryAfter time.Duration
		want       time.Time
	}{
		{"Retry-After past the digest time", 0, pastTheDigestTime, polledAt.Add(pastTheDigestTime)},
		{"backing off, Retry-After past the digest time", config.WarnAfterFailures, pastTheDigestTime, polledAt.Add(pastTheDigestTime)},
		{"Retry-After beyond the backoff cap", 0, 2 * config.MaxBackoff, polledAt.Add(config.MaxBackoff)},
		{"short Retry-After: the digest time", config.WarnAfterFailures, shortWait, localTime(1, 7, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			f := addFeed(t, st, feedURL, polledAt)
			for range tc.failures {
				mustNoErr(t, st.RecordFetchError(t.Context(), f.ID, "HTTP 503", polledAt, polledAt))
			}
			timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FeedIDs: []int64{f.ID}})
			tooMany := &fetch.StatusError{URL: feedURL, StatusCode: http.StatusTooManyRequests, RetryAfter: tc.retryAfter}
			s := newZonedScheduler(t, st, staticFetcher(nil, tooMany), newClock(polledAt))

			s.st = failingWrites{st}
			if hold := pollOnce(t, s, st, f.ID); !hold.Equal(tc.want) {
				t.Errorf("unstored failure: held until %s, want %s", hold.In(testZone), tc.want)
			}
			s.st = st
			pollOnce(t, s, st, f.ID)
			if got := getFeed(t, st, f.ID).NextFetchAt; !got.Equal(tc.want) {
				t.Errorf("next fetch %s, want %s", got.In(testZone), tc.want)
			}
		})
	}
}

// TestPoll_ADigestTimeDuringTheFetchMakesTheFeedDueAgain: a request sent
// just before a digest's time and answered just after it may have missed
// what was published at that time, so it is not that day's ingestion: the
// feed is due again at once. The fetch after that started after the digest
// time and schedules the next day's: one extra fetch, no loop. This holds
// for every outcome, and for the hold of an outcome that cannot be stored.
func TestPoll_ADigestTimeDuringTheFetchMakesTheFeedDueAgain(t *testing.T) {
	sevenOClock := localTime(1, 7, 0)
	sentAt := sevenOClock.Add(-fetchTook / 2).Truncate(time.Second) // answered fetchTook later, after 07:00
	body := atomDoc("Feed", itemsNewestFirst("e", 1, t0)...)
	unavailable := &fetch.StatusError{URL: feedURL, StatusCode: http.StatusServiceUnavailable}
	for _, tc := range []struct {
		name string
		res  *fetch.Result
		err  error
		// after is the next fetch scheduled by the fetch that follows,
		// answered at answeredAt.
		after func(answeredAt time.Time) time.Time
	}{
		{"304", notModified(feedURL), nil, func(at time.Time) time.Time { return at.Add(config.DefaultPollInterval) }},
		{"200", ok(feedURL, body), nil, func(at time.Time) time.Time { return at.Add(config.DefaultPollInterval) }},
		{"failure", nil, unavailable, func(at time.Time) time.Time { return at.Add(secondBackoff) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !sentAt.Before(sevenOClock) || !sentAt.Add(fetchTook).After(sevenOClock) {
				t.Fatalf("the fetch from %s does not straddle %s", sentAt, sevenOClock)
			}
			st := openStore(t)
			f := addFeed(t, st, feedURL, sentAt)
			timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FeedIDs: []int64{f.ID}})
			clock := newClock(sentAt)
			s := newZonedScheduler(t, st, slowFetcher(clock, tc.res, tc.err), clock)

			s.st = failingWrites{st}
			if hold := pollOnce(t, s, st, f.ID); !hold.Equal(sevenOClock) {
				t.Errorf("unstored outcome: held until %s, want %s", hold.In(testZone), sevenOClock)
			}
			s.st = st
			clock.Set(sentAt)
			pollOnce(t, s, st, f.ID)
			if got := getFeed(t, st, f.ID).NextFetchAt; !got.Equal(sevenOClock) {
				t.Fatalf("next fetch %s, want the digest time %s, which the fetch straddled", got.In(testZone), sevenOClock)
			}
			pollOnce(t, s, st, f.ID) // due at once
			if got, want := getFeed(t, st, f.ID).NextFetchAt, tc.after(clock.Now()); !got.Equal(want) {
				t.Errorf("after the extra fetch: next fetch %s, want %s", got.In(testZone), want.In(testZone))
			}
		})
	}
}

// TestPoll_FeedMovedIntoATimedFolderDuringItsFetch: a feed moved into the
// folder of a timed digest while it is being fetched gets that day's digest
// time. ScheduleIngest, called after the move, cannot set it, since the
// feed is due (it only moves a fetch earlier); the poll must read the
// feed's folder when it stores its outcome, not when the fetch started (and
// again once it is stored: TestPoll_ChangeCommittedBeforeTheOutcomeIsStored).
// A failing feed is left to the poll by ScheduleIngest in any case.
func TestPoll_FeedMovedIntoATimedFolderDuringItsFetch(t *testing.T) {
	body := atomDoc("Feed", itemsNewestFirst("e", 1, t0)...)
	unavailable := &fetch.StatusError{URL: feedURL, StatusCode: http.StatusServiceUnavailable}
	for _, tc := range []struct {
		name     string
		failures int // failures in a row before this poll
		res      *fetch.Result
		err      error
	}{
		{"304", 0, notModified(feedURL), nil},
		{"200", 0, ok(feedURL, body), nil},
		{"failure while backing off", config.WarnAfterFailures, nil, unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			ctx := t.Context()
			polledAt := localTime(1, 4, 0)
			folder, err := st.CreateFolder(ctx, "Morning reads")
			mustNoErr(t, err)
			timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FolderIDs: []int64{folder.ID}})
			f := addFeed(t, st, feedURL, polledAt)
			for range tc.failures {
				mustNoErr(t, st.RecordFetchError(ctx, f.ID, "HTTP 503", polledAt, polledAt))
			}
			var s *Scheduler
			fetcher := newFetcher(func(ctx context.Context, _ fetch.Request) (*fetch.Result, error) {
				// What moving the feed on its settings page does.
				mustNoErr(t, st.MoveFeed(ctx, f.ID, folder.ID))
				mustNoErr(t, s.ScheduleIngest(ctx, []int64{f.ID}))
				return tc.res, tc.err
			})
			s = newZonedScheduler(t, st, fetcher, newClock(polledAt))
			pollOnce(t, s, st, f.ID)
			if got, want := getFeed(t, st, f.ID).NextFetchAt, localTime(1, 7, 0); !got.Equal(want) {
				t.Errorf("next fetch %s, want the digest time %s", got.In(testZone), want)
			}
		})
	}
}

// TestPoll_ChangeCommittedBeforeTheOutcomeIsStored: a poll reads the feed's
// row and its ingestion times, then stores its outcome in a transaction of
// its own. A feed moved into a timed digest's folder, or a time given to a
// digest of its folder, in between still gets that day's digest time,
// whatever the outcome: ScheduleIngest, called after the change, leaves the
// feed alone (it is due), so the poll reads them again once its outcome is
// stored.
func TestPoll_ChangeCommittedBeforeTheOutcomeIsStored(t *testing.T) {
	body := atomDoc("Feed", itemsNewestFirst("e", 1, t0)...)
	unavailable := &fetch.StatusError{URL: feedURL, StatusCode: http.StatusServiceUnavailable}
	polledAt := localTime(1, 4, 0)
	for _, change := range []struct {
		name     string
		inFolder bool // whether the feed is in the digest's folder before the change
		minute   int  // the digest's time before the change; negative = none
		// apply makes the change and returns the feeds that saving it hands
		// to ScheduleIngest.
		apply func(ctx context.Context, st store.Store, feedID, folderID int64, d store.Digest) ([]int64, error)
	}{
		{"moved into the folder", false, sevenAM, func(ctx context.Context, st store.Store, feedID, folderID int64, _ store.Digest) ([]int64, error) {
			return []int64{feedID}, st.MoveFeed(ctx, feedID, folderID)
		}},
		{"digest time set", true, -1, func(ctx context.Context, st store.Store, _, _ int64, d store.Digest) ([]int64, error) {
			if err := st.SetDigestIngest(ctx, d.ID, sevenAM, true); err != nil {
				return nil, err
			}
			return st.DigestFeedIDs(ctx, d.ID)
		}},
	} {
		for _, outcome := range []struct {
			name     string
			failures int // failures in a row before this poll
			res      *fetch.Result
			err      error
		}{
			{"304", 0, notModified(feedURL), nil},
			{"200", 0, ok(feedURL, body), nil},
			{"failure while backing off", config.WarnAfterFailures, nil, unavailable},
		} {
			t.Run(change.name+"/"+outcome.name, func(t *testing.T) {
				st := openStore(t)
				ctx := t.Context()
				folder, err := st.CreateFolder(ctx, "Morning reads")
				mustNoErr(t, err)
				d := timedDigest(t, st, "Morning", change.minute, store.DigestSources{FolderIDs: []int64{folder.ID}})
				f := addFeed(t, st, feedURL, polledAt)
				if change.inFolder {
					mustNoErr(t, st.MoveFeed(ctx, f.ID, folder.ID))
				}
				for range outcome.failures {
					mustNoErr(t, st.RecordFetchError(ctx, f.ID, "HTTP 503", polledAt, polledAt))
				}
				w := &changeOnIngestRead{Store: st}
				s := newZonedScheduler(t, w, staticFetcher(outcome.res, outcome.err), newClock(polledAt))
				w.change = func(ctx context.Context) {
					// What saving the feed's or the digest's settings does.
					ids, err := change.apply(ctx, st, f.ID, folder.ID, d)
					mustNoErr(t, err)
					mustNoErr(t, s.ScheduleIngest(ctx, ids))
				}

				if hold := pollOnce(t, s, st, f.ID); !hold.IsZero() {
					t.Fatalf("hold until %s", hold)
				}
				if !w.done.Load() {
					t.Fatal("the poll never read the ingestion times")
				}
				if got, want := getFeed(t, st, f.ID).NextFetchAt, localTime(1, 7, 0); !got.Equal(want) {
					t.Errorf("next fetch %s, want the digest time %s", got.In(testZone), want)
				}
			})
		}
	}
}

// TestPoll_FallBackDayIngestsOnce: on the day a time repeats, the digest is
// fetched at one of its two occurrences only: the first west of UTC, the
// second east of it (see TestNextIngest_DaylightSavingTimeChanges).
func TestPoll_FallBackDayIngestsOnce(t *testing.T) {
	for _, tc := range []struct {
		zone      string
		minute    int
		ingestion time.Time // the one ingestion of the fall-back day
	}{
		{"America/New_York", halfPastOne, utcTime(time.November, 1, 5, 30)}, // the first 01:30 (EDT)
		{"Europe/Rome", halfPastTwo, utcTime(time.October, 25, 1, 30)},      // the second 02:30 (CET)
	} {
		t.Run(tc.zone, func(t *testing.T) {
			loc := loadZone(t, tc.zone)
			st := openStore(t)
			f := addFeed(t, st, feedURL, t0)
			timedDigest(t, st, "Night", tc.minute, store.DigestSources{FeedIDs: []int64{f.ID}})
			clock := newClock(tc.ingestion.Add(-time.Hour))
			s := newClockedScheduler(t, st, staticFetcher(notModified(feedURL), nil), clock)
			s.loc = loc

			pollOnce(t, s, st, f.ID)
			if got := getFeed(t, st, f.ID).NextFetchAt; !got.Equal(tc.ingestion) {
				t.Fatalf("next fetch %s, want %s", got.In(loc), tc.ingestion.In(loc))
			}
			clock.Set(tc.ingestion)
			pollOnce(t, s, st, f.ID)
			if got, want := getFeed(t, st, f.ID).NextFetchAt, tc.ingestion.Add(config.DefaultPollInterval); !got.Equal(want) {
				t.Errorf("after the ingestion: next fetch %s, want one interval later %s (no second one that day)",
					got.In(loc), want.In(loc))
			}
		})
	}
}

// TestSubscribe_IntoAFolderOfATimedDigest: a feed subscribed into a folder
// that a digest names gets its first poll at the digest's time when that
// comes first.
func TestSubscribe_IntoAFolderOfATimedDigest(t *testing.T) {
	const otherURL = "https://other.example/feed.xml"
	st := openStore(t)
	folder, err := st.CreateFolder(t.Context(), "Morning reads")
	mustNoErr(t, err)
	timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FolderIDs: []int64{folder.ID}})
	doc := atomDoc("Feed", itemsNewestFirst("e", 1, t0)...)
	routes := map[string]*fetch.Result{feedURL: ok(feedURL, doc), otherURL: ok(otherURL, doc)}
	clock := newClock(localTime(1, 4, 0))
	s := newZonedScheduler(t, st, routeFetcher(routes), clock)

	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: feedURL, FolderID: folder.ID})
	mustNoErr(t, err)
	if want := localTime(1, 7, 0); !feed.NextFetchAt.Equal(want) || !getFeed(t, st, feed.ID).NextFetchAt.Equal(want) {
		t.Errorf("into the digest's folder: first poll %s, want %s", feed.NextFetchAt.In(testZone), want)
	}
	other, err := s.Subscribe(t.Context(), Subscription{FeedURL: otherURL})
	mustNoErr(t, err)
	if want := clock.Now().Add(config.DefaultPollInterval); !other.NextFetchAt.Equal(want) {
		t.Errorf("without a folder: first poll %s, want %s", other.NextFetchAt, want)
	}
}

// cachedAddFetches are the add-flow fetches whose result Subscribe stores
// from the preview cache instead of fetching the feed again.
var cachedAddFetches = []struct {
	name  string
	fetch func(t *testing.T, s *Scheduler)
}{
	{"Preview", func(t *testing.T, s *Scheduler) {
		_, err := s.Preview(t.Context(), feedURL)
		mustNoErr(t, err)
	}},
	{"resolver", func(t *testing.T, s *Scheduler) {
		_, err := s.ResolveFetcher().Fetch(t.Context(), fetch.Request{URL: feedURL, Accept: fetch.FeedAccept})
		mustNoErr(t, err)
	}},
}

// TestSubscribe_AFetchFromBeforeTheDigestTimeMakesTheFeedDue: Subscribe
// stores the fetch that a Preview, or the resolver, made up to
// previewCacheTTL before. Its ingestion time is the first after that request
// was sent, not after the subscription: previewed at 06:55 and confirmed at
// 07:03 into the folder of a 07:00 digest, the feed may lack what was
// published for 07:00, so it is due at once rather than at its regular time
// hours later. The poll that follows, sent after 07:00, schedules the
// regular time: one extra fetch.
func TestSubscribe_AFetchFromBeforeTheDigestTimeMakesTheFeedDue(t *testing.T) {
	previewedAt, confirmedAt, sevenOClock := localTime(1, 6, 55), localTime(1, 7, 3), localTime(1, 7, 0)
	if confirmedAt.Sub(previewedAt) >= previewCacheTTL {
		t.Fatalf("a preview at %s is no longer cached at %s", previewedAt, confirmedAt)
	}
	doc := atomDoc("Feed", itemsNewestFirst("e", 1, t0)...)
	for _, tc := range cachedAddFetches {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			folder, err := st.CreateFolder(t.Context(), "Morning reads")
			mustNoErr(t, err)
			timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FolderIDs: []int64{folder.ID}})
			clock := newClock(previewedAt)
			fetcher := staticFetcher(ok(feedURL, doc), nil)
			s := newZonedScheduler(t, st, fetcher, clock)

			tc.fetch(t, s)
			clock.Set(confirmedAt)
			feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: feedURL, FolderID: folder.ID})
			mustNoErr(t, err)
			if n := fetcher.CallCount(); n != 1 {
				t.Fatalf("fetches = %d, want 1: Subscribe reuses the cached fetch", n)
			}
			if got := getFeed(t, st, feed.ID).NextFetchAt; !got.Equal(sevenOClock) || !feed.NextFetchAt.Equal(got) {
				t.Fatalf("first poll %s (returned %s), want the digest time %s, which the fetch preceded",
					got.In(testZone), feed.NextFetchAt.In(testZone), sevenOClock)
			}
			pollOnce(t, s, st, feed.ID) // due at once
			if got, want := getFeed(t, st, feed.ID).NextFetchAt, confirmedAt.Add(config.DefaultPollInterval); !got.Equal(want) {
				t.Errorf("after the extra fetch: next fetch %s, want %s", got.In(testZone), want.In(testZone))
			}
		})
	}
}

// TestSubscribe_AFetchThatStraddlesTheDigestTimeMakesTheFeedDue: the fetch
// Subscribe stores (its own, or a cached one), sent just before a digest's
// time and answered just after it, leaves the new feed due at once, as a
// poll does (TestPoll_ADigestTimeDuringTheFetchMakesTheFeedDueAgain): its
// start is taken before the request is sent, not when the answer arrives.
func TestSubscribe_AFetchThatStraddlesTheDigestTimeMakesTheFeedDue(t *testing.T) {
	sevenOClock := localTime(1, 7, 0)
	sentAt := sevenOClock.Add(-fetchTook / 2).Truncate(time.Second) // answered fetchTook later, after 07:00
	if !sentAt.Before(sevenOClock) || !sentAt.Add(fetchTook).After(sevenOClock) {
		t.Fatalf("the fetch from %s does not straddle %s", sentAt, sevenOClock)
	}
	doc := atomDoc("Feed", itemsNewestFirst("e", 1, t0)...)
	fetches := append([]struct {
		name  string
		fetch func(t *testing.T, s *Scheduler)
	}{{"Subscribe's own", nil}}, cachedAddFetches...)
	for _, tc := range fetches {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			folder, err := st.CreateFolder(t.Context(), "Morning reads")
			mustNoErr(t, err)
			timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FolderIDs: []int64{folder.ID}})
			clock := newClock(sentAt)
			fetcher := slowFetcher(clock, ok(feedURL, doc), nil)
			s := newZonedScheduler(t, st, fetcher, clock)

			if tc.fetch != nil {
				tc.fetch(t, s)
			}
			feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: feedURL, FolderID: folder.ID})
			mustNoErr(t, err)
			if n := fetcher.CallCount(); n != 1 {
				t.Fatalf("fetches = %d, want 1", n)
			}
			if got := getFeed(t, st, feed.ID).NextFetchAt; !got.Equal(sevenOClock) {
				t.Errorf("first poll %s, want the digest time %s, which the fetch straddled", got.In(testZone), sevenOClock)
			}
		})
	}
}

// changeOnIngestRead is a store that makes change, once, right after the
// scheduler first reads a feed's ingestion times: after a poll has read the
// feed's row and before it stores its outcome, or after Subscribe has
// computed the new feed's first poll and before it creates the feed. That
// is where a move or a digest change saved on a settings page commits
// unseen by the read.
type changeOnIngestRead struct {
	store.Store
	done   atomic.Bool
	change func(ctx context.Context)
}

func (w *changeOnIngestRead) IngestMinutes(ctx context.Context, feedID, folderID int64) ([]int, error) {
	minutes, err := w.Store.IngestMinutes(ctx, feedID, folderID)
	if w.done.CompareAndSwap(false, true) {
		w.change(ctx)
	}
	return minutes, err
}

// TestSubscribe_DigestChangedBeforeTheFeedIsCreated: a digest of the new
// feed's folder given a time, or a timed digest given that folder, after
// Subscribe read the folder's ingestion times and before it created the
// feed, still sets the feed's first poll: ScheduleIngest, called after the
// change, did not see the feed yet, so Subscribe reads them again once the
// feed is created.
func TestSubscribe_DigestChangedBeforeTheFeedIsCreated(t *testing.T) {
	subscribedAt := localTime(1, 4, 0)
	doc := atomDoc("Feed", itemsNewestFirst("e", 1, t0)...)
	for _, tc := range []struct {
		name   string
		minute int  // the digest's time before the change; negative = none
		named  bool // whether the digest names the folder before the change
		change func(ctx context.Context, st store.Store, d store.Digest, folderID int64) error
	}{
		{"digest time set", -1, true, func(ctx context.Context, st store.Store, d store.Digest, _ int64) error {
			return st.SetDigestIngest(ctx, d.ID, sevenAM, true)
		}},
		{"folder added to a timed digest", sevenAM, false, func(ctx context.Context, st store.Store, d store.Digest, folderID int64) error {
			folders, err := st.ListFolders(ctx)
			if err != nil {
				return err
			}
			feeds, err := st.ListFeeds(ctx)
			if err != nil {
				return err
			}
			tags, err := st.ListTags(ctx)
			if err != nil {
				return err
			}
			return st.SetDigestSources(ctx, d.ID, store.DigestSources{FolderIDs: []int64{folderID}},
				store.SourcesFingerprint(folders, feeds, tags))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := openStore(t)
			ctx := t.Context()
			folder, err := st.CreateFolder(ctx, "Morning reads")
			mustNoErr(t, err)
			var src store.DigestSources
			if tc.named {
				src.FolderIDs = []int64{folder.ID}
			}
			d := timedDigest(t, st, "Morning", tc.minute, src)
			w := &changeOnIngestRead{Store: st}
			s := newZonedScheduler(t, w, staticFetcher(ok(feedURL, doc), nil), newClock(subscribedAt))
			w.change = func(ctx context.Context) {
				// What saving the digest's settings does.
				mustNoErr(t, tc.change(ctx, st, d, folder.ID))
				ids, err := st.DigestFeedIDs(ctx, d.ID)
				mustNoErr(t, err)
				mustNoErr(t, s.ScheduleIngest(ctx, ids))
			}

			feed, err := s.Subscribe(ctx, Subscription{FeedURL: feedURL, FolderID: folder.ID})
			mustNoErr(t, err)
			if !w.done.Load() {
				t.Fatal("Subscribe never read the ingestion times")
			}
			want := localTime(1, 7, 0)
			if got := getFeed(t, st, feed.ID).NextFetchAt; !got.Equal(want) || !feed.NextFetchAt.Equal(got) {
				t.Errorf("first poll %s (returned %s), want the digest time %s",
					got.In(testZone), feed.NextFetchAt.In(testZone), want)
			}
		})
	}
}

// TestScheduleIngest_OnlyEverMovesAFetchEarlier: after a digest's time or
// sources change, or a feed moves to another folder, its feeds' next fetch
// moves to their next ingestion time when that is earlier, never later.
func TestScheduleIngest_OnlyEverMovesAFetchEarlier(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	regular := localTime(1, 12, 0)
	f := addFeed(t, st, feedURL, regular)
	clock := newClock(localTime(1, 4, 0))
	s := newZonedScheduler(t, st, staticFetcher(nil, nil), clock)
	next := func(id int64) time.Time { return getFeed(t, st, id).NextFetchAt }
	schedule := func(d store.Digest) (woke bool) {
		t.Helper()
		ids, err := st.DigestFeedIDs(ctx, d.ID)
		mustNoErr(t, err)
		drainWake(s)
		mustNoErr(t, s.ScheduleIngest(ctx, ids))
		return len(s.wake) == 1
	}

	d := timedDigest(t, st, "Morning", -1, store.DigestSources{FeedIDs: []int64{f.ID}})
	if woke := schedule(d); woke || !next(f.ID).Equal(regular) {
		t.Errorf("digest without a time: next fetch %s (woke %v), want %s untouched", next(f.ID), woke, regular)
	}
	mustNoErr(t, st.SetDigestIngest(ctx, d.ID, sevenAM, true))
	if woke := schedule(d); !woke || !next(f.ID).Equal(localTime(1, 7, 0)) {
		t.Errorf("time set: next fetch %s (woke %v), want %s and a wake-up", next(f.ID).In(testZone), woke, localTime(1, 7, 0))
	}
	mustNoErr(t, st.SetDigestIngest(ctx, d.ID, tenAM, true))
	if woke := schedule(d); woke || !next(f.ID).Equal(localTime(1, 7, 0)) {
		t.Errorf("later time: next fetch %s (woke %v), want %s kept", next(f.ID).In(testZone), woke, localTime(1, 7, 0))
	}

	// A refresh asked for meanwhile is never pushed back.
	mustNoErr(t, s.Refresh(ctx, f.ID))
	schedule(d)
	if got := next(f.ID); !got.Equal(clock.Now()) {
		t.Errorf("refreshed feed: next fetch %s, want now %s", got, clock.Now())
	}

	// A feed moved into a folder that a timed digest names.
	folder, err := st.CreateFolder(ctx, "Morning reads")
	mustNoErr(t, err)
	timedDigest(t, st, "Early", sixAM, store.DigestSources{FolderIDs: []int64{folder.ID}})
	g := addFeed(t, st, "https://g.example/feed.xml", regular)
	mustNoErr(t, st.MoveFeed(ctx, g.ID, folder.ID))
	mustNoErr(t, s.ScheduleIngest(ctx, []int64{g.ID}))
	if got := next(g.ID); !got.Equal(localTime(1, 6, 0)) {
		t.Errorf("moved into the folder: next fetch %s, want %s", got.In(testZone), localTime(1, 6, 0))
	}

	// Feeds that no longer exist are skipped.
	mustNoErr(t, st.DeleteFeed(ctx, g.ID))
	mustNoErr(t, s.ScheduleIngest(ctx, []int64{g.ID, f.ID}))
}

// TestScheduleIngest_LeavesAFailureBackoffAlone: a failing feed may be
// waiting out a Retry-After, which is not stored, so a digest time set
// meanwhile does not move its next fetch; its next failure is capped again.
func TestScheduleIngest_LeavesAFailureBackoffAlone(t *testing.T) {
	const pastTheDigestTime = 3 * time.Hour // from 06:45, past 07:00
	st := openStore(t)
	ctx := t.Context()
	polledAt := localTime(1, 6, 45)
	f := addFeed(t, st, feedURL, polledAt)
	tooMany := &fetch.StatusError{URL: feedURL, StatusCode: http.StatusTooManyRequests, RetryAfter: pastTheDigestTime}
	s := newZonedScheduler(t, st, staticFetcher(nil, tooMany), newClock(polledAt))
	pollOnce(t, s, st, f.ID)
	want := polledAt.Add(pastTheDigestTime)
	if got := getFeed(t, st, f.ID).NextFetchAt; !got.Equal(want) {
		t.Fatalf("after the 429: next fetch %s, want %s", got.In(testZone), want)
	}

	d := timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FeedIDs: []int64{f.ID}})
	ids, err := st.DigestFeedIDs(ctx, d.ID)
	mustNoErr(t, err)
	drainWake(s)
	mustNoErr(t, s.ScheduleIngest(ctx, ids))
	if got := getFeed(t, st, f.ID).NextFetchAt; !got.Equal(want) || len(s.wake) != 0 {
		t.Errorf("digest time set: next fetch %s (woke %v), want %s untouched", got.In(testZone), len(s.wake) != 0, want)
	}
}

// TestReschedule_TheDigestTimeStaysAnUpperBound: an interval change never
// schedules a feed past its next ingestion time. A feed in failure backoff
// is left alone, as without digests.
func TestReschedule_TheDigestTimeStaysAnUpperBound(t *testing.T) {
	st := openStore(t)
	ctx := t.Context()
	f := addFeed(t, st, feedURL, t0)
	clock := newClock(localTime(1, 4, 0))
	s := newZonedScheduler(t, st, staticFetcher(notModified(feedURL), nil), clock)
	pollOnce(t, s, st, f.ID) // no digest yet: one interval
	next := func() time.Time { return getFeed(t, st, f.ID).NextFetchAt }

	timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FeedIDs: []int64{f.ID}})
	mustNoErr(t, st.SetFeedInterval(ctx, f.ID, int(2*config.DefaultPollInterval/time.Second)))
	mustNoErr(t, s.Reschedule(ctx, f.ID))
	if got, want := next(), localTime(1, 7, 0); !got.Equal(want) {
		t.Errorf("longer interval: next fetch %s, want the digest time %s", got.In(testZone), want)
	}
	mustNoErr(t, st.SetFeedInterval(ctx, f.ID, int(2*time.Hour/time.Second)))
	mustNoErr(t, s.Reschedule(ctx, f.ID))
	if got, want := next(), localTime(1, 6, 0); !got.Equal(want) {
		t.Errorf("2h interval: next fetch %s, want last fetch + 2h = %s", got.In(testZone), want)
	}

	// A failing feed keeps its backoff: it may be waiting out a Retry-After,
	// which is not stored. Its next failure is capped at the digest time
	// again (see TestPoll_FailureBackoffIsCappedAtTheDigestTime).
	backoff := localTime(2, 8, 0)
	mustNoErr(t, st.RecordFetchError(ctx, f.ID, "HTTP 503", clock.Now(), backoff))
	mustNoErr(t, s.Reschedule(ctx, f.ID))
	if got := next(); !got.Equal(backoff) {
		t.Errorf("failing feed: next fetch %s, want its backoff %s kept", got.In(testZone), backoff.In(testZone))
	}
}

// TestRun_DigestFeedsDueAtTheSameMinuteStayWithinFetchWorkers: a digest
// time makes all its feeds due at once; the pool still bounds the fetches,
// and each feed is fetched once.
func TestRun_DigestFeedsDueAtTheSameMinuteStayWithinFetchWorkers(t *testing.T) {
	const workers, feeds = 3, 10
	st := openStore(t)
	ctx := t.Context()
	folder, err := st.CreateFolder(ctx, "Morning reads")
	mustNoErr(t, err)
	for i := range feeds {
		f := addFeed(t, st, fmt.Sprintf("https://f%d.example/feed", i), localTime(1, 12, 0))
		mustNoErr(t, st.MoveFeed(ctx, f.ID, folder.ID))
	}
	d := timedDigest(t, st, "Morning", sevenAM, store.DigestSources{FolderIDs: []int64{folder.ID}})
	fetcher, release := gatedFetcher()
	defer release()
	clock := newClock(localTime(1, 6, 0))
	s := newTestScheduler(t, st, fetcher, workers)
	s.now, s.loc = clock.Now, testZone
	ids, err := st.DigestFeedIDs(ctx, d.ID)
	mustNoErr(t, err)
	mustNoErr(t, s.ScheduleIngest(ctx, ids))
	startRun(t, s)

	time.Sleep(100 * time.Millisecond)
	if n := fetcher.CallCount(); n != 0 {
		t.Fatalf("%d fetches before the digest time", n)
	}
	clock.Set(localTime(1, 7, 0))
	s.signal()
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
	if n, perURL := fetcher.CallCount(), fetcher.MaxPerURL(); n != feeds || perURL != 1 {
		t.Errorf("fetches = %d (at most %d at once per feed), want %d and 1", n, perURL, feeds)
	}
}

// TestRun_TimedDigestWithoutFeedsMakesNoRequest: a digest time alone never
// makes the scheduler fetch anything.
func TestRun_TimedDigestWithoutFeedsMakesNoRequest(t *testing.T) {
	st := openStore(t)
	timedDigest(t, st, "Empty", sevenAM, store.DigestSources{})
	fetcher := staticFetcher(nil, nil)
	s := newTestScheduler(t, st, fetcher, config.DefaultFetchWorkers)
	startRun(t, s)
	mustNoErr(t, s.ScheduleIngest(context.Background(), nil))
	time.Sleep(200 * time.Millisecond)
	if n := fetcher.CallCount(); n != 0 {
		t.Fatalf("fetches = %d with no feeds, want 0", n)
	}
}
