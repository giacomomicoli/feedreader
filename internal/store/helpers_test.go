package store

import (
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// t0 is a fixed reference time with whole seconds (the store's precision).
var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// openTest opens a fresh database in a temporary directory.
func openTest(t *testing.T) *SQLite {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "feedreader.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	s.now = func() time.Time { return t0 }
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

// entryAt builds an entry whose fields are derived from guid.
func entryAt(guid string, published time.Time) NewEntry {
	return NewEntry{
		GUID:        guid,
		URL:         "https://example.com/" + guid,
		Title:       "Title " + guid,
		SummaryHTML: "<p>summary " + guid + "</p>",
		Author:      "author " + guid,
		PublishedAt: published,
	}
}

// entriesNewestFirst returns n entries "<prefix>0".."<prefix>n-1", the first
// published at newest and each following one an hour older, as feeds usually
// list them.
func entriesNewestFirst(prefix string, n int, newest time.Time) []NewEntry {
	out := make([]NewEntry, n)
	for i := range out {
		out[i] = entryAt(fmt.Sprintf("%s%d", prefix, i), newest.Add(-time.Duration(i)*time.Hour))
	}
	return out
}

// newFeed describes a feed subscribed at t0.
func newFeed(url string, kind Kind) NewFeed {
	return NewFeed{
		Kind:          kind,
		URL:           url,
		SiteURL:       url + "/site",
		Title:         "Feed " + url,
		OriginalTitle: "Original " + url,
		IconURL:       url + "/icon.png",
		ETag:          `"v1"`,
		LastModified:  "Wed, 01 Oct 2026 12:00:00 GMT",
		FetchedAt:     t0,
		NextFetchAt:   t0.Add(6 * time.Hour),
	}
}

func mustCreateFeed(t *testing.T, s *SQLite, nf NewFeed, entries []NewEntry, initialUnread int) Feed {
	t.Helper()
	f, err := s.CreateFeed(t.Context(), nf, entries, initialUnread)
	if err != nil {
		t.Fatalf("CreateFeed(%s): %v", nf.URL, err)
	}
	return f
}

func mustFolder(t *testing.T, s *SQLite, name string) Folder {
	t.Helper()
	f, err := s.CreateFolder(t.Context(), name)
	if err != nil {
		t.Fatalf("CreateFolder(%q): %v", name, err)
	}
	return f
}

// entryID returns the id of the entry (feedID, guid).
func entryID(t *testing.T, s *SQLite, feedID int64, guid string) int64 {
	t.Helper()
	var id int64
	err := s.db.QueryRowContext(t.Context(),
		`SELECT id FROM entries WHERE feed_id = ? AND guid = ?`, feedID, guid).Scan(&id)
	if err != nil {
		t.Fatalf("entry (%d, %q): %v", feedID, guid, err)
	}
	return id
}

// entryByGUID loads the entry (feedID, guid).
func entryByGUID(t *testing.T, s *SQLite, feedID int64, guid string) EntryView {
	t.Helper()
	v, err := s.GetEntry(t.Context(), entryID(t, s, feedID, guid))
	if err != nil {
		t.Fatalf("GetEntry: %v", err)
	}
	return v
}

// countRows counts rows of a table matching an optional WHERE clause.
func countRows(t *testing.T, s *SQLite, table, where string, args ...any) int {
	t.Helper()
	q := "SELECT count(*) FROM " + table
	if where != "" {
		q += " WHERE " + where
	}
	var n int
	if err := s.db.QueryRowContext(t.Context(), q, args...).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

// listAll pages through q until HasMore is false and returns every entry.
func listAll(t *testing.T, s *SQLite, q ListQuery) []EntryView {
	t.Helper()
	var all []EntryView
	for range 10000 {
		p, err := s.ListEntries(t.Context(), q)
		if err != nil {
			t.Fatalf("ListEntries(%+v): %v", q, err)
		}
		all = append(all, p.Entries...)
		if !p.HasMore {
			return all
		}
		next := p.Next
		q.After = &next
	}
	t.Fatal("ListEntries: pagination did not terminate")
	return nil
}

func guids(views []EntryView) []string {
	out := make([]string, len(views))
	for i, v := range views {
		out[i] = v.GUID
	}
	return out
}

func sortedGUIDs(views []EntryView) []string {
	out := guids(views)
	slices.Sort(out)
	return out
}

func tagNames(tags []Tag) []string {
	out := make([]string, len(tags))
	for i, tg := range tags {
		out[i] = tg.Name
	}
	return out
}
