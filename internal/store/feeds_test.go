package store

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
)

func TestCreateFeed_StoresAllFeedFields(t *testing.T) {
	s := openTest(t)
	folder := mustFolder(t, s, "Blogs")
	nf := newFeed("https://blog.example/feed.xml", KindRSS)
	nf.FolderID = folder.ID
	f := mustCreateFeed(t, s, nf, nil, config.InitialUnread)

	want := Feed{
		ID: f.ID, Kind: KindRSS, URL: nf.URL, SiteURL: nf.SiteURL, Title: nf.Title,
		OriginalTitle: nf.OriginalTitle, IconURL: nf.IconURL, FolderID: folder.ID,
		ETag: nf.ETag, LastModified: nf.LastModified, LastFetchedAt: nf.FetchedAt,
		NextFetchAt: nf.NextFetchAt, CreatedAt: t0,
	}
	if f != want {
		t.Fatalf("CreateFeed =\n%+v\nwant\n%+v", f, want)
	}
	for _, get := range []func() (Feed, error){
		func() (Feed, error) { return s.GetFeed(t.Context(), f.ID) },
		func() (Feed, error) { return s.GetFeedByURL(t.Context(), nf.URL) },
	} {
		if got, err := get(); err != nil || got != want {
			t.Errorf("get = %+v, %v; want %+v", got, err, want)
		}
	}
}

func TestCreateFeed_FiveNewestUnreadRestStoredAsReadArchive(t *testing.T) {
	s := openTest(t)
	// Twelve entries in a scrambled order, as some feeds deliver them.
	all := entriesNewestFirst("e", 12, t0)
	scrambled := []NewEntry{all[7], all[2], all[11], all[0], all[5], all[9], all[1], all[4], all[10], all[3], all[8], all[6]}
	f := mustCreateFeed(t, s, newFeed("https://blog.example/feed", KindRSS), scrambled, config.InitialUnread)

	for i, e := range all {
		v := entryByGUID(t, s, f.ID, e.GUID)
		wantUnread := i < config.InitialUnread
		if v.IsRead == wantUnread {
			t.Errorf("%s (rank %d): IsRead = %v, want %v", e.GUID, i, v.IsRead, !wantUnread)
		}
		wantReadAt := t0
		if wantUnread {
			wantReadAt = time.Time{}
		}
		if !v.ReadAt.Equal(wantReadAt) {
			t.Errorf("%s: ReadAt = %v, want %v", e.GUID, v.ReadAt, wantReadAt)
		}
		if !v.FetchedAt.Equal(t0) || !v.PublishedAt.Equal(e.PublishedAt) {
			t.Errorf("%s: FetchedAt = %v, PublishedAt = %v", e.GUID, v.FetchedAt, v.PublishedAt)
		}
		if v.IsLater || v.IsFavourite {
			t.Errorf("%s: new entry has later/favourite set", e.GUID)
		}
	}
}

func TestCreateFeed_LoadMorePagesThroughStoredEntriesInBlocksOfFive(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, newFeed("https://blog.example/feed", KindRSS),
		entriesNewestFirst("e", 12, t0), config.InitialUnread)

	q := ListQuery{Scope: Scope{Kind: ScopeFeed, ID: f.ID}, Limit: config.FeedPageSize}
	var sizes []int
	var more []bool
	var seen []string
	for {
		p, err := s.ListEntries(t.Context(), q)
		if err != nil {
			t.Fatal(err)
		}
		sizes = append(sizes, len(p.Entries))
		more = append(more, p.HasMore)
		seen = append(seen, guids(p.Entries)...)
		if !p.HasMore {
			break
		}
		q.After = &p.Next
	}
	if !slices.Equal(sizes, []int{5, 5, 2}) || !slices.Equal(more, []bool{true, true, false}) {
		t.Fatalf("page sizes %v, HasMore %v; want [5 5 2], [true true false]", sizes, more)
	}
	if want := guids(asViews(entriesNewestFirst("e", 12, t0))); !slices.Equal(seen, want) {
		t.Fatalf("order = %v, want %v", seen, want)
	}
}

func TestCreateFeed_FewerThanFiveEntries_AllUnreadAndNoLoadMore(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, newFeed("https://blog.example/feed", KindRSS),
		entriesNewestFirst("e", 3, t0), config.InitialUnread)
	p, err := s.ListEntries(t.Context(), ListQuery{Scope: Scope{Kind: ScopeFeed, ID: f.ID}, Limit: config.FeedPageSize})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Entries) != 3 || p.HasMore {
		t.Fatalf("page = %d entries, HasMore %v; want 3, false", len(p.Entries), p.HasMore)
	}
	for _, v := range p.Entries {
		if v.IsRead {
			t.Errorf("%s is read, want unread", v.GUID)
		}
	}
}

func TestCreateFeed_EqualPublishedAtEarlierPositionCountsAsNewer(t *testing.T) {
	s := openTest(t)
	var entries []NewEntry
	for _, g := range []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6"} {
		entries = append(entries, entryAt(g, t0))
	}
	f := mustCreateFeed(t, s, newFeed("https://blog.example/feed", KindRSS), entries, 5)
	views := listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeFeed, ID: f.ID}, Limit: 3})
	if want := []string{"p0", "p1", "p2", "p3", "p4", "p5", "p6"}; !slices.Equal(guids(views), want) {
		t.Fatalf("grid order = %v, want slice order %v", guids(views), want)
	}
	for i, v := range views {
		if v.IsRead != (i >= 5) {
			t.Errorf("%s: IsRead = %v, want %v", v.GUID, v.IsRead, i >= 5)
		}
	}
}

func TestCreateFeed_DuplicateGUIDsCollapseLastWins(t *testing.T) {
	s := openTest(t)
	first := entryAt("dup", t0.Add(-time.Hour))
	last := entryAt("dup", t0.Add(-2*time.Hour))
	last.Title = "last version"
	f := mustCreateFeed(t, s, newFeed("https://blog.example/feed", KindRSS),
		[]NewEntry{first, entryAt("other", t0), last}, 5)
	if n := countRows(t, s, "entries", "feed_id = ?", f.ID); n != 2 {
		t.Fatalf("entries = %d, want 2", n)
	}
	v := entryByGUID(t, s, f.ID, "dup")
	if v.Title != "last version" || !v.PublishedAt.Equal(last.PublishedAt) {
		t.Fatalf("dup = %q at %v, want the last occurrence", v.Title, v.PublishedAt)
	}
}

func TestCreateFeed_DuplicateURLIsConflict(t *testing.T) {
	s := openTest(t)
	nf := newFeed("https://blog.example/feed", KindRSS)
	mustCreateFeed(t, s, nf, entriesNewestFirst("e", 2, t0), 5)
	_, err := s.CreateFeed(t.Context(), nf, entriesNewestFirst("x", 3, t0), 5)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if n := countRows(t, s, "entries", ""); n != 2 {
		t.Fatalf("entries = %d, want 2 (nothing from the rejected add)", n)
	}
}

// TestFeedURLErrors_HidePasswords: store errors end up in logs; a feed URL
// with embedded credentials must not leak its password there.
func TestFeedURLErrors_HidePasswords(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	const secret = "s3cret"
	nf := newFeed("https://jack:"+secret+"@private.example/feed", KindRSS)
	f := mustCreateFeed(t, s, nf, entriesNewestFirst("e", 1, t0), 5)
	other := mustCreateFeed(t, s, newFeed("https://jack:"+secret+"@other.example/feed", KindRSS), nil, 5)

	_, createErr := s.CreateFeed(ctx, nf, nil, 5)
	updateErr := s.UpdateFeedURL(ctx, other.ID, f.URL)
	_, getErr := s.GetFeedByURL(ctx, "https://jack:"+secret+"@missing.example/feed")
	for name, err := range map[string]error{"CreateFeed": createErr, "UpdateFeedURL": updateErr, "GetFeedByURL": getErr} {
		if err == nil {
			t.Errorf("%s: no error", name)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s error leaks the password: %v", name, err)
		}
	}
}

func TestCreateFeed_IsAtomic(t *testing.T) {
	s := openTest(t)
	// A TEMP trigger lives on one connection only: pin the pool to a single
	// connection first, then make an entry insert fail after the feed row
	// and other entries were written.
	s.db.SetMaxOpenConns(1)
	if _, err := s.db.Exec(`CREATE TEMP TRIGGER boom BEFORE INSERT ON entries
		WHEN NEW.guid = 'boom' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	// Inserted last to first, so "boom" (first in the slice) fails last.
	entries := append([]NewEntry{entryAt("boom", t0)}, entriesNewestFirst("ok", 4, t0)...)
	if _, err := s.CreateFeed(t.Context(), newFeed("https://blog.example/feed", KindRSS), entries, 5); err == nil {
		t.Fatal("CreateFeed succeeded despite a failing entry")
	}
	if n := countRows(t, s, "feeds", ""); n != 0 {
		t.Errorf("feeds = %d, want 0", n)
	}
	if n := countRows(t, s, "entries", ""); n != 0 {
		t.Errorf("entries = %d, want 0", n)
	}
}

func TestCreateFeed_RejectsInvalidInput(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	bad := newFeed("https://blog.example/feed", "atom")
	if _, err := s.CreateFeed(ctx, bad, nil, 5); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown kind err = %v, want ErrInvalid", err)
	}
	if _, err := s.CreateFeed(ctx, newFeed(" ", KindRSS), nil, 5); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty URL err = %v, want ErrInvalid", err)
	}
	noGUID := entryAt("", t0)
	if _, err := s.CreateFeed(ctx, newFeed("https://a.example", KindRSS), []NewEntry{noGUID}, 5); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty GUID err = %v, want ErrInvalid", err)
	}
	inMissingFolder := newFeed("https://b.example", KindRSS)
	inMissingFolder.FolderID = 42
	if _, err := s.CreateFeed(ctx, inMissingFolder, nil, 5); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing folder err = %v, want ErrNotFound", err)
	}
	if n := countRows(t, s, "feeds", ""); n != 0 {
		t.Errorf("feeds = %d, want 0", n)
	}
}

func TestCreateFeed_TitleFallsBackToOriginalTitleThenURL(t *testing.T) {
	s := openTest(t)
	nf := newFeed("https://a.example/feed", KindRSS)
	nf.Title = "  "
	if f := mustCreateFeed(t, s, nf, nil, 5); f.Title != nf.OriginalTitle {
		t.Errorf("Title = %q, want OriginalTitle %q", f.Title, nf.OriginalTitle)
	}
	nf = newFeed("https://b.example/feed", KindRSS)
	nf.Title, nf.OriginalTitle = "", ""
	if f := mustCreateFeed(t, s, nf, nil, 5); f.Title != nf.URL {
		t.Errorf("Title = %q, want URL", f.Title)
	}
}

func TestCreateFeed_PublishedMissingFallsBackToUpdatedThenFetchedAt(t *testing.T) {
	s := openTest(t)
	upd := entryAt("upd", time.Time{})
	upd.UpdatedAt = t0.Add(-3 * time.Hour)
	none := entryAt("none", time.Time{})
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), []NewEntry{upd, none}, 5)
	if v := entryByGUID(t, s, f.ID, "upd"); !v.PublishedAt.Equal(upd.UpdatedAt) || !v.UpdatedAt.Equal(upd.UpdatedAt) {
		t.Errorf("upd: PublishedAt = %v, UpdatedAt = %v; want both %v", v.PublishedAt, v.UpdatedAt, upd.UpdatedAt)
	}
	if v := entryByGUID(t, s, f.ID, "none"); !v.PublishedAt.Equal(t0) || !v.UpdatedAt.IsZero() {
		t.Errorf("none: PublishedAt = %v, UpdatedAt = %v; want fetched_at %v and zero", v.PublishedAt, v.UpdatedAt, t0)
	}
}

func TestGetFeed_NotFound(t *testing.T) {
	s := openTest(t)
	if _, err := s.GetFeed(t.Context(), 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFeed err = %v, want ErrNotFound", err)
	}
	if _, err := s.GetFeedByURL(t.Context(), "https://nope.example"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFeedByURL err = %v, want ErrNotFound", err)
	}
}

func TestListFeeds_OrderedByTitleCaseInsensitive(t *testing.T) {
	s := openTest(t)
	for i, title := range []string{"beta", "Alpha", "gamma"} {
		nf := newFeed("https://"+string(rune('a'+i))+".example", KindRSS)
		nf.Title = title
		mustCreateFeed(t, s, nf, nil, 5)
	}
	feeds, err := s.ListFeeds(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var titles []string
	for _, f := range feeds {
		titles = append(titles, f.Title)
	}
	if want := []string{"Alpha", "beta", "gamma"}; !slices.Equal(titles, want) {
		t.Fatalf("titles = %v, want %v", titles, want)
	}
}

func TestRenameFeed(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed("https://a.example", KindRSS), nil, 5)
	if err := s.RenameFeed(ctx, f.ID, "  My blog "); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetFeed(ctx, f.ID)
	if got.Title != "My blog" || got.OriginalTitle != f.OriginalTitle {
		t.Errorf("Title = %q, OriginalTitle = %q", got.Title, got.OriginalTitle)
	}
	if err := s.RenameFeed(ctx, f.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty title err = %v, want ErrInvalid", err)
	}
	if err := s.RenameFeed(ctx, f.ID+1, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing feed err = %v, want ErrNotFound", err)
	}
}

func TestMoveFeed(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	folder := mustFolder(t, s, "Blogs")
	f := mustCreateFeed(t, s, newFeed("https://a.example", KindRSS), nil, 5)

	if err := s.MoveFeed(ctx, f.ID, folder.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetFeed(ctx, f.ID); got.FolderID != folder.ID {
		t.Errorf("FolderID = %d, want %d", got.FolderID, folder.ID)
	}
	if err := s.MoveFeed(ctx, f.ID, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetFeed(ctx, f.ID); got.FolderID != 0 {
		t.Errorf("FolderID = %d, want 0", got.FolderID)
	}
	if err := s.MoveFeed(ctx, f.ID, folder.ID+10); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing folder err = %v, want ErrNotFound", err)
	}
	if err := s.MoveFeed(ctx, f.ID+10, folder.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing feed err = %v, want ErrNotFound", err)
	}
}

func TestSetFeedInterval(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed("https://a.example", KindRSS), nil, 5)
	if err := s.SetFeedInterval(ctx, f.ID, 3600); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetFeed(ctx, f.ID); got.IntervalSec != 3600 {
		t.Errorf("IntervalSec = %d, want 3600", got.IntervalSec)
	}
	if err := s.SetFeedInterval(ctx, f.ID, 0); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetFeed(ctx, f.ID); got.IntervalSec != 0 {
		t.Errorf("IntervalSec = %d, want 0 (global default)", got.IntervalSec)
	}
	tooShort := int(config.MinPollInterval/time.Second) - 1
	for _, sec := range []int{-1, tooShort} {
		if err := s.SetFeedInterval(ctx, f.ID, sec); !errors.Is(err, ErrInvalid) {
			t.Errorf("SetFeedInterval(%d) err = %v, want ErrInvalid", sec, err)
		}
	}
	if err := s.SetFeedInterval(ctx, f.ID+1, 3600); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing feed err = %v, want ErrNotFound", err)
	}
}

func TestDeleteFeed_UnsubscribeDeletesEntriesEntryTagsAndSavedEntries(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	gone := mustCreateFeed(t, s, newFeed("https://gone.example", KindRSS), entriesNewestFirst("g", 4, t0), 5)
	kept := mustCreateFeed(t, s, newFeed("https://kept.example", KindRSS), entriesNewestFirst("k", 2, t0), 5)

	g0, g1 := entryID(t, s, gone.ID, "g0"), entryID(t, s, gone.ID, "g1")
	k0 := entryID(t, s, kept.ID, "k0")
	mustNoErr(t, s.SetLater(ctx, g0, true))
	mustNoErr(t, s.SetFavourite(ctx, g1, true))
	tag, err := s.AddEntryTag(ctx, g0, "go")
	mustNoErr(t, err)
	_, err = s.AddEntryTag(ctx, k0, "go")
	mustNoErr(t, err)

	st, err := s.FeedEntryStats(ctx, gone.ID)
	mustNoErr(t, err)
	if st != (FeedEntryStats{Total: 4, Saved: 2}) {
		t.Errorf("FeedEntryStats = %+v, want {4 2}", st)
	}

	mustNoErr(t, s.DeleteFeed(ctx, gone.ID))

	if _, err := s.GetFeed(ctx, gone.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetFeed after delete err = %v, want ErrNotFound", err)
	}
	if n := countRows(t, s, "entries", "feed_id = ?", gone.ID); n != 0 {
		t.Errorf("entries of deleted feed = %d, want 0", n)
	}
	if n := countRows(t, s, "entry_tags", "entry_id IN (?, ?)", g0, g1); n != 0 {
		t.Errorf("entry_tags of deleted entries = %d, want 0", n)
	}
	if n := countRows(t, s, "entries", "feed_id = ?", kept.ID); n != 2 {
		t.Errorf("entries of other feed = %d, want 2", n)
	}
	tags, err := s.ListTags(ctx)
	mustNoErr(t, err)
	if len(tags) != 1 || tags[0].ID != tag.ID || tags[0].Count != 1 {
		t.Errorf("ListTags = %+v, want tag kept with Count 1", tags)
	}
	if err := s.DeleteFeed(ctx, gone.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteFeed err = %v, want ErrNotFound", err)
	}
}

func TestFeedEntryStats_CountsSavedEntriesOnce(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed("https://a.example", KindRSS), entriesNewestFirst("e", 3, t0), 5)
	id := entryID(t, s, f.ID, "e1")
	mustNoErr(t, s.SetLater(ctx, id, true))
	mustNoErr(t, s.SetFavourite(ctx, id, true))
	st, err := s.FeedEntryStats(ctx, f.ID)
	if err != nil || st != (FeedEntryStats{Total: 3, Saved: 1}) {
		t.Fatalf("FeedEntryStats = %+v, %v; want {3 1}", st, err)
	}
	if _, err := s.FeedEntryStats(ctx, f.ID+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing feed err = %v, want ErrNotFound", err)
	}
}

func TestUpdateFeedURL_PermanentRedirect(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	a := mustCreateFeed(t, s, newFeed("http://a.example/feed", KindRSS), nil, 5)
	b := mustCreateFeed(t, s, newFeed("https://b.example/feed", KindRSS), nil, 5)

	mustNoErr(t, s.UpdateFeedURL(ctx, a.ID, "https://a.example/feed"))
	if got, err := s.GetFeedByURL(ctx, "https://a.example/feed"); err != nil || got.ID != a.ID {
		t.Errorf("GetFeedByURL(new) = %+v, %v", got, err)
	}
	if err := s.UpdateFeedURL(ctx, a.ID, b.URL); !errors.Is(err, ErrConflict) {
		t.Errorf("redirect onto another feed's URL err = %v, want ErrConflict", err)
	}
	if err := s.UpdateFeedURL(ctx, b.ID+1, "https://c.example"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing feed err = %v, want ErrNotFound", err)
	}
	if err := s.UpdateFeedURL(ctx, a.ID, ""); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty URL err = %v, want ErrInvalid", err)
	}
}

func TestSetFeedIcon_KeptByPollsOfADocumentWithoutIcon(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	const avatar = "https://images.example/avatar.jpg"
	nf := newFeed("https://a.example/feed", KindYouTube)
	nf.IconURL = ""
	a := mustCreateFeed(t, s, nf, nil, config.InitialUnread)
	b := mustCreateFeed(t, s, newFeed("https://b.example/feed", KindRSS), nil, config.InitialUnread)

	mustNoErr(t, s.SetFeedIcon(ctx, a.ID, avatar))
	_, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: a.ID, FetchedAt: a.LastFetchedAt.Add(time.Hour)})
	mustNoErr(t, err)
	if got, err := s.GetFeed(ctx, a.ID); err != nil || got.IconURL != avatar {
		t.Errorf("icon = %q, %v; want %q", got.IconURL, err, avatar)
	}
	if got, err := s.GetFeed(ctx, b.ID); err != nil || got.IconURL != b.IconURL {
		t.Errorf("other feed's icon = %q, %v; want %q", got.IconURL, err, b.IconURL)
	}
	if err := s.SetFeedIcon(ctx, b.ID+1, avatar); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing feed err = %v, want ErrNotFound", err)
	}
	if err := s.SetFeedIcon(ctx, a.ID, " "); !errors.Is(err, ErrInvalid) {
		t.Errorf("empty URL err = %v, want ErrInvalid", err)
	}
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// asViews wraps entries so guids() can be used on them.
func asViews(entries []NewEntry) []EntryView {
	out := make([]EntryView, len(entries))
	for i, e := range entries {
		out[i].GUID = e.GUID
	}
	return out
}

func TestFeedErrorsNeverShowURLCredentials(t *testing.T) {
	s := openTest(t)
	for _, u := range []string{"https://tok3n@a.example/feed", "https://reader:s3cret@b.example/feed"} {
		mustCreateFeed(t, s, newFeed(u, KindRSS), nil, config.InitialUnread)
		_, err := s.CreateFeed(t.Context(), newFeed(u, KindRSS), nil, config.InitialUnread)
		if !errors.Is(err, ErrConflict) {
			t.Fatalf("err = %v, want ErrConflict", err)
		}
		for _, secret := range []string{"tok3n", "reader", "s3cret"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%q shows %q", err, secret)
			}
		}
	}
}

// TestFeedTitleFallbackNeverShowsURLCredentials: a feed with no title
// falls back to its URL, and titles are shown and logged.
func TestFeedTitleFallbackNeverShowsURLCredentials(t *testing.T) {
	s := openTest(t)
	nf := newFeed("https://tok3n@a.example/feed", KindRSS)
	nf.Title, nf.OriginalTitle = "", ""
	f := mustCreateFeed(t, s, nf, nil, config.InitialUnread)
	if strings.Contains(f.Title, "tok3n") || f.Title == "" {
		t.Errorf("title = %q; want the URL without its credentials", f.Title)
	}
}
