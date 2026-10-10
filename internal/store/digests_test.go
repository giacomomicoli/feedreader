package store

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/migrations"
)

func mustDigest(t *testing.T, s *SQLite, name string) Digest {
	t.Helper()
	d, err := s.CreateDigest(t.Context(), name)
	if err != nil {
		t.Fatalf("CreateDigest(%q): %v", name, err)
	}
	return d
}

// mustSources saves src as the digest's sources, as the settings form does
// right after it was rendered.
func mustSources(t *testing.T, s *SQLite, id int64, src DigestSources) {
	t.Helper()
	if err := s.SetDigestSources(t.Context(), id, src, offeredSources(t, s)); err != nil {
		t.Fatalf("SetDigestSources(%d, %+v): %v", id, src, err)
	}
}

// offeredSources is the fingerprint of the sources a digest settings form
// rendered now offers.
func offeredSources(t *testing.T, s *SQLite) string {
	t.Helper()
	ctx := t.Context()
	folders, err := s.ListFolders(ctx)
	mustNoErr(t, err)
	feeds, err := s.ListFeeds(ctx)
	mustNoErr(t, err)
	tags, err := s.ListTags(ctx)
	mustNoErr(t, err)
	return SourcesFingerprint(folders, feeds, tags)
}

func idList(xs ...int64) []int64 { return xs }

// minutesPerHour converts clock times to the minutes of the day that
// digests' ingestion times are.
const minutesPerHour = int(time.Hour / time.Minute)

// Ingestion times used by the tests, in minutes of the day.
const (
	sevenAM = 7 * minutesPerHour
	sevenPM = 19 * minutesPerHour
)

func TestCreateDigest_AppendsTrimmedWithoutIngestTime(t *testing.T) {
	s := openTest(t)
	b := mustDigest(t, s, "Morning")
	a := mustDigest(t, s, "  Evening  ")
	if a.Name != "Evening" {
		t.Errorf("name = %q, want trimmed %q", a.Name, "Evening")
	}
	if b.Position != 0 || a.Position != 1 {
		t.Errorf("positions = %d, %d; want 0, 1", b.Position, a.Position)
	}
	if a.HasIngest || !a.CreatedAt.Equal(t0) {
		t.Errorf("new digest = %+v, want no ingest time, created at %s", a, t0)
	}
	got, err := s.ListDigests(t.Context())
	mustNoErr(t, err)
	if !slices.Equal(got, []Digest{b, a}) {
		t.Fatalf("ListDigests = %+v, want [%+v %+v] (by position, then name)", got, b, a)
	}
	if d, err := s.GetDigest(t.Context(), a.ID); err != nil || d != a {
		t.Fatalf("GetDigest = %+v, %v; want %+v", d, err, a)
	}
}

// TestDigestIDs_NeverReused: a page or form left open on a deleted digest
// must not act on a digest created later.
func TestDigestIDs_NeverReused(t *testing.T) {
	s := openTest(t)
	old := mustDigest(t, s, "Old")
	mustNoErr(t, s.DeleteDigest(t.Context(), old.ID))
	if d := mustDigest(t, s, "New"); d.ID <= old.ID {
		t.Fatalf("new digest id = %d, want more than the deleted %d", d.ID, old.ID)
	}
}

func TestListDigests_SamePositionOrderedByName(t *testing.T) {
	s := openTest(t)
	for _, n := range []string{"beta", "Alpha", "gamma"} {
		d := mustDigest(t, s, n)
		if _, err := s.db.Exec(`UPDATE digests SET position = 0 WHERE id = ?`, d.ID); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.ListDigests(t.Context())
	mustNoErr(t, err)
	var names []string
	for _, d := range got {
		names = append(names, d.Name)
	}
	if want := []string{"Alpha", "beta", "gamma"}; !slices.Equal(names, want) {
		t.Fatalf("names = %v, want %v", names, want)
	}
}

func TestDigestNames_UniqueCaseInsensitiveBeyondASCII(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	eco := mustDigest(t, s, "Économie")
	citta := mustDigest(t, s, "Città")
	for _, dup := range []string{"économie", "ÉCONOMIE", "  CITTÀ ", "cittÀ"} {
		if _, err := s.CreateDigest(ctx, dup); !errors.Is(err, ErrConflict) {
			t.Errorf("CreateDigest(%q) err = %v, want ErrConflict", dup, err)
		}
	}
	for _, name := range []string{"", "   "} {
		if _, err := s.CreateDigest(ctx, name); !errors.Is(err, ErrInvalid) {
			t.Errorf("CreateDigest(%q) err = %v, want ErrInvalid", name, err)
		}
		if err := s.RenameDigest(ctx, eco.ID, name); !errors.Is(err, ErrInvalid) {
			t.Errorf("RenameDigest(%q) err = %v, want ErrInvalid", name, err)
		}
	}
	if err := s.RenameDigest(ctx, citta.ID, "économie"); !errors.Is(err, ErrConflict) {
		t.Errorf("rename onto another digest's name err = %v, want ErrConflict", err)
	}
	if err := s.RenameDigest(ctx, eco.ID, "ÉCONOMIE"); err != nil {
		t.Errorf("case-only rename of itself: %v", err)
	}
	if got, _ := s.GetDigest(ctx, eco.ID); got.Name != "ÉCONOMIE" {
		t.Errorf("name = %q, want ÉCONOMIE", got.Name)
	}
	// Digest names are their own namespace: a folder or tag may share one.
	mustFolder(t, s, "Città")
	if n := countRows(t, s, "digests", ""); n != 2 {
		t.Errorf("digests = %d, want 2", n)
	}
}

func TestDigest_UnknownIDIsNotFound(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	const missing = 404
	checks := map[string]error{
		"GetDigest":        func() error { _, err := s.GetDigest(ctx, missing); return err }(),
		"RenameDigest":     s.RenameDigest(ctx, missing, "x"),
		"SetDigestIngest":  s.SetDigestIngest(ctx, missing, 0, true),
		"DeleteDigest":     s.DeleteDigest(ctx, missing),
		"DigestSources":    func() error { _, err := s.DigestSources(ctx, missing); return err }(),
		"SetDigestSources": s.SetDigestSources(ctx, missing, DigestSources{}, offeredSources(t, s)),
		"DigestFeedIDs":    func() error { _, err := s.DigestFeedIDs(ctx, missing); return err }(),
	}
	for name, err := range checks {
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("%s(missing) err = %v, want ErrNotFound", name, err)
		}
	}
}

func TestSetDigestIngest_MinuteOfTheDayOrNone(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	d := mustDigest(t, s, "Morning")
	for _, minute := range []int{0, sevenAM, MinutesPerDay - 1} {
		mustNoErr(t, s.SetDigestIngest(ctx, d.ID, minute, true))
		if got, _ := s.GetDigest(ctx, d.ID); !got.HasIngest || got.IngestMinute != minute {
			t.Errorf("after setting %d: %+v", minute, got)
		}
	}
	for _, bad := range []int{-1, MinutesPerDay} {
		if err := s.SetDigestIngest(ctx, d.ID, bad, true); !errors.Is(err, ErrInvalid) {
			t.Errorf("SetDigestIngest(%d) err = %v, want ErrInvalid", bad, err)
		}
	}
	mustNoErr(t, s.SetDigestIngest(ctx, d.ID, MinutesPerDay, false)) // the minute is ignored when clearing
	if got, _ := s.GetDigest(ctx, d.ID); got.HasIngest || got.IngestMinute != 0 {
		t.Errorf("after clearing: %+v, want no ingest time", got)
	}
	// The schema refuses out-of-range minutes too.
	if _, err := s.db.Exec(`UPDATE digests SET ingest_minute = ? WHERE id = ?`, MinutesPerDay, d.ID); err == nil {
		t.Error("the schema accepted an ingest minute past the end of the day")
	}
}

func TestSetDigestSources_ReplacesDedupesAndSorts(t *testing.T) {
	fx := newGridFixture(t)
	s, ctx := fx.s, t.Context()
	d := mustDigest(t, s, "Morning")

	got, err := s.DigestSources(ctx, d.ID)
	mustNoErr(t, err)
	if len(got.FeedIDs)+len(got.FolderIDs)+len(got.TagIDs) != 0 {
		t.Fatalf("new digest sources = %+v, want none", got)
	}

	mustSources(t, s, d.ID, DigestSources{
		FeedIDs:   idList(fx.b.ID, fx.a.ID, fx.b.ID),
		FolderIDs: idList(fx.blogs.ID, fx.videos.ID),
		TagIDs:    idList(fx.music.ID, fx.goTag.ID, fx.music.ID),
	})
	got, err = s.DigestSources(ctx, d.ID)
	mustNoErr(t, err)
	want := DigestSources{
		FeedIDs:   slices.Sorted(slices.Values(idList(fx.a.ID, fx.b.ID))),
		FolderIDs: slices.Sorted(slices.Values(idList(fx.videos.ID, fx.blogs.ID))),
		TagIDs:    slices.Sorted(slices.Values(idList(fx.goTag.ID, fx.music.ID))),
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("sources = %+v, want %+v", got, want)
	}

	// Saving replaces every list, empty ones included.
	mustSources(t, s, d.ID, DigestSources{FolderIDs: idList(fx.videos.ID)})
	got, err = s.DigestSources(ctx, d.ID)
	mustNoErr(t, err)
	if fmt.Sprint(got) != fmt.Sprint(DigestSources{FolderIDs: idList(fx.videos.ID)}) {
		t.Fatalf("sources after replacing = %+v", got)
	}
}

func TestSetDigestSources_RejectsUnknownAndInvalidIDsAtomically(t *testing.T) {
	fx := newGridFixture(t)
	s, ctx := fx.s, t.Context()
	d := mustDigest(t, s, "Morning")
	kept := DigestSources{FeedIDs: idList(fx.a.ID)}
	mustSources(t, s, d.ID, kept)

	const missing = 999
	for name, src := range map[string]DigestSources{
		"feed":   {FeedIDs: idList(fx.b.ID, missing)},
		"folder": {FeedIDs: idList(fx.b.ID), FolderIDs: idList(missing)},
		"tag":    {FeedIDs: idList(fx.b.ID), TagIDs: idList(missing)},
	} {
		if err := s.SetDigestSources(ctx, d.ID, src, offeredSources(t, s)); !errors.Is(err, ErrNotFound) {
			t.Errorf("unknown %s: err = %v, want ErrNotFound", name, err)
		}
	}
	for name, src := range map[string]DigestSources{
		"zero feed":       {FeedIDs: idList(0)},
		"negative folder": {FolderIDs: idList(-1)},
		"negative tag":    {TagIDs: idList(-fx.goTag.ID)},
	} {
		if err := s.SetDigestSources(ctx, d.ID, src, offeredSources(t, s)); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	got, err := s.DigestSources(ctx, d.ID)
	mustNoErr(t, err)
	if fmt.Sprint(got) != fmt.Sprint(kept) {
		t.Fatalf("a rejected save changed the sources to %+v, want %+v", got, kept)
	}
}

// TestSetDigestSources_RefusesAStaleForm: feed, folder and tag ids are
// reused once the highest one is deleted, so a settings form left open
// across a delete and a create would attach another source than the one it
// showed. The form posts back the fingerprint of the sources it offered;
// once any of them changed, the save is refused and nothing changes.
func TestSetDigestSources_RefusesAStaleForm(t *testing.T) {
	for _, tc := range []struct {
		kind string
		// reuse deletes the source of that kind with the highest id and
		// creates another one, which gets its id; it returns both ids and
		// what the stale form saves when the old source was picked.
		reuse func(t *testing.T, fx *gridFixture) (old, reused int64, src DigestSources)
	}{
		{"feed", func(t *testing.T, fx *gridFixture) (int64, int64, DigestSources) {
			mustNoErr(t, fx.s.DeleteFeed(t.Context(), fx.b.ID))
			c := mustCreateFeed(t, fx.s, newFeed("https://c.example/feed", KindRSS), nil, 0)
			return fx.b.ID, c.ID, DigestSources{FeedIDs: idList(c.ID)}
		}},
		{"folder", func(t *testing.T, fx *gridFixture) (int64, int64, DigestSources) {
			mustNoErr(t, fx.s.DeleteFolder(t.Context(), fx.blogs.ID))
			podcasts := mustFolder(t, fx.s, "Podcasts")
			return fx.blogs.ID, podcasts.ID, DigestSources{FolderIDs: idList(podcasts.ID)}
		}},
		{"tag", func(t *testing.T, fx *gridFixture) (int64, int64, DigestSources) {
			mustNoErr(t, fx.s.DeleteTag(t.Context(), fx.music.ID))
			jazz, err := fx.s.AddEntryTag(t.Context(), fx.ids["y3"], "jazz")
			mustNoErr(t, err)
			return fx.music.ID, jazz.ID, DigestSources{TagIDs: idList(jazz.ID)}
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			fx := newGridFixture(t)
			s, ctx := fx.s, t.Context()
			d := mustDigest(t, s, "Morning")
			kept := DigestSources{FeedIDs: idList(fx.a.ID)}
			mustSources(t, s, d.ID, kept)
			offered := offeredSources(t, s) // the settings form is rendered

			old, reused, src := tc.reuse(t, fx)
			if reused != old {
				t.Fatalf("the new %s got id %d, not the deleted one's %d", tc.kind, reused, old)
			}
			if err := s.SetDigestSources(ctx, d.ID, src, offered); !errors.Is(err, ErrConflict) {
				t.Fatalf("stale form: err = %v, want ErrConflict", err)
			}
			got, err := s.DigestSources(ctx, d.ID)
			mustNoErr(t, err)
			if fmt.Sprint(got) != fmt.Sprint(kept) {
				t.Fatalf("a stale form changed the sources to %+v, want %+v", got, kept)
			}
			// Rendered again, the form shows the new source and saves it.
			mustNoErr(t, s.SetDigestSources(ctx, d.ID, src, offeredSources(t, s)))
			if got, _ := s.DigestSources(ctx, d.ID); fmt.Sprint(got) != fmt.Sprint(src) {
				t.Errorf("sources = %+v, want %+v", got, src)
			}
		})
	}
}

// TestSourcesFingerprint_ChangesWithTheSourcesOffered: the fingerprint
// depends on the ids and identities of the folders, feeds and tags offered,
// not on their order, titles or counts.
func TestSourcesFingerprint_ChangesWithTheSourcesOffered(t *testing.T) {
	fx := newGridFixture(t)
	s, ctx := fx.s, t.Context()
	folders, err := s.ListFolders(ctx)
	mustNoErr(t, err)
	feeds, err := s.ListFeeds(ctx)
	mustNoErr(t, err)
	tags, err := s.ListTags(ctx)
	mustNoErr(t, err)
	base := SourcesFingerprint(folders, feeds, tags)
	if base == "" || base == SourcesFingerprint(nil, nil, nil) {
		t.Fatalf("fingerprint %q of a library with sources", base)
	}

	renamedFeed := slices.Clone(feeds)
	renamedFeed[0].Title = "Another title"
	recounted := slices.Clone(tags)
	recounted[0].Count++
	for name, fp := range map[string]string{
		"reversed lists":      SourcesFingerprint(reversed(folders), reversed(feeds), reversed(tags)),
		"feed title":          SourcesFingerprint(folders, renamedFeed, tags),
		"tag count":           SourcesFingerprint(folders, feeds, recounted),
		"folder renamed case": SourcesFingerprint(withName(folders, 0, strings.ToUpper(folders[0].Name)), feeds, tags),
	} {
		if fp != base {
			t.Errorf("%s: fingerprint changed", name)
		}
	}

	movedFeed := slices.Clone(feeds)
	movedFeed[0].URL += "/moved"
	renamedTag := slices.Clone(tags)
	renamedTag[0].Name = "other"
	swapped := slices.Clone(feeds)
	swapped[0].ID, swapped[1].ID = swapped[1].ID, swapped[0].ID
	for name, fp := range map[string]string{
		"folder removed":  SourcesFingerprint(folders[1:], feeds, tags),
		"feed removed":    SourcesFingerprint(folders, feeds[1:], tags),
		"tag removed":     SourcesFingerprint(folders, feeds, tags[1:]),
		"folder renamed":  SourcesFingerprint(withName(folders, 0, "Elsewhere"), feeds, tags),
		"feed URL":        SourcesFingerprint(folders, movedFeed, tags),
		"tag renamed":     SourcesFingerprint(folders, feeds, renamedTag),
		"feed ids":        SourcesFingerprint(folders, swapped, tags),
		"folder as a tag": SourcesFingerprint(folders[1:], feeds, append(slices.Clone(tags), Tag{ID: folders[0].ID, Name: folders[0].Name})),
	} {
		if fp == base {
			t.Errorf("%s: fingerprint unchanged", name)
		}
	}
	// A form without a fingerprint saves nothing.
	d := mustDigest(t, s, "Morning")
	if err := s.SetDigestSources(ctx, d.ID, DigestSources{FeedIDs: idList(fx.a.ID)}, ""); !errors.Is(err, ErrConflict) {
		t.Errorf("no fingerprint: err = %v, want ErrConflict", err)
	}
}

// reversed returns a reversed copy of xs.
func reversed[T any](xs []T) []T {
	out := slices.Clone(xs)
	slices.Reverse(out)
	return out
}

// withName returns a copy of folders with the i-th renamed to name.
func withName(folders []Folder, i int, name string) []Folder {
	out := slices.Clone(folders)
	out[i].Name = name
	return out
}

func TestSetDigestSources_BoundedInSize(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	d := mustDigest(t, s, "Everything")
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), nil, 0)
	// Duplicates do not count: they collapse before the bound is checked.
	many := make([]int64, MaxDigestSources+1)
	for i := range many {
		many[i] = f.ID
	}
	mustSources(t, s, d.ID, DigestSources{FeedIDs: many})

	over := DigestSources{}
	for i := range MaxDigestSources + 1 {
		id := int64(i + 1)
		switch i % 3 {
		case 0:
			over.FeedIDs = append(over.FeedIDs, id)
		case 1:
			over.FolderIDs = append(over.FolderIDs, id)
		default:
			over.TagIDs = append(over.TagIDs, id)
		}
	}
	if err := s.SetDigestSources(ctx, d.ID, over, offeredSources(t, s)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("%d sources: err = %v, want ErrInvalid", MaxDigestSources+1, err)
	}
}

// TestDigestMemberships_OnlyMembershipsCascade: deleting a feed, a folder or
// a tag removes it from every digest; deleting a digest removes its
// memberships and nothing else. Entries are never deleted by digest changes.
func TestDigestMemberships_OnlyMembershipsCascade(t *testing.T) {
	fx := newGridFixture(t)
	s, ctx := fx.s, t.Context()
	d := mustDigest(t, s, "Morning")
	other := mustDigest(t, s, "Evening")
	all := DigestSources{FeedIDs: idList(fx.b.ID, fx.a.ID), FolderIDs: idList(fx.videos.ID, fx.blogs.ID), TagIDs: idList(fx.goTag.ID, fx.music.ID)}
	mustSources(t, s, d.ID, all)
	mustSources(t, s, other.ID, all)
	entries := countRows(t, s, "entries", "")

	mustNoErr(t, s.DeleteFeed(ctx, fx.b.ID))
	mustNoErr(t, s.DeleteFolder(ctx, fx.blogs.ID))
	mustNoErr(t, s.DeleteTag(ctx, fx.music.ID))
	want := DigestSources{FeedIDs: idList(fx.a.ID), FolderIDs: idList(fx.videos.ID), TagIDs: idList(fx.goTag.ID)}
	for _, id := range []int64{d.ID, other.ID} {
		got, err := s.DigestSources(ctx, id)
		mustNoErr(t, err)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("digest %d sources = %+v, want %+v", id, got, want)
		}
	}
	entries -= 3 // feed b's entries went with it, as unsubscribing does
	if n := countRows(t, s, "entries", ""); n != entries {
		t.Fatalf("entries = %d, want %d", n, entries)
	}

	mustNoErr(t, s.DeleteDigest(ctx, d.ID))
	if _, err := s.GetDigest(ctx, d.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetDigest after delete err = %v, want ErrNotFound", err)
	}
	for _, table := range []string{"digest_feeds", "digest_folders", "digest_tags"} {
		if n := countRows(t, s, table, "digest_id = ?", d.ID); n != 0 {
			t.Errorf("%s rows of the deleted digest = %d, want 0", table, n)
		}
		if n := countRows(t, s, table, "digest_id = ?", other.ID); n != 1 {
			t.Errorf("%s rows of the other digest = %d, want 1", table, n)
		}
	}
	if n := countRows(t, s, "entries", ""); n != entries {
		t.Errorf("deleting a digest deleted entries: %d, want %d", n, entries)
	}
	for _, check := range []func() error{
		func() error { _, err := s.GetFeed(ctx, fx.a.ID); return err },
		func() error { _, err := s.GetFolder(ctx, fx.videos.ID); return err },
		func() error { _, err := s.GetTag(ctx, fx.goTag.ID); return err },
	} {
		mustNoErr(t, check())
	}
	if n := countRows(t, s, "entry_tags", ""); n == 0 {
		t.Error("deleting a digest removed entry tags")
	}
}

// TestDigestsNamingFolderAndTag lists the digests that deleting a folder or
// a tag removes it from, as ListDigests orders them.
func TestDigestsNamingFolderAndTag(t *testing.T) {
	fx := newGridFixture(t)
	s, ctx := fx.s, t.Context()
	breakfast := mustDigest(t, s, "Breakfast")
	lunch := mustDigest(t, s, "Lunch")
	other := mustDigest(t, s, "Other")
	mustSources(t, s, breakfast.ID, DigestSources{FolderIDs: idList(fx.blogs.ID), TagIDs: idList(fx.goTag.ID)})
	mustSources(t, s, lunch.ID, DigestSources{FolderIDs: idList(fx.videos.ID, fx.blogs.ID)})
	// Naming a folder's feed, or a tagged entry's feed, is not naming them.
	mustSources(t, s, other.ID, DigestSources{FeedIDs: idList(fx.a.ID, fx.yt.ID), TagIDs: idList(fx.music.ID)})
	mustNoErr(t, s.RenameDigest(ctx, lunch.ID, "Afternoon")) // the order is by position, not name

	names := func(ds []Digest, err error) string {
		t.Helper()
		mustNoErr(t, err)
		var out []string
		for _, d := range ds {
			out = append(out, d.Name)
		}
		return strings.Join(out, ", ")
	}
	const missing = 999
	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"folder Blogs", names(s.DigestsNamingFolder(ctx, fx.blogs.ID)), "Breakfast, Afternoon"},
		{"folder Videos", names(s.DigestsNamingFolder(ctx, fx.videos.ID)), "Afternoon"},
		{"unknown folder", names(s.DigestsNamingFolder(ctx, missing)), ""},
		{"tag go", names(s.DigestsNamingTag(ctx, fx.goTag.ID)), "Breakfast"},
		{"tag music", names(s.DigestsNamingTag(ctx, fx.music.ID)), "Other"},
		{"unknown tag", names(s.DigestsNamingTag(ctx, missing)), ""},
	} {
		if tc.got != tc.want {
			t.Errorf("%s: digests %q, want %q", tc.name, tc.got, tc.want)
		}
	}
	mustNoErr(t, s.DeleteFolder(ctx, fx.blogs.ID))
	if got := names(s.DigestsNamingFolder(ctx, fx.blogs.ID)); got != "" {
		t.Errorf("deleted folder: digests %q, want none", got)
	}
}

// digestFixture adds to the grid fixture (see newGridFixture) the digest
// "Morning" with feed b, folder Videos and tag "go": b0 is both in feed b and
// tagged "go", a0 is in neither feed but tagged "go".
func digestFixture(t *testing.T) (*gridFixture, Digest) {
	t.Helper()
	fx := newGridFixture(t)
	d := mustDigest(t, fx.s, "Morning")
	mustSources(t, fx.s, d.ID, DigestSources{FeedIDs: idList(fx.b.ID), FolderIDs: idList(fx.videos.ID), TagIDs: idList(fx.goTag.ID)})
	return fx, d
}

func TestListEntries_DigestScopeListsEachEntryOnce(t *testing.T) {
	fx, d := digestFixture(t)
	s := fx.s
	blogsOnly := mustDigest(t, s, "Blogs only")
	mustSources(t, s, blogsOnly.ID, DigestSources{FolderIDs: idList(fx.blogs.ID)})
	tagOnly := mustDigest(t, s, "Music only")
	mustSources(t, s, tagOnly.ID, DigestSources{TagIDs: idList(fx.music.ID)})
	empty := mustDigest(t, s, "Empty")

	cases := []struct {
		name       string
		scope      Scope
		unreadOnly bool
		want       string
	}{
		// Global order: y0 b0 a0 y1 b1 a1 y2 b2 a2 y3 a3; read: y1 a0 b2.
		{"feed, folder and tag", Scope{Kind: ScopeDigest, ID: d.ID}, false, "y0 b0 a0 y1 b1 y2 b2 y3"},
		{"feed, folder and tag unread", Scope{Kind: ScopeDigest, ID: d.ID}, true, "y0 b0 b1 y2 y3"},
		{"folder only", Scope{Kind: ScopeDigest, ID: blogsOnly.ID}, false, "a0 a1 a2 a3"},
		{"tag only", Scope{Kind: ScopeDigest, ID: tagOnly.ID}, false, "y0"},
		{"no sources", Scope{Kind: ScopeDigest, ID: empty.ID}, false, ""},
		{"unknown digest", Scope{Kind: ScopeDigest, ID: 999}, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, limit := range []int{1, 3, 100} {
				got := listAll(t, s, ListQuery{Scope: tc.scope, UnreadOnly: tc.unreadOnly, Limit: limit})
				if g := strings.Join(guids(got), " "); g != tc.want {
					t.Fatalf("limit %d: got %q, want %q", limit, g, tc.want)
				}
			}
		})
	}
	if (Scope{Kind: ScopeDigest}).IgnoresReadFilter() {
		t.Error("the digest scope ignores the Unread filter")
	}
}

// TestListEntries_DigestFollowsItsFoldersAndTags: a digest lists what its
// folders and tags hold now, not when it was saved.
func TestListEntries_DigestFollowsItsFoldersAndTags(t *testing.T) {
	fx := newGridFixture(t)
	s, ctx := fx.s, t.Context()
	d := mustDigest(t, s, "Blogs")
	mustSources(t, s, d.ID, DigestSources{FolderIDs: idList(fx.blogs.ID), TagIDs: idList(fx.music.ID)})
	scope := ListQuery{Scope: Scope{Kind: ScopeDigest, ID: d.ID}, Limit: 100}

	mustNoErr(t, s.MoveFeed(ctx, fx.b.ID, fx.blogs.ID))
	_, err := s.AddEntryTag(ctx, fx.ids["y3"], "MUSIC")
	mustNoErr(t, err)
	if got := strings.Join(guids(listAll(t, s, scope)), " "); got != "y0 b0 a0 b1 a1 b2 a2 y3 a3" {
		t.Fatalf("after moving b into Blogs and tagging y3: %q", got)
	}
	mustNoErr(t, s.MoveFeed(ctx, fx.a.ID, 0))
	mustNoErr(t, s.RemoveEntryTag(ctx, fx.ids["y0"], fx.music.ID))
	if got := strings.Join(guids(listAll(t, s, scope)), " "); got != "b0 b1 b2 y3" {
		t.Fatalf("after moving a out and untagging y0: %q", got)
	}
}

// TestListEntries_DigestPagesMatchAPlainFilter: the digest's grid, read from
// each of its feeds' newest entries and its tagged entries, pages through
// exactly what a plain filter over the whole library selects, in grid order,
// at every page size and in both filters. The digest names feed a and its
// folder Blogs, so a is reached twice; a0 is in feed a and tagged "go", b0
// and y0 are reached only through their tags; feed c, also in Blogs, holds
// more than a page, all published at a0's instant, as a feed without dates
// is (its entries get their fetch time).
func TestListEntries_DigestPagesMatchAPlainFilter(t *testing.T) {
	fx := newGridFixture(t)
	s := fx.s
	nf := newFeed("https://c.example/feed", KindRSS)
	nf.FolderID = fx.blogs.ID
	a0 := entryByGUID(t, s, fx.a.ID, "a0")
	undated := make([]NewEntry, config.ScopePageSize)
	for i := range undated {
		undated[i] = entryAt(fmt.Sprintf("c%d", i), a0.PublishedAt)
	}
	mustCreateFeed(t, s, nf, undated, config.InitialUnread)
	d := mustDigest(t, s, "Everything")
	mustSources(t, s, d.ID, DigestSources{
		FeedIDs: idList(fx.a.ID), FolderIDs: idList(fx.blogs.ID), TagIDs: idList(fx.goTag.ID, fx.music.ID),
	})

	all := plainDigestIDs(t, s, d.ID, false)
	for _, g := range []string{"a0", "b0", "y0"} {
		if !slices.Contains(all, fx.ids[g]) {
			t.Fatalf("the plain filter misses %s: %v", g, all)
		}
	}
	// Mark all as read selects the digest's entries with digestFilter.
	viaFilter, err := queryIDs(t.Context(), s.db, `SELECT e.id FROM entries e WHERE `+digestFilter+`
		ORDER BY e.published_at DESC, e.id DESC`, d.ID, d.ID, d.ID)
	mustNoErr(t, err)
	if !slices.Equal(viaFilter, all) {
		t.Fatalf("digestFilter selects %v, want %v", viaFilter, all)
	}
	for _, unread := range []bool{false, true} {
		want := plainDigestIDs(t, s, d.ID, unread)
		for limit := 1; limit <= len(want)+1; limit++ {
			got := listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeDigest, ID: d.ID}, UnreadOnly: unread, Limit: limit})
			ids := make([]int64, len(got))
			for i, v := range got {
				ids[i] = v.ID
			}
			if !slices.Equal(ids, want) {
				t.Fatalf("unread=%v limit %d: listed %v, want %v", unread, limit, ids, want)
			}
		}
	}
}

// plainDigestIDs returns, in grid order, the ids of the entries of digest id
// (only the unread ones when unread is set) selected the plain way: one
// filter over every entry of the library.
func plainDigestIDs(t *testing.T, s *SQLite, id int64, unread bool) []int64 {
	t.Helper()
	ids, err := queryIDs(t.Context(), s.db, `SELECT e.id FROM entries e
		WHERE e.id IN (
			SELECT x.id FROM entries x JOIN digest_feeds df ON df.feed_id = x.feed_id
			WHERE df.digest_id = ?1
			UNION
			SELECT x.id FROM entries x JOIN feeds f ON f.id = x.feed_id
				JOIN digest_folders dfo ON dfo.folder_id = f.folder_id
			WHERE dfo.digest_id = ?1
			UNION
			SELECT et.entry_id FROM entry_tags et JOIN digest_tags dt ON dt.tag_id = et.tag_id
			WHERE dt.digest_id = ?1)
		AND (?2 = 0 OR e.is_read = 0)
		ORDER BY e.published_at DESC, e.id DESC`, id, boolInt(unread))
	mustNoErr(t, err)
	return ids
}

func TestMarkScopeReadUpTo_DigestTouchesOnlyItsEntries(t *testing.T) {
	fx, d := digestFixture(t)
	s, ctx := fx.s, t.Context()
	maxID, err := s.MaxEntryID(ctx)
	mustNoErr(t, err)
	// Stored after the page was rendered: stays unread.
	_, err = s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: fx.b.ID, FetchedAt: t0, Entries: []NewEntry{entryAt("late", t0.Add(time.Minute))}})
	mustNoErr(t, err)

	at := t0.Add(time.Hour)
	n, err := s.MarkScopeReadUpTo(ctx, Scope{Kind: ScopeDigest, ID: d.ID}, maxID, at)
	mustNoErr(t, err)
	if n != 5 {
		t.Errorf("changed = %d, want 5 (y0 b0 b1 y2 y3)", n)
	}
	unread := listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeAll}, UnreadOnly: true, Limit: 50})
	if got := strings.Join(guids(unread), " "); got != "late a1 a2 a3" {
		t.Errorf("unread after = %q, want %q", got, "late a1 a2 a3")
	}
	if v := entryByGUID(t, s, fx.b.ID, "b0"); !v.ReadAt.Equal(at) {
		t.Errorf("b0 ReadAt = %v, want %v", v.ReadAt, at)
	}
}

func TestUnreadCounts_PerDigestCountsEachUnreadEntryOnce(t *testing.T) {
	fx, d := digestFixture(t)
	s, ctx := fx.s, t.Context()
	empty := mustDigest(t, s, "Empty")
	blogs := mustDigest(t, s, "Blogs")
	mustSources(t, s, blogs.ID, DigestSources{FeedIDs: idList(fx.a.ID), FolderIDs: idList(fx.blogs.ID)})

	c, err := s.UnreadCounts(ctx)
	mustNoErr(t, err)
	want := UnreadCounts{
		All:      8,
		ByFeed:   map[int64]int{fx.yt.ID: 3, fx.a.ID: 3, fx.b.ID: 2},
		ByFolder: map[int64]int{fx.videos.ID: 3, fx.blogs.ID: 3},
		// Morning: y0 b0 b1 y2 y3 (b0 is in feed b and tagged "go").
		// Blogs: a1 a2 a3 (feed a, and its folder).
		ByDigest: map[int64]int{d.ID: 5, blogs.ID: 3}, // Empty absent = 0
	}
	if !equalCounts(c, want) {
		t.Fatalf("UnreadCounts = %+v, want %+v", c, want)
	}

	// The badge agrees with the scope's unread listing after every action.
	mustNoErr(t, s.SetRead(ctx, fx.ids["b0"], true, t0))
	mustNoErr(t, s.SetRead(ctx, fx.ids["a0"], false, t0)) // tagged "go"
	c, err = s.UnreadCounts(ctx)
	mustNoErr(t, err)
	for _, dg := range []Digest{d, blogs, empty} {
		listed := listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeDigest, ID: dg.ID}, UnreadOnly: true, Limit: 50})
		if c.ByDigest[dg.ID] != len(listed) {
			t.Errorf("digest %q: badge %d, unread listed %d", dg.Name, c.ByDigest[dg.ID], len(listed))
		}
	}
	if c.ByDigest[d.ID] != 5 || c.ByDigest[blogs.ID] != 4 {
		t.Errorf("ByDigest = %v, want Morning 5 (a0 in, b0 out) and Blogs 4", c.ByDigest)
	}
}

func TestDigestFeedIDs_ItsFeedsAndTheFeedsOfItsFolders(t *testing.T) {
	fx, d := digestFixture(t)
	s, ctx := fx.s, t.Context()
	got, err := s.DigestFeedIDs(ctx, d.ID)
	mustNoErr(t, err)
	if want := slices.Sorted(slices.Values(idList(fx.b.ID, fx.yt.ID))); !slices.Equal(got, want) {
		t.Fatalf("DigestFeedIDs = %v, want %v (feed b, folder Videos; tags have no feeds)", got, want)
	}
	// A feed in the digest both directly and through its folder is listed once.
	mustNoErr(t, s.MoveFeed(ctx, fx.b.ID, fx.videos.ID))
	got, err = s.DigestFeedIDs(ctx, d.ID)
	mustNoErr(t, err)
	if want := slices.Sorted(slices.Values(idList(fx.b.ID, fx.yt.ID))); !slices.Equal(got, want) {
		t.Fatalf("DigestFeedIDs = %v, want %v", got, want)
	}
	empty := mustDigest(t, s, "Empty")
	if got, err := s.DigestFeedIDs(ctx, empty.ID); err != nil || len(got) != 0 {
		t.Fatalf("DigestFeedIDs(empty) = %v, %v; want none", got, err)
	}
}

func TestIngestMinutes_OfTheDigestsOfAFeedAndItsFolder(t *testing.T) {
	fx := newGridFixture(t)
	s, ctx := fx.s, t.Context()
	direct := mustDigest(t, s, "Direct")
	mustSources(t, s, direct.ID, DigestSources{FeedIDs: idList(fx.a.ID)})
	viaFolder := mustDigest(t, s, "Via folder")
	mustSources(t, s, viaFolder.ID, DigestSources{FolderIDs: idList(fx.blogs.ID)})
	sameTime := mustDigest(t, s, "Same time")
	mustSources(t, s, sameTime.ID, DigestSources{FeedIDs: idList(fx.a.ID)})
	untimed := mustDigest(t, s, "No time")
	mustSources(t, s, untimed.ID, DigestSources{FeedIDs: idList(fx.a.ID), FolderIDs: idList(fx.blogs.ID)})
	tagOnly := mustDigest(t, s, "Tag")
	mustSources(t, s, tagOnly.ID, DigestSources{TagIDs: idList(fx.goTag.ID)})

	mustNoErr(t, s.SetDigestIngest(ctx, direct.ID, sevenPM, true))
	mustNoErr(t, s.SetDigestIngest(ctx, viaFolder.ID, sevenAM, true))
	mustNoErr(t, s.SetDigestIngest(ctx, sameTime.ID, sevenPM, true))
	mustNoErr(t, s.SetDigestIngest(ctx, tagOnly.ID, sevenAM+1, true))

	for _, tc := range []struct {
		name             string
		feedID, folderID int64
		want             []int
	}{
		{"direct and via its folder", fx.a.ID, fx.blogs.ID, []int{sevenAM, sevenPM}},
		{"direct only", fx.a.ID, 0, []int{sevenPM}},
		{"subscribing into the folder", 0, fx.blogs.ID, []int{sevenAM}},
		{"in no timed digest", fx.yt.ID, fx.videos.ID, nil},
		{"tagged entries are not fetched", fx.b.ID, 0, nil},
	} {
		got, err := s.IngestMinutes(ctx, tc.feedID, tc.folderID)
		mustNoErr(t, err)
		if !slices.Equal(got, tc.want) {
			t.Errorf("%s: IngestMinutes = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestAdvanceNextFetch_OnlyEverMovesEarlier(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), nil, 0)
	next := f.NextFetchAt
	for _, tc := range []struct {
		at    time.Time
		moved bool
	}{
		{next.Add(time.Hour), false},
		{next, false},
		{next.Add(-time.Hour), true},
	} {
		moved, err := s.AdvanceNextFetch(ctx, f.ID, tc.at)
		mustNoErr(t, err)
		if moved != tc.moved {
			t.Errorf("AdvanceNextFetch(%s) moved = %v, want %v", tc.at, moved, tc.moved)
		}
		if moved {
			next = tc.at
		}
		if got, _ := s.GetFeed(ctx, f.ID); !got.NextFetchAt.Equal(next) {
			t.Errorf("next fetch = %s, want %s", got.NextFetchAt, next)
		}
	}
	// A feed that is due now (next_fetch_at = 0) stays due.
	mustNoErr(t, s.SetNextFetch(ctx, f.ID, time.Time{}))
	if moved, err := s.AdvanceNextFetch(ctx, f.ID, t0); err != nil || moved {
		t.Errorf("due feed: moved = %v, %v; want false", moved, err)
	}
	if _, err := s.AdvanceNextFetch(ctx, f.ID+1, t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown feed err = %v, want ErrNotFound", err)
	}
}

// TestMigrate_FromV4AddsDigestsAndKeepsEverything upgrades a populated
// database from the schema before digests.
func TestMigrate_FromV4AddsDigestsAndKeepsEverything(t *testing.T) {
	db := rawDB(t)
	migrateTo(t, db, 4)
	for _, q := range []string{
		`INSERT INTO folders (id, name, name_key, position) VALUES (1, 'Videos', 'videos', 0), (2, 'Blogs', 'blogs', 1)`,
		`INSERT INTO feeds (id, kind, url, title, folder_id, next_fetch_at, interval_sec, error_count, last_error,
			created_at, ttl_sec) VALUES
			(1, 'youtube', 'https://v.example/feed', 'V', 1, 100, NULL, 0, NULL, 0, 0),
			(2, 'rss', 'https://b.example/feed', 'B', 2, 200, 3600, 2, 'HTTP 503', 0, 1800),
			(3, 'rss', 'https://c.example/feed', 'C', NULL, 300, NULL, 0, NULL, 0, 0)`,
		`INSERT INTO entries (id, feed_id, guid, title, published_at, fetched_at, is_read, is_later, is_favourite) VALUES
			(1, 1, 'v1', 'V1', 10, 10, 0, 1, 0), (2, 2, 'b1', 'B1', 20, 20, 1, 0, 1),
			(3, 3, 'c1', 'C1', 30, 30, 0, 0, 0), (7, 2, 'b2', 'B2', 40, 40, 0, 0, 0)`,
		`INSERT INTO tags (id, name, name_key) VALUES (1, 'Go', 'go')`,
		`INSERT INTO entry_tags (entry_id, tag_id) VALUES (3, 1), (2, 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(t.Context(), db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v := userVersion(t, db); v < 5 {
		t.Fatalf("user_version = %d, want at least 5", v)
	}
	for _, table := range []string{"digests", "digest_feeds", "digest_folders", "digest_tags"} {
		if !tableExists(t, db, table) {
			t.Errorf("table %s missing after migrating", table)
		}
	}
	s := &SQLite{db: db, now: func() time.Time { return t0 }}
	ctx := t.Context()

	// Everything that was there is unchanged.
	var rows [4]int
	mustNoErr(t, db.QueryRow(`SELECT (SELECT count(*) FROM folders), (SELECT count(*) FROM feeds),
		(SELECT count(*) FROM entries), (SELECT count(*) FROM entry_tags)`).Scan(&rows[0], &rows[1], &rows[2], &rows[3]))
	if rows != [4]int{2, 3, 4, 2} {
		t.Errorf("folders, feeds, entries, entry_tags = %v, want [2 3 4 2]", rows)
	}
	b, err := s.GetFeed(ctx, 2)
	mustNoErr(t, err)
	if b.FolderID != 2 || b.IntervalSec != 3600 || b.ErrorCount != 2 || b.TTLSec != 1800 || b.NextFetchAt.Unix() != 200 {
		t.Errorf("feed 2 after migrating = %+v", b)
	}
	if e, err := s.GetEntry(ctx, 2); err != nil || !e.IsRead || !e.IsFavourite || len(e.Tags) != 1 {
		t.Errorf("entry 2 after migrating = %+v, %v", e, err)
	}
	if ds, err := s.ListDigests(ctx); err != nil || len(ds) != 0 {
		t.Errorf("digests after migrating = %v, %v; want none", ds, err)
	}

	// Digests work on the migrated rows.
	d := mustDigest(t, s, "Morning")
	mustSources(t, s, d.ID, DigestSources{FeedIDs: idList(3), FolderIDs: idList(1), TagIDs: idList(1)})
	mustNoErr(t, s.SetDigestIngest(ctx, d.ID, sevenAM, true))
	got := listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeDigest, ID: d.ID}, Limit: 10})
	if g := strings.Join(guids(got), " "); g != "c1 b1 v1" {
		t.Errorf("digest entries = %q, want %q", g, "c1 b1 v1")
	}
	c, err := s.UnreadCounts(ctx)
	mustNoErr(t, err)
	if c.ByDigest[d.ID] != 2 || c.All != 3 {
		t.Errorf("counts = %+v, want digest 2, all 3", c)
	}
	if m, err := s.IngestMinutes(ctx, 1, 1); err != nil || !slices.Equal(m, []int{sevenAM}) {
		t.Errorf("IngestMinutes(feed 1) = %v, %v", m, err)
	}
	mustNoErr(t, s.DeleteFeed(ctx, 3))
	mustNoErr(t, s.DeleteFolder(ctx, 1))
	if src, err := s.DigestSources(ctx, d.ID); err != nil || fmt.Sprint(src) != fmt.Sprint(DigestSources{TagIDs: idList(1)}) {
		t.Errorf("sources after deleting feed 3 and folder 1 = %+v, %v", src, err)
	}
	foreignKeysOnEveryConn(t, db, 4)
}
