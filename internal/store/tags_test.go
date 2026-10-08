package store

import (
	"errors"
	"slices"
	"testing"
)

// tagFixture returns a store with one feed of n entries and their ids.
func tagFixture(t *testing.T, n int) (*SQLite, []int64) {
	t.Helper()
	s := openTest(t)
	f := mustCreateFeed(t, s, newFeed("https://a.example", KindRSS), entriesNewestFirst("e", n, t0), n)
	ids := make([]int64, n)
	for i := range ids {
		ids[i] = entryID(t, s, f.ID, "e"+string(rune('0'+i)))
	}
	return s, ids
}

func TestAddEntryTag_TrimsAndMatchesExistingTagCaseInsensitively(t *testing.T) {
	s, ids := tagFixture(t, 2)
	ctx := t.Context()
	first, err := s.AddEntryTag(ctx, ids[0], "  Golang ")
	mustNoErr(t, err)
	if first.Name != "Golang" || first.ID == 0 {
		t.Fatalf("tag = %+v, want trimmed name", first)
	}
	second, err := s.AddEntryTag(ctx, ids[1], "golang")
	mustNoErr(t, err)
	if second.ID != first.ID || second.Name != "Golang" {
		t.Fatalf("second tag = %+v, want the existing %+v (original spelling kept)", second, first)
	}
	if n := countRows(t, s, "tags", ""); n != 1 {
		t.Fatalf("tags = %d, want 1", n)
	}
}

func TestAddEntryTag_IsIdempotent(t *testing.T) {
	s, ids := tagFixture(t, 1)
	ctx := t.Context()
	for range 3 {
		if _, err := s.AddEntryTag(ctx, ids[0], "music"); err != nil {
			t.Fatal(err)
		}
	}
	if n := countRows(t, s, "entry_tags", ""); n != 1 {
		t.Fatalf("entry_tags = %d, want 1", n)
	}
	v, _ := s.GetEntry(ctx, ids[0])
	if !slices.Equal(tagNames(v.Tags), []string{"music"}) {
		t.Fatalf("tags = %v", tagNames(v.Tags))
	}
}

func TestAddEntryTag_RejectsEmptyNameAndUnknownEntry(t *testing.T) {
	s, ids := tagFixture(t, 1)
	ctx := t.Context()
	for _, name := range []string{"", " \t "} {
		if _, err := s.AddEntryTag(ctx, ids[0], name); !errors.Is(err, ErrInvalid) {
			t.Errorf("AddEntryTag(%q) err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := s.AddEntryTag(ctx, ids[0]+100, "orphan"); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown entry err = %v, want ErrNotFound", err)
	}
	if n := countRows(t, s, "tags", ""); n != 0 {
		t.Errorf("tags = %d, want 0 (nothing created on failure)", n)
	}
}

func TestListTags_CountsAndOrderByNameCaseInsensitive(t *testing.T) {
	s, ids := tagFixture(t, 3)
	ctx := t.Context()
	for _, add := range []struct {
		entry int
		name  string
	}{{0, "zeta"}, {1, "zeta"}, {2, "zeta"}, {0, "Alpha"}, {1, "beta"}} {
		_, err := s.AddEntryTag(ctx, ids[add.entry], add.name)
		mustNoErr(t, err)
	}
	unused, err := s.AddEntryTag(ctx, ids[2], "unused")
	mustNoErr(t, err)
	mustNoErr(t, s.RemoveEntryTag(ctx, ids[2], unused.ID))

	tags, err := s.ListTags(ctx)
	mustNoErr(t, err)
	type nc struct {
		name  string
		count int
	}
	var got []nc
	for _, tg := range tags {
		got = append(got, nc{tg.Name, tg.Count})
	}
	want := []nc{{"Alpha", 1}, {"beta", 1}, {"unused", 0}, {"zeta", 3}}
	if !slices.Equal(got, want) {
		t.Fatalf("ListTags = %v, want %v", got, want)
	}
}

func TestGetTag(t *testing.T) {
	s, ids := tagFixture(t, 1)
	tag, err := s.AddEntryTag(t.Context(), ids[0], "news")
	mustNoErr(t, err)
	got, err := s.GetTag(t.Context(), tag.ID)
	if err != nil || got.ID != tag.ID || got.Name != "news" {
		t.Fatalf("GetTag = %+v, %v", got, err)
	}
	if _, err := s.GetTag(t.Context(), tag.ID+1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTag(missing) err = %v, want ErrNotFound", err)
	}
}

func TestSuggestTags_PrefixCaseInsensitiveLimitedAndWildcardSafe(t *testing.T) {
	s, ids := tagFixture(t, 1)
	ctx := t.Context()
	for _, n := range []string{"Go", "golang", "Gopher", "rust", "50%off", "50 cents", "a_b", "axb"} {
		_, err := s.AddEntryTag(ctx, ids[0], n)
		mustNoErr(t, err)
	}
	check := func(prefix string, limit int, want ...string) {
		t.Helper()
		got, err := s.SuggestTags(ctx, prefix, limit)
		mustNoErr(t, err)
		if !slices.Equal(tagNames(got), want) {
			t.Errorf("SuggestTags(%q, %d) = %v, want %v", prefix, limit, tagNames(got), want)
		}
	}
	check("go", 10, "Go", "golang", "Gopher")
	check("GO", 2, "Go", "golang")
	check("50%", 10, "50%off")
	check("a_", 10, "a_b")
	check("zzz", 10)
	check("go", 0)
}

func TestRemoveEntryTag_KeepsTagAndIsIdempotent(t *testing.T) {
	s, ids := tagFixture(t, 2)
	ctx := t.Context()
	tag, err := s.AddEntryTag(ctx, ids[0], "go")
	mustNoErr(t, err)
	_, err = s.AddEntryTag(ctx, ids[1], "go")
	mustNoErr(t, err)

	mustNoErr(t, s.RemoveEntryTag(ctx, ids[0], tag.ID))
	mustNoErr(t, s.RemoveEntryTag(ctx, ids[0], tag.ID)) // already gone: no-op
	if v, _ := s.GetEntry(ctx, ids[0]); len(v.Tags) != 0 {
		t.Errorf("tags after remove = %v", v.Tags)
	}
	if v, _ := s.GetEntry(ctx, ids[1]); len(v.Tags) != 1 {
		t.Errorf("other entry lost its tag: %v", v.Tags)
	}
	if _, err := s.GetTag(ctx, tag.ID); err != nil {
		t.Errorf("tag deleted by RemoveEntryTag: %v", err)
	}
	if err := s.RemoveEntryTag(ctx, ids[1]+100, tag.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing entry err = %v, want ErrNotFound", err)
	}
	if err := s.RemoveEntryTag(ctx, ids[1], tag.ID+100); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing tag err = %v, want ErrNotFound", err)
	}
}

func TestDeleteTag_RemovesEntryTagsOnlyAndKeepsEntries(t *testing.T) {
	s, ids := tagFixture(t, 2)
	ctx := t.Context()
	tag, err := s.AddEntryTag(ctx, ids[0], "go")
	mustNoErr(t, err)
	_, err = s.AddEntryTag(ctx, ids[1], "go")
	mustNoErr(t, err)
	keep, err := s.AddEntryTag(ctx, ids[1], "keep")
	mustNoErr(t, err)

	mustNoErr(t, s.DeleteTag(ctx, tag.ID))
	if n := countRows(t, s, "entries", ""); n != 2 {
		t.Errorf("entries = %d, want 2", n)
	}
	if n := countRows(t, s, "entry_tags", "tag_id = ?", tag.ID); n != 0 {
		t.Errorf("entry_tags of deleted tag = %d, want 0", n)
	}
	if v, _ := s.GetEntry(ctx, ids[1]); len(v.Tags) != 1 || v.Tags[0].ID != keep.ID {
		t.Errorf("remaining tags = %v, want [keep]", v.Tags)
	}
	p, err := s.ListEntries(ctx, ListQuery{Scope: Scope{Kind: ScopeTag, ID: tag.ID}, Limit: 10})
	if err != nil || len(p.Entries) != 0 {
		t.Errorf("deleted tag scope = %d entries, %v", len(p.Entries), err)
	}
	if err := s.DeleteTag(ctx, tag.ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("second DeleteTag err = %v, want ErrNotFound", err)
	}
}

func TestTagNames_UniqueCaseInsensitiveBeyondASCII(t *testing.T) {
	s, ids := tagFixture(t, 2)
	ctx := t.Context()
	for _, pair := range [][2]string{{"Città", "CITTÀ"}, {"über", "ÜBER"}, {"Économie", "économie"}} {
		first, err := s.AddEntryTag(ctx, ids[0], pair[0])
		mustNoErr(t, err)
		second, err := s.AddEntryTag(ctx, ids[1], pair[1])
		mustNoErr(t, err)
		if second.ID != first.ID || second.Name != pair[0] {
			t.Errorf("AddEntryTag(%q) = %+v, want the existing %+v (original spelling kept)", pair[1], second, first)
		}
	}
	tags, err := s.ListTags(ctx)
	mustNoErr(t, err)
	if len(tags) != 3 {
		t.Fatalf("ListTags = %+v, want 3 tags", tags)
	}
	for _, tg := range tags {
		if tg.Count != 2 {
			t.Errorf("tag %q count = %d, want 2", tg.Name, tg.Count)
		}
	}
	// The same entry tagged with another spelling keeps one row.
	again, err := s.AddEntryTag(ctx, ids[0], "CITTÀ")
	mustNoErr(t, err)
	if v, _ := s.GetEntry(ctx, ids[0]); len(v.Tags) != 3 || again.Name != "Città" {
		t.Errorf("tags after re-adding CITTÀ = %v (returned %+v)", tagNames(v.Tags), again)
	}
}

func TestSuggestTags_PrefixCaseInsensitiveBeyondASCII(t *testing.T) {
	s, ids := tagFixture(t, 1)
	ctx := t.Context()
	for _, n := range []string{"Città", "über", "Économie", "economia"} {
		_, err := s.AddEntryTag(ctx, ids[0], n)
		mustNoErr(t, err)
	}
	for prefix, want := range map[string][]string{
		"cittÀ": {"Città"},
		"CITTÀ": {"Città"},
		"Ü":     {"über"},
		"ü":     {"über"},
		"éco":   {"Économie"},
		"ÉCO":   {"Économie"},
		"eco":   {"economia"},
	} {
		got, err := s.SuggestTags(ctx, prefix, 10)
		mustNoErr(t, err)
		if !slices.Equal(tagNames(got), want) {
			t.Errorf("SuggestTags(%q) = %v, want %v", prefix, tagNames(got), want)
		}
	}
}
