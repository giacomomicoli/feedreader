package store

import (
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"modernc.org/sqlite"

	"github.com/giacomomicoli/feedreader/migrations"
)

func TestOpenSQLite_ForeignKeysAndPragmasOnEveryPooledConnection(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()

	// Holding several connections at once forces the pool to open distinct
	// ones; each must have been configured by the DSN.
	const n = 6
	conns := make([]*sql.Conn, n)
	for i := range conns {
		c, err := s.db.Conn(ctx)
		if err != nil {
			t.Fatalf("Conn %d: %v", i, err)
		}
		defer c.Close()
		conns[i] = c
	}
	for i, c := range conns {
		var fk, busy, syncMode int
		var journal string
		if err := c.QueryRowContext(ctx, "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA busy_timeout").Scan(&busy); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA synchronous").Scan(&syncMode); err != nil {
			t.Fatal(err)
		}
		if err := c.QueryRowContext(ctx, "PRAGMA journal_mode").Scan(&journal); err != nil {
			t.Fatal(err)
		}
		if fk != 1 || busy != busyTimeoutMS || syncMode != 1 || journal != "wal" {
			t.Errorf("conn %d: foreign_keys=%d busy_timeout=%d synchronous=%d journal_mode=%s; want 1, %d, 1 (NORMAL), wal",
				i, fk, busy, syncMode, journal, busyTimeoutMS)
		}
		// Behavioural check: an orphan entry must be rejected.
		_, err := c.ExecContext(ctx, `INSERT INTO entries (feed_id, guid, title, published_at, fetched_at)
			VALUES (999999, 'g', 't', 0, 0)`)
		if !errors.Is(classify(err), ErrNotFound) {
			t.Errorf("conn %d: orphan insert err = %v, want foreign key violation", i, err)
		}
	}
}

func TestOpenSQLite_PathWithSpecialCharactersAndMissingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new dir #1", "feed?reader%20.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	if _, err := s.CreateFolder(t.Context(), "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("database not created at the exact path: %v", err)
	}
}

func TestOpenSQLite_EmptyPathIsInvalid(t *testing.T) {
	if _, err := OpenSQLite(""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}
}

func TestOpenSQLite_ReopenKeepsDataAndSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedreader.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateFolder(t.Context(), "Kept"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	s, err = OpenSQLite(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	folders, err := s.ListFolders(t.Context())
	if err != nil || len(folders) != 1 || folders[0].Name != "Kept" {
		t.Fatalf("ListFolders = %v, %v; want [Kept]", folders, err)
	}
	list, err := listMigrations(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	var v int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	if want := list[len(list)-1].version; v != want {
		t.Fatalf("user_version = %d, want %d", v, want)
	}
}

func TestOpenSQLite_NotADatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "garbage.db")
	if err := os.WriteFile(path, []byte("this is definitely not an SQLite database file, no sir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := OpenSQLite(path); err == nil {
		s.Close()
		t.Fatal("OpenSQLite on a non-database file succeeded")
	}
}

func TestErrors_SentinelsWrapTheDriverError(t *testing.T) {
	s := openTest(t)
	mustFolder(t, s, "dup")
	_, err := s.CreateFolder(t.Context(), "DUP")
	var se *sqlite.Error
	if !errors.Is(err, ErrConflict) || !errors.As(err, &se) {
		t.Fatalf("err = %v; want ErrConflict wrapping *sqlite.Error", err)
	}
	if _, err := s.GetFeed(t.Context(), 1); !errors.Is(err, ErrNotFound) || !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("err = %v; want ErrNotFound wrapping sql.ErrNoRows", err)
	}
}

// --- migrations ---

// rawDB opens a database with the store's DSN but without migrating.
func rawDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(filepath.Join(t.TempDir(), "m.db")))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func userVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM sqlite_schema WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func TestMigrate_AppliesInNumericOrderOnce(t *testing.T) {
	fsys := fstest.MapFS{
		"10_c.sql": {Data: []byte("ALTER TABLE a ADD COLUMN c INTEGER;")},
		"2_b.sql":  {Data: []byte("ALTER TABLE a ADD COLUMN b INTEGER;")},
		"1_a.sql":  {Data: []byte("CREATE TABLE a (id INTEGER PRIMARY KEY);\nCREATE TABLE z (id INTEGER);")},
	}
	db := rawDB(t)
	ctx := t.Context()
	if err := migrate(ctx, db, fsys); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if v := userVersion(t, db); v != 10 {
		t.Fatalf("user_version = %d, want 10", v)
	}
	if !tableExists(t, db, "z") {
		t.Fatal("multi-statement migration only partially applied")
	}
	// Re-running must skip everything (re-applying would fail: table exists).
	if err := migrate(ctx, db, fsys); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	// A new migration is applied on top.
	fsys["11_d.sql"] = &fstest.MapFile{Data: []byte("CREATE TABLE d (id INTEGER);")}
	if err := migrate(ctx, db, fsys); err != nil {
		t.Fatalf("third migrate: %v", err)
	}
	if v := userVersion(t, db); v != 11 || !tableExists(t, db, "d") {
		t.Fatalf("user_version = %d, table d exists = %v; want 11, true", v, tableExists(t, db, "d"))
	}
}

func TestMigrate_FailedMigrationRollsBack(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_ok.sql":  {Data: []byte("CREATE TABLE a (id INTEGER);")},
		"0002_bad.sql": {Data: []byte("CREATE TABLE b (id INTEGER);\nTHIS IS NOT SQL;")},
	}
	db := rawDB(t)
	if err := migrate(t.Context(), db, fsys); err == nil {
		t.Fatal("migrate with a broken file succeeded")
	}
	if v := userVersion(t, db); v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
	if tableExists(t, db, "b") {
		t.Fatal("partial migration 0002 was not rolled back")
	}
}

func TestMigrate_RefusesNewerDatabase(t *testing.T) {
	db := rawDB(t)
	if _, err := db.Exec("PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	fsys := fstest.MapFS{"0001_a.sql": {Data: []byte("CREATE TABLE a (id INTEGER);")}}
	if err := migrate(t.Context(), db, fsys); err == nil {
		t.Fatal("migrate accepted a database from a newer binary")
	}
}

func TestListMigrations_RejectsBadNames(t *testing.T) {
	for name, fsys := range map[string]fstest.MapFS{
		"no number":         {"init.sql": {}},
		"version zero":      {"0000_init.sql": {}},
		"duplicate version": {"1_a.sql": {}, "001_b.sql": {}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := listMigrations(fsys); err == nil {
				t.Fatal("listMigrations accepted invalid names")
			}
		})
	}
}

func TestEmbeddedMigrations_ContainNoPragmas(t *testing.T) {
	list, err := listMigrations(migrations.FS)
	if err != nil || len(list) == 0 {
		t.Fatalf("listMigrations(embedded) = %v, %v", list, err)
	}
	// journal_mode cannot change inside the migration transaction (and
	// foreign_keys is silently ignored there); pragmas belong in the DSN.
	for _, m := range list {
		body, err := fs.ReadFile(migrations.FS, m.name)
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(strings.ToUpper(strings.TrimSpace(line)), "PRAGMA") {
				t.Errorf("%s:%d: PRAGMA in a migration: %s", m.name, n+1, line)
			}
		}
	}
	db := rawDB(t)
	if err := migrate(t.Context(), db, migrations.FS); err != nil {
		t.Fatalf("migrate(embedded): %v", err)
	}
}

// foreignKeysOnEveryConn fails the test unless n simultaneously held pooled
// connections all enforce foreign keys.
func foreignKeysOnEveryConn(t *testing.T, db *sql.DB, n int) {
	t.Helper()
	for i := range n {
		c, err := db.Conn(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		var fk int
		if err := c.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&fk); err != nil {
			t.Fatal(err)
		}
		if fk != 1 {
			t.Errorf("pooled conn %d: foreign_keys = %d after migrating, want 1", i, fk)
		}
	}
}

func TestMigrate_TableRebuildDoesNotFireOnDeleteActions(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_init.sql": {Data: []byte(`CREATE TABLE p (id INTEGER PRIMARY KEY, x TEXT);
			CREATE TABLE c (id INTEGER PRIMARY KEY, p_id INTEGER REFERENCES p(id) ON DELETE CASCADE);
			CREATE TABLE n (id INTEGER PRIMARY KEY, p_id INTEGER REFERENCES p(id) ON DELETE SET NULL);
			INSERT INTO p VALUES (1, 'a');
			INSERT INTO c VALUES (1, 1);
			INSERT INTO n VALUES (1, 1);`)},
		// SQLite's documented procedure for schema changes ALTER TABLE cannot make.
		"0002_rebuild.sql": {Data: []byte(`CREATE TABLE p_new (id INTEGER PRIMARY KEY, x TEXT NOT NULL);
			INSERT INTO p_new SELECT id, x FROM p;
			DROP TABLE p;
			ALTER TABLE p_new RENAME TO p;`)},
	}
	db := rawDB(t)
	if err := migrate(t.Context(), db, fsys); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	var children, setNull int
	if err := db.QueryRow(`SELECT (SELECT count(*) FROM c), (SELECT count(*) FROM n WHERE p_id IS NULL)`).Scan(&children, &setNull); err != nil {
		t.Fatal(err)
	}
	if children != 1 || setNull != 0 {
		t.Fatalf("after rebuilding the parent: %d cascade children (want 1), %d nulled references (want 0)", children, setNull)
	}
	foreignKeysOnEveryConn(t, db, 4)
	// The rebuilt parent is enforced again.
	if _, err := db.Exec(`DELETE FROM p WHERE id = 1`); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT count(*) FROM c`).Scan(&children); err != nil || children != 0 {
		t.Fatalf("cascade after migrating: %d children, %v; want 0", children, err)
	}
}

func TestMigrate_RefusesToCommitDanglingForeignKeys(t *testing.T) {
	fsys := fstest.MapFS{
		"0001_init.sql":   {Data: []byte("CREATE TABLE p (id INTEGER PRIMARY KEY);\nCREATE TABLE c (p_id INTEGER REFERENCES p(id));")},
		"0002_orphan.sql": {Data: []byte("INSERT INTO c (p_id) VALUES (42);")},
	}
	db := rawDB(t)
	err := migrate(t.Context(), db, fsys)
	if err == nil || !strings.Contains(err.Error(), "foreign key") {
		t.Fatalf("migrate err = %v, want a foreign key violation", err)
	}
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM c`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("orphan rows = %d, %v; want 0 (rolled back)", n, err)
	}
	if v := userVersion(t, db); v != 1 {
		t.Fatalf("user_version = %d, want 1", v)
	}
}

// migrateTo applies the embedded migrations up to and including version.
func migrateTo(t *testing.T, db *sql.DB, version int) {
	t.Helper()
	list, err := listMigrations(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	sub := fstest.MapFS{}
	for _, m := range list {
		if m.version > version {
			continue
		}
		body, err := fs.ReadFile(migrations.FS, m.name)
		if err != nil {
			t.Fatal(err)
		}
		sub[m.name] = &fstest.MapFile{Data: body}
	}
	if err := migrate(t.Context(), db, sub); err != nil {
		t.Fatalf("migrate to %d: %v", version, err)
	}
}

func TestMigrate_FromV1MergesUnicodeCaseVariantNamesAndKeepsEverything(t *testing.T) {
	db := rawDB(t)
	migrateTo(t, db, 1)
	// Data the ASCII-only NOCASE rules let in.
	for _, q := range []string{
		`INSERT INTO folders (id, name, position) VALUES
			(1, 'Économie', 0), (2, 'économie', 1), (3, 'Città', 2), (4, 'CITTÀ', 3)`,
		`INSERT INTO feeds (id, kind, url, title, folder_id, created_at) VALUES
			(1, 'rss', 'https://a', 'A', 2, 0), (2, 'rss', 'https://b', 'B', 4, 0),
			(3, 'rss', 'https://c', 'C', 1, 0), (4, 'rss', 'https://d', 'D', NULL, 0)`,
		`INSERT INTO entries (id, feed_id, guid, title, published_at, fetched_at, is_read, is_favourite) VALUES
			(1, 1, 'e1', 'E1', 10, 10, 1, 0), (2, 1, 'e2', 'E2', 20, 20, 0, 1), (5, 2, 'e5', 'E5', 30, 30, 0, 0)`,
		`INSERT INTO tags (id, name) VALUES (1, 'Città'), (2, 'go'), (3, 'CITTÀ'), (4, 'über'), (5, 'ÜBER')`,
		`INSERT INTO entry_tags (entry_id, tag_id) VALUES
			(1, 1), (1, 3), (2, 3), (5, 2), (1, 2), (5, 5), (2, 4)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := migrate(t.Context(), db, migrations.FS); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	s := &SQLite{db: db, now: func() time.Time { return t0 }}
	ctx := t.Context()

	folders, err := s.ListFolders(ctx)
	mustNoErr(t, err)
	if want := []Folder{{1, "Économie", 0}, {3, "Città", 2}}; !slices.Equal(folders, want) {
		t.Errorf("folders = %+v, want %+v (oldest spelling kept)", folders, want)
	}
	for feedID, folderID := range map[int64]int64{1: 1, 2: 3, 3: 1, 4: 0} {
		f, err := s.GetFeed(ctx, feedID)
		mustNoErr(t, err)
		if f.FolderID != folderID {
			t.Errorf("feed %d folder = %d, want %d", feedID, f.FolderID, folderID)
		}
	}
	tags, err := s.ListTags(ctx)
	mustNoErr(t, err)
	if want := []Tag{{1, "Città", 2}, {2, "go", 2}, {4, "über", 2}}; !slices.Equal(tags, want) {
		t.Errorf("tags = %+v, want %+v", tags, want)
	}
	var entries, entryTags int
	mustNoErr(t, db.QueryRow(`SELECT (SELECT count(*) FROM entries), (SELECT count(*) FROM entry_tags)`).Scan(&entries, &entryTags))
	if entries != 3 || entryTags != 6 {
		t.Errorf("entries = %d, entry_tags = %d; want 3 and 6 (nothing cascaded away)", entries, entryTags)
	}
	e2, err := s.GetEntry(ctx, 2)
	mustNoErr(t, err)
	if e2.IsRead || !e2.IsFavourite || e2.FeedTitle != "A" || !slices.Equal(tagNames(e2.Tags), []string{"Città", "über"}) {
		t.Errorf("entry 2 after migrating = %+v", e2)
	}

	// The new rules hold for the migrated rows.
	if _, err := s.CreateFolder(ctx, "ÉCONOMIE"); !errors.Is(err, ErrConflict) {
		t.Errorf("CreateFolder(ÉCONOMIE) err = %v, want ErrConflict", err)
	}
	if tag, err := s.AddEntryTag(ctx, 5, "CITTÀ"); err != nil || tag.ID != 1 {
		t.Errorf("AddEntryTag(CITTÀ) = %+v, %v; want tag 1", tag, err)
	}
	// Ids of deleted entries are not handed out again.
	mustNoErr(t, s.DeleteFeed(ctx, 2))
	_, err = s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: 1, FetchedAt: t0, Entries: []NewEntry{entryAt("new", t0)}})
	mustNoErr(t, err)
	if id := entryID(t, s, 1, "new"); id <= 5 {
		t.Errorf("new entry id = %d, want > 5 (the deleted maximum)", id)
	}
	foreignKeysOnEveryConn(t, db, 4)
}
