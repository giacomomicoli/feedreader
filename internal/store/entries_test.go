package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
)

// gridFixture is a small library:
//
//	folder Videos: youtube feed yt  (y0..y3, published t0, -1h, -2h, -3h)
//	folder Blogs:  rss feed a       (a0..a3, published t0-30m, -1h30m, ...)
//	uncategorized: rss feed b       (b0..b2, published t0-15m, -1h15m, ...)
//
// Global order: y0 b0 a0 y1 b1 a1 y2 b2 a2 y3 a3.
// State: read y1 a0 b2; later y0 y1 a1 b2; favourite a0 y3;
// tag "go" on a0 b0; tag "music" on y0.
type gridFixture struct {
	s             *SQLite
	videos, blogs Folder
	yt, a, b      Feed
	goTag, music  Tag
	ids           map[string]int64
}

func newGridFixture(t *testing.T) *gridFixture {
	t.Helper()
	s := openTest(t)
	ctx := t.Context()
	fx := &gridFixture{s: s, ids: map[string]int64{}}
	fx.videos = mustFolder(t, s, "Videos")
	fx.blogs = mustFolder(t, s, "Blogs")

	nf := newFeed("https://www.youtube.com/feeds/videos.xml?channel_id=UC1", KindYouTube)
	nf.FolderID = fx.videos.ID
	fx.yt = mustCreateFeed(t, s, nf, entriesNewestFirst("y", 4, t0), 10)
	nf = newFeed("https://a.example/feed", KindRSS)
	nf.FolderID = fx.blogs.ID
	fx.a = mustCreateFeed(t, s, nf, entriesNewestFirst("a", 4, t0.Add(-30*time.Minute)), 10)
	fx.b = mustCreateFeed(t, s, newFeed("https://b.example/feed", KindRSS), entriesNewestFirst("b", 3, t0.Add(-15*time.Minute)), 10)

	for _, f := range []Feed{fx.yt, fx.a, fx.b} {
		rows, err := s.db.Query(`SELECT guid, id FROM entries WHERE feed_id = ?`, f.ID)
		mustNoErr(t, err)
		for rows.Next() {
			var g string
			var id int64
			mustNoErr(t, rows.Scan(&g, &id))
			fx.ids[g] = id
		}
		rows.Close()
	}
	for _, g := range []string{"y1", "a0", "b2"} {
		mustNoErr(t, s.SetRead(ctx, fx.ids[g], true, t0))
	}
	for _, g := range []string{"y0", "y1", "a1", "b2"} {
		mustNoErr(t, s.SetLater(ctx, fx.ids[g], true))
	}
	for _, g := range []string{"a0", "y3"} {
		mustNoErr(t, s.SetFavourite(ctx, fx.ids[g], true))
	}
	var err error
	fx.goTag, err = s.AddEntryTag(ctx, fx.ids["a0"], "go")
	mustNoErr(t, err)
	_, err = s.AddEntryTag(ctx, fx.ids["b0"], "Go")
	mustNoErr(t, err)
	fx.music, err = s.AddEntryTag(ctx, fx.ids["y0"], "music")
	mustNoErr(t, err)
	return fx
}

func TestListEntries_EveryScopeWithUnreadFilter(t *testing.T) {
	fx := newGridFixture(t)
	cases := []struct {
		name       string
		scope      Scope
		unreadOnly bool
		want       string
	}{
		{"all", Scope{Kind: ScopeAll}, false, "y0 b0 a0 y1 b1 a1 y2 b2 a2 y3 a3"},
		{"all unread", Scope{Kind: ScopeAll}, true, "y0 b0 b1 a1 y2 a2 y3 a3"},
		{"folder", Scope{Kind: ScopeFolder, ID: fx.videos.ID}, false, "y0 y1 y2 y3"},
		{"folder unread", Scope{Kind: ScopeFolder, ID: fx.videos.ID}, true, "y0 y2 y3"},
		{"other folder unread", Scope{Kind: ScopeFolder, ID: fx.blogs.ID}, true, "a1 a2 a3"},
		{"feed", Scope{Kind: ScopeFeed, ID: fx.b.ID}, false, "b0 b1 b2"},
		{"feed unread", Scope{Kind: ScopeFeed, ID: fx.b.ID}, true, "b0 b1"},
		// Watch later / Read later / Favourites list entries regardless of
		// read state, even when the Unread filter is on.
		{"watch later", Scope{Kind: ScopeWatchLater}, false, "y0 y1"},
		{"watch later ignores unread", Scope{Kind: ScopeWatchLater}, true, "y0 y1"},
		{"read later", Scope{Kind: ScopeReadLater}, false, "a1 b2"},
		{"read later ignores unread", Scope{Kind: ScopeReadLater}, true, "a1 b2"},
		{"favourites", Scope{Kind: ScopeFavourites}, false, "a0 y3"},
		{"favourites ignores unread", Scope{Kind: ScopeFavourites}, true, "a0 y3"},
		{"tag", Scope{Kind: ScopeTag, ID: fx.goTag.ID}, false, "b0 a0"},
		{"tag unread", Scope{Kind: ScopeTag, ID: fx.goTag.ID}, true, "b0"},
		{"other tag", Scope{Kind: ScopeTag, ID: fx.music.ID}, false, "y0"},
		{"unknown folder", Scope{Kind: ScopeFolder, ID: 999}, false, ""},
		{"unknown feed", Scope{Kind: ScopeFeed, ID: 999}, false, ""},
		{"unknown tag", Scope{Kind: ScopeTag, ID: 999}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, limit := range []int{1, 3, 100} {
				got := listAll(t, fx.s, ListQuery{Scope: tc.scope, UnreadOnly: tc.unreadOnly, Limit: limit})
				if s := strings.Join(guids(got), " "); s != tc.want {
					t.Fatalf("limit %d: got %q, want %q", limit, s, tc.want)
				}
			}
		})
	}
}

func TestListEntries_EntryViewCarriesFeedFieldsStateAndSortedTags(t *testing.T) {
	fx := newGridFixture(t)
	ctx := t.Context()
	_, err := fx.s.AddEntryTag(ctx, fx.ids["a0"], "Alpha")
	mustNoErr(t, err)
	_, err = fx.s.AddEntryTag(ctx, fx.ids["a0"], "zeta")
	mustNoErr(t, err)

	p, err := fx.s.ListEntries(ctx, ListQuery{Scope: Scope{Kind: ScopeFavourites}, Limit: 10})
	mustNoErr(t, err)
	if len(p.Entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(p.Entries))
	}
	a0, y3 := p.Entries[0], p.Entries[1]
	if a0.FeedTitle != fx.a.Title || a0.FeedKind != KindRSS || a0.FeedIconURL != fx.a.IconURL {
		t.Errorf("a0 feed fields = %q %q %q", a0.FeedTitle, a0.FeedKind, a0.FeedIconURL)
	}
	if y3.FeedKind != KindYouTube || y3.FeedTitle != fx.yt.Title {
		t.Errorf("y3 feed fields = %q %q", y3.FeedTitle, y3.FeedKind)
	}
	if !a0.IsRead || !a0.IsFavourite || a0.IsLater || a0.FeedID != fx.a.ID {
		t.Errorf("a0 state = %+v", a0.Entry)
	}
	if got := tagNames(a0.Tags); !slices.Equal(got, []string{"Alpha", "go", "zeta"}) {
		t.Errorf("a0 tags = %v, want [Alpha go zeta] (by name, case-insensitive)", got)
	}
	if len(y3.Tags) != 0 {
		t.Errorf("y3 tags = %v, want none", y3.Tags)
	}
	got, err := fx.s.GetEntry(ctx, fx.ids["a0"])
	mustNoErr(t, err)
	if !slices.Equal(tagNames(got.Tags), tagNames(a0.Tags)) || got.FeedTitle != a0.FeedTitle || got.Entry != a0.Entry {
		t.Errorf("GetEntry = %+v, want the same view as ListEntries %+v", got, a0)
	}
	if _, err := fx.s.GetEntry(ctx, 99999); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetEntry(missing) err = %v, want ErrNotFound", err)
	}
}

func TestListEntries_SortedByPublishedDescThenIDDesc(t *testing.T) {
	s := openTest(t)
	// Ties in published_at across two feeds: id DESC breaks them.
	a := mustCreateFeed(t, s, newFeed("https://a.example", KindRSS), []NewEntry{entryAt("a-tie", t0), entryAt("a-old", t0.Add(-time.Hour))}, 5)
	b := mustCreateFeed(t, s, newFeed("https://b.example", KindRSS), []NewEntry{entryAt("b-tie", t0), entryAt("b-new", t0.Add(time.Hour))}, 5)
	_ = a
	_ = b
	got := listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeAll}, Limit: 1})
	if want := []string{"b-new", "b-tie", "a-tie", "a-old"}; !slices.Equal(guids(got), want) {
		t.Fatalf("order = %v, want %v", guids(got), want)
	}
	for i := 1; i < len(got); i++ {
		p, c := got[i-1], got[i]
		if c.PublishedAt.After(p.PublishedAt) || (c.PublishedAt.Equal(p.PublishedAt) && c.ID > p.ID) {
			t.Fatalf("not sorted by (published_at DESC, id DESC) at %d", i)
		}
	}
}

func TestListEntries_PaginationHasNoGapsOrDuplicatesWhileEntriesAreMarkedRead(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	// 23 unread entries across two feeds, with pairs sharing a timestamp so
	// pages break inside ties.
	var ea, eb []NewEntry
	for i := range 12 {
		ea = append(ea, entryAt(fmt.Sprintf("a%02d", i), t0.Add(-time.Duration(i)*time.Hour)))
	}
	for i := range 11 {
		eb = append(eb, entryAt(fmt.Sprintf("b%02d", i), t0.Add(-time.Duration(i)*time.Hour)))
	}
	fa := mustCreateFeed(t, s, newFeed("https://a.example", KindRSS), ea, 100)
	fb := mustCreateFeed(t, s, newFeed("https://b.example", KindRSS), eb, 100)
	all := listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeAll}, Limit: 100})
	if len(all) != 23 {
		t.Fatalf("setup: %d entries", len(all))
	}

	q := ListQuery{Scope: Scope{Kind: ScopeAll}, UnreadOnly: true, Limit: 5}
	var seen []EntryView
	skipped := map[string]bool{}
	for page := 0; ; page++ {
		p, err := s.ListEntries(ctx, q)
		mustNoErr(t, err)
		seen = append(seen, p.Entries...)
		if !p.HasMore {
			break
		}
		if p.Next != (Cursor{PublishedAt: p.Entries[len(p.Entries)-1].PublishedAt, ID: p.Entries[len(p.Entries)-1].ID}) {
			t.Fatalf("page %d: Next = %+v is not the last entry", page, p.Next)
		}
		// The user marks everything on this page read (as the card grid's
		// Mark read would), plus one entry further down that they have not
		// seen yet; also a new entry arrives at the top.
		for _, v := range p.Entries {
			mustNoErr(t, s.SetRead(ctx, v.ID, true, t0))
		}
		idx := slices.IndexFunc(all, func(v EntryView) bool { return v.ID == p.Entries[len(p.Entries)-1].ID })
		if idx+3 < len(all) {
			ahead := all[idx+3]
			mustNoErr(t, s.SetRead(ctx, ahead.ID, true, t0))
			skipped[ahead.GUID] = true
		}
		_, err = s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: fa.ID, FetchedAt: t0,
			Entries: []NewEntry{entryAt(fmt.Sprintf("late%d", page), t0.Add(time.Duration(page+1)*time.Hour))}})
		mustNoErr(t, err)
		q.After = &p.Next
	}
	_ = fb

	var want []string
	for _, v := range all {
		if !skipped[v.GUID] {
			want = append(want, v.GUID)
		}
	}
	if got := guids(seen); !slices.Equal(got, want) {
		t.Fatalf("paged = %v\nwant    %v", got, want)
	}
}

func TestListEntries_HasMoreIsFalseWhenLastPageIsExactlyFull(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, newFeed("https://a.example", KindRSS), entriesNewestFirst("e", 10, t0), 0)
	q := ListQuery{Scope: Scope{Kind: ScopeFeed, ID: f.ID}, Limit: 5}
	p1, err := s.ListEntries(t.Context(), q)
	mustNoErr(t, err)
	q.After = &p1.Next
	p2, err := s.ListEntries(t.Context(), q)
	mustNoErr(t, err)
	if !p1.HasMore || len(p2.Entries) != 5 || p2.HasMore {
		t.Fatalf("p1.HasMore=%v p2=%d entries HasMore=%v; want true, 5, false", p1.HasMore, len(p2.Entries), p2.HasMore)
	}
	q.After = &p2.Next
	p3, err := s.ListEntries(t.Context(), q)
	if err != nil || len(p3.Entries) != 0 || p3.HasMore {
		t.Fatalf("page past the end = %+v, %v; want empty", p3, err)
	}
}

func TestListEntries_RejectsInvalidQuery(t *testing.T) {
	s := openTest(t)
	for _, q := range []ListQuery{
		{Scope: Scope{Kind: ScopeAll}, Limit: 0},
		{Scope: Scope{Kind: ScopeAll}, Limit: -1},
		{Scope: Scope{Kind: "inbox"}, Limit: 5},
		{Scope: Scope{}, Limit: 5},
	} {
		if _, err := s.ListEntries(t.Context(), q); !errors.Is(err, ErrInvalid) {
			t.Errorf("ListEntries(%+v) err = %v, want ErrInvalid", q, err)
		}
	}
}

// TestListEntries_GridQueriesWalkAnIndexInOrder guards the index strategy:
// the hot scopes must read rows in grid order from an index and stop at
// LIMIT instead of sorting the whole scope.
func TestListEntries_GridQueriesWalkAnIndexInOrder(t *testing.T) {
	s := openTest(t)
	cursor := &Cursor{PublishedAt: t0, ID: 10}
	for _, sc := range []Scope{
		{Kind: ScopeAll}, {Kind: ScopeFeed, ID: 1}, {Kind: ScopeFavourites},
		{Kind: ScopeWatchLater}, {Kind: ScopeReadLater},
	} {
		for _, unread := range []bool{false, true} {
			for _, after := range []*Cursor{nil, cursor} {
				query, args, err := listQuerySQL(ListQuery{Scope: sc, UnreadOnly: unread, Limit: config.ScopePageSize, After: after})
				mustNoErr(t, err)
				plan := explain(t, s, query, args...)
				// "SCAN e USING INDEX x" is an ordered index walk (fine);
				// a bare "SCAN e" is a full table scan.
				fullScan := slices.ContainsFunc(strings.Split(plan, "\n"), func(l string) bool {
					return strings.HasPrefix(l, "SCAN e") && !strings.Contains(l, "USING")
				})
				if strings.Contains(plan, "TEMP B-TREE") || fullScan {
					t.Errorf("%s unread=%v after=%v: plan sorts or scans:\n%s", sc.Kind, unread, after != nil, plan)
				}
			}
		}
	}
	plan := explain(t, s, `SELECT f.id, f.folder_id,
		(SELECT count(*) FROM entries e WHERE e.feed_id = f.id AND e.is_read = 0) FROM feeds f`)
	if !strings.Contains(plan, "COVERING INDEX entries_unread") {
		t.Errorf("unread counts do not use the covering partial index:\n%s", plan)
	}
}

func explain(t *testing.T, s *SQLite, query string, args ...any) string {
	t.Helper()
	rows, err := s.db.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	mustNoErr(t, err)
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		mustNoErr(t, rows.Scan(&id, &parent, &unused, &detail))
		lines = append(lines, detail)
	}
	mustNoErr(t, rows.Err())
	return strings.Join(lines, "\n")
}

func TestSetRead_MarkReadAndUnread(t *testing.T) {
	fx := newGridFixture(t)
	ctx := t.Context()
	id := fx.ids["y0"] // unread, later, tagged
	at := t0.Add(42 * time.Minute)

	mustNoErr(t, fx.s.SetRead(ctx, id, true, at))
	v, _ := fx.s.GetEntry(ctx, id)
	if !v.IsRead || !v.ReadAt.Equal(at) {
		t.Fatalf("after mark watched: IsRead=%v ReadAt=%v", v.IsRead, v.ReadAt)
	}
	// Marking an entry read/watched does not clear is_later.
	if !v.IsLater {
		t.Fatal("mark read cleared is_later")
	}
	mustNoErr(t, fx.s.SetRead(ctx, id, false, at))
	v, _ = fx.s.GetEntry(ctx, id)
	if v.IsRead || !v.ReadAt.IsZero() || !v.IsLater {
		t.Fatalf("after mark unwatched: IsRead=%v ReadAt=%v IsLater=%v", v.IsRead, v.ReadAt, v.IsLater)
	}
	if err := fx.s.SetRead(ctx, 99999, true, at); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing entry err = %v, want ErrNotFound", err)
	}
}

func TestSetLaterAndSetFavourite_NeverChangeReadState(t *testing.T) {
	fx := newGridFixture(t)
	ctx := t.Context()
	for _, g := range []string{"a0", "a2"} { // read, unread
		id := fx.ids[g]
		before, _ := fx.s.GetEntry(ctx, id)
		mustNoErr(t, fx.s.SetLater(ctx, id, true))
		mustNoErr(t, fx.s.SetFavourite(ctx, id, true))
		mid, _ := fx.s.GetEntry(ctx, id)
		mustNoErr(t, fx.s.SetLater(ctx, id, false))
		mustNoErr(t, fx.s.SetFavourite(ctx, id, false))
		after, _ := fx.s.GetEntry(ctx, id)
		if !mid.IsLater || !mid.IsFavourite || after.IsLater || after.IsFavourite {
			t.Errorf("%s: flags mid=%v/%v after=%v/%v", g, mid.IsLater, mid.IsFavourite, after.IsLater, after.IsFavourite)
		}
		for _, v := range []EntryView{mid, after} {
			if v.IsRead != before.IsRead || !v.ReadAt.Equal(before.ReadAt) {
				t.Errorf("%s: read state changed from %v/%v to %v/%v", g, before.IsRead, before.ReadAt, v.IsRead, v.ReadAt)
			}
		}
	}
	if err := fx.s.SetLater(ctx, 99999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetLater missing err = %v", err)
	}
	if err := fx.s.SetFavourite(ctx, 99999, true); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetFavourite missing err = %v", err)
	}
}

func TestMarkScopeReadUpTo_OnlyTouchesTheScope(t *testing.T) {
	cases := []struct {
		name     string
		scope    func(fx *gridFixture) Scope
		changed  int64
		stillNew string // unread entries afterwards, in grid order
	}{
		{"all", func(*gridFixture) Scope { return Scope{Kind: ScopeAll} }, 8, ""},
		{"folder", func(fx *gridFixture) Scope { return Scope{Kind: ScopeFolder, ID: fx.videos.ID} }, 3, "b0 b1 a1 a2 a3"},
		{"feed", func(fx *gridFixture) Scope { return Scope{Kind: ScopeFeed, ID: fx.b.ID} }, 2, "y0 a1 y2 a2 y3 a3"},
		{"watch later", func(*gridFixture) Scope { return Scope{Kind: ScopeWatchLater} }, 1, "b0 b1 a1 y2 a2 y3 a3"},
		{"read later", func(*gridFixture) Scope { return Scope{Kind: ScopeReadLater} }, 1, "y0 b0 b1 y2 a2 y3 a3"},
		{"favourites", func(*gridFixture) Scope { return Scope{Kind: ScopeFavourites} }, 1, "y0 b0 b1 a1 y2 a2 a3"},
		{"tag", func(fx *gridFixture) Scope { return Scope{Kind: ScopeTag, ID: fx.goTag.ID} }, 1, "y0 b1 a1 y2 a2 y3 a3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fx := newGridFixture(t)
			at := t0.Add(time.Hour)
			maxID, err := fx.s.MaxEntryID(t.Context())
			mustNoErr(t, err)
			n, err := fx.s.MarkScopeReadUpTo(t.Context(), tc.scope(fx), maxID, at)
			mustNoErr(t, err)
			if n != tc.changed {
				t.Errorf("changed = %d, want %d", n, tc.changed)
			}
			unread := listAll(t, fx.s, ListQuery{Scope: Scope{Kind: ScopeAll}, UnreadOnly: true, Limit: 50})
			if got := strings.Join(guids(unread), " "); got != tc.stillNew {
				t.Errorf("unread after = %q, want %q", got, tc.stillNew)
			}
			// Entries marked by this call carry read_at = at; earlier reads keep theirs.
			if v, _ := fx.s.GetEntry(t.Context(), fx.ids["a0"]); !v.ReadAt.Equal(t0) {
				t.Errorf("previously read a0: ReadAt = %v, want unchanged %v", v.ReadAt, t0)
			}
		})
	}
}

func TestMarkScopeReadUpTo_LeavesEntriesStoredAfterThePageUnread(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed("https://a.example", KindRSS), entriesNewestFirst("old", 3, t0), 5)
	// The page is rendered at t0: the watermark is read before the grid.
	maxID, err := s.MaxEntryID(ctx)
	mustNoErr(t, err)
	// Then a poll whose response arrived before the page was rendered is
	// stored (fetched_at predates the page)...
	_, err = s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: t0.Add(-time.Second),
		Entries: []NewEntry{entryAt("slow-poll", t0)}})
	mustNoErr(t, err)
	// ...and a feed previewed ten minutes earlier is subscribed in another tab.
	nf := newFeed("https://b.example", KindRSS)
	nf.FetchedAt = t0.Add(-10 * time.Minute)
	g := mustCreateFeed(t, s, nf, entriesNewestFirst("new", 2, t0), 5)

	clickAt := t0.Add(time.Hour)
	n, err := s.MarkScopeReadUpTo(ctx, Scope{Kind: ScopeAll}, maxID, clickAt)
	mustNoErr(t, err)
	if n != 3 {
		t.Fatalf("changed = %d, want 3 (only the entries the page could show)", n)
	}
	for _, v := range []EntryView{entryByGUID(t, s, f.ID, "slow-poll"), entryByGUID(t, s, g.ID, "new0"), entryByGUID(t, s, g.ID, "new1")} {
		if v.IsRead {
			t.Errorf("%s, stored after the page was rendered, was swept", v.GUID)
		}
	}
	if v := entryByGUID(t, s, f.ID, "old1"); !v.IsRead || !v.ReadAt.Equal(clickAt) {
		t.Fatalf("old1: IsRead=%v ReadAt=%v; want read at %v", v.IsRead, v.ReadAt, clickAt)
	}
	// Running it again changes nothing (only unread entries are touched).
	if n, err := s.MarkScopeReadUpTo(ctx, Scope{Kind: ScopeAll}, maxID, clickAt); err != nil || n != 0 {
		t.Fatalf("second run = %d, %v; want 0", n, err)
	}
	// A non-positive bound matches nothing; an unknown scope is invalid.
	for _, bound := range []int64{0, -1} {
		if n, err := s.MarkScopeReadUpTo(ctx, Scope{Kind: ScopeAll}, bound, clickAt); err != nil || n != 0 {
			t.Fatalf("maxID %d = %d, %v; want 0, nil", bound, n, err)
		}
	}
	if _, err := s.MarkScopeReadUpTo(ctx, Scope{Kind: "nope"}, maxID, clickAt); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown scope err = %v, want ErrInvalid", err)
	}
}

func TestMaxEntryID_IDsOfDeletedEntriesAreNeverReused(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	if id, err := s.MaxEntryID(ctx); err != nil || id != 0 {
		t.Fatalf("MaxEntryID(empty) = %d, %v; want 0", id, err)
	}
	a := mustCreateFeed(t, s, newFeed("https://a.example", KindRSS), entriesNewestFirst("a", 2, t0), 5)
	b := mustCreateFeed(t, s, newFeed("https://b.example", KindRSS), entriesNewestFirst("b", 2, t0), 5)
	maxID, err := s.MaxEntryID(ctx)
	mustNoErr(t, err)
	if want := max(entryID(t, s, b.ID, "b0"), entryID(t, s, b.ID, "b1")); maxID != want {
		t.Fatalf("MaxEntryID = %d, want %d", maxID, want)
	}
	// Unsubscribing removes the newest ids; the next poll must not reuse them.
	mustNoErr(t, s.DeleteFeed(ctx, b.ID))
	_, err = s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: a.ID, FetchedAt: t0, Entries: []NewEntry{entryAt("later", t0)}})
	mustNoErr(t, err)
	if id := entryID(t, s, a.ID, "later"); id <= maxID {
		t.Fatalf("new entry got id %d, not above the earlier watermark %d", id, maxID)
	}
	n, err := s.MarkScopeReadUpTo(ctx, Scope{Kind: ScopeAll}, maxID, t0)
	mustNoErr(t, err)
	if v := entryByGUID(t, s, a.ID, "later"); n != 2 || v.IsRead {
		t.Fatalf("changed = %d, later read = %v; want 2 and unread", n, v.IsRead)
	}
}

func TestUnreadCounts_PerFeedPerFolderAndAll(t *testing.T) {
	fx := newGridFixture(t)
	ctx := t.Context()
	c, err := fx.s.UnreadCounts(ctx)
	mustNoErr(t, err)
	want := UnreadCounts{
		All:      8,
		ByFeed:   map[int64]int{fx.yt.ID: 3, fx.a.ID: 3, fx.b.ID: 2},
		ByFolder: map[int64]int{fx.videos.ID: 3, fx.blogs.ID: 3},
	}
	if !equalCounts(c, want) {
		t.Fatalf("UnreadCounts = %+v, want %+v", c, want)
	}

	// Counts follow every action.
	mustNoErr(t, fx.s.SetRead(ctx, fx.ids["b0"], true, t0))
	mustNoErr(t, fx.s.SetRead(ctx, fx.ids["b1"], true, t0))
	mustNoErr(t, fx.s.SetRead(ctx, fx.ids["y1"], false, t0))
	c, err = fx.s.UnreadCounts(ctx)
	mustNoErr(t, err)
	want = UnreadCounts{
		All:      7,
		ByFeed:   map[int64]int{fx.yt.ID: 4, fx.a.ID: 3}, // b absent = 0
		ByFolder: map[int64]int{fx.videos.ID: 4, fx.blogs.ID: 3},
	}
	if !equalCounts(c, want) {
		t.Fatalf("UnreadCounts after actions = %+v, want %+v", c, want)
	}
}

func TestUnreadCounts_EmptyLibrary(t *testing.T) {
	s := openTest(t)
	c, err := s.UnreadCounts(t.Context())
	mustNoErr(t, err)
	if c.All != 0 || c.ByFeed == nil || c.ByFolder == nil || len(c.ByFeed) != 0 || len(c.ByFolder) != 0 {
		t.Fatalf("UnreadCounts = %+v, want zero with empty non-nil maps", c)
	}
}

func equalCounts(a, b UnreadCounts) bool {
	eq := func(x, y map[int64]int) bool {
		if len(x) != len(y) {
			return false
		}
		for k, v := range x {
			if y[k] != v {
				return false
			}
		}
		return true
	}
	return a.All == b.All && eq(a.ByFeed, b.ByFeed) && eq(a.ByFolder, b.ByFolder)
}
