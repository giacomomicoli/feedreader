package sched

import (
	"time"

	"github.com/giacomomicoli/feedreader/internal/store"
)

// Manual refreshes. A Refresh or RefreshAll click marks the feeds it
// schedules; the dispatcher takes a feed's mark when it starts the feed's
// next fetch (dispatcher.start, the only place a poll begins), so a fetch
// counts as manual exactly when a click asked for it, without the poll
// reading shared state. A manual fetch that fails stores its error but does
// not count as one more failure in a row, and never moves the feed's next
// fetch later than it was before the click (see recordFailure): clicking
// Refresh on a feed that is down must not push it into a longer backoff.
//
// A mark stays, flagged taken, while its fetch runs, and is dropped when
// that fetch is done (dispatcher.finish). A click that arrives meanwhile
// re-arms it for the fetch that follows (see keepRequested), keeping the
// next fetch from before the first click: it reads the schedule the first
// click wrote, which says nothing about where the feed stood before.
//
// Marks live in memory only: one pending when the process stops is lost,
// and that fetch then counts as a scheduled one.

// manualMarkTTL bounds how long a mark waits for its fetch. A refreshed feed
// is due at once, so its mark is normally taken within moments; one older
// than this (the scheduler was not running, or the feed was removed) is
// dropped rather than applied to a fetch nobody asked for. It is an
// implementation detail, not a documented default.
const manualMarkTTL = time.Hour

// manualRefresh is a mark: the feed it was set for (id, created_at and URL,
// so that a new subscription given a deleted feed's id never takes it), its
// next fetch before the click, and when the click happened.
type manualRefresh struct {
	createdAt time.Time
	url       string
	prevNext  time.Time
	markedAt  time.Time
	// taken is set while the fetch the mark was taken for runs.
	taken bool
	// gen tells this setting of the mark from any other (see restoreManual):
	// two marks of the same feed set at the same clock reading are otherwise
	// equal.
	gen uint64
}

// markManual marks f's next fetch as manual and returns undo, which puts the
// marks back as they were (for a click that then failed to schedule the
// fetch). A feed already marked keeps its mark, and with it the next fetch
// from before the first click; a mark whose fetch is running is re-armed for
// the next fetch. Expired marks are dropped on the way, so the map never
// holds more than the feeds refreshed within manualMarkTTL.
func (s *Scheduler) markManual(f store.Feed) (undo func()) {
	now := s.now()
	s.manualMu.Lock()
	defer s.manualMu.Unlock()
	for id, m := range s.manual {
		if expired(m, now) {
			delete(s.manual, id)
		}
	}
	m, ok := s.manual[f.ID]
	switch {
	case ok && sameFeed(m, f) && !m.taken:
		return func() {}
	case ok && sameFeed(m, f):
		prev := m
		s.manualGen++
		m.taken, m.markedAt, m.gen = false, now, s.manualGen
		s.manual[f.ID] = m
		return func() { s.restoreManual(f.ID, m.gen, &prev) }
	}
	s.manualGen++
	m = manualRefresh{createdAt: f.CreatedAt, url: f.URL, prevNext: f.NextFetchAt, markedAt: now, gen: s.manualGen}
	s.manual[f.ID] = m
	return func() { s.restoreManual(f.ID, m.gen, nil) }
}

// restoreManual undoes markManual: when id still has the mark of
// generation gen, untaken, it is replaced by prev, or removed when nil. Any
// other mark (taken since, or set by a later click) is left alone.
func (s *Scheduler) restoreManual(id int64, gen uint64, prev *manualRefresh) {
	s.manualMu.Lock()
	defer s.manualMu.Unlock()
	if cur, ok := s.manual[id]; !ok || cur.gen != gen || cur.taken {
		return
	}
	if prev == nil {
		delete(s.manual, id)
		return
	}
	s.manual[id] = *prev
}

// takeManual flags f's mark taken and returns a copy, or nil when its next
// fetch was not asked for by a click (no mark, an expired one, one set for
// another subscription that had the same id, or one already taken by a
// fetch that never finished; those are dropped).
func (s *Scheduler) takeManual(f store.Feed) *manualRefresh {
	now := s.now()
	s.manualMu.Lock()
	defer s.manualMu.Unlock()
	m, ok := s.manual[f.ID]
	if !ok {
		return nil
	}
	if m.taken || expired(m, now) || !sameFeed(m, f) {
		delete(s.manual, f.ID)
		return nil
	}
	m.taken = true
	s.manual[f.ID] = m
	return &m
}

// finishManual drops the mark of feed id once the fetch that took it is
// done, unless a click re-armed it meanwhile.
func (s *Scheduler) finishManual(id int64) {
	s.manualMu.Lock()
	defer s.manualMu.Unlock()
	if m, ok := s.manual[id]; ok && m.taken {
		delete(s.manual, id)
	}
}

// manualCount is the number of marks held.
func (s *Scheduler) manualCount() int {
	s.manualMu.Lock()
	defer s.manualMu.Unlock()
	return len(s.manual)
}

// expired reports whether m is too old to apply at now. A mark from the
// future (the clock was set back) has expired too.
func expired(m manualRefresh, now time.Time) bool {
	age := now.Sub(m.markedAt)
	return age < 0 || age > manualMarkTTL
}

// sameFeed reports whether m was set for f.
func sameFeed(m manualRefresh, f store.Feed) bool {
	return m.createdAt.Equal(f.CreatedAt) && m.url == f.URL
}

// inBackoff reports whether f's last fetch failed, so that its next fetch
// may be a failure backoff or a server's Retry-After, which Reschedule and
// ScheduleIngest must not shorten. A failed manual fetch leaves error_count
// alone but stores last_error, which every success and 304 clears.
func inBackoff(f store.Feed) bool {
	return f.ErrorCount > 0 || f.LastError != ""
}

// manualNext bounds the next fetch after a failed manual fetch of a feed
// whose next fetch was m.prevNext before the click: never later than that,
// when it was still ahead at fetchedAt. A feed that was already due gets
// next as computed.
func manualNext(m *manualRefresh, next, fetchedAt time.Time) time.Time {
	if m == nil || !m.prevNext.After(fetchedAt) {
		return next
	}
	return earliest(next, m.prevNext)
}
