package store

import (
	"errors"
	"testing"
)

func TestCreateFolder_AppendsAfterExistingFolders(t *testing.T) {
	s := openTest(t)
	b := mustFolder(t, s, "Tech newsletters")
	a := mustFolder(t, s, "  YouTube  ")
	if a.Name != "YouTube" {
		t.Errorf("name = %q, want trimmed %q", a.Name, "YouTube")
	}
	if b.Position != 0 || a.Position != 1 {
		t.Errorf("positions = %d, %d; want 0, 1", b.Position, a.Position)
	}
	got, err := s.ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != b || got[1] != a {
		t.Fatalf("ListFolders = %+v, want [%+v %+v] (by position, then name)", got, b, a)
	}
}

func TestListFolders_SamePositionOrderedByName(t *testing.T) {
	s := openTest(t)
	for _, n := range []string{"beta", "Alpha", "gamma"} {
		f := mustFolder(t, s, n)
		if _, err := s.db.Exec(`UPDATE folders SET position = 0 WHERE id = ?`, f.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListFolders(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range got {
		names = append(names, f.Name)
	}
	if want := []string{"Alpha", "beta", "gamma"}; !equalStrings(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestFolderNames_UniqueCaseInsensitive(t *testing.T) {
	s := openTest(t)
	mustFolder(t, s, "Tech channels")
	for _, dup := range []string{"Tech channels", "tech CHANNELS", "  tech channels "} {
		if _, err := s.CreateFolder(t.Context(), dup); !errors.Is(err, ErrConflict) {
			t.Errorf("CreateFolder(%q) err = %v, want ErrConflict", dup, err)
		}
	}
}

func TestCreateFolder_EmptyNameInvalid(t *testing.T) {
	s := openTest(t)
	for _, name := range []string{"", "   "} {
		if _, err := s.CreateFolder(t.Context(), name); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateFolder(%q) err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestGetFolder(t *testing.T) {
	s := openTest(t)
	f := mustFolder(t, s, "YouTube")
	got, err := s.GetFolder(t.Context(), f.ID)
	if err != nil || got != f {
		t.Fatalf("GetFolder = %+v, %v; want %+v", got, err, f)
	}
	if _, err := s.GetFolder(t.Context(), f.ID+100); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetFolder(missing) err = %v, want ErrNotFound", err)
	}
}

func TestRenameFolder(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	a := mustFolder(t, s, "a")
	mustFolder(t, s, "b")

	if err := s.RenameFolder(ctx, a.ID, " Blogs "); err != nil {
		t.Fatalf("RenameFolder: %v", err)
	}
	if got, _ := s.GetFolder(ctx, a.ID); got.Name != "Blogs" {
		t.Errorf("name = %q, want Blogs", got.Name)
	}
	if err := s.RenameFolder(ctx, a.ID, "BLOGS"); err != nil {
		t.Errorf("case-only rename of itself: %v", err)
	}
	if err := s.RenameFolder(ctx, a.ID, "B"); !errors.Is(err, ErrConflict) {
		t.Errorf("rename onto existing name err = %v, want ErrConflict", err)
	}
	if err := s.RenameFolder(ctx, a.ID+100, "x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rename missing err = %v, want ErrNotFound", err)
	}
	if err := s.RenameFolder(ctx, a.ID, " "); !errors.Is(err, ErrInvalid) {
		t.Errorf("rename to blank err = %v, want ErrInvalid", err)
	}
}

func TestDeleteFolder_FeedsBecomeUncategorized(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	folder := mustFolder(t, s, "YouTube")
	nf := newFeed("https://www.youtube.com/feeds/videos.xml?channel_id=UCx", KindYouTube)
	nf.FolderID = folder.ID
	f := mustCreateFeed(t, s, nf, entriesNewestFirst("v", 3, t0), 5)

	if err := s.DeleteFolder(ctx, folder.ID); err != nil {
		t.Fatalf("DeleteFolder: %v", err)
	}
	got, err := s.GetFeed(ctx, f.ID)
	if err != nil {
		t.Fatalf("feed was deleted with its folder: %v", err)
	}
	if got.FolderID != 0 {
		t.Errorf("FolderID = %d, want 0 (uncategorized)", got.FolderID)
	}
	if n := countRows(t, s, "entries", "feed_id = ?", f.ID); n != 3 {
		t.Errorf("entries = %d, want 3 (kept)", n)
	}
	c, err := s.UnreadCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := c.ByFolder[folder.ID]; ok || c.ByFeed[f.ID] != 3 {
		t.Errorf("counts after delete = %+v", c)
	}
	if err := s.DeleteFolder(ctx, folder.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteFolder err = %v, want ErrNotFound", err)
	}
}

func TestDeleteFolderIfEmpty_KeepsFolderInUse(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	used := mustFolder(t, s, "Used")
	empty := mustFolder(t, s, "Empty")
	nf := newFeed("https://example.org/feed.xml", KindRSS)
	nf.FolderID = used.ID
	f := mustCreateFeed(t, s, nf, entriesNewestFirst("e", 1, t0), 5)

	if deleted, err := s.DeleteFolderIfEmpty(ctx, used.ID); err != nil || deleted {
		t.Fatalf("DeleteFolderIfEmpty(in use) = %v, %v; want false, nil", deleted, err)
	}
	if got, err := s.GetFeed(ctx, f.ID); err != nil || got.FolderID != used.ID {
		t.Errorf("feed folder = %d, %v; want %d", got.FolderID, err, used.ID)
	}
	if deleted, err := s.DeleteFolderIfEmpty(ctx, empty.ID); err != nil || !deleted {
		t.Fatalf("DeleteFolderIfEmpty(empty) = %v, %v; want true, nil", deleted, err)
	}
	if _, err := s.GetFolder(ctx, empty.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("empty folder still there: %v", err)
	}
	if deleted, err := s.DeleteFolderIfEmpty(ctx, empty.ID); err != nil || deleted {
		t.Errorf("DeleteFolderIfEmpty(missing) = %v, %v; want false, nil", deleted, err)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestFolderNames_UniqueCaseInsensitiveBeyondASCII(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	eco := mustFolder(t, s, "Économie")
	citta := mustFolder(t, s, "Città")
	for _, dup := range []string{"économie", "ÉCONOMIE", "éCONOMIE", "CITTÀ", "cittÀ"} {
		if _, err := s.CreateFolder(ctx, dup); !errors.Is(err, ErrConflict) {
			t.Errorf("CreateFolder(%q) err = %v, want ErrConflict", dup, err)
		}
	}
	if err := s.RenameFolder(ctx, citta.ID, "économie"); !errors.Is(err, ErrConflict) {
		t.Errorf("rename onto another folder's name err = %v, want ErrConflict", err)
	}
	if err := s.RenameFolder(ctx, eco.ID, "ÉCONOMIE"); err != nil {
		t.Errorf("case-only rename of itself: %v", err)
	}
	if got, _ := s.GetFolder(ctx, eco.ID); got.Name != "ÉCONOMIE" {
		t.Errorf("name = %q, want ÉCONOMIE", got.Name)
	}
	if n := countRows(t, s, "folders", ""); n != 2 {
		t.Errorf("folders = %d, want 2", n)
	}
}
