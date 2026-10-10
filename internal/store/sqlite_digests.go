package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// digestCols is the column list scanDigest expects, in order.
const digestCols = `id, name, ingest_minute, position, created_at`

func scanDigest(r rowScanner) (Digest, error) {
	var (
		d       Digest
		minute  sql.NullInt64
		created int64
	)
	if err := r.Scan(&d.ID, &d.Name, &minute, &d.Position, &created); err != nil {
		return Digest{}, err
	}
	d.HasIngest = minute.Valid
	d.IngestMinute = int(minute.Int64)
	d.CreatedAt = timeFromUnix(created)
	return d, nil
}

// digestOrder is the order of the digests in the sidebar: by position, then
// name (case-insensitive).
const digestOrder = ` ORDER BY position, name_key, id`

// ListDigests returns all digests ordered by position, then name
// (case-insensitive).
func (s *SQLite) ListDigests(ctx context.Context) ([]Digest, error) {
	out, err := queryDigests(ctx, s.db, `SELECT `+digestCols+` FROM digests`+digestOrder)
	if err != nil {
		return nil, fmt.Errorf("store: list digests: %w", err)
	}
	return out, nil
}

// DigestsNamingFolder returns the digests that name the folder, ordered as
// ListDigests: those that deleting it removes it from.
func (s *SQLite) DigestsNamingFolder(ctx context.Context, id int64) ([]Digest, error) {
	out, err := queryDigests(ctx, s.db, `SELECT `+digestCols+` FROM digests
		WHERE id IN (SELECT digest_id FROM digest_folders WHERE folder_id = ?)`+digestOrder, id)
	if err != nil {
		return nil, fmt.Errorf("store: digests of folder %d: %w", id, err)
	}
	return out, nil
}

// DigestsNamingTag returns the digests that name the tag, ordered as
// ListDigests: those that deleting it removes it from.
func (s *SQLite) DigestsNamingTag(ctx context.Context, id int64) ([]Digest, error) {
	out, err := queryDigests(ctx, s.db, `SELECT `+digestCols+` FROM digests
		WHERE id IN (SELECT digest_id FROM digest_tags WHERE tag_id = ?)`+digestOrder, id)
	if err != nil {
		return nil, fmt.Errorf("store: digests of tag %d: %w", id, err)
	}
	return out, nil
}

// queryDigests runs a query selecting digestCols.
func queryDigests(ctx context.Context, q queryer, query string, args ...any) ([]Digest, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Digest
	for rows.Next() {
		d, err := scanDigest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDigest returns one digest.
func (s *SQLite) GetDigest(ctx context.Context, id int64) (Digest, error) {
	d, err := scanDigest(s.db.QueryRowContext(ctx, `SELECT `+digestCols+` FROM digests WHERE id = ?`, id))
	if err != nil {
		return Digest{}, fmt.Errorf("store: get digest %d: %w", id, classify(err))
	}
	return d, nil
}

// CreateDigest appends a digest after the existing ones, without sources or
// ingestion time. The name is trimmed; it returns ErrConflict if the name is
// taken (case-insensitive, see foldName) and ErrInvalid if it is empty.
func (s *SQLite) CreateDigest(ctx context.Context, name string) (Digest, error) {
	name, err := cleanName("digest", name)
	if err != nil {
		return Digest{}, fmt.Errorf("store: create digest: %w", err)
	}
	d, err := scanDigest(s.db.QueryRowContext(ctx,
		`INSERT INTO digests (name, name_key, position, created_at)
		 VALUES (?, ?, COALESCE((SELECT MAX(position) FROM digests), -1) + 1, ?)
		 RETURNING `+digestCols, name, foldName(name), s.now().Unix()))
	if err != nil {
		return Digest{}, fmt.Errorf("store: create digest %q: %w", name, classify(err))
	}
	return d, nil
}

// RenameDigest renames a digest. The name is trimmed; it returns ErrConflict
// if the name is taken (case-insensitive, see foldName), ErrNotFound if the
// digest does not exist and ErrInvalid if the name is empty. Changing only
// the case of the current name is allowed.
func (s *SQLite) RenameDigest(ctx context.Context, id int64, name string) error {
	name, err := cleanName("digest", name)
	if err != nil {
		return fmt.Errorf("store: rename digest %d: %w", id, err)
	}
	if err := execOne(ctx, s.db, "digest", id,
		`UPDATE digests SET name = ?, name_key = ? WHERE id = ?`, name, foldName(name), id); err != nil {
		return fmt.Errorf("store: rename digest %d to %q: %w", id, name, err)
	}
	return nil
}

// SetDigestIngest sets (on) or clears (!on) the digest's daily ingestion
// time, a minute of the day from 0 to MinutesPerDay-1.
func (s *SQLite) SetDigestIngest(ctx context.Context, id int64, minute int, on bool) error {
	var v any
	if on {
		if minute < 0 || minute >= MinutesPerDay {
			return fmt.Errorf("store: set ingestion time of digest %d: minute %d: %w", id, minute, ErrInvalid)
		}
		v = minute
	}
	if err := execOne(ctx, s.db, "digest", id, `UPDATE digests SET ingest_minute = ? WHERE id = ?`, v, id); err != nil {
		return fmt.Errorf("store: set ingestion time of digest: %w", err)
	}
	return nil
}

// DeleteDigest removes the digest; its memberships go with it (ON DELETE
// CASCADE). Feeds, folders, tags and entries are kept.
func (s *SQLite) DeleteDigest(ctx context.Context, id int64) error {
	if err := execOne(ctx, s.db, "digest", id, `DELETE FROM digests WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete digest: %w", err)
	}
	return nil
}

// digestMembers holds the statements that read, clear and add the
// memberships of each kind of digest source, in the order of
// DigestSources' fields (see lists).
var digestMembers = [...]struct{ what, list, clear, insert string }{
	{"folder",
		`SELECT folder_id FROM digest_folders WHERE digest_id = ? ORDER BY folder_id`,
		`DELETE FROM digest_folders WHERE digest_id = ?`,
		`INSERT INTO digest_folders (digest_id, folder_id) VALUES (?, ?)`},
	{"feed",
		`SELECT feed_id FROM digest_feeds WHERE digest_id = ? ORDER BY feed_id`,
		`DELETE FROM digest_feeds WHERE digest_id = ?`,
		`INSERT INTO digest_feeds (digest_id, feed_id) VALUES (?, ?)`},
	{"tag",
		`SELECT tag_id FROM digest_tags WHERE digest_id = ? ORDER BY tag_id`,
		`DELETE FROM digest_tags WHERE digest_id = ?`,
		`INSERT INTO digest_tags (digest_id, tag_id) VALUES (?, ?)`},
}

// lists returns the three id lists of src in digestMembers order.
func (src *DigestSources) lists() [len(digestMembers)]*[]int64 {
	return [...]*[]int64{&src.FolderIDs, &src.FeedIDs, &src.TagIDs}
}

// DigestSources returns the folders, feeds and tags the digest names, each
// sorted by id. It returns ErrNotFound if the digest does not exist.
func (s *SQLite) DigestSources(ctx context.Context, id int64) (DigestSources, error) {
	var src DigestSources
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		if err := digestExists(ctx, tx, id); err != nil {
			return err
		}
		lists := src.lists()
		for i, m := range digestMembers {
			got, err := queryIDs(ctx, tx, m.list, id)
			if err != nil {
				return fmt.Errorf("%s ids: %w", m.what, err)
			}
			*lists[i] = got
		}
		return nil
	})
	if err != nil {
		return DigestSources{}, fmt.Errorf("store: sources of digest %d: %w", id, err)
	}
	return src, nil
}

// SourcesFingerprint identifies the sources a digest settings form offers:
// the folders, feeds and tags given, each by its kind, its id and an
// identity that another source given the same id later would not share (a
// folder's or tag's case-folded name, a feed's URL). The order of the lists,
// and anything else about the sources (titles, counts), does not matter.
//
// The form posts it back with the ids picked, and SetDigestSources refuses
// the save when the sources it would offer now have another fingerprint:
// feed, folder and tag ids are reused once the highest one is deleted, so a
// form left open while a source is deleted and another one created would
// otherwise attach the new source in place of the one it showed.
func SourcesFingerprint(folders []Folder, feeds []Feed, tags []Tag) string {
	items := make([]string, 0, len(folders)+len(feeds)+len(tags))
	add := func(kind string, id int64, identity string) {
		items = append(items, kind+" "+strconv.FormatInt(id, 10)+" "+strconv.Quote(identity))
	}
	for _, f := range folders {
		add("folder", f.ID, foldName(f.Name))
	}
	for _, f := range feeds {
		add("feed", f.ID, f.URL)
	}
	for _, t := range tags {
		add("tag", t.ID, foldName(t.Name))
	}
	slices.Sort(items)
	sum := sha256.Sum256([]byte(strings.Join(items, "\n")))
	return hex.EncodeToString(sum[:])
}

// currentSourcesFingerprint is the SourcesFingerprint of every folder, feed
// and tag in the database, as a settings form rendered now offers them.
func currentSourcesFingerprint(ctx context.Context, q queryer) (string, error) {
	var (
		folders []Folder
		feeds   []Feed
		tags    []Tag
	)
	for _, list := range []struct {
		query string
		add   func(id int64, identity string)
	}{
		{`SELECT id, name FROM folders`, func(id int64, name string) { folders = append(folders, Folder{ID: id, Name: name}) }},
		{`SELECT id, url FROM feeds`, func(id int64, url string) { feeds = append(feeds, Feed{ID: id, URL: url}) }},
		{`SELECT id, name FROM tags`, func(id int64, name string) { tags = append(tags, Tag{ID: id, Name: name}) }},
	} {
		if err := scanIdentities(ctx, q, list.query, list.add); err != nil {
			return "", err
		}
	}
	return SourcesFingerprint(folders, feeds, tags), nil
}

// scanIdentities runs query, which selects an id and a text column, and
// passes each row to add.
func scanIdentities(ctx context.Context, q queryer, query string, add func(id int64, identity string)) error {
	rows, err := q.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var (
			id       int64
			identity string
		)
		if err := rows.Scan(&id, &identity); err != nil {
			return err
		}
		add(id, identity)
	}
	return rows.Err()
}

// SetDigestSources replaces the digest's folders, feeds and tags in one
// transaction. Duplicate ids collapse. offered is the SourcesFingerprint of
// the sources the settings form offered: when the folders, feeds and tags
// have changed since, it returns ErrConflict. It returns ErrInvalid for a
// non-positive id or more than MaxDigestSources sources, and ErrNotFound when
// the digest or one of the sources does not exist. Nothing is changed on
// any error.
func (s *SQLite) SetDigestSources(ctx context.Context, id int64, src DigestSources, offered string) error {
	var clean DigestSources
	total := 0
	in, out := src.lists(), clean.lists()
	for i, m := range digestMembers {
		list := slices.Clone(*in[i])
		slices.Sort(list)
		list = slices.Compact(list)
		if len(list) > 0 && list[0] <= 0 {
			return fmt.Errorf("store: set sources of digest %d: %s id %d: %w", id, m.what, list[0], ErrInvalid)
		}
		*out[i] = list
		total += len(list)
	}
	if total > MaxDigestSources {
		return fmt.Errorf("store: set sources of digest %d: %d sources, at most %d: %w",
			id, total, MaxDigestSources, ErrInvalid)
	}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := digestExists(ctx, tx, id); err != nil {
			return err
		}
		// In the write transaction, so no source can be deleted and its id
		// reused between this check and the inserts.
		current, err := currentSourcesFingerprint(ctx, tx)
		if err != nil {
			return fmt.Errorf("sources offered: %w", err)
		}
		if current != offered {
			return fmt.Errorf("the folders, feeds or tags offered have changed: %w", ErrConflict)
		}
		for i, m := range digestMembers {
			if _, err := tx.ExecContext(ctx, m.clear, id); err != nil {
				return fmt.Errorf("clear %ss: %w", m.what, err)
			}
			if err := insertMembers(ctx, tx, m.insert, id, *out[i]); err != nil {
				return fmt.Errorf("%s: %w", m.what, err)
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: set sources of digest %d: %w", id, err)
	}
	return nil
}

// insertMembers adds one membership row per id with insert, a statement of
// digestMembers. A missing source fails the foreign key, which classify
// reports as ErrNotFound.
func insertMembers(ctx context.Context, tx *sql.Tx, insert string, digestID int64, list []int64) error {
	if len(list) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, insert)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()
	for _, member := range list {
		if _, err := stmt.ExecContext(ctx, digestID, member); err != nil {
			return fmt.Errorf("add %d: %w", member, classify(err))
		}
	}
	return nil
}

// DigestFeedIDs returns, sorted, the feeds the digest names and the feeds in
// the folders it names: those its ingestion time fetches. It returns
// ErrNotFound if the digest does not exist.
func (s *SQLite) DigestFeedIDs(ctx context.Context, id int64) ([]int64, error) {
	var out []int64
	err := s.inReadTx(ctx, func(tx *sql.Tx) error {
		if err := digestExists(ctx, tx, id); err != nil {
			return err
		}
		var err error
		out, err = queryIDs(ctx, tx,
			`SELECT feed_id FROM digest_feeds WHERE digest_id = ?1
			 UNION
			 SELECT f.id FROM feeds f JOIN digest_folders dfo ON dfo.folder_id = f.folder_id
			 WHERE dfo.digest_id = ?1
			 ORDER BY 1`, id)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: feeds of digest %d: %w", id, err)
	}
	return out, nil
}

// IngestMinutes returns, sorted and distinct, the ingestion times of the
// digests that name feedID or folderID (0 matches nothing).
func (s *SQLite) IngestMinutes(ctx context.Context, feedID, folderID int64) ([]int, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT ingest_minute FROM digests
		 WHERE ingest_minute IS NOT NULL
		   AND (id IN (SELECT digest_id FROM digest_feeds WHERE feed_id = ?)
		        OR id IN (SELECT digest_id FROM digest_folders WHERE folder_id = ?))
		 ORDER BY ingest_minute`, feedID, folderID)
	if err != nil {
		return nil, fmt.Errorf("store: ingestion times of feed %d: %w", feedID, err)
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var m int
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("store: ingestion times of feed %d: %w", feedID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: ingestion times of feed %d: %w", feedID, err)
	}
	return out, nil
}

// digestExists returns ErrNotFound unless the digest exists.
func digestExists(ctx context.Context, q queryer, id int64) error {
	var one int
	if err := q.QueryRowContext(ctx, `SELECT 1 FROM digests WHERE id = ?`, id).Scan(&one); err != nil {
		return fmt.Errorf("digest %d: %w", id, classify(err))
	}
	return nil
}

// queryIDs collects a single integer column.
func queryIDs(ctx context.Context, q queryer, query string, args ...any) ([]int64, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// digestFilter selects the entries of the digest whose id is bound to the
// three placeholders: the entries of its feeds, of the feeds in its folders,
// and those carrying its tags. Each entry matches once however many of these
// it belongs to. Like every scope filter it refers to the entry as "e".
// MarkScopeReadUpTo uses it, visiting the unread entries much as for a
// folder; the grid does not (see digestListSQL).
const digestFilter = `(e.feed_id IN (SELECT feed_id FROM digest_feeds WHERE digest_id = ?)
	OR e.feed_id IN (SELECT dfe.id FROM feeds dfe JOIN digest_folders dfo ON dfo.folder_id = dfe.folder_id
		WHERE dfo.digest_id = ?)
	OR e.id IN (SELECT et.entry_id FROM entry_tags et JOIN digest_tags dt ON dt.tag_id = et.tag_id
		WHERE dt.digest_id = ?))`

// digestListSQL builds the grid query of the digest scope q, fetching
// Limit+1 rows like listQuerySQL.
//
// A filter over the whole library such as digestFilter lets the planner walk
// every entry in grid order until it has found a page of the digest's, which
// it prefers once the database has statistics (PRAGMA optimize writes them
// on Close): a digest of a few old entries, or none, then reads the whole
// library on every page. So the query collects a bounded set of candidates
// and sorts only those:
//
//   - from each feed of the digest (named, or in a folder it names), its
//     newest Limit+1 entries that pass the Unread filter and the cursor,
//     read in order from the feed's index (entries_unread or
//     entries_feed_pub);
//   - every entry carrying one of the digest's tags that passes them.
//
// The page is among them: an entry of the page has at most Limit entries of
// its own feed before it. The outer query only looks the candidates up by
// rowid, each once however many sources reach it (IN), and the "+" keeps
// the planner from walking entries_pub for the order instead. The cost grows
// with the digest's feeds times the page size, plus its tagged entries (as
// the tag scope reads them), not with the library.
func digestListSQL(q ListQuery) (string, []any) {
	var feedCond, tagCond string
	var after []any
	if q.UnreadOnly && !q.Scope.IgnoresReadFilter() {
		feedCond += " AND x.is_read = 0"
		tagCond += " AND t.is_read = 0"
	}
	if q.After != nil {
		feedCond += " AND (x.published_at, x.id) < (?, ?)"
		tagCond += " AND (t.published_at, t.id) < (?, ?)"
		after = []any{q.After.PublishedAt.Unix(), q.After.ID}
	}
	query := entryViewSelect + `
	WHERE e.id IN (
		SELECT c.id FROM (
			SELECT feed_id AS fid FROM digest_feeds WHERE digest_id = ?
			UNION
			SELECT dfe.id FROM feeds dfe JOIN digest_folders dfo ON dfo.folder_id = dfe.folder_id
			WHERE dfo.digest_id = ?
		) AS d JOIN entries c ON c.id IN (
			SELECT x.id FROM entries x WHERE x.feed_id = d.fid` + feedCond + `
			ORDER BY x.published_at DESC, x.id DESC LIMIT ?)
		UNION ALL
		SELECT t.id FROM digest_tags dt CROSS JOIN entry_tags et ON et.tag_id = dt.tag_id
			CROSS JOIN entries t ON t.id = et.entry_id
		WHERE dt.digest_id = ?` + tagCond + `
	)
	ORDER BY +e.published_at DESC, +e.id DESC LIMIT ?`
	id, n := q.Scope.ID, q.Limit+1
	args := append([]any{id, id}, after...)
	args = append(args, n, id)
	args = append(args, after...)
	return query, append(args, n)
}

// digestUnreadSQL selects one row (digest id, unread entry id) per digest and
// unread entry in it, for every digest at once: the membership of
// digestFilter. UNION keeps one row per digest and entry, so an entry in
// several of a digest's sources counts once.
//
// Each arm is driven by its membership table (CROSS JOIN fixes the join
// order), so it only looks up the unread entries of the feeds, folders and
// tags that digests name: from the covering partial index entries_unread,
// the folders' feeds and the tags' entry_tags rows. Left to choose, the
// planner may instead walk every unread entry, or every entry tag, and look
// each up in the memberships, which it prefers when they are empty or have
// no statistics; that would read the whole library after every action.
const digestUnreadSQL = `SELECT dfe.digest_id, e.id FROM digest_feeds dfe
		CROSS JOIN entries e ON e.feed_id = dfe.feed_id AND e.is_read = 0
	UNION
	SELECT dfo.digest_id, e.id FROM digest_folders dfo
		CROSS JOIN feeds ff ON ff.folder_id = dfo.folder_id
		CROSS JOIN entries e ON e.feed_id = ff.id AND e.is_read = 0
	UNION
	SELECT dt.digest_id, e.id FROM digest_tags dt
		CROSS JOIN entry_tags et ON et.tag_id = dt.tag_id
		CROSS JOIN entries e ON e.id = et.entry_id AND e.is_read = 0`

// inReadTx runs fn, which only reads, in a deferred transaction, so that
// its queries see one snapshot without taking the write lock (the driver
// begins a ReadOnly transaction with a plain BEGIN, not the DSN's BEGIN
// IMMEDIATE).
func (s *SQLite) inReadTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	return fn(tx)
}
