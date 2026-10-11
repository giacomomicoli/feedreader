package store

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
)

// Switching what a YouTube subscription follows (regular videos, Shorts or
// both) swaps its feed URL in place: ChangeFeedURL.

const (
	modeFromURL = "https://www.youtube.com/feeds/videos.xml?channel_id=UCsynthetic0channel0id00"
	modeToURL   = "https://www.youtube.com/feeds/videos.xml?playlist_id=UULFsynthetic0channel0id00"
)

func TestChangeFeedURL_StartsTheNewURLAfreshAndKeepsEverythingElse(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	folder := mustFolder(t, s, "Channels")
	nf := newFeed(modeFromURL, KindYouTube)
	nf.FolderID = folder.ID
	stored := entriesNewestFirst("e", 2*config.InitialUnread, t0)
	f := mustCreateFeed(t, s, nf, stored, config.InitialUnread)
	mustNoErr(t, s.SetFeedInterval(ctx, f.ID, int(2*time.Hour/time.Second)))
	_, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: t0, NextFetchAt: t0.Add(time.Hour),
		ETag: `"v3"`, LastModified: "Thu, 01 Oct 2026 12:00:00 GMT", TTLSec: 3600})
	mustNoErr(t, err)
	mustNoErr(t, s.RecordFetchError(ctx, f.ID, "HTTP 503", t0.Add(time.Minute), t0.Add(time.Hour)))
	for _, v := range listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeFeed, ID: f.ID}, Limit: 100}) {
		switch v.GUID {
		case "e1":
			mustNoErr(t, s.SetFavourite(ctx, v.ID, true))
		case "e7":
			mustNoErr(t, s.SetRead(ctx, v.ID, false, t0))
			mustNoErr(t, s.SetLater(ctx, v.ID, true))
		}
	}
	before, err := s.GetFeed(ctx, f.ID)
	mustNoErr(t, err)
	viewsBefore := listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeFeed, ID: f.ID}, Limit: 100})

	at := t0.Add(2 * time.Hour)
	mustNoErr(t, s.ChangeFeedURL(ctx, f.ID, modeFromURL, modeToURL, at, false))

	got, err := s.GetFeed(ctx, f.ID)
	mustNoErr(t, err)
	want := before
	want.URL = modeToURL
	want.ETag, want.LastModified, want.TTLSec = "", "", 0
	want.ErrorCount, want.LastError = 0, ""
	want.InitialPending = true
	want.NextFetchAt = at
	if got != want {
		t.Errorf("after the change =\n%+v\nwant\n%+v", got, want)
	}
	if byURL, err := s.GetFeedByURL(ctx, modeToURL); err != nil || byURL.ID != f.ID {
		t.Errorf("GetFeedByURL(new) = %+v, %v", byURL, err)
	}
	if _, err := s.GetFeedByURL(ctx, modeFromURL); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFeedByURL(old) err = %v, want ErrNotFound", err)
	}
	viewsAfter := listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeFeed, ID: f.ID}, Limit: 100})
	if len(viewsAfter) != len(viewsBefore) {
		t.Fatalf("%d entries after the change, want the %d stored before", len(viewsAfter), len(viewsBefore))
	}
	for i := range viewsAfter {
		if !reflect.DeepEqual(viewsAfter[i], viewsBefore[i]) {
			t.Errorf("entry %d changed:\n%+v\nwas\n%+v", i, viewsAfter[i], viewsBefore[i])
		}
	}

	// The first document of the new URL is an initial add for the entries
	// it brings; the entries kept from before are left as they are.
	fetchedAt := at.Add(time.Minute)
	list := append(entriesNewestFirst("e", 2, t0), entriesNewestFirst("n", 2*config.InitialUnread, t0.Add(-time.Hour))...)
	n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: fetchedAt,
		NextFetchAt: fetchedAt.Add(time.Hour), Entries: list, InitialUnread: config.InitialUnread})
	mustNoErr(t, err)
	if n != 2*config.InitialUnread {
		t.Fatalf("inserted %d, want %d", n, 2*config.InitialUnread)
	}
	unread := map[string]bool{}
	for _, v := range listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeFeed, ID: f.ID}, Limit: 100}) {
		if !v.IsRead {
			unread[v.GUID] = true
		}
	}
	for _, v := range viewsBefore {
		if unread[v.GUID] == v.IsRead {
			t.Errorf("kept entry %s: read = %v, want %v as before", v.GUID, !unread[v.GUID], v.IsRead)
		}
	}
	for i := range 2 * config.InitialUnread {
		guid := fmt.Sprintf("n%d", i)
		if wantUnread := i < config.InitialUnread; unread[guid] != wantUnread {
			t.Errorf("new entry %s: unread = %v, want %v", guid, unread[guid], wantUnread)
		}
	}
	if got, _ := s.GetFeed(ctx, f.ID); got.InitialPending {
		t.Error("still pending after the first fetch of the new URL")
	}
}

// TestFeed_HasEntries: every way of reading a feed tells whether it has any
// stored entry, so the UI can tell a subscription that never answered from
// one that is fetching a new URL.
func TestFeed_HasEntries(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	empty := mustCreateFeed(t, s, newFeed(modeFromURL, KindYouTube), nil, config.InitialUnread)
	full := mustCreateFeed(t, s, newFeed(modeToURL, KindYouTube), entriesNewestFirst("e", 2, t0), config.InitialUnread)
	if empty.HasEntries || !full.HasEntries {
		t.Fatalf("CreateFeed: HasEntries = %v, %v; want false, true", empty.HasEntries, full.HasEntries)
	}
	read := map[string]func(Feed) (Feed, error){
		"GetFeed":      func(f Feed) (Feed, error) { return s.GetFeed(ctx, f.ID) },
		"GetFeedByURL": func(f Feed) (Feed, error) { return s.GetFeedByURL(ctx, f.URL) },
		"ListFeeds": func(f Feed) (Feed, error) {
			all, err := s.ListFeeds(ctx)
			for _, g := range all {
				if g.ID == f.ID {
					return g, err
				}
			}
			return Feed{}, err
		},
		"DueFeeds": func(f Feed) (Feed, error) {
			due, err := s.DueFeeds(ctx, t0.Add(24*time.Hour))
			for _, g := range due {
				if g.ID == f.ID {
					return g, err
				}
			}
			return Feed{}, err
		},
	}
	for name, get := range read {
		for _, f := range []Feed{empty, full} {
			if got, err := get(f); err != nil || got.HasEntries != f.HasEntries || got.ID != f.ID {
				t.Errorf("%s(%d) = %+v, %v; want HasEntries %v", name, f.ID, got, err, f.HasEntries)
			}
		}
	}
	_, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: empty.ID, FetchedAt: t0, Entries: entriesNewestFirst("n", 1, t0)})
	mustNoErr(t, err)
	if got, err := s.GetFeed(ctx, empty.ID); err != nil || !got.HasEntries {
		t.Errorf("after a fetch with entries: %+v, %v", got, err)
	}
}

// TestChangeFeedURL_KeepFailures: a switch during a rate-limit wait keeps
// the failure count and last error, and otherwise changes as usual.
func TestChangeFeedURL_KeepFailures(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(modeFromURL, KindYouTube), nil, config.InitialUnread)
	for range config.WarnAfterFailures {
		mustNoErr(t, s.RecordFetchError(ctx, f.ID, "HTTP 429 from upstream", t0, t0.Add(6*time.Hour)))
	}
	at := t0.Add(6 * time.Hour)
	mustNoErr(t, s.ChangeFeedURL(ctx, f.ID, modeFromURL, modeToURL, at, true))
	got, err := s.GetFeed(ctx, f.ID)
	mustNoErr(t, err)
	if got.URL != modeToURL || !got.InitialPending || got.ETag != "" || !got.NextFetchAt.Equal(at) ||
		got.ErrorCount != config.WarnAfterFailures || got.LastError != "HTTP 429 from upstream" {
		t.Errorf("after a switch keeping the failures: %+v", got)
	}
}

// TestChangeFeedURL_KeepsTheTranscriptSwitch: the source's transcript
// switch, on or off, survives a switch of its URL.
func TestChangeFeedURL_KeepsTheTranscriptSwitch(t *testing.T) {
	for _, on := range []bool{true, false} {
		for _, keep := range []bool{true, false} {
			s := openTest(t)
			ctx := t.Context()
			f := mustCreateFeed(t, s, newFeed(modeFromURL, KindYouTube), nil, config.InitialUnread)
			mustNoErr(t, s.SetFeedTranscripts(ctx, f.ID, on, t0))
			mustNoErr(t, s.ChangeFeedURL(ctx, f.ID, modeFromURL, modeToURL, t0.Add(time.Hour), keep))
			if got, err := s.FeedTranscripts(ctx, f.ID); err != nil || got != on {
				t.Errorf("switch %v, keepFailures %v: transcripts = %v, %v after the change", on, keep, got, err)
			}
		}
	}
}

func TestChangeFeedURL_Refusals(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	a := mustCreateFeed(t, s, newFeed(modeFromURL, KindYouTube), nil, config.InitialUnread)
	b := mustCreateFeed(t, s, newFeed(modeToURL, KindYouTube), nil, config.InitialUnread)
	at := t0.Add(time.Hour)

	if err := s.ChangeFeedURL(ctx, a.ID, a.URL, b.URL, at, false); !errors.Is(err, ErrConflict) {
		t.Errorf("onto another feed's URL: err = %v, want ErrConflict", err)
	}
	// The URL changed meanwhile (a redirect, another switch): nothing is
	// written.
	if err := s.ChangeFeedURL(ctx, a.ID, "https://www.youtube.com/feeds/videos.xml?channel_id=UCsomething0else00000000",
		"https://a.example/feed", at, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("from a URL the feed no longer has: err = %v, want ErrNotFound", err)
	}
	if err := s.ChangeFeedURL(ctx, b.ID+1, a.URL, "https://a.example/feed", at, false); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing feed: err = %v, want ErrNotFound", err)
	}
	if err := s.ChangeFeedURL(ctx, a.ID, a.URL, " ", at, false); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty URL: err = %v, want ErrInvalid", err)
	}
	for _, f := range []Feed{a, b} {
		if got, err := s.GetFeed(ctx, f.ID); err != nil || got != f {
			t.Errorf("feed %d after refusals = %+v, %v; want unchanged %+v", f.ID, got, err, f)
		}
	}
}
