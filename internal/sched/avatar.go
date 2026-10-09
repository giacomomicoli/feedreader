package sched

import (
	"context"
	"errors"

	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Channel avatars. A YouTube channel's feed has no icon, so the channel's
// avatar is read from its page on YouTube (resolve.Resolver.ChannelAvatar):
// once a subscription is stored, unless the add flow already found it on
// the page it fetched to resolve the URL, and after a successful poll of a
// channel feed that still has none. Polls fetch each channel feed's page
// for this at most once per run of the scheduler, unless the avatar was
// found (but could not be stored) or the request was cut short.

// firstAvatarTry reports whether a poll has not fetched the channel page of
// feedURL for its avatar yet in this run, and records that one now does.
func (s *Scheduler) firstAvatarTry(feedURL string) bool {
	s.avatarMu.Lock()
	defer s.avatarMu.Unlock()
	if _, tried := s.avatarTried[feedURL]; tried {
		return false
	}
	s.avatarTried[feedURL] = struct{}{}
	return true
}

// forgetAvatarTry lets the next poll fetch the channel page of feedURL
// again.
func (s *Scheduler) forgetAvatarTry(feedURL string) {
	s.avatarMu.Lock()
	defer s.avatarMu.Unlock()
	delete(s.avatarTried, feedURL)
}

// lookupAvatar returns the avatar of the YouTube channel whose feed is
// feedURL, fetching the channel's page through f, or "": not a channel
// feed, no avatar on the page, or the page could not be fetched.
func (s *Scheduler) lookupAvatar(ctx context.Context, f Fetcher, feedURL string) string {
	if !resolve.IsChannelFeed(feedURL) {
		return ""
	}
	avatar, err := resolve.New(f).ChannelAvatar(ctx, feedURL)
	switch {
	case err != nil:
		s.log.Debug("could not fetch the channel page for its avatar", "url", redact(feedURL), "err", err)
	case avatar == "":
		s.log.Debug("channel page has no avatar", "url", redact(feedURL))
	}
	return avatar
}

// storeAvatar writes avatar as the icon of f while f is still the same
// subscription (see writeFeed) and has no icon. Nothing is written once ctx
// is done.
func (s *Scheduler) storeAvatar(ctx context.Context, f store.Feed, avatar string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	err := s.writeFeed(ctx, f, func(cur store.Feed) error {
		if cur.IconURL != "" {
			return nil
		}
		return s.st.SetFeedIcon(ctx, f.ID, avatar)
	})
	switch {
	case err == nil:
		s.log.Info("stored channel avatar", "feed", f.ID)
	case ctx.Err() == nil && !errors.Is(err, store.ErrNotFound):
		s.log.Warn("could not store the channel avatar", "feed", f.ID, "err", err)
	}
	return err
}

// backfillAvatar gives f, a feed whose poll was just stored, its channel's
// avatar when it is a YouTube channel feed without an icon. The page is
// fetched by the worker that polled the feed, after the feed, so polling
// never runs more than cfg.FetchWorkers requests at once.
func (s *Scheduler) backfillAvatar(ctx context.Context, f store.Feed) {
	if f.IconURL != "" || !resolve.IsChannelFeed(f.URL) || !s.firstAvatarTry(f.URL) {
		return
	}
	avatar := s.lookupAvatar(ctx, s.fetcher, f.URL)
	if avatar != "" || ctx.Err() != nil {
		// Only a page without an avatar, or one that could not be fetched,
		// uses up this run's try: a found avatar may still fail to be
		// stored, and a cut-short request says nothing about the page.
		s.forgetAvatarTry(f.URL)
	}
	if avatar != "" {
		_ = s.storeAvatar(ctx, f, avatar)
	}
}
