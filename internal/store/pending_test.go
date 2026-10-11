package store

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/migrations"
)

const pendingURL = "https://www.youtube.com/feeds/videos.xml?channel_id=UCaaaaaaaaaaaaaaaaaaaaaa"

// pendingFeed describes a subscription whose feed did not answer: no
// validators, one failure recorded, and the initial-add rule still owed.
func pendingFeed(url string) NewFeed {
	return NewFeed{
		Kind:           KindYouTube,
		URL:            url,
		Title:          "A channel",
		FetchedAt:      t0,
		NextFetchAt:    t0.Add(config.FirstRetryDelay),
		InitialPending: true,
		LastError:      "HTTP 404 from " + url,
	}
}

func TestCreateFeed_PendingSubscriptionHasNoEntriesAndOneFailure(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, pendingFeed(pendingURL), nil, 0)
	if !f.InitialPending || f.ErrorCount != 1 || f.LastError != "HTTP 404 from "+pendingURL {
		t.Errorf("pending feed = %+v, want initial_pending, error_count 1 and the error", f)
	}
	if f.ETag != "" || f.LastModified != "" || !f.LastFetchedAt.Equal(t0) || !f.NextFetchAt.Equal(t0.Add(config.FirstRetryDelay)) {
		t.Errorf("pending feed fetch state = %+v", f)
	}
	if n := countRows(t, s, "entries", "feed_id = ?", f.ID); n != 0 {
		t.Errorf("entries = %d, want none", n)
	}

	// A normal subscription is neither pending nor failing.
	g := mustCreateFeed(t, s, newFeed("https://b.example/feed", KindRSS), entriesNewestFirst("e", 2, t0), config.InitialUnread)
	if g.InitialPending || g.ErrorCount != 0 || g.LastError != "" {
		t.Errorf("normal feed = %+v", g)
	}
	// Reading it back gives the same flag.
	got, err := s.GetFeed(t.Context(), f.ID)
	mustNoErr(t, err)
	if !got.InitialPending {
		t.Error("GetFeed lost initial_pending")
	}
}

func TestCreateFeed_PendingSubscriptionWithEntriesIsInvalid(t *testing.T) {
	s := openTest(t)
	_, err := s.CreateFeed(t.Context(), pendingFeed(pendingURL), entriesNewestFirst("e", 1, t0), config.InitialUnread)
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestCreateFeed_PendingSubscriptionURLIsStillUnique(t *testing.T) {
	s := openTest(t)
	mustCreateFeed(t, s, pendingFeed(pendingURL), nil, 0)
	if _, err := s.CreateFeed(t.Context(), pendingFeed(pendingURL), nil, 0); !errors.Is(err, ErrConflict) {
		t.Fatalf("second pending subscription err = %v, want ErrConflict", err)
	}
}

// TestRecordFetchSuccess_InitialPendingStoresNewestUnreadRestRead: the first
// successful fetch of a pending subscription is stored as an initial add.
func TestRecordFetchSuccess_InitialPendingStoresNewestUnreadRestRead(t *testing.T) {
	const total = 3 * config.InitialUnread
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, pendingFeed(pendingURL), nil, 0)
	fetchedAt := t0.Add(time.Hour)
	// Listed oldest first, so the newest entries are not simply the first ones.
	list := entriesNewestFirst("e", total, fetchedAt)
	slices.Reverse(list)
	n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: fetchedAt,
		NextFetchAt: fetchedAt.Add(time.Hour), Entries: list, InitialUnread: config.InitialUnread})
	mustNoErr(t, err)
	if n != total {
		t.Fatalf("inserted = %d, want %d", n, total)
	}
	views := listAll(t, s, ListQuery{Scope: Scope{Kind: ScopeFeed, ID: f.ID}, Limit: total})
	for i, v := range views { // newest first
		wantUnread := i < config.InitialUnread
		if v.IsRead == wantUnread {
			t.Errorf("%s (rank %d): read = %v, want %v", v.GUID, i, v.IsRead, !wantUnread)
		}
		if v.IsRead && !v.ReadAt.Equal(fetchedAt) {
			t.Errorf("%s: read_at = %s, want the fetch time %s", v.GUID, v.ReadAt, fetchedAt)
		}
		if !v.IsRead && !v.ReadAt.IsZero() {
			t.Errorf("%s: unread with read_at %s", v.GUID, v.ReadAt)
		}
	}
	got, err := s.GetFeed(ctx, f.ID)
	mustNoErr(t, err)
	if got.InitialPending || got.ErrorCount != 0 || got.LastError != "" {
		t.Errorf("after the first success: %+v, want the flag and the failure cleared", got)
	}
	c, err := s.UnreadCounts(ctx)
	mustNoErr(t, err)
	if c.ByFeed[f.ID] != config.InitialUnread {
		t.Errorf("unread = %d, want %d", c.ByFeed[f.ID], config.InitialUnread)
	}

	// The rule applies once: the next poll's new entries are all unread.
	later := fetchedAt.Add(time.Hour)
	more := entriesNewestFirst("n", config.InitialUnread+1, later)
	n, err = s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: later, Entries: more,
		InitialUnread: config.InitialUnread})
	mustNoErr(t, err)
	if n != len(more) {
		t.Fatalf("second fetch inserted = %d, want %d", n, len(more))
	}
	for _, e := range more {
		if entryByGUID(t, s, f.ID, e.GUID).IsRead {
			t.Errorf("%s from a regular poll stored read", e.GUID)
		}
	}
}

// TestRecordFetchSuccess_InitialUnreadIgnoredForANormalFeed: the count only
// matters while the row is flagged.
func TestRecordFetchSuccess_InitialUnreadIgnoredForANormalFeed(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), nil, config.InitialUnread)
	list := entriesNewestFirst("e", 2*config.InitialUnread, t0)
	_, err := s.RecordFetchSuccess(t.Context(), FetchSuccess{FeedID: f.ID, FetchedAt: t0, Entries: list,
		InitialUnread: config.InitialUnread})
	mustNoErr(t, err)
	if n := countRows(t, s, "entries", "feed_id = ? AND is_read = 0", f.ID); n != len(list) {
		t.Errorf("unread = %d, want all %d", n, len(list))
	}
}

// TestRecordFetchSuccess_InitialRuleOnlyTouchesInsertedEntries: a flagged
// feed that already has entries keeps their state; only what the fetch
// inserts follows the initial rule.
func TestRecordFetchSuccess_InitialRuleOnlyTouchesInsertedEntries(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	old := entriesNewestFirst("old", 2, t0)
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), old, 1) // old0 unread, old1 read
	if _, err := s.db.ExecContext(ctx, `UPDATE feeds SET initial_pending = 1 WHERE id = ?`, f.ID); err != nil {
		t.Fatal(err)
	}
	fetchedAt := t0.Add(time.Hour)
	fresh := entriesNewestFirst("new", config.InitialUnread+2, fetchedAt)
	all := append(slices.Clone(fresh), old...)
	n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: fetchedAt, Entries: all,
		InitialUnread: config.InitialUnread})
	mustNoErr(t, err)
	if n != len(fresh) {
		t.Fatalf("inserted = %d, want %d", n, len(fresh))
	}
	if e := entryByGUID(t, s, f.ID, "old0"); e.IsRead {
		t.Error("old0 was marked read by the initial rule")
	}
	if e := entryByGUID(t, s, f.ID, "old1"); !e.IsRead || !e.ReadAt.Equal(t0) {
		t.Errorf("old1 = read %v at %s, want kept as read at %s", e.IsRead, e.ReadAt, t0)
	}
	for i, e := range fresh {
		if got := entryByGUID(t, s, f.ID, e.GUID); got.IsRead != (i >= config.InitialUnread) {
			t.Errorf("%s: read = %v", e.GUID, got.IsRead)
		}
	}
}

// TestPendingFlag_OnlyAStoredDocumentClearsIt: a 304, a failure or a
// manual failure leave a pending subscription pending.
func TestPendingFlag_OnlyAStoredDocumentClearsIt(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, pendingFeed(pendingURL), nil, 0)
	at := t0.Add(time.Hour)
	mustNoErr(t, s.RecordNotModified(ctx, f.ID, at, at.Add(time.Hour)))
	mustNoErr(t, s.RecordFetchError(ctx, f.ID, "HTTP 500", at, at.Add(time.Hour)))
	mustNoErr(t, s.RecordFetchErrorKeepCount(ctx, f.ID, "HTTP 404", at, at.Add(time.Hour)))
	got, err := s.GetFeed(ctx, f.ID)
	mustNoErr(t, err)
	if !got.InitialPending {
		t.Error("initial_pending cleared without a stored document")
	}
}

// TestRecordFetchSuccess_FailedWriteKeepsThePendingFlag: the flag is cleared
// in the same transaction as the entries are stored.
func TestRecordFetchSuccess_FailedWriteKeepsThePendingFlag(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, pendingFeed(pendingURL), nil, 0)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER boom BEFORE INSERT ON entries
		WHEN NEW.guid = 'boom' BEGIN SELECT RAISE(ABORT, 'boom'); END`); err != nil {
		t.Fatal(err)
	}
	list := append(entriesNewestFirst("e", 2, t0), entryAt("boom", t0))
	if _, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: t0, Entries: list,
		InitialUnread: config.InitialUnread}); err == nil {
		t.Fatal("RecordFetchSuccess succeeded despite the failing insert")
	}
	got, err := s.GetFeed(ctx, f.ID)
	mustNoErr(t, err)
	if !got.InitialPending || got.ErrorCount != 1 {
		t.Errorf("after a failed write: %+v, want still pending with its failure", got)
	}
	if n := countRows(t, s, "entries", "feed_id = ?", f.ID); n != 0 {
		t.Errorf("entries = %d after a failed write, want 0", n)
	}
}

func TestRecordFetchErrorKeepCount_StoresTheErrorButNotTheCount(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), entriesNewestFirst("e", 2, t0), config.InitialUnread)
	mustNoErr(t, s.RecordFetchError(ctx, f.ID, "HTTP 503", t0, t0.Add(time.Hour)))
	at, next := t0.Add(2*time.Hour), t0.Add(3*time.Hour)
	mustNoErr(t, s.RecordFetchErrorKeepCount(ctx, f.ID, "HTTP 404", at, next))
	got, err := s.GetFeed(ctx, f.ID)
	mustNoErr(t, err)
	if got.ErrorCount != 1 || got.LastError != "HTTP 404" || !got.LastFetchedAt.Equal(at) || !got.NextFetchAt.Equal(next) {
		t.Errorf("after a kept-count failure: %+v", got)
	}
	if got.ETag != `"v1"` || countRows(t, s, "entries", "feed_id = ?", f.ID) != 2 {
		t.Error("a failure touched validators or entries")
	}
	if err := s.RecordFetchErrorKeepCount(ctx, f.ID+100, "x", at, next); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown feed err = %v, want ErrNotFound", err)
	}
}

// TestMigrate_FromV5AddsInitialPendingAndKeepsEverything upgrades a
// populated database from the schema before pending subscriptions.
func TestMigrate_FromV5AddsInitialPendingAndKeepsEverything(t *testing.T) {
	db := rawDB(t)
	migrateTo(t, db, 5)
	for _, q := range []string{
		`INSERT INTO folders (id, name, name_key, position) VALUES (1, 'Videos', 'videos', 0)`,
		`INSERT INTO feeds (id, kind, url, title, folder_id, etag, last_fetched_at, next_fetch_at, interval_sec,
			error_count, last_error, created_at, ttl_sec) VALUES
			(1, 'youtube', 'https://v.example/feed', 'V', 1, '"e1"', 50, 100, NULL, 0, NULL, 0, 0),
			(2, 'rss', 'https://b.example/feed', 'B', NULL, NULL, 60, 200, 3600, 4, 'HTTP 404', 0, 1800)`,
		`INSERT INTO entries (id, feed_id, guid, title, published_at, fetched_at, is_read, read_at, is_later, is_favourite) VALUES
			(1, 1, 'v1', 'V1', 10, 10, 0, NULL, 1, 0), (2, 2, 'b1', 'B1', 20, 20, 1, 25, 0, 1)`,
		`INSERT INTO tags (id, name, name_key) VALUES (1, 'Go', 'go')`,
		`INSERT INTO entry_tags (entry_id, tag_id) VALUES (2, 1)`,
		`INSERT INTO digests (id, name, name_key, ingest_minute, position, created_at) VALUES (1, 'D', 'd', 420, 0, 0)`,
		`INSERT INTO digest_feeds (digest_id, feed_id) VALUES (1, 2)`,
		`INSERT INTO digest_folders (digest_id, folder_id) VALUES (1, 1)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(t.Context(), db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v := userVersion(t, db); v < 6 {
		t.Fatalf("user_version = %d, want at least 6", v)
	}
	s := &SQLite{db: db, now: func() time.Time { return t0 }}
	ctx := t.Context()

	var rows [6]int
	mustNoErr(t, db.QueryRow(`SELECT (SELECT count(*) FROM folders), (SELECT count(*) FROM feeds),
		(SELECT count(*) FROM entries), (SELECT count(*) FROM entry_tags), (SELECT count(*) FROM digest_feeds),
		(SELECT count(*) FROM digest_folders)`).Scan(&rows[0], &rows[1], &rows[2], &rows[3], &rows[4], &rows[5]))
	if rows != [6]int{1, 2, 2, 1, 1, 1} {
		t.Errorf("folders, feeds, entries, entry_tags, digest_feeds, digest_folders = %v", rows)
	}
	feeds, err := s.ListFeeds(ctx)
	mustNoErr(t, err)
	for _, f := range feeds {
		if f.InitialPending {
			t.Errorf("existing feed %d became pending", f.ID)
		}
	}
	b, err := s.GetFeed(ctx, 2)
	mustNoErr(t, err)
	if b.ErrorCount != 4 || b.LastError != "HTTP 404" || b.IntervalSec != 3600 || b.TTLSec != 1800 || b.NextFetchAt.Unix() != 200 {
		t.Errorf("feed 2 after migrating = %+v", b)
	}
	if e, err := s.GetEntry(ctx, 2); err != nil || !e.IsRead || !e.IsFavourite || len(e.Tags) != 1 {
		t.Errorf("entry 2 after migrating = %+v, %v", e, err)
	}
	// The column only takes 0 or 1.
	if _, err := db.Exec(`UPDATE feeds SET initial_pending = 2 WHERE id = 1`); err == nil {
		t.Error("initial_pending accepted 2")
	}
	// Existing feeds keep polling as before: a success stores new entries unread.
	_, err = s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: 1, FetchedAt: t0,
		Entries: entriesNewestFirst("n", 2*config.InitialUnread, t0), InitialUnread: config.InitialUnread})
	mustNoErr(t, err)
	if n := countRows(t, s, "entries", "feed_id = 1 AND is_read = 0"); n != 2*config.InitialUnread+1 {
		t.Errorf("unread on feed 1 = %d, want every new entry plus v1", n)
	}
	// And a pending subscription can be stored on the migrated schema.
	if _, err := s.CreateFeed(ctx, pendingFeed(pendingURL), nil, 0); err != nil {
		t.Errorf("CreateFeed(pending) on the migrated schema: %v", err)
	}
	foreignKeysOnEveryConn(t, db, 4)
}

// TestRecordFetchSuccess_InitialRuleTiesFavourTheEarlierEntry: as in
// CreateFeed, among equal published times the entry listed first is newer.
func TestRecordFetchSuccess_InitialRuleTiesFavourTheEarlierEntry(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, pendingFeed(pendingURL), nil, 0)
	list := []NewEntry{entryAt("a", t0), entryAt("b", t0), entryAt("c", t0)}
	_, err := s.RecordFetchSuccess(t.Context(), FetchSuccess{FeedID: f.ID, FetchedAt: t0, Entries: list, InitialUnread: 1})
	mustNoErr(t, err)
	for _, g := range []string{"a", "b", "c"} {
		if read := entryByGUID(t, s, f.ID, g).IsRead; read != (g != "a") {
			t.Errorf("%s: read = %v", g, read)
		}
	}
}
