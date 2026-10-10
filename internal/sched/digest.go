package sched

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/giacomomicoli/feedreader/internal/store"
)

// Digest ingestion times. A digest may have a fixed daily ingestion time, a
// minute of the day in the server's time zone: its feeds (those it names and
// those in the folders it names; tags have no feeds) are then fetched every
// day at that time, on top of their regular schedule. Whenever the scheduler
// stores a feed's next fetch (after a poll, a failure, a subscription or a
// changed interval), it is the earlier of the regular one and the feed's
// next ingestion time, so a failing feed is still tried once a day at that
// time, unless the server asked it to wait longer with Retry-After: that
// stays a lower bound (see recordFailure). After a poll or a subscription,
// that is the first ingestion time after the request was sent (for
// Subscribe, maybe a Preview's from minutes before, whose fetch it stores),
// so a time that passed since leaves the feed due at once. It is computed
// for the feed's folder and the digests as read just before the store call,
// and checked against them again right after it, since a change can commit
// in between (see ingestAfterPoll and recheckIngest). When a digest's time
// or sources change, or a feed moves to another folder, ScheduleIngest
// moves the affected fetches earlier. It and Reschedule leave a feed in
// failure backoff alone, since the Retry-After it may be waiting out is not
// stored; its next failure is capped at the ingestion time again.
//
// The next ingestion time is at most a day and an hour away (a fall-back
// day has 25 hours) but never later than the regular next fetch it caps, so
// the guard against schedules stored while the clock was ahead
// (rescheduleFuture) still holds.

// minutesPerHour converts a minute of the day to a clock time.
const minutesPerHour = int(time.Hour / time.Minute)

// NextIngest returns the first instant strictly after now at which the clock
// in loc shows minute, a minute of the day (0 to store.MinutesPerDay-1).
//
// Clock times are built with time.Date, which normalizes those that a
// daylight saving change skips or repeats, so a digest is still ingested
// once a day. Which way depends on the zone: a skipped time moves by the
// length of the change, earlier in some zones and later in others (02:30
// becomes 01:30 standard time in New York, 03:30 summer time in Rome), and
// a repeated time counts at its first occurrence in some zones and at its
// second in others (New York and Rome again). A skipped time close to
// midnight can thus fall on the day before or after; the result is still
// always after now.
func NextIngest(now time.Time, minute int, loc *time.Location) time.Time {
	l := now.In(loc)
	h, m := minute/minutesPerHour, minute%minutesPerHour
	at := time.Date(l.Year(), l.Month(), l.Day(), h, m, 0, 0, loc)
	// Each step adds a day, so this ends after at most a few steps.
	for day := 1; !at.After(now); day++ {
		at = time.Date(l.Year(), l.Month(), l.Day()+day, h, m, 0, 0, loc)
	}
	return at
}

// nextIngest returns the earliest ingestion time strictly after `after` of
// the digests that name feedID or folderID (0 = none), or the zero time when
// none of them has one.
func (s *Scheduler) nextIngest(ctx context.Context, feedID, folderID int64, after time.Time) (time.Time, error) {
	minutes, err := s.st.IngestMinutes(ctx, feedID, folderID)
	if err != nil {
		return time.Time{}, err
	}
	var next time.Time
	for _, m := range minutes {
		next = earliest(next, NextIngest(after, m, s.loc))
	}
	return next, nil
}

// pollIngest is nextIngest for a feed being polled or subscribed. A store
// error is logged and treated as no ingestion time: the feed keeps its
// regular schedule, and the next poll looks again.
func (s *Scheduler) pollIngest(ctx context.Context, feedID, folderID int64, after time.Time) time.Time {
	next, err := s.nextIngest(ctx, feedID, folderID, after)
	if err != nil && ctx.Err() == nil {
		s.log.Warn("could not read the digest ingestion times of a feed", "feed", feedID, "err", err)
	}
	return next
}

// earliest returns the earlier of a and b, where the zero time means none.
func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}

// ScheduleIngest brings the next fetch of each of feedIDs forward to its
// next digest ingestion time when that is earlier. Call it after a digest's
// ingestion time or sources changed, with the digest's feeds
// (store.DigestFeedIDs), and after a feed moved to another folder. It never
// moves a fetch later, so an earlier one (the regular schedule, a Refresh)
// is kept, and it wakes the dispatcher when it moved one. Feeds that no
// longer exist are skipped, and so are feeds in failure backoff, which may
// be waiting out a Retry-After (see recordFailure).
func (s *Scheduler) ScheduleIngest(ctx context.Context, feedIDs []int64) error {
	now := s.now()
	for _, id := range feedIDs {
		f, err := s.st.GetFeed(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return fmt.Errorf("sched: schedule the digest ingestion of feed %d: %w", id, err)
		}
		if f.ErrorCount > 0 {
			continue
		}
		at, err := s.nextIngest(ctx, f.ID, f.FolderID, now)
		if err == nil {
			err = s.advance(ctx, f, at)
		}
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("sched: schedule the digest ingestion of feed %d: %w", id, err)
		}
	}
	return nil
}

// advance moves f's next fetch to at when at is earlier than the stored
// one, and wakes the dispatcher if it did. A zero at changes nothing. The
// write happens only while f is still the same subscription (writeFeed) and
// only if no earlier fetch was stored meanwhile (store.AdvanceNextFetch).
func (s *Scheduler) advance(ctx context.Context, f store.Feed, at time.Time) error {
	if at.IsZero() || !at.Before(f.NextFetchAt) {
		return nil
	}
	var moved bool
	err := s.writeFeed(ctx, f, func(store.Feed) (err error) {
		moved, err = s.st.AdvanceNextFetch(ctx, f.ID, at)
		return err
	})
	if err != nil {
		return err
	}
	if moved {
		s.log.Debug("moved a feed's next fetch earlier", "feed", f.ID, "next_fetch", at)
		s.signal()
	}
	return nil
}

// recheckIngest closes the gap between reading a feed's folder and ingestion
// times and storing the next fetch computed from them, which the store call
// does in a transaction of its own. Call it under idGuard right after that
// store call succeeded (in writeFeed's record, or where Subscribe creates the
// feed), with next, the next fetch the caller computes for a row of the
// feed. It reads the feed's row and ingestion times again and moves the
// stored next fetch to next(row) when that is earlier.
//
// A change committed between the first read and the store call (a move to
// another folder, a digest's time or sources saved) is then seen here: the
// ScheduleIngest that follows it left the feed alone, as it was due (being
// polled), not created yet, or in failure backoff, and the store call wrote
// a next fetch computed without it. A change committed after this read is
// left to ScheduleIngest, which then reads the row as the store call left
// it (and leaves a feed in failure backoff alone, as always).
//
// It returns the next fetch it stored, or the zero time when it moved none.
// A store error is logged, not returned: the outcome is stored, and the
// feed keeps the next fetch it was given.
func (s *Scheduler) recheckIngest(ctx context.Context, id int64, next func(cur store.Feed) time.Time) time.Time {
	cur, err := s.st.GetFeed(ctx, id)
	if err != nil {
		s.warnRecheck(ctx, id, err)
		return time.Time{}
	}
	at := next(cur)
	if at.IsZero() || !at.Before(cur.NextFetchAt) {
		return time.Time{}
	}
	moved, err := s.st.AdvanceNextFetch(ctx, id, at)
	if err != nil {
		s.warnRecheck(ctx, id, err)
		return time.Time{}
	}
	if !moved {
		return time.Time{}
	}
	s.log.Debug("moved a feed's next fetch earlier after a folder or digest change", "feed", id, "next_fetch", at)
	s.signal()
	return at
}

// warnRecheck logs a store error of recheckIngest, unless the request ended
// or the feed was deleted meanwhile.
func (s *Scheduler) warnRecheck(ctx context.Context, id int64, err error) {
	if ctx.Err() == nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("could not check a feed's next fetch against its digest ingestion times", "feed", id, "err", err)
	}
}
