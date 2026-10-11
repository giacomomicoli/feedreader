package sched

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/parse"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/safeurl"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// errUnexpected304 is returned when a server answers an unconditional
// request with 304 Not Modified, leaving nothing to preview.
var errUnexpected304 = errors.New("server answered 304 Not Modified to an unconditional request")

// fetched is one fetch + parse of a feed for the add flow, as shared by
// Preview and Subscribe through the preview cache. It is never modified
// after creation.
type fetched struct {
	feedURL      string // URL to subscribe: the permanent redirect target, else the requested URL
	kind         store.Kind
	etag         string
	lastModified string
	maxAge       time.Duration
	doc          *parse.Feed
	// started is when the request was sent, or a little before: it is taken
	// before the request waits for its add-flow slot. Subscribe schedules the
	// first poll no later than the first digest ingestion time after it.
	started time.Time
	// fetchedAt is when the response arrived: the feed's last fetch, and
	// what the age of the cached fetch counts from.
	fetchedAt time.Time
	// fallback is set when the feed did not answer and doc holds its
	// channel's videos as YouTube's web API lists them (see addFallback).
	fallback bool
}

// fetchFeed fetches and parses feedURL unconditionally and caches the
// result under both the requested URL and the final feed URL.
func (s *Scheduler) fetchFeed(ctx context.Context, feedURL string) (*fetched, error) {
	started := s.now()
	res, err := s.addFetch(ctx, fetch.Request{URL: feedURL, Accept: fetch.FeedAccept})
	if err != nil {
		return nil, err
	}
	return s.keep(feedURL, res, started)
}

// fetchFeedOrFallback is fetchFeed, falling back to YouTube's web API when
// a YouTube channel's feed answers 404 or 5xx (see addFallback). err is the
// feed's own error when both fail.
func (s *Scheduler) fetchFeedOrFallback(ctx context.Context, feedURL string) (*fetched, error) {
	started := s.now()
	fd, err := s.fetchFeed(ctx, feedURL)
	if err == nil {
		return fd, nil
	}
	if fb := s.addFallback(ctx, feedURL, started, err); fb != nil {
		return fb, nil
	}
	return nil, err
}

// addFallback reads the videos of the YouTube channel whose feed feedURL
// failed with cause, through the add flow's fetch slots and within
// config.YouTubeFallbackAddTimeout, when the fallback applies (see
// fallback.go). The result is cached like a fetch of the feed, as a
// document titled with the channel's name that lists the videos, so that
// Subscribe stores them as an initial add. It returns nil when the
// fallback does not apply, fails, or finds no video to store.
func (s *Scheduler) addFallback(ctx context.Context, feedURL string, started time.Time, cause error) *fetched {
	channelID, m, ok := s.fallbackChannel(resolve.KindForURL(feedURL), feedURL, cause)
	if !ok {
		return nil
	}
	fctx, cancel := context.WithTimeout(ctx, config.YouTubeFallbackAddTimeout)
	defer cancel()
	res, err := s.readChannel(fctx, addFetcher{s}, 0, channelID, m)
	if err == nil && len(res.entries) == 0 {
		// Nothing to show, nor the channel's name: the fallback did not
		// help, and a pending subscription (named from the channel's
		// page) may be offered instead.
		err = errFallbackNothing
	}
	if err != nil {
		s.log.Info("YouTube fallback failed for a new feed", "url", redact(feedURL), "mode", m, "err", failureMessage(err))
		return nil
	}
	fd := &fetched{
		feedURL:   feedURL,
		kind:      store.KindYouTube,
		doc:       &parse.Feed{Title: res.author, Entries: res.entries},
		started:   started,
		fetchedAt: s.now(),
		fallback:  true,
	}
	s.cache.put(fd, feedURL)
	s.log.Info("read a new feed's videos from YouTube's web API", "url", redact(feedURL), "mode", m,
		"entries", len(res.entries), "skipped", res.skipped, "source", fallbackSource)
	return fd
}

// addFetcher fetches through the add flow's slots (addFetch).
type addFetcher struct{ s *Scheduler }

func (f addFetcher) Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
	return f.s.addFetch(ctx, r)
}

// keep parses res, the response to an add-flow fetch of feedURL started at
// started, and caches the result under both the requested URL and the final
// feed URL.
func (s *Scheduler) keep(feedURL string, res *fetch.Result, started time.Time) (*fetched, error) {
	if res == nil {
		return nil, fmt.Errorf("fetch %s: %w", redact(feedURL), errNoResult)
	}
	if res.NotModified {
		return nil, fmt.Errorf("fetch %s: %w", redact(feedURL), errUnexpected304)
	}
	fetchedAt := s.now()
	finalURL := cmp.Or(res.FinalURL, feedURL)
	doc, err := parse.Parse(res.Body, res.ContentType, finalURL)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", redact(finalURL), err)
	}
	fd := &fetched{
		feedURL:      cmp.Or(res.PermanentURL, feedURL),
		kind:         resolve.KindForURL(finalURL),
		etag:         res.ETag,
		lastModified: res.LastModified,
		maxAge:       res.MaxAge,
		doc:          doc,
		started:      started,
		fetchedAt:    fetchedAt,
	}
	s.cache.put(fd, feedURL, fd.feedURL)
	s.log.Debug("fetched feed for preview", "url", redact(feedURL), "feed_url", redact(fd.feedURL),
		"kind", fd.kind, "entries", len(doc.Entries))
	return fd, nil
}

// ResolveFetcher returns the Fetcher for the add flow's URL resolution
// (resolve.New). Its requests take addFetchSlots like Preview's, so
// resolving a URL (a page, then up to nine fallback paths) never adds to the
// outbound fetches beyond that bound. A response that is itself a feed is
// parsed and cached as Preview's fetch would be, so previewing the feed the
// resolver found there does not download it a second time.
func (s *Scheduler) ResolveFetcher() Fetcher { return resolveFetcher{s} }

type resolveFetcher struct{ s *Scheduler }

func (f resolveFetcher) Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
	started := f.s.now()
	res, err := f.s.addFetch(ctx, r)
	if err == nil && res != nil && !res.NotModified && parse.Detect(res.Body, res.ContentType) {
		// A feed that does not parse is not cached: Preview fetches it
		// again and reports the error.
		_, _ = f.s.keep(r.URL, res, started)
	}
	if err == nil {
		// A channel's own page on YouTube (an @handle being resolved)
		// confirms the channel for a pending subscription, so an outage
		// does not cost a second request of the same page.
		if ch, ok := resolve.ChannelFromPage(r.URL, res); ok {
			f.s.channels.put(ch, f.s.now())
		}
	}
	return res, err
}

// addFetch runs one add-flow fetch in one of the addFetchSlots, waiting for
// a free slot until ctx is done. It never waits for the polling pool.
func (s *Scheduler) addFetch(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
	select {
	case s.addSlots <- struct{}{}:
		defer func() { <-s.addSlots }()
	case <-ctx.Done():
		return nil, fmt.Errorf("fetch %s: %w", redact(r.URL), ctx.Err())
	}
	return s.fetcher.Fetch(ctx, r)
}

// iconHint returns the icon a Subscription carries, trimmed, when it is an
// absolute http(s) URL of at most resolve.MaxIconURLBytes bytes, else "":
// it came back through the add form.
func iconHint(raw string) string {
	raw = strings.TrimSpace(raw)
	if len(raw) > resolve.MaxIconURLBytes {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return raw
}

// subscriptionTitle picks the stored title: the user's (trimmed), else the
// feed's own, else the host of its URL, else the URL without its
// credentials.
func subscriptionTitle(userTitle string, fd *fetched) string {
	if t := strings.TrimSpace(userTitle); t != "" {
		return t
	}
	if t := strings.TrimSpace(fd.doc.Title); t != "" {
		return t
	}
	if u, err := url.Parse(fd.feedURL); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return safeurl.String(fd.feedURL) // titles are shown and logged
}
