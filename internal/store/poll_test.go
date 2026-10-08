package store

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
)

func TestRecordFetchSuccess_RefetchRefreshesContentButNeverUserState(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	orig := entriesNewestFirst("e", 4, t0)
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), orig, 2)
	// e0, e1 unread; e2, e3 stored read at t0. Now the user acts:
	readAt := t0.Add(10 * time.Minute)
	mustNoErr(t, s.SetRead(ctx, entryID(t, s, f.ID, "e0"), true, readAt)) // read by user
	mustNoErr(t, s.SetRead(ctx, entryID(t, s, f.ID, "e2"), false, t0))    // back to unread
	mustNoErr(t, s.SetLater(ctx, entryID(t, s, f.ID, "e1"), true))        // saved for later
	mustNoErr(t, s.SetFavourite(ctx, entryID(t, s, f.ID, "e3"), true))    // favourite
	before := map[string]EntryView{}
	for _, e := range orig {
		before[e.GUID] = entryByGUID(t, s, f.ID, e.GUID)
	}

	// The feed re-publishes every entry with edited content, a new updated
	// date, a different published date and author.
	refetchAt := t0.Add(6 * time.Hour)
	edited := make([]NewEntry, len(orig))
	for i, e := range orig {
		e.Title += " (edited)"
		e.SummaryHTML = "<p>new body</p>"
		e.URL += "?v=2"
		e.ThumbnailURL = "https://img.example/" + e.GUID + ".jpg"
		e.UpdatedAt = refetchAt.Add(-time.Minute)
		e.PublishedAt = refetchAt // must be ignored: entries never jump to the top
		e.Author = "someone else"
		edited[i] = e
	}
	n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: refetchAt,
		NextFetchAt: refetchAt.Add(6 * time.Hour), Entries: edited})
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("inserted = %d, want 0", n)
	}
	for i, e := range edited {
		got := entryByGUID(t, s, f.ID, e.GUID)
		old := before[e.GUID]
		// Refreshed fields.
		if got.Title != e.Title || got.SummaryHTML != e.SummaryHTML || got.URL != e.URL ||
			got.ThumbnailURL != e.ThumbnailURL || !got.UpdatedAt.Equal(e.UpdatedAt) {
			t.Errorf("%s: content not refreshed: %+v", e.GUID, got.Entry)
		}
		// Untouched fields.
		if got.IsRead != old.IsRead || !got.ReadAt.Equal(old.ReadAt) || got.IsLater != old.IsLater ||
			got.IsFavourite != old.IsFavourite || !got.PublishedAt.Equal(orig[i].PublishedAt) ||
			!got.FetchedAt.Equal(old.FetchedAt) || got.Author != old.Author || got.ID != old.ID {
			t.Errorf("%s: user state or immutable fields changed:\n got %+v\nwant %+v", e.GUID, got.Entry, old.Entry)
		}
	}
	// The grid order is unchanged (sorted by the original published_at).
	views := listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeFeed, ID: f.ID}, Limit: 10})
	if want := []string{"e0", "e1", "e2", "e3"}; !slices.Equal(guids(views), want) {
		t.Errorf("order after refetch = %v, want %v", guids(views), want)
	}
}

func TestRecordFetchSuccess_ClearsOptionalContentWhenFeedDropsIt(t *testing.T) {
	s := openTest(t)
	e := entryAt("e", t0)
	e.ThumbnailURL = "https://img.example/e.jpg"
	e.UpdatedAt = t0
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindYouTube), []NewEntry{e}, 5)
	e.ThumbnailURL, e.UpdatedAt, e.SummaryHTML = "", time.Time{}, ""
	if _, err := s.RecordFetchSuccess(t.Context(), FetchSuccess{FeedID: f.ID, FetchedAt: t0, Entries: []NewEntry{e}}); err != nil {
		t.Fatal(err)
	}
	got := entryByGUID(t, s, f.ID, "e")
	if got.ThumbnailURL != "" || !got.UpdatedAt.IsZero() || got.SummaryHTML != "" {
		t.Fatalf("got %+v, want cleared thumbnail, updated_at and summary", got.Entry)
	}
}

func TestRecordFetchSuccess_NewEntriesOnRegularPollAreUnread(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), entriesNewestFirst("e", 8, t0), config.InitialUnread)

	// A newer post, and an old post the feed only now exposes (e.g. a GUID
	// change after a WordPress migration): both are new and must be unread,
	// even though the initial add archived older entries as read.
	pollAt := t0.Add(6 * time.Hour)
	fresh := entryAt("fresh", t0.Add(time.Hour))
	old := entryAt("old-new-guid", t0.Add(-100*time.Hour))
	n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: pollAt,
		Entries: append([]NewEntry{fresh, old}, entriesNewestFirst("e", 8, t0)...)})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("inserted = %d, want 2", n)
	}
	for _, g := range []string{"fresh", "old-new-guid"} {
		v := entryByGUID(t, s, f.ID, g)
		if v.IsRead || !v.ReadAt.IsZero() || !v.FetchedAt.Equal(pollAt) {
			t.Errorf("%s: IsRead=%v ReadAt=%v FetchedAt=%v; want unread, fetched at %v", g, v.IsRead, v.ReadAt, v.FetchedAt, pollAt)
		}
	}
	c, err := s.UnreadCounts(ctx)
	mustNoErr(t, err)
	if c.ByFeed[f.ID] != config.InitialUnread+2 {
		t.Errorf("unread = %d, want %d", c.ByFeed[f.ID], config.InitialUnread+2)
	}
}

func TestRecordFetchSuccess_CountsInsertedRowsAccurately(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), entriesNewestFirst("e", 3, t0), 5)
	dupA := entryAt("new1", t0)
	dupB := entryAt("new1", t0)
	dupB.Title = "second copy wins"
	entries := append(entriesNewestFirst("e", 3, t0), dupA, entryAt("new2", t0), dupB)
	n, err := s.RecordFetchSuccess(t.Context(), FetchSuccess{FeedID: f.ID, FetchedAt: t0, Entries: entries})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("inserted = %d, want 2", n)
	}
	if got := entryByGUID(t, s, f.ID, "new1").Title; got != "second copy wins" {
		t.Errorf("duplicate GUID in one fetch: title = %q, want last one", got)
	}
	if c := countRows(t, s, "entries", "feed_id = ?", f.ID); c != 5 {
		t.Errorf("entries = %d, want 5", c)
	}
}

func TestRecordFetchSuccess_DedupIsPerFeedOnly(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	// The same video in a channel feed and a playlist feed.
	video := entryAt("yt:video:abc123", t0)
	channel := mustCreateFeed(t, s, newFeed("https://www.youtube.com/feeds/videos.xml?channel_id=UC1", KindYouTube), nil, 5)
	playlist := mustCreateFeed(t, s, newFeed("https://www.youtube.com/feeds/videos.xml?playlist_id=PL1", KindYouTube), nil, 5)
	for _, f := range []Feed{channel, playlist} {
		n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: t0, Entries: []NewEntry{video}})
		if err != nil || n != 1 {
			t.Fatalf("feed %d: inserted = %d, %v; want 1", f.ID, n, err)
		}
	}
	if c := countRows(t, s, "entries", "guid = ?", video.GUID); c != 2 {
		t.Fatalf("entries with the shared GUID = %d, want 2", c)
	}
	// Their state is independent.
	mustNoErr(t, s.SetRead(ctx, entryID(t, s, channel.ID, video.GUID), true, t0))
	if entryByGUID(t, s, playlist.ID, video.GUID).IsRead {
		t.Fatal("marking one feed's entry read changed the other feed's entry")
	}
}

func TestRecordFetchSuccess_GUIDComparisonIsExactAndCaseSensitive(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), []NewEntry{entryAt("urn:ABC", t0)}, 5)
	n, err := s.RecordFetchSuccess(t.Context(), FetchSuccess{FeedID: f.ID, FetchedAt: t0,
		Entries: []NewEntry{entryAt("urn:abc", t0), entryAt("urn:ABC ", t0), entryAt("urn:ABC", t0)}})
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("inserted = %d, want 2 (case and whitespace variants are distinct GUIDs)", n)
	}
}

func TestRecordFetchSuccess_VeryLargeFeedInsertsOnlyNewEntries(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	const size = 3000
	f := mustCreateFeed(t, s, newFeed("https://big.example/feed", KindRSS), nil, 5)
	big := entriesNewestFirst("big", size, t0)

	n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: t0, Entries: big})
	if err != nil || n != size {
		t.Fatalf("first poll: inserted = %d, %v; want %d", n, err, size)
	}
	more := append(entriesNewestFirst("newer", 7, t0.Add(time.Hour)), big...)
	n, err = s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: t0.Add(time.Hour), Entries: more})
	if err != nil || n != 7 {
		t.Fatalf("second poll: inserted = %d, %v; want 7", n, err)
	}
	if c := countRows(t, s, "entries", "feed_id = ?", f.ID); c != size+7 {
		t.Fatalf("entries = %d, want %d", c, size+7)
	}
}

func TestRecordFetchSuccess_UpdatesFetchStateAndNonEmptyMetadata(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), nil, 5)
	mustNoErr(t, s.RenameFeed(ctx, f.ID, "My title"))
	mustNoErr(t, s.RecordFetchError(ctx, f.ID, "HTTP 503", t0, t0.Add(time.Hour)))
	mustNoErr(t, s.RecordFetchError(ctx, f.ID, "HTTP 503", t0, t0.Add(time.Hour)))

	at := t0.Add(2 * time.Hour)
	_, err := s.RecordFetchSuccess(ctx, FetchSuccess{
		FeedID: f.ID, FetchedAt: at, NextFetchAt: at.Add(6 * time.Hour),
		ETag: "", LastModified: "Thu, 01 Oct 2026 14:00:00 GMT",
		SiteURL: "", IconURL: "https://a.example/new-icon.png", OriginalTitle: "Renamed upstream",
	})
	mustNoErr(t, err)
	got, err := s.GetFeed(ctx, f.ID)
	mustNoErr(t, err)
	want := f
	want.Title = "My title" // the user's title is never overwritten
	want.ErrorCount, want.LastError = 0, ""
	want.LastFetchedAt, want.NextFetchAt = at, at.Add(6*time.Hour)
	want.ETag, want.LastModified = "", "Thu, 01 Oct 2026 14:00:00 GMT"
	want.IconURL, want.OriginalTitle = "https://a.example/new-icon.png", "Renamed upstream"
	if got != want {
		t.Fatalf("feed =\n%+v\nwant\n%+v", got, want)
	}
}

func TestRecordFetchSuccess_UnknownFeedWritesNothing(t *testing.T) {
	s := openTest(t)
	_, err := s.RecordFetchSuccess(t.Context(), FetchSuccess{FeedID: 7, FetchedAt: t0, Entries: entriesNewestFirst("e", 2, t0)})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if c := countRows(t, s, "entries", ""); c != 0 {
		t.Fatalf("entries = %d, want 0", c)
	}
}

func TestRecordFetchSuccess_InvalidEntryRejectsWholeFetch(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), nil, 5)
	_, err := s.RecordFetchSuccess(t.Context(), FetchSuccess{FeedID: f.ID, FetchedAt: t0.Add(time.Hour),
		Entries: []NewEntry{entryAt("ok", t0), entryAt("", t0)}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
	got, _ := s.GetFeed(t.Context(), f.ID)
	if c := countRows(t, s, "entries", ""); c != 0 || !got.LastFetchedAt.Equal(f.LastFetchedAt) {
		t.Fatalf("entries = %d, LastFetchedAt = %v; want nothing written", c, got.LastFetchedAt)
	}
}

func TestRecordNotModified_ResetsFailuresAndKeepsEntriesAndValidators(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), entriesNewestFirst("e", 3, t0), 5)
	mustNoErr(t, s.RecordFetchError(ctx, f.ID, "timeout", t0, t0))
	at := t0.Add(time.Hour)
	mustNoErr(t, s.RecordNotModified(ctx, f.ID, at, at.Add(6*time.Hour)))
	got, _ := s.GetFeed(ctx, f.ID)
	if got.ErrorCount != 0 || got.LastError != "" || !got.LastFetchedAt.Equal(at) ||
		!got.NextFetchAt.Equal(at.Add(6*time.Hour)) || got.ETag != f.ETag || got.LastModified != f.LastModified {
		t.Fatalf("feed after 304 = %+v", got)
	}
	if c := countRows(t, s, "entries", "feed_id = ?", f.ID); c != 3 {
		t.Fatalf("entries = %d, want 3", c)
	}
	if err := s.RecordNotModified(ctx, f.ID+1, at, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing feed err = %v, want ErrNotFound", err)
	}
}

// TestTTLSec_StoredWithTheDocumentAndKeptWithoutOne: <ttl> comes from a
// parsed document, so a 304 or a failure (no document) keeps the stored one.
func TestTTLSec_StoredWithTheDocumentAndKeptWithoutOne(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	nf := newFeed("https://a.example/feed", KindRSS)
	nf.TTLSec = 3600
	f := mustCreateFeed(t, s, nf, entriesNewestFirst("e", 1, t0), 5)
	if f.TTLSec != 3600 {
		t.Fatalf("TTLSec after create = %d, want 3600", f.TTLSec)
	}
	ttl := func() int {
		got, err := s.GetFeed(ctx, f.ID)
		mustNoErr(t, err)
		return got.TTLSec
	}
	mustNoErr(t, s.RecordNotModified(ctx, f.ID, t0, t0.Add(time.Hour)))
	mustNoErr(t, s.RecordFetchError(ctx, f.ID, "timeout", t0, t0.Add(time.Hour)))
	if got := ttl(); got != 3600 {
		t.Fatalf("TTLSec after 304 and failure = %d, want 3600", got)
	}
	if _, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: t0, TTLSec: 7200}); err != nil {
		t.Fatal(err)
	}
	if got := ttl(); got != 7200 {
		t.Fatalf("TTLSec after a document with <ttl> = %d, want 7200", got)
	}
	if _, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: t0}); err != nil {
		t.Fatal(err)
	}
	if got := ttl(); got != 0 {
		t.Fatalf("TTLSec after a document without <ttl> = %d, want 0", got)
	}
}

func TestRecordFetchError_CountsConsecutiveFailuresAndKeepsEntries(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), entriesNewestFirst("e", 3, t0), 5)
	for i := range config.WarnAfterFailures {
		at := t0.Add(time.Duration(i+1) * time.Hour)
		mustNoErr(t, s.RecordFetchError(ctx, f.ID, fmt.Sprintf("HTTP 404 #%d", i+1), at, at.Add(time.Hour)))
	}
	got, _ := s.GetFeed(ctx, f.ID)
	last := t0.Add(time.Duration(config.WarnAfterFailures) * time.Hour)
	if got.ErrorCount != config.WarnAfterFailures || got.LastError != fmt.Sprintf("HTTP 404 #%d", config.WarnAfterFailures) ||
		!got.LastFetchedAt.Equal(last) || !got.NextFetchAt.Equal(last.Add(time.Hour)) {
		t.Fatalf("feed after %d failures = %+v", config.WarnAfterFailures, got)
	}
	if c := countRows(t, s, "entries", "feed_id = ?", f.ID); c != 3 {
		t.Fatalf("entries = %d, want 3 (cached entries kept)", c)
	}
	if _, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: last}); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetFeed(ctx, f.ID); got.ErrorCount != 0 || got.LastError != "" {
		t.Fatalf("after success: ErrorCount = %d, LastError = %q", got.ErrorCount, got.LastError)
	}
	if err := s.RecordFetchError(ctx, f.ID+1, "x", t0, t0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing feed err = %v, want ErrNotFound", err)
	}
}

func TestDueFeeds_WithNoSubscriptionsNothingIsDue(t *testing.T) {
	s := openTest(t)
	due, err := s.DueFeeds(t.Context(), t0.Add(1000*time.Hour))
	if err != nil || len(due) != 0 {
		t.Fatalf("DueFeeds = %v, %v; want none", due, err)
	}
	if _, ok, err := s.NextFetchAfter(t.Context(), t0); ok || err != nil {
		t.Fatalf("NextFetchAfter = _, %v, %v; want false", ok, err)
	}
}

func TestDueFeeds_ReturnsFeedsDueAtOrBeforeNowOrderedByNextFetch(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	mk := func(url string, next time.Time) Feed {
		nf := newFeed(url, KindRSS)
		nf.NextFetchAt = next
		return mustCreateFeed(t, s, nf, nil, 5)
	}
	late := mk("https://late.example", t0.Add(time.Hour))
	now := mk("https://now.example", t0)
	early := mk("https://early.example", t0.Add(-time.Hour))
	never := mk("https://unscheduled.example", time.Time{}) // zero = due immediately
	if !never.NextFetchAt.IsZero() {
		t.Fatalf("zero NextFetchAt read back as %v", never.NextFetchAt)
	}

	due, err := s.DueFeeds(ctx, t0.Add(500*time.Millisecond))
	mustNoErr(t, err)
	var ids []int64
	for _, f := range due {
		ids = append(ids, f.ID)
	}
	if want := []int64{never.ID, early.ID, now.ID}; !slices.Equal(ids, want) {
		t.Fatalf("DueFeeds ids = %v, want %v", ids, want)
	}
	next, ok, err := s.NextFetchAfter(ctx, t0)
	if err != nil || !ok || !next.Equal(late.NextFetchAt) {
		t.Fatalf("NextFetchAfter(t0) = %v, %v, %v; want %v (strictly after)", next, ok, err, late.NextFetchAt)
	}
	if _, ok, _ := s.NextFetchAfter(ctx, late.NextFetchAt); ok {
		t.Fatal("NextFetchAfter(latest) reported a feed")
	}
}

func TestSetNextFetchAndSetAllNextFetch_RefreshAll(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	a := mustCreateFeed(t, s, newFeed("https://a.example", KindRSS), nil, 5)
	b := mustCreateFeed(t, s, newFeed("https://b.example", KindRSS), nil, 5)

	mustNoErr(t, s.SetNextFetch(ctx, a.ID, t0.Add(time.Minute)))
	if got, _ := s.GetFeed(ctx, a.ID); !got.NextFetchAt.Equal(t0.Add(time.Minute)) {
		t.Errorf("NextFetchAt = %v", got.NextFetchAt)
	}
	if err := s.SetNextFetch(ctx, b.ID+1, t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing feed err = %v, want ErrNotFound", err)
	}
	mustNoErr(t, s.SetAllNextFetch(ctx, t0.Add(-time.Second)))
	due, err := s.DueFeeds(ctx, t0)
	if err != nil || len(due) != 2 {
		t.Fatalf("DueFeeds after SetAllNextFetch = %d feeds, %v; want 2", len(due), err)
	}
}
