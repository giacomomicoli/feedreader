package sched

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/parse"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// maxErrorRunes bounds the last_error message stored for a feed; it is shown
// in the feed status view, not meant to be a full log line.
const maxErrorRunes = 200

// errNoResult guards against a Fetcher that returns neither a result nor an
// error.
var errNoResult = errors.New("fetch returned no response")

// poll fetches one feed and records the outcome:
//
//   - 304: last fetch time, error count reset, next poll in one interval
//     (raised by the <ttl> stored from the last document and max-age);
//   - 200: the document is parsed and its entries upserted, new ones
//     unread; validators and feed metadata are refreshed;
//   - after a stored 304 or 200, a YouTube channel feed that still has no
//     icon gets its channel's avatar (see backfillAvatar);
//   - 301/308 ending in a 304 or a parseable feed: the stored URL follows
//     the permanent redirect (kept when another feed already uses the new
//     URL). A redirect to anything else is recorded as that failure and the
//     stored URL is kept, so a parked domain or a maintenance page cannot
//     replace the feed's address for good;
//   - anything else (HTTP error status, network, size cap, redirect loop,
//     unparseable or non-feed body): error_count++ and last_error, with the
//     next poll delayed by failureDelay. Entries are never touched.
//
// A fetch cut short by ctx (shutdown) records nothing, and so does the poll
// of a feed that was unsubscribed while it was being fetched, even when a
// new subscription has taken its id (see writeFeed). poll returns a non-zero
// time when the outcome could not be stored; see outcome.holdUntil.
func (s *Scheduler) poll(ctx context.Context, f store.Feed) (holdUntil time.Time) {
	base := s.baseInterval(f)
	started := s.now()
	res, err := s.fetcher.Fetch(ctx, fetch.Request{
		URL:          f.URL,
		ETag:         f.ETag,
		LastModified: f.LastModified,
		Accept:       fetch.FeedAccept,
	})
	if ctx.Err() != nil {
		return time.Time{} // our own shutdown, not the feed's fault
	}
	fetchedAt := s.now()
	if err == nil && res == nil {
		err = errNoResult
	}
	if err != nil {
		return s.recordFailure(ctx, f, fetchedAt, base, err)
	}
	s.log.Debug("fetched feed", "feed", f.ID, "url", redact(f.URL), "status", res.StatusCode,
		"bytes", len(res.Body), "took", fetchedAt.Sub(started))

	redirected := res.PermanentURL != "" && res.PermanentURL != f.URL
	if res.NotModified {
		// The new location accepted the stored validators: same feed.
		if redirected {
			f.URL = s.followPermanentRedirect(ctx, f, res.PermanentURL)
		}
		next := fetchedAt.Add(withHints(base, storedTTL(f), res.MaxAge))
		err := s.writeFeed(ctx, f, func(cur store.Feed) error {
			return s.st.RecordNotModified(ctx, f.ID, fetchedAt, keepRequested(f, cur, next))
		})
		if err == nil {
			s.backfillAvatar(ctx, f)
		}
		return s.checkStored(ctx, f, next, err)
	}

	doc, err := parse.Parse(res.Body, res.ContentType, cmp.Or(res.FinalURL, f.URL))
	if err != nil {
		if redirected {
			err = fmt.Errorf("moved permanently to %s: %w", redact(res.PermanentURL), err)
		}
		return s.recordFailure(ctx, f, fetchedAt, base, err)
	}
	if redirected {
		f.URL = s.followPermanentRedirect(ctx, f, res.PermanentURL)
	}
	next := fetchedAt.Add(withHints(base, doc.TTL, res.MaxAge))
	var inserted int
	err = s.writeFeed(ctx, f, func(cur store.Feed) (err error) {
		inserted, err = s.st.RecordFetchSuccess(ctx, store.FetchSuccess{
			FeedID:        f.ID,
			FetchedAt:     fetchedAt,
			NextFetchAt:   keepRequested(f, cur, next),
			ETag:          res.ETag,
			LastModified:  res.LastModified,
			TTLSec:        ttlSec(doc.TTL),
			SiteURL:       doc.SiteURL,
			IconURL:       doc.IconURL,
			OriginalTitle: doc.Title,
			Entries:       newEntries(doc.Entries, fetchedAt),
		})
		return err
	})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, store.ErrNotFound) {
			return s.checkStored(ctx, f, next, err)
		}
		s.log.Error("could not store fetched entries", "feed", f.ID, "err", err)
		return s.recordFailure(ctx, f, fetchedAt, base, fmt.Errorf("could not save entries: %w", err))
	}
	if inserted > 0 {
		s.log.Info("new entries", "feed", f.ID, "title", f.Title, "count", inserted)
	}
	if doc.IconURL == "" {
		s.backfillAvatar(ctx, f)
	}
	return time.Time{}
}

// recordFailure stores a failed poll: error_count + 1, last_error, and the
// next poll after the backoff delay (honouring Retry-After).
func (s *Scheduler) recordFailure(ctx context.Context, f store.Feed, fetchedAt time.Time, base time.Duration, cause error) time.Time {
	errorCount := f.ErrorCount + 1
	next := fetchedAt.Add(failureDelay(base, errorCount, retryAfter(cause)))
	msg := failureMessage(cause)
	s.log.Warn("feed fetch failed", "feed", f.ID, "url", redact(f.URL), "err", msg,
		"consecutive_failures", errorCount, "next_fetch", next)
	return s.checkStored(ctx, f, next, s.writeFeed(ctx, f, func(cur store.Feed) error {
		return s.st.RecordFetchError(ctx, f.ID, msg, fetchedAt, keepRequested(f, cur, next))
	}))
}

// writeFeed runs record, a store call that writes the outcome of polling f,
// only if f is still subscribed, passing it the feed's current row.
// Otherwise it returns an error wrapping store.ErrNotFound, also when a
// subscription created since f was read has been given f's id: the row is f
// only if its created_at and URL are f's (created_at alone has one-second
// precision). The check and the write happen under idGuard, so no feed can
// be created in between.
func (s *Scheduler) writeFeed(ctx context.Context, f store.Feed, record func(cur store.Feed) error) error {
	s.idGuard.RLock()
	defer s.idGuard.RUnlock()
	cur, err := s.st.GetFeed(ctx, f.ID)
	if err != nil {
		return err
	}
	if !cur.CreatedAt.Equal(f.CreatedAt) || cur.URL != f.URL {
		return fmt.Errorf("feed %d was replaced by a new subscription: %w", f.ID, store.ErrNotFound)
	}
	return record(cur)
}

// keepRequested returns next, the schedule a poll of f computed, unless the
// feed's stored next fetch (cur) was moved earlier while f was being
// fetched: a Refresh, or a shorter interval, asked for a fetch after this one
// had started. Overwriting that would silently drop the request, so the
// earlier time is kept and the feed is fetched again once this poll is done.
// Stored times have one-second precision, so a refresh made within the
// second of the one that started this fetch is absorbed by it.
func keepRequested(f, cur store.Feed, next time.Time) time.Time {
	if !cur.NextFetchAt.Equal(f.NextFetchAt) && cur.NextFetchAt.Before(next) {
		return cur.NextFetchAt
	}
	return next
}

// checkStored interprets the error of the store call that recorded a poll's
// outcome, returning the time the feed must be held until when the new
// schedule (next) was not saved.
func (s *Scheduler) checkStored(ctx context.Context, f store.Feed, next time.Time, err error) time.Time {
	switch {
	case err == nil, ctx.Err() != nil:
		return time.Time{}
	case errors.Is(err, store.ErrNotFound):
		s.log.Debug("feed removed while it was being fetched", "feed", f.ID)
		return time.Time{}
	}
	s.log.Error("could not store fetch outcome", "feed", f.ID, "err", err, "hold_until", next)
	return next
}

// followPermanentRedirect stores the feed's new URL after a 301/308 and
// returns the URL the feed now has. When another subscription already uses
// that URL the old one is kept, so the two feeds and their entries stay
// separate.
func (s *Scheduler) followPermanentRedirect(ctx context.Context, f store.Feed, newURL string) string {
	err := s.writeFeed(ctx, f, func(store.Feed) error { return s.st.UpdateFeedURL(ctx, f.ID, newURL) })
	switch {
	case err == nil:
		s.log.Info("feed moved permanently", "feed", f.ID, "from", redact(f.URL), "to", redact(newURL))
		return newURL
	case errors.Is(err, store.ErrConflict):
		s.log.Warn("feed moved to a URL that is already subscribed; keeping the old URL",
			"feed", f.ID, "url", redact(f.URL), "moved_to", redact(newURL))
	case ctx.Err() == nil && !errors.Is(err, store.ErrNotFound):
		s.log.Error("could not store the feed's new URL", "feed", f.ID, "moved_to", redact(newURL), "err", err)
	}
	return f.URL
}

// retryAfter extracts the server's Retry-After from a fetch error.
func retryAfter(err error) time.Duration {
	var se *fetch.StatusError
	if errors.As(err, &se) {
		return se.RetryAfter
	}
	return 0
}

// failureMessage turns a poll error into the short, single-line last_error
// shown in the feed status view.
func failureMessage(err error) string {
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if utf8.RuneCountInString(msg) <= maxErrorRunes {
		return msg
	}
	r := []rune(msg)
	return strings.TrimSpace(string(r[:maxErrorRunes-1])) + "…"
}

// newEntries converts parsed entries for the store, applying the fallback
// for a missing published date: updated, else fetchedAt.
func newEntries(list []parse.Entry, fetchedAt time.Time) []store.NewEntry {
	out := make([]store.NewEntry, 0, len(list))
	for _, e := range list {
		if e.GUID == "" {
			continue // parse always derives one; the store would reject the batch
		}
		published := e.PublishedAt
		if published.IsZero() {
			published = e.UpdatedAt
		}
		if published.IsZero() {
			published = fetchedAt
		}
		out = append(out, store.NewEntry{
			GUID:         e.GUID,
			URL:          e.URL,
			Title:        e.Title,
			SummaryHTML:  e.SummaryHTML,
			Author:       e.Author,
			ThumbnailURL: e.ThumbnailURL,
			PublishedAt:  published,
			UpdatedAt:    e.UpdatedAt,
		})
	}
	return out
}

// redact hides the password of a URL for logs and error messages.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	return u.Redacted()
}
