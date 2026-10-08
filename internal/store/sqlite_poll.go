package store

import (
	"cmp"
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// insertEntrySQL inserts one entry unless (feed_id, guid) already exists, in
// which case nothing is written and RowsAffected is 0.
const insertEntrySQL = `INSERT INTO entries (feed_id, guid, url, title, summary_html, author,
		thumbnail_url, published_at, updated_at, fetched_at, is_read, read_at)
	VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT (feed_id, guid) DO NOTHING`

// refreshEntrySQL refreshes the fields a re-fetch may change and nothing
// else: is_read, is_later, is_favourite, read_at, published_at, fetched_at
// and author are never touched. Rows whose content is unchanged are skipped
// so that polling a large, static feed writes nothing.
const refreshEntrySQL = `UPDATE entries
	SET title = ?1, summary_html = ?2, url = ?3, thumbnail_url = ?4, updated_at = ?5
	WHERE feed_id = ?6 AND guid = ?7
	  AND (title IS NOT ?1 OR summary_html IS NOT ?2 OR url IS NOT ?3
	       OR thumbnail_url IS NOT ?4 OR updated_at IS NOT ?5)`

// entryInsertArgs returns the arguments of insertEntrySQL.
func entryInsertArgs(feedID int64, e NewEntry, fetchedAt time.Time, read bool, readAt any) []any {
	return []any{
		feedID, e.GUID, nullStr(e.URL), e.Title, nullStr(e.SummaryHTML), nullStr(e.Author),
		nullStr(e.ThumbnailURL), e.PublishedAt.Unix(), nullUnix(e.UpdatedAt), fetchedAt.Unix(),
		boolInt(read), readAt,
	}
}

// prepareEntries validates and normalizes entries before storage: every
// entry needs a GUID; a zero PublishedAt falls back to UpdatedAt, then to
// fetchedAt; entries sharing a GUID collapse into one, keeping the first
// one's position and the last one's content.
func prepareEntries(entries []NewEntry, fetchedAt time.Time) ([]NewEntry, error) {
	out := make([]NewEntry, 0, len(entries))
	pos := make(map[string]int, len(entries))
	for i, e := range entries {
		if e.GUID == "" {
			return nil, fmt.Errorf("entry %d has an empty GUID: %w", i, ErrInvalid)
		}
		if e.PublishedAt.IsZero() {
			e.PublishedAt = e.UpdatedAt
			if e.PublishedAt.IsZero() {
				e.PublishedAt = fetchedAt
			}
		}
		if j, dup := pos[e.GUID]; dup {
			out[j] = e
			continue
		}
		pos[e.GUID] = len(out)
		out = append(out, e)
	}
	return out, nil
}

// newestN returns the indices of the n entries with the newest PublishedAt
// (compared at the stored one-second precision); among equal times the
// earlier index counts as newer.
func newestN(list []NewEntry, n int) map[int]bool {
	if n <= 0 || len(list) == 0 {
		return nil
	}
	order := make([]int, len(list))
	for i := range order {
		order[i] = i
	}
	slices.SortStableFunc(order, func(a, b int) int {
		return cmp.Compare(list[b].PublishedAt.Unix(), list[a].PublishedAt.Unix())
	})
	order = order[:min(n, len(order))]
	set := make(map[int]bool, len(order))
	for _, i := range order {
		set[i] = true
	}
	return set
}

// upsertEntries inserts new (feed_id, guid) pairs as unread and refreshes
// existing ones, returning how many rows were inserted. Entries are processed
// from last to first so that, among equal published_at values, the earlier
// slice position gets the higher id and sorts first in the grid.
func upsertEntries(ctx context.Context, tx *sql.Tx, feedID int64, list []NewEntry, fetchedAt time.Time) (int, error) {
	ins, err := tx.PrepareContext(ctx, insertEntrySQL)
	if err != nil {
		return 0, fmt.Errorf("prepare entry insert: %w", err)
	}
	defer ins.Close()
	upd, err := tx.PrepareContext(ctx, refreshEntrySQL)
	if err != nil {
		return 0, fmt.Errorf("prepare entry refresh: %w", err)
	}
	defer upd.Close()

	inserted := 0
	for i := len(list) - 1; i >= 0; i-- {
		e := list[i]
		res, err := ins.ExecContext(ctx, entryInsertArgs(feedID, e, fetchedAt, false, nil)...)
		if err != nil {
			return 0, fmt.Errorf("insert entry %q: %w", e.GUID, classify(err))
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("insert entry %q: %w", e.GUID, err)
		}
		if n > 0 {
			inserted++
			continue
		}
		if _, err := upd.ExecContext(ctx, e.Title, nullStr(e.SummaryHTML), nullStr(e.URL),
			nullStr(e.ThumbnailURL), nullUnix(e.UpdatedAt), feedID, e.GUID); err != nil {
			return 0, fmt.Errorf("refresh entry %q: %w", e.GUID, err)
		}
	}
	return inserted, nil
}

// DueFeeds returns feeds with next_fetch_at <= now, ordered by next_fetch_at.
func (s *SQLite) DueFeeds(ctx context.Context, now time.Time) ([]Feed, error) {
	feeds, err := queryFeeds(ctx, s.db,
		`SELECT `+feedCols+` FROM feeds WHERE next_fetch_at <= ? ORDER BY next_fetch_at, id`,
		now.Unix())
	if err != nil {
		return nil, fmt.Errorf("store: due feeds: %w", err)
	}
	return feeds, nil
}

// NextFetchAfter returns the earliest next_fetch_at strictly after t, and
// false if there is none.
func (s *SQLite) NextFetchAfter(ctx context.Context, t time.Time) (time.Time, bool, error) {
	var next sql.NullInt64
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(next_fetch_at) FROM feeds WHERE next_fetch_at > ?`, t.Unix()).Scan(&next)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store: next fetch after %s: %w", t, err)
	}
	if !next.Valid {
		return time.Time{}, false, nil
	}
	return timeFromUnixOrZero(next.Int64), true, nil
}

// SetNextFetch schedules one feed.
func (s *SQLite) SetNextFetch(ctx context.Context, id int64, at time.Time) error {
	if err := execOne(ctx, s.db, "feed", id,
		`UPDATE feeds SET next_fetch_at = ? WHERE id = ?`, unixOrZero(at), id); err != nil {
		return fmt.Errorf("store: set next fetch: %w", err)
	}
	return nil
}

// SetAllNextFetch schedules every feed at the same time.
func (s *SQLite) SetAllNextFetch(ctx context.Context, at time.Time) error {
	if _, err := s.db.ExecContext(ctx, `UPDATE feeds SET next_fetch_at = ?`, unixOrZero(at)); err != nil {
		return fmt.Errorf("store: set next fetch of all feeds: %w", err)
	}
	return nil
}

// RecordFetchSuccess upserts entries and updates fetch state in one
// transaction: error_count = 0, last_error = NULL, last_fetched_at,
// next_fetch_at, etag, last_modified, ttl_sec, and non-empty metadata fields. Entries
// whose (feed_id, guid) is new are inserted unread with fetched_at =
// r.FetchedAt. Existing ones get title, summary_html, url, thumbnail_url and
// updated_at refreshed; is_read, is_later, is_favourite, read_at,
// published_at and fetched_at are never touched. Duplicate GUIDs in
// r.Entries collapse (last one wins). Returns the number of inserted entries,
// or ErrNotFound if the feed no longer exists (nothing is written then).
func (s *SQLite) RecordFetchSuccess(ctx context.Context, r FetchSuccess) (int, error) {
	fetchedAt := r.FetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = s.now()
	}
	list, err := prepareEntries(r.Entries, fetchedAt)
	if err != nil {
		return 0, fmt.Errorf("store: record fetch of feed %d: %w", r.FeedID, err)
	}
	var inserted int
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		err := execOne(ctx, tx, "feed", r.FeedID,
			`UPDATE feeds SET error_count = 0, last_error = NULL, last_fetched_at = ?,
				next_fetch_at = ?, etag = ?, last_modified = ?, ttl_sec = ?,
				site_url = COALESCE(?, site_url), icon_url = COALESCE(?, icon_url),
				original_title = COALESCE(?, original_title)
			 WHERE id = ?`,
			fetchedAt.Unix(), unixOrZero(r.NextFetchAt), nullStr(r.ETag), nullStr(r.LastModified),
			max(r.TTLSec, 0), nullStr(r.SiteURL), nullStr(r.IconURL), nullStr(r.OriginalTitle), r.FeedID)
		if err != nil {
			return err
		}
		inserted, err = upsertEntries(ctx, tx, r.FeedID, list, fetchedAt)
		return err
	})
	if err != nil {
		return 0, fmt.Errorf("store: record fetch of feed %d: %w", r.FeedID, err)
	}
	return inserted, nil
}

// RecordNotModified handles a 304: last_fetched_at, next_fetch_at,
// error_count = 0, last_error = NULL.
func (s *SQLite) RecordNotModified(ctx context.Context, id int64, fetchedAt, next time.Time) error {
	err := execOne(ctx, s.db, "feed", id,
		`UPDATE feeds SET last_fetched_at = ?, next_fetch_at = ?, error_count = 0, last_error = NULL
		 WHERE id = ?`, nullUnix(fetchedAt), unixOrZero(next), id)
	if err != nil {
		return fmt.Errorf("store: record not modified: %w", err)
	}
	return nil
}

// RecordFetchError increments error_count and sets last_error,
// last_fetched_at and next_fetch_at. Entries are left untouched.
func (s *SQLite) RecordFetchError(ctx context.Context, id int64, msg string, fetchedAt, next time.Time) error {
	err := execOne(ctx, s.db, "feed", id,
		`UPDATE feeds SET error_count = error_count + 1, last_error = ?, last_fetched_at = ?,
			next_fetch_at = ?
		 WHERE id = ?`, nullStr(msg), nullUnix(fetchedAt), unixOrZero(next), id)
	if err != nil {
		return fmt.Errorf("store: record fetch error: %w", err)
	}
	return nil
}
