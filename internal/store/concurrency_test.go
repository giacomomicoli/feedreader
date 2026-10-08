package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// TestConcurrentWritersAndReaders_NoBusyErrors runs the scheduler's pollers,
// user actions, feed churn and sidebar readers at the same time. With WAL,
// BEGIN IMMEDIATE and the busy timeout, none of them may fail with
// SQLITE_BUSY (or anything else), and no write may be lost.
func TestConcurrentWritersAndReaders_NoBusyErrors(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	const (
		nFeeds   = 4 // = config.DefaultFetchWorkers concurrent pollers
		initial  = 20
		rounds   = 12
		perRound = 40
	)
	feeds := make([]Feed, nFeeds)
	for i := range feeds {
		feeds[i] = mustCreateFeed(t, s, newFeed(fmt.Sprintf("https://f%d.example/feed", i), KindRSS),
			entriesNewestFirst(fmt.Sprintf("f%d-init-", i), initial, t0), 5)
	}

	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	report := func(who string, err error) {
		if err != nil {
			mu.Lock()
			errs = append(errs, fmt.Errorf("%s: %w", who, err))
			mu.Unlock()
		}
	}
	// ignoreGone tolerates rows deleted concurrently by the churn worker.
	ignoreGone := func(err error) error {
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		return err
	}

	// Pollers: each round brings perRound new entries and re-sends the
	// previous round's entries (refresh path).
	for i, f := range feeds {
		wg.Go(func() {
			var prev []NewEntry
			for r := range rounds {
				at := t0.Add(time.Duration(r+1) * time.Minute)
				fresh := entriesNewestFirst(fmt.Sprintf("f%d-r%d-", i, r), perRound, at)
				n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: at,
					NextFetchAt: at.Add(time.Hour), Entries: append(fresh, prev...)})
				if err == nil && n != perRound {
					err = fmt.Errorf("round %d inserted %d, want %d", r, n, perRound)
				}
				report("poller", err)
				report("poller 304", s.RecordNotModified(ctx, f.ID, at, at.Add(time.Hour)))
				prev = fresh
			}
		})
	}
	// User actions on whatever the grid shows.
	for w := range 4 {
		wg.Go(func() {
			for r := range rounds * 2 {
				p, err := s.ListEntries(ctx, ListQuery{Scope: Scope{Kind: ScopeAll}, UnreadOnly: true, Limit: 10})
				report("grid", err)
				for j, v := range p.Entries {
					switch (j + w) % 4 {
					case 0:
						err = s.SetRead(ctx, v.ID, true, t0)
					case 1:
						err = s.SetLater(ctx, v.ID, true)
					case 2:
						err = s.SetFavourite(ctx, v.ID, true)
					case 3:
						_, err = s.AddEntryTag(ctx, v.ID, fmt.Sprintf("tag%d", r%3))
					}
					report("action", ignoreGone(err))
				}
				if r%6 == 0 {
					maxID, err := s.MaxEntryID(ctx)
					report("watermark", err)
					_, err = s.MarkScopeReadUpTo(ctx, Scope{Kind: ScopeFeed, ID: feeds[w].ID}, maxID, t0)
					report("mark all", err)
				}
			}
		})
	}
	// Subscribe/unsubscribe churn exercising the cascades.
	wg.Go(func() {
		for r := range rounds {
			f, err := s.CreateFeed(ctx, newFeed(fmt.Sprintf("https://churn%d.example", r), KindYouTube),
				entriesNewestFirst(fmt.Sprintf("c%d-", r), 30, t0), 5)
			if err != nil {
				report("churn create", err)
				continue
			}
			_, err = s.AddEntryTag(ctx, entryID(t, s, f.ID, fmt.Sprintf("c%d-0", r)), "churn")
			report("churn tag", err)
			report("churn delete", s.DeleteFeed(ctx, f.ID))
		}
	})
	// Sidebar readers.
	for range 4 {
		wg.Go(func() {
			for range rounds * 3 {
				_, err := s.UnreadCounts(ctx)
				report("counts", err)
				_, err = s.ListTags(ctx)
				report("tags", err)
				_, err = s.ListFeeds(ctx)
				report("feeds", err)
				_, err = s.ListEntries(ctx, ListQuery{Scope: Scope{Kind: ScopeFavourites}, Limit: 20})
				report("favourites", err)
			}
		})
	}
	wg.Wait()

	for _, err := range errs {
		t.Error(err)
	}
	if want := nFeeds * (initial + rounds*perRound); countRows(t, s, "entries", "") != want {
		t.Errorf("entries = %d, want %d (lost or duplicated writes)", countRows(t, s, "entries", ""), want)
	}
	if n := countRows(t, s, "entry_tags", "entry_id NOT IN (SELECT id FROM entries)"); n != 0 {
		t.Errorf("%d orphan entry_tags rows", n)
	}
}
