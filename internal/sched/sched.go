// Package sched runs the polling scheduler: a dispatcher that
// watches next_fetch_at, a bounded worker pool (at most cfg.FetchWorkers
// concurrent polls, at most one in flight per feed), startup staggering,
// failure backoff, and the add-feed pipeline (preview + subscribe).
//
// The add flow's fetch happens immediately, so it does not queue
// behind the polling pool: it has its own addFetchSlots, and the scheduler
// never runs more than cfg.FetchWorkers + addFetchSlots fetches at once.
package sched

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Fetcher is the subset of *fetch.Client the scheduler needs.
type Fetcher interface {
	Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error)
}

// Preview is what the add-feed confirmation form shows after the immediate
// first fetch.
type Preview struct {
	FeedURL    string // final feed URL (after permanent redirects)
	Kind       store.Kind
	Title      string // feed title, used to pre-fill the editable title
	SiteURL    string
	EntryCount int
}

// Subscription is the confirmed add-feed form.
type Subscription struct {
	FeedURL  string
	Title    string // user-edited; "" = use the feed's own title
	FolderID int64  // 0 = uncategorized
	// IconURL is the icon the resolver found for the feed (a YouTube channel's
	// avatar), stored when the feed has none of its own and it is an http(s)
	// URL; "" = none.
	IconURL string
}

// addFetchSlots bounds the add flow's concurrent fetches (Preview, and
// Subscribe without a cached preview). It is an implementation detail, not a
// documented default: one user adds feeds one at a time, so two slots leave
// room for a second tab without letting the add form multiply outbound
// requests.
const addFetchSlots = 2

// errAlreadyRunning is returned by a Run call that overlaps another one.
var errAlreadyRunning = errors.New("sched: Run is already running")

// Scheduler polls feeds and implements the add-feed pipeline.
//
// One dispatcher goroutine (Run) reads the schedule from the store and hands
// due feeds to at most cfg.FetchWorkers concurrent fetches, never two of the
// same feed at once. It sleeps until the earliest of: the next next_fetch_at,
// a wake-up from Refresh, RefreshAll or Subscribe, or a fetch finishing. With
// no feeds it waits for a wake-up only, so nothing is ever fetched before the
// first subscription.
//
// The methods are safe for concurrent use. Preview and Subscribe work
// whether or not Run is active.
type Scheduler struct {
	st      store.Store
	fetcher Fetcher
	cfg     config.Config
	log     *slog.Logger
	now     func() time.Time // clock; replaced in tests

	wake    chan struct{} // capacity 1: pending wake-ups coalesce
	running atomic.Bool
	cache   *previewCache

	// addSlots is a semaphore of addFetchSlots for the add flow's fetches.
	addSlots chan struct{}

	// idGuard stops a poll from writing into a subscription created while
	// its feed was being fetched: SQLite gives a new feed the id of a deleted
	// one when that was the highest. Poll writes hold the read lock while
	// they check that the row is still the feed that was fetched and write
	// it; Subscribe, the only code that creates feeds, does so under the
	// write lock, so no id can be reused between that check and the write.
	idGuard sync.RWMutex

	// unhold carries Refresh and RefreshAll requests to the dispatcher, which
	// then lifts the matching holds (see outcome.holdUntil).
	unholdMu  sync.Mutex
	unhold    map[int64]struct{}
	unholdAll bool

	// avatarTried holds the channel feed URLs whose page a poll has fetched
	// for an avatar in this run (see backfillAvatar).
	avatarMu    sync.Mutex
	avatarTried map[string]struct{}
}

// New creates a scheduler. It makes no HTTP request until Run is called and
// at least one feed exists.
//
// A non-positive cfg.FetchWorkers, cfg.PollInterval or
// cfg.PollIntervalYouTube falls back to config.DefaultFetchWorkers,
// config.DefaultPollInterval or config.DefaultYouTubePollInterval; a nil log
// discards output.
func New(st store.Store, f Fetcher, cfg config.Config, log *slog.Logger) *Scheduler {
	if cfg.FetchWorkers < 1 {
		cfg.FetchWorkers = config.DefaultFetchWorkers
	}
	cfg.PollInterval = DefaultInterval(cfg, store.KindRSS)
	cfg.PollIntervalYouTube = DefaultInterval(cfg, store.KindYouTube)
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Scheduler{
		st:          st,
		fetcher:     f,
		cfg:         cfg,
		log:         log,
		now:         time.Now,
		wake:        make(chan struct{}, 1),
		cache:       newPreviewCache(previewCacheTTL, previewCacheMax),
		addSlots:    make(chan struct{}, addFetchSlots),
		unhold:      make(map[int64]struct{}),
		avatarTried: make(map[string]struct{}),
	}
}

// Run staggers overdue feeds, then dispatches due feeds until ctx is done.
// It returns after in-flight fetches have stopped.
//
// Fetches interrupted by ctx are not recorded as failures: such feeds stay
// due and are fetched again after the next start. Store errors are logged
// and retried, never returned. Run returns an error only when another Run
// call on the same scheduler is still active.
func (s *Scheduler) Run(ctx context.Context) error {
	if !s.running.CompareAndSwap(false, true) {
		return errAlreadyRunning
	}
	defer s.running.Store(false)

	s.log.Info("scheduler started", "workers", s.cfg.FetchWorkers, "interval", s.cfg.PollInterval,
		"youtube_interval", s.cfg.PollIntervalYouTube)
	if err := s.stagger(ctx); err != nil && ctx.Err() == nil {
		s.log.Warn("could not stagger overdue feeds", "err", err)
	}
	newDispatcher(s).run(ctx)
	s.log.Info("scheduler stopped")
	return nil
}

// Refresh forces an immediate fetch of one feed. It returns an error
// wrapping store.ErrNotFound when the feed does not exist. A feed that is
// already being fetched is not fetched a second time concurrently: it is
// fetched again as soon as that fetch is done (see keepRequested), however
// many refreshes arrived meanwhile.
//
// A feed held back because the outcome of its last poll could not be stored
// is released: the schedule write that Refresh just made shows the store is
// writable again.
func (s *Scheduler) Refresh(ctx context.Context, feedID int64) error {
	if err := s.st.SetNextFetch(ctx, feedID, s.now()); err != nil {
		return fmt.Errorf("sched: refresh feed %d: %w", feedID, err)
	}
	s.unholdMu.Lock()
	s.unhold[feedID] = struct{}{}
	s.unholdMu.Unlock()
	s.signal()
	return nil
}

// RefreshAll forces an immediate fetch of every feed, releasing held feeds
// as Refresh does. The fetches still go through the worker pool, so at most
// cfg.FetchWorkers run at once.
func (s *Scheduler) RefreshAll(ctx context.Context) error {
	if err := s.st.SetAllNextFetch(ctx, s.now()); err != nil {
		return fmt.Errorf("sched: refresh all feeds: %w", err)
	}
	s.unholdMu.Lock()
	s.unholdAll = true
	s.unholdMu.Unlock()
	s.signal()
	return nil
}

// Reschedule applies a changed poll interval to feedID's next fetch now,
// instead of after the schedule made with the old interval fires (the
// per-feed override): the next fetch moves to the last fetch plus the new
// interval, raised by the feed's stored <ttl>, or to now when that time has
// passed. It only ever moves a fetch earlier, and leaves alone a feed that
// is in failure backoff or has never been fetched. It returns an error
// wrapping store.ErrNotFound when the feed does not exist.
func (s *Scheduler) Reschedule(ctx context.Context, feedID int64) error {
	f, err := s.st.GetFeed(ctx, feedID)
	if err != nil {
		return fmt.Errorf("sched: reschedule feed %d: %w", feedID, err)
	}
	if f.ErrorCount > 0 || f.LastFetchedAt.IsZero() {
		return nil
	}
	next := f.LastFetchedAt.Add(withHints(s.baseInterval(f), storedTTL(f), 0))
	if now := s.now(); next.Before(now) {
		next = now
	}
	if !next.Before(f.NextFetchAt) {
		return nil
	}
	if err := s.st.SetNextFetch(ctx, feedID, next); err != nil {
		return fmt.Errorf("sched: reschedule feed %d: %w", feedID, err)
	}
	s.signal()
	return nil
}

// takeUnholds returns and clears the feeds whose holds Refresh asked to
// lift, and whether RefreshAll asked to lift them all.
func (s *Scheduler) takeUnholds() (ids map[int64]struct{}, all bool) {
	s.unholdMu.Lock()
	defer s.unholdMu.Unlock()
	if len(s.unhold) == 0 && !s.unholdAll {
		return nil, false
	}
	ids, all = s.unhold, s.unholdAll
	s.unhold, s.unholdAll = make(map[int64]struct{}), false
	return ids, all
}

// Preview fetches and parses feedURL once, immediately, and caches the
// result briefly for Subscribe. When the resolver (ResolveFetcher) or an
// earlier Preview fetched the feed less than previewCacheTTL ago, that fetch
// is used instead. The fetch does not wait
// for the polling pool; it waits only while addFetchSlots other add-flow
// fetches are running.
//
// Errors wrap the fetch and parse errors (*fetch.StatusError,
// fetch.ErrTooLarge, parse.ErrNotAFeed, …) so callers can tell the user what
// went wrong. An empty URL yields an error wrapping store.ErrInvalid.
func (s *Scheduler) Preview(ctx context.Context, feedURL string) (*Preview, error) {
	feedURL = strings.TrimSpace(feedURL)
	if feedURL == "" {
		return nil, fmt.Errorf("sched: preview: empty feed URL: %w", store.ErrInvalid)
	}
	fd := s.cache.get(feedURL, s.now()) // fetched moments ago by the resolver
	if fd == nil {
		var err error
		if fd, err = s.fetchFeed(ctx, feedURL); err != nil {
			return nil, fmt.Errorf("sched: preview: %w", err)
		}
	}
	return &Preview{
		FeedURL:    fd.feedURL,
		Kind:       fd.kind,
		Title:      fd.doc.Title,
		SiteURL:    fd.doc.SiteURL,
		EntryCount: len(fd.doc.Entries),
	}, nil
}

// Subscribe stores the feed and all its entries (newest
// config.InitialUnread unread, rest read), reusing the cached Preview fetch
// when fresh, and schedules its next poll. Returns store.ErrConflict if
// already subscribed.
//
// The stored URL is the final feed URL (after permanent redirects). The
// title is sub.Title, else the feed's own title, else the URL's host. The
// icon is the feed's own, else sub.IconURL, else for a YouTube channel feed
// the avatar on the channel's page, looked up with one more request once
// the subscription is stored (see lookupAvatar). The first poll is
// scheduled one interval of the feed's kind (raised by the feed's <ttl> and
// Cache-Control max-age hints) after now.
func (s *Scheduler) Subscribe(ctx context.Context, sub Subscription) (store.Feed, error) {
	feedURL := strings.TrimSpace(sub.FeedURL)
	if feedURL == "" {
		return store.Feed{}, fmt.Errorf("sched: subscribe: empty feed URL: %w", store.ErrInvalid)
	}
	if err := s.ensureNotSubscribed(ctx, feedURL); err != nil {
		return store.Feed{}, err
	}
	fd := s.cache.get(feedURL, s.now())
	if fd == nil {
		var err error
		if fd, err = s.fetchFeed(ctx, feedURL); err != nil {
			return store.Feed{}, fmt.Errorf("sched: subscribe: %w", err)
		}
	}

	now := s.now()
	nf := store.NewFeed{
		Kind:          fd.kind,
		URL:           fd.feedURL,
		SiteURL:       fd.doc.SiteURL,
		Title:         subscriptionTitle(sub.Title, fd),
		OriginalTitle: fd.doc.Title,
		IconURL:       cmp.Or(fd.doc.IconURL, iconHint(sub.IconURL)),
		FolderID:      sub.FolderID,
		ETag:          fd.etag,
		LastModified:  fd.lastModified,
		FetchedAt:     fd.fetchedAt,
		NextFetchAt:   now.Add(withHints(s.baseInterval(store.Feed{Kind: fd.kind}), fd.doc.TTL, fd.maxAge)),
		TTLSec:        ttlSec(fd.doc.TTL),
	}
	entries := newEntries(fd.doc.Entries, fd.fetchedAt)
	s.idGuard.Lock()
	feed, err := s.st.CreateFeed(ctx, nf, entries, config.InitialUnread)
	s.idGuard.Unlock()
	if err != nil {
		return store.Feed{}, fmt.Errorf("sched: subscribe %s: %w", redact(fd.feedURL), err)
	}
	s.cache.drop(feedURL, fd.feedURL)
	if feed.IconURL == "" {
		// Best-effort, once the subscription is stored: a slow page, or a
		// request that ends meanwhile, cannot make it fail.
		if avatar := s.lookupAvatar(ctx, resolveFetcher{s}, feed.URL); avatar != "" && s.storeAvatar(ctx, feed, avatar) == nil {
			feed.IconURL = avatar
		}
	}
	s.log.Info("stored new subscription", "feed", feed.ID, "url", redact(feed.URL), "entries", len(entries),
		"next_fetch", feed.NextFetchAt)
	s.signal()
	return feed, nil
}

// ensureNotSubscribed returns an error wrapping store.ErrConflict when
// feedURL is already a subscription, without any network access.
func (s *Scheduler) ensureNotSubscribed(ctx context.Context, feedURL string) error {
	f, err := s.st.GetFeedByURL(ctx, feedURL)
	switch {
	case err == nil:
		return fmt.Errorf("sched: subscribe %s: already subscribed as feed %d: %w",
			redact(feedURL), f.ID, store.ErrConflict)
	case errors.Is(err, store.ErrNotFound):
		return nil
	default:
		return fmt.Errorf("sched: subscribe %s: %w", redact(feedURL), err)
	}
}

// signal wakes the dispatcher without blocking; wake-ups that arrive while
// one is already pending coalesce.
func (s *Scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
