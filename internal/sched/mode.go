package sched

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Shorts modes. A subscription to a YouTube channel follows all its uploads,
// only its regular videos or only its Shorts; the mode is the feed URL (see
// resolve.Mode), so switching it swaps the URL in place. A mode feed that
// fails is a failure of that feed like any other (see cappedFailure): the
// channel feed is never fetched instead, since it carries what the mode
// leaves out.

// SetMode makes the YouTube channel subscription feedID follow its channel
// in mode m, from a fetch due at once (or, while YouTube is rate-limiting
// the feed, when its backoff ends; see rateLimitedFeed): the feed's URL
// becomes the mode's canonical feed URL and the feed is otherwise started afresh there as a
// pending subscription (see store.Store.ChangeFeedURL), so the first
// document of the new URL is stored as an initial add (newest
// config.InitialUnread unread, the rest read) instead of making every video
// the old URL did not carry unread. Stored entries, title, folder, icon and
// interval are kept. A feed already in mode m is left as it is.
//
// It returns the feed as stored, or an error wrapping store.ErrNotFound when
// the feed does not exist, store.ErrInvalid when m is not a mode or the feed
// does not follow a YouTube channel (see resolve.FeedMode), and
// store.ErrConflict when another subscription already follows the channel in
// mode m.
//
// The switch is written under idGuard, so a poll of the old URL that is still
// running stores nothing once it answers (see writeFeed). A feed held back
// because the outcome of its last poll could not be stored is released, as
// by Refresh.
func (s *Scheduler) SetMode(ctx context.Context, feedID int64, m resolve.Mode) (store.Feed, error) {
	if _, ok := resolve.ParseMode(string(m)); !ok {
		return store.Feed{}, fmt.Errorf("sched: set mode of feed %d: unknown mode %q: %w", feedID, m, store.ErrInvalid)
	}
	f, changed, err := s.changeMode(ctx, feedID, m)
	if err != nil || !changed {
		return f, err
	}
	s.unholdMu.Lock()
	s.unhold[feedID] = struct{}{}
	s.unholdMu.Unlock()
	s.signal()
	return f, nil
}

// changeMode is SetMode's write, under idGuard. changed reports whether the
// feed's URL changed.
func (s *Scheduler) changeMode(ctx context.Context, feedID int64, m resolve.Mode) (f store.Feed, changed bool, err error) {
	s.idGuard.Lock()
	defer s.idGuard.Unlock()
	f, err = s.st.GetFeed(ctx, feedID)
	if err != nil {
		return store.Feed{}, false, fmt.Errorf("sched: set mode of feed %d: %w", feedID, err)
	}
	id, cur, ok := resolve.FeedMode(f.URL)
	if !ok || f.Kind != store.KindYouTube {
		return store.Feed{}, false, fmt.Errorf("sched: set mode of feed %d: not a YouTube channel: %w", feedID, store.ErrInvalid)
	}
	if cur == m {
		return f, false, nil
	}
	to := resolve.ModeFeedURL(id, m)
	// While the host is rate-limiting the feed, the switch keeps the wait
	// and the failure that makes it one (inBackoff), so that a later
	// interval or digest change cannot move the fetch before it ends.
	next, keep := s.now(), false
	if s.rateLimitedFeed(f) && f.NextFetchAt.After(next) {
		next, keep = f.NextFetchAt, true // the mode lists are served by the same host
	}
	if err := s.st.ChangeFeedURL(ctx, f.ID, f.URL, to, next, keep); err != nil {
		return store.Feed{}, false, fmt.Errorf("sched: set mode of feed %d: %w", feedID, err)
	}
	if keep {
		s.rateMu.Lock()
		s.rateLimited[f.ID] = rateLimitMark{createdAt: f.CreatedAt, url: to}
		s.rateMu.Unlock()
	}
	s.log.Info("switched feed mode", "feed", f.ID, "from", redact(f.URL), "to", redact(to), "mode", m)
	if f, err = s.st.GetFeed(ctx, feedID); err != nil {
		return store.Feed{}, false, fmt.Errorf("sched: set mode of feed %d: %w", feedID, err)
	}
	return f, true, nil
}

// Rate limits. A switch normally makes the new URL due at once, but the
// mode lists are served by the same host as the channel feed: while that
// host is rate-limiting the feed, the switch keeps the next fetch the
// backoff (and any Retry-After) gave it. A feed counts as rate-limited while
// its last poll failed (inBackoff) with a 429, or with any status carrying
// a Retry-After. The scheduler remembers this in memory for the
// subscription it saw fail; after a restart a 429 is still recognised from
// the stored last error, a Retry-After on another status is not.

// rateLimitMark is the subscription (created_at and URL, as for manual
// marks) whose last failed poll was rate-limited.
type rateLimitMark struct {
	createdAt time.Time
	url       string
}

// stored429 matches the last error stored after a 429 (see
// fetch.StatusError and failureMessage). It is anchored: the rest of the
// message is the final URL, which a redirecting server chooses.
var stored429 = regexp.MustCompile(`^HTTP 429 from `)

// rateLimitFailure reports whether cause is a server's rate limit: a 429,
// or any status carrying a Retry-After.
func rateLimitFailure(cause error) bool {
	var se *fetch.StatusError
	return errors.As(cause, &se) && (se.StatusCode == http.StatusTooManyRequests || se.RetryAfter > 0)
}

// noteFailure records whether the failed poll of f, with cause, was
// rate-limited.
func (s *Scheduler) noteFailure(f store.Feed, cause error) {
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	if rateLimitFailure(cause) {
		s.rateLimited[f.ID] = rateLimitMark{createdAt: f.CreatedAt, url: f.URL}
		return
	}
	delete(s.rateLimited, f.ID)
}

// rateLimitedFeed reports whether f's last poll failed because its server
// is rate-limiting it (see noteFailure); a success since clears that.
func (s *Scheduler) rateLimitedFeed(f store.Feed) bool {
	if !inBackoff(f) {
		return false
	}
	if stored429.MatchString(f.LastError) {
		return true
	}
	s.rateMu.Lock()
	defer s.rateMu.Unlock()
	m, ok := s.rateLimited[f.ID]
	if ok && (!m.createdAt.Equal(f.CreatedAt) || m.url != f.URL) {
		delete(s.rateLimited, f.ID) // another subscription's
		return false
	}
	return ok
}
