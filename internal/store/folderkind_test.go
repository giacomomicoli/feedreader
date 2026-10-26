package store

import (
	"errors"
	"strings"
	"testing"
)

// mixedFolderFixture is the grid fixture with feed b (a blog) moved into the
// Videos folder, next to the YouTube feed: a folder of both kinds.
func mixedFolderFixture(t *testing.T) *gridFixture {
	t.Helper()
	fx := newGridFixture(t)
	mustNoErr(t, fx.s.MoveFeed(t.Context(), fx.b.ID, fx.videos.ID))
	return fx
}

// listedGUIDs lists q's entries by GUID, sorted.
func listedGUIDs(t *testing.T, s *SQLite, q ListQuery) string {
	t.Helper()
	return strings.Join(sortedGUIDs(listAll(t, s, q)), " ")
}

// TestFolderScope_NarrowedToOneKind: a folder narrowed to a feed kind lists,
// counts and marks read only its feeds of that kind; unnarrowed it keeps
// covering all of them.
func TestFolderScope_NarrowedToOneKind(t *testing.T) {
	fx := mixedFolderFixture(t)
	s, ctx := fx.s, t.Context()
	folder := func(k Kind) Scope { return Scope{Kind: ScopeFolder, ID: fx.videos.ID, FeedKind: k} }

	for _, tc := range []struct {
		kind   Kind
		unread bool
		want   string
	}{
		{"", false, "b0 b1 b2 y0 y1 y2 y3"},
		{KindYouTube, false, "y0 y1 y2 y3"},
		{KindYouTube, true, "y0 y2 y3"},
		{KindRSS, false, "b0 b1 b2"},
		{KindRSS, true, "b0 b1"},
	} {
		if got := listedGUIDs(t, s, ListQuery{Scope: folder(tc.kind), UnreadOnly: tc.unread, Limit: 50}); got != tc.want {
			t.Errorf("folder narrowed to %q (unread %v) = %q, want %q", tc.kind, tc.unread, got, tc.want)
		}
	}

	c, err := s.UnreadCounts(ctx)
	mustNoErr(t, err)
	wantKinds := map[FolderKind]int{
		{FolderID: fx.videos.ID, Kind: KindYouTube}: 3,
		{FolderID: fx.videos.ID, Kind: KindRSS}:     2,
		{FolderID: fx.blogs.ID, Kind: KindRSS}:      3,
	}
	if len(c.ByFolderKind) != len(wantKinds) {
		t.Errorf("ByFolderKind = %v, want %v", c.ByFolderKind, wantKinds)
	}
	for k, n := range wantKinds {
		if c.ByFolderKind[k] != n {
			t.Errorf("ByFolderKind[%+v] = %d, want %d", k, c.ByFolderKind[k], n)
		}
	}
	if c.ByFolder[fx.videos.ID] != 5 {
		t.Errorf("ByFolder[videos] = %d, want 5 (both kinds)", c.ByFolder[fx.videos.ID])
	}

	// Mark all as read of the folder's blogs leaves its videos unread.
	maxID, err := s.MaxEntryID(ctx)
	mustNoErr(t, err)
	n, err := s.MarkScopeReadUpTo(ctx, folder(KindRSS), maxID, t0)
	mustNoErr(t, err)
	if n != 2 {
		t.Errorf("MarkScopeReadUpTo(folder blogs) changed %d, want 2", n)
	}
	if got := listedGUIDs(t, s, ListQuery{Scope: folder(""), UnreadOnly: true, Limit: 50}); got != "y0 y2 y3" {
		t.Errorf("folder unread after marking its blogs read = %q, want its videos", got)
	}
}

// TestFolderScope_FeedKindRefused: an unknown kind, or a kind on a scope
// other than a folder, is ErrInvalid.
func TestFolderScope_FeedKindRefused(t *testing.T) {
	fx := newGridFixture(t)
	for _, sc := range []Scope{
		{Kind: ScopeFolder, ID: fx.videos.ID, FeedKind: "podcast"},
		{Kind: ScopeAll, FeedKind: KindYouTube},
		{Kind: ScopeFeed, ID: fx.yt.ID, FeedKind: KindYouTube},
	} {
		if _, err := fx.s.ListEntries(t.Context(), ListQuery{Scope: sc, Limit: 10}); !errors.Is(err, ErrInvalid) {
			t.Errorf("ListEntries(%+v) = %v, want ErrInvalid", sc, err)
		}
		if _, err := fx.s.MarkScopeReadUpTo(t.Context(), sc, 1000, t0); !errors.Is(err, ErrInvalid) {
			t.Errorf("MarkScopeReadUpTo(%+v) = %v, want ErrInvalid", sc, err)
		}
	}
}
