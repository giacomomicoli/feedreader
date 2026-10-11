package sched

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Pending subscriptions. YouTube's feed endpoint answers 404 or 5xx for
// working channels for hours at a time, while the channel's page on YouTube
// keeps working. The add flow then offers to subscribe anyway: the
// subscription is stored without entries, as a feed whose first fetch failed
// and whose first stored document follows the initial-add rule
// (store.Feed.InitialPending). It is polled like any YouTube feed in an
// outage (see cappedFailure) until the feed answers.
//
// The add form's consent (Subscription.AllowPending) is never trusted on its
// own: the server stores a pending subscription only for the canonical feed
// URL of a YouTube channel, after its own fetch of that feed failed with 404
// or 5xx, and after a channel page served by YouTube confirmed the channel
// less than previewCacheTTL ago: youtube.com/channel/<id> (a URL built from
// the checked ID only) fetched by this request or the Preview, or the
// channel's own page the resolver fetched for an @handle, /c/… or /user/…
// URL (see resolve.ChannelFromPage).
//
// A channel followed in a mode (only its regular videos or only its Shorts,
// see mode.go) is never stored pending: its playlist feed may answer 404
// also when the channel has no video of that type, which the channel's page
// cannot tell from an outage. Such a subscription can be stored pending as
// the channel's feed and switched to its mode afterwards.

// pendingChannel returns the channel ID of feedURL when err, the failure of
// a fetch of it, lets the add flow offer a pending subscription: feedURL is
// a YouTube channel feed and its endpoint answered 404 or 5xx.
func pendingChannel(feedURL string, err error) (string, bool) {
	if !feedEndpointDown(err) {
		return "", false
	}
	return resolve.ChannelFeedID(feedURL)
}

// pendingPreview returns the pending Preview of feedURL, whose fetch failed
// with err, or nil when the add flow cannot offer one (see pendingChannel)
// or YouTube's page does not confirm the channel (see confirmChannel).
func (s *Scheduler) pendingPreview(ctx context.Context, feedURL string, err error) *Preview {
	id, ok := pendingChannel(feedURL, err)
	if !ok {
		return nil
	}
	ch, cerr := s.confirmChannel(ctx, id)
	if cerr != nil {
		s.log.Debug("channel not confirmed for a pending subscription", "url", redact(feedURL), "err", cerr)
		return nil
	}
	s.log.Debug("channel feed not answering; a pending subscription is possible", "url", redact(feedURL), "err", err)
	return &Preview{
		FeedURL: resolve.ChannelFeedURL(id),
		Kind:    store.KindYouTube,
		Title:   ch.Title,
		IconURL: ch.IconURL,
		Pending: true,
	}
}

// subscribePending stores sub as a pending subscription after the fetch of
// feedURL, sent at started, failed at failedAt with cause. ok is false, and
// nothing is stored or requested, unless feedURL is the canonical feed URL
// of a YouTube channel (the URL a pending Preview offers), cause is a 404 or
// 5xx from its endpoint, and the channel is confirmed by its page (one
// request, unless the add flow confirmed it less than previewCacheTTL ago).
//
// The subscription has no entries and no validators. Its failed fetch is
// recorded as a first failure (error_count 1, last_error), so it is retried
// after the first failure's delay and then hourly (see cappedFailure),
// sooner at a digest ingestion time of its folder, never sooner than a
// Retry-After. Its title is the user's, else the channel's name from its
// page; its icon the channel's avatar from that page, else sub.IconURL.
func (s *Scheduler) subscribePending(ctx context.Context, sub Subscription, feedURL string,
	started, failedAt time.Time, cause error) (feed store.Feed, ok bool, err error) {
	id, eligible := pendingChannel(feedURL, cause)
	if !eligible || resolve.ChannelFeedURL(id) != feedURL {
		return store.Feed{}, false, nil
	}
	ch, err := s.confirmChannel(ctx, id)
	if err != nil {
		s.log.Debug("channel not confirmed for a pending subscription", "url", redact(feedURL), "err", err)
		return store.Feed{}, false, nil
	}

	pending := store.Feed{Kind: store.KindYouTube, URL: feedURL}
	ra := retryAfter(cause)
	regular := failedAt.Add(failureDelay(s.baseInterval(pending), 1, ra, cappedFailure(pending, cause)))
	// next is the first poll of the new feed, for its row cur.
	next := func(cur store.Feed) time.Time {
		return retryFloor(earliest(regular, s.pollIngest(ctx, cur.ID, cur.FolderID, started)), failedAt, ra)
	}
	nf := store.NewFeed{
		Kind:           store.KindYouTube,
		URL:            feedURL,
		Title:          cmp.Or(strings.TrimSpace(sub.Title), ch.Title),
		IconURL:        cmp.Or(ch.IconURL, iconHint(sub.IconURL)),
		FolderID:       sub.FolderID,
		FetchedAt:      failedAt,
		NextFetchAt:    next(store.Feed{FolderID: sub.FolderID}),
		InitialPending: true,
		LastError:      failureMessage(cause),
	}
	s.idGuard.Lock()
	feed, err = s.st.CreateFeed(ctx, nf, nil, 0)
	if err == nil {
		if at := s.recheckIngest(ctx, feed.ID, next); !at.IsZero() {
			feed.NextFetchAt = at
		}
	}
	s.idGuard.Unlock()
	if err != nil {
		return store.Feed{}, true, fmt.Errorf("sched: subscribe %s: %w", redact(feedURL), err)
	}
	s.log.Info("stored pending subscription", "feed", feed.ID, "url", redact(feed.URL),
		"err", nf.LastError, "next_fetch", feed.NextFetchAt)
	s.signal()
	return feed, true, nil
}

// confirmChannel confirms the YouTube channel id with its page (see
// resolve.Resolver.ConfirmChannel), through the add flow's fetch slots,
// reusing a confirmation less than previewCacheTTL old (from an earlier
// Preview, or from the page the resolver fetched; see resolveFetcher).
func (s *Scheduler) confirmChannel(ctx context.Context, id string) (resolve.Channel, error) {
	if ch, ok := s.channels.get(id, s.now()); ok {
		return ch, nil
	}
	ch, err := resolve.New(resolveFetcher{s}).ConfirmChannel(ctx, id)
	if err != nil {
		return resolve.Channel{}, err
	}
	s.channels.put(ch, s.now())
	return ch, nil
}

// channelCache keeps the channels YouTube's pages confirmed recently, so
// that subscribing after a pending Preview needs no second page request. It
// holds at most previewCacheMax channels, each for previewCacheTTL.
type channelCache struct {
	mu    sync.Mutex
	items map[string]confirmedChannel
}

type confirmedChannel struct {
	ch resolve.Channel
	at time.Time
}

func newChannelCache() *channelCache {
	return &channelCache{items: make(map[string]confirmedChannel)}
}

// get returns the channel id confirmed less than previewCacheTTL before now.
// A confirmation from the future (the clock went back) is not trusted.
func (c *channelCache) get(id string, now time.Time) (resolve.Channel, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	it, ok := c.items[id]
	if !ok {
		return resolve.Channel{}, false
	}
	if age := now.Sub(it.at); age < 0 || age >= previewCacheTTL {
		delete(c.items, id)
		return resolve.Channel{}, false
	}
	return it.ch, true
}

// put stores ch, confirmed at now, dropping expired confirmations and then
// the oldest ones beyond previewCacheMax.
func (c *channelCache) put(ch resolve.Channel, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, it := range c.items {
		if age := now.Sub(it.at); age < 0 || age >= previewCacheTTL {
			delete(c.items, id)
		}
	}
	c.items[ch.ID] = confirmedChannel{ch: ch, at: now}
	for len(c.items) > previewCacheMax {
		oldest := ""
		for id, it := range c.items {
			if oldest == "" || it.at.Before(c.items[oldest].at) {
				oldest = id
			}
		}
		delete(c.items, oldest)
	}
}
