package sched

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/parse"
	"github.com/giacomomicoli/feedreader/internal/resolve"
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
	fetchedAt    time.Time
}

// fetchFeed fetches and parses feedURL unconditionally and caches the
// result under both the requested URL and the final feed URL.
func (s *Scheduler) fetchFeed(ctx context.Context, feedURL string) (*fetched, error) {
	res, err := s.addFetch(ctx, fetch.Request{URL: feedURL, Accept: fetch.FeedAccept})
	if err != nil {
		return nil, err
	}
	return s.keep(feedURL, res)
}

// keep parses res, the response to an add-flow fetch of feedURL, and caches
// the result under both the requested URL and the final feed URL.
func (s *Scheduler) keep(feedURL string, res *fetch.Result) (*fetched, error) {
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
	res, err := f.s.addFetch(ctx, r)
	if err == nil && res != nil && !res.NotModified && parse.Detect(res.Body, res.ContentType) {
		// A feed that does not parse is not cached: Preview fetches it
		// again and reports the error.
		_, _ = f.s.keep(r.URL, res)
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

// httpURL returns raw, trimmed, when it is an absolute http(s) URL, else "".
func httpURL(raw string) string {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return raw
}

// subscriptionTitle picks the stored title: the user's (trimmed), else the
// feed's own, else the host of its URL, else the URL itself.
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
	return fd.feedURL
}
