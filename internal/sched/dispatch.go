package sched

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Dispatcher tuning. These are implementation details, not documented
// defaults, so they live here rather than in internal/config.
const (
	// storeRetryDelay is how long the dispatcher waits before reading the
	// schedule again after a store error.
	storeRetryDelay = 30 * time.Second
	// maxSleep bounds a single sleep so that a wall-clock jump (NTP step,
	// host suspend) delays polling by at most this much. Waking up only
	// re-reads the schedule; it never fetches a feed that is not due.
	maxSleep = 5 * time.Minute
)

// outcome is what a fetch worker reports when it is done with a feed.
type outcome struct {
	feedID int64
	// holdUntil is non-zero when the feed's new schedule could not be
	// stored. The feed then still looks due in the store, so the dispatcher
	// must not hand it out again before holdUntil: an unwritable database
	// must not turn into a tight fetch loop against the feed's server.
	holdUntil time.Time
}

// dispatcher is the state of one Run: the feeds being fetched and those
// held back after a failed store write. Only the Run goroutine touches it.
type dispatcher struct {
	s        *Scheduler
	workers  int
	inFlight map[int64]struct{}
	holds    map[int64]time.Time
	// wall is the wall-clock time the previous iteration read; reading an
	// earlier one means the clock was set back.
	wall time.Time
	// rescan is set when the clock was set back, until the schedules stored
	// while it was ahead have been brought back (see rescheduleFuture).
	rescan bool
	// done receives one outcome per started fetch. Its capacity equals the
	// pool size and at most that many fetches are unaccounted for, so
	// workers never block on it, not even after the loop has stopped.
	done chan outcome
	wg   sync.WaitGroup
}

func newDispatcher(s *Scheduler) *dispatcher {
	return &dispatcher{
		s:        s,
		workers:  s.cfg.FetchWorkers,
		inFlight: make(map[int64]struct{}),
		holds:    make(map[int64]time.Time),
		done:     make(chan outcome, s.cfg.FetchWorkers),
	}
}

// run dispatches due feeds until ctx is done, then waits for the fetches
// still in flight.
func (d *dispatcher) run(ctx context.Context) {
	defer d.wg.Wait()
	for ctx.Err() == nil {
		now := d.s.now()
		delay, timed := storeRetryDelay, true
		if d.dispatchDue(ctx, now) {
			delay, timed = d.nextWake(ctx, now)
		}
		d.wait(ctx, delay, timed)
	}
}

// dispatchDue starts a fetch for every due feed that is neither in flight nor
// held, oldest next_fetch_at first, while the pool has room. It reports
// false when the due feeds could not be read; the caller then retries after
// storeRetryDelay, since those feeds would not show up in NextFetchAfter.
func (d *dispatcher) dispatchDue(ctx context.Context, now time.Time) bool {
	d.releaseHolds(now)
	d.checkClock(ctx, now)
	if d.full() {
		return true
	}
	due, err := d.s.st.DueFeeds(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			d.s.log.Warn("could not read due feeds", "err", err)
		}
		return false
	}
	for _, f := range due {
		if d.full() {
			break
		}
		if !d.busy(f.ID) {
			d.start(ctx, f)
		}
	}
	return true
}

// releaseHolds drops the holds that have expired at now and those of the
// feeds refreshed since the last call. A refresh may lift a hold because it
// has just written the feed's schedule: the store is writable again.
func (d *dispatcher) releaseHolds(now time.Time) {
	refreshed, all := d.s.takeUnholds()
	for id, until := range d.holds {
		if _, ok := refreshed[id]; ok || all || !until.After(now) {
			delete(d.holds, id)
		}
	}
}

// checkClock notices when the wall clock has been set back since the
// previous iteration, and then brings back the schedules stored while it was
// ahead, retrying on later iterations until that succeeds.
func (d *dispatcher) checkClock(ctx context.Context, now time.Time) {
	wall := now.Round(0) // drop the monotonic reading: only the wall clock steps
	if wall.Before(d.wall) {
		d.rescan = true
	}
	d.wall = wall
	if !d.rescan {
		return
	}
	if err := d.s.rescheduleFuture(ctx, now); err != nil {
		if ctx.Err() == nil {
			d.s.log.Warn("could not reschedule feeds after the clock was set back", "err", err)
		}
		return
	}
	d.rescan = false
}

// nextWake returns how long to sleep before the next feed falls due or a
// hold expires. timed is false when nothing is scheduled (or the pool is
// full): the dispatcher then waits for a wake-up or a finished fetch only.
//
// Feeds that are due but in flight or held are excluded (NextFetchAfter
// only reports times strictly after now), so this never busy-loops.
func (d *dispatcher) nextWake(ctx context.Context, now time.Time) (delay time.Duration, timed bool) {
	if d.full() {
		return 0, false
	}
	next, ok, err := d.s.st.NextFetchAfter(ctx, now)
	if err != nil {
		if ctx.Err() == nil {
			d.s.log.Warn("could not read the schedule", "err", err)
		}
		return storeRetryDelay, true
	}
	for _, until := range d.holds {
		if !ok || until.Before(next) {
			next, ok = until, true
		}
	}
	if !ok {
		return 0, false
	}
	return min(max(next.Sub(now), 0), maxSleep), true
}

// wait blocks until ctx is done, a wake-up arrives, a fetch finishes or, when
// timed, delay has passed. Finished fetches are all accounted for.
func (d *dispatcher) wait(ctx context.Context, delay time.Duration, timed bool) {
	var timeout <-chan time.Time
	if timed {
		t := time.NewTimer(delay)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-ctx.Done():
	case <-d.s.wake:
	case o := <-d.done:
		d.finish(o)
	case <-timeout:
	}
	for {
		select {
		case o := <-d.done:
			d.finish(o)
		default:
			return
		}
	}
}

// start hands f to a new fetch worker.
func (d *dispatcher) start(ctx context.Context, f store.Feed) {
	d.inFlight[f.ID] = struct{}{}
	d.wg.Go(func() {
		d.done <- outcome{feedID: f.ID, holdUntil: d.s.safePoll(ctx, f)}
	})
}

// finish records a worker's outcome.
func (d *dispatcher) finish(o outcome) {
	delete(d.inFlight, o.feedID)
	if !o.holdUntil.IsZero() {
		d.holds[o.feedID] = o.holdUntil
	}
}

func (d *dispatcher) full() bool { return len(d.inFlight) >= d.workers }

func (d *dispatcher) busy(id int64) bool {
	if _, ok := d.inFlight[id]; ok {
		return true
	}
	_, held := d.holds[id]
	return held
}

// safePoll runs poll, turning a panic into a logged error so that one bad
// feed cannot take the scheduler down. The feed is then held for one
// interval.
func (s *Scheduler) safePoll(ctx context.Context, f store.Feed) (holdUntil time.Time) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("feed poll panicked", "feed", f.ID, "url", redact(f.URL), "panic", r)
			holdUntil = s.now().Add(s.baseInterval(f))
		}
	}()
	return s.poll(ctx, f)
}

// stagger spreads the feeds that are overdue at startup evenly across
// config.StartupStagger, in next_fetch_at order, so they do not all fire at
// once. The first one stays due immediately. Feeds scheduled
// further ahead than the scheduler ever sets (see rescheduleFuture) count as
// overdue.
func (s *Scheduler) stagger(ctx context.Context) error {
	now := s.now()
	if err := s.rescheduleFuture(ctx, now); err != nil {
		return err
	}
	due, err := s.st.DueFeeds(ctx, now)
	if err != nil {
		return fmt.Errorf("sched: stagger: %w", err)
	}
	for i, at := range staggerTimes(now, len(due), config.StartupStagger) {
		err := s.st.SetNextFetch(ctx, due[i].ID, at)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("sched: stagger feed %d: %w", due[i].ID, err)
		}
	}
	if len(due) > 0 {
		s.log.Info("staggered overdue feeds", "count", len(due), "window", config.StartupStagger)
	}
	return nil
}

// rescheduleFuture makes due at now every feed whose next fetch lies further
// ahead than poll and Subscribe ever schedule one: the longest of its
// baseInterval, both kinds' default intervals and config.MaxBackoff from
// now. (YouTube feeds were scheduled with the general default before they
// had their own, so a schedule within the longer default is kept.) A time
// further ahead was stored while the wall clock was ahead (bad RTC, NTP
// step, VM restore); trusted as it is, the feed would not be polled again
// until the clock caught up.
func (s *Scheduler) rescheduleFuture(ctx context.Context, now time.Time) error {
	feeds, err := s.st.ListFeeds(ctx)
	if err != nil {
		return fmt.Errorf("sched: find feeds scheduled too far ahead: %w", err)
	}
	n := 0
	for _, f := range feeds {
		limit := max(s.baseInterval(f), s.cfg.PollInterval, s.cfg.PollIntervalYouTube, config.MaxBackoff)
		if !f.NextFetchAt.After(now.Add(limit)) {
			continue
		}
		err := s.st.SetNextFetch(ctx, f.ID, now)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("sched: reschedule feed %d: %w", f.ID, err)
		}
		n++
	}
	if n > 0 {
		s.log.Warn("rescheduled feeds whose next fetch was set while the clock was ahead", "count", n)
	}
	return nil
}
