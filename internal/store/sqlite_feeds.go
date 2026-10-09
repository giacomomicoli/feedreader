package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/safeurl"
)

// redactURL hides the credentials of a feed URL, its whole userinfo
// (https://xxxxx@host/…), in error messages, which end up in logs; feed URLs
// may embed a user name and password, or a token as the user name. See
// safeurl.
func redactURL(raw string) string {
	return safeurl.String(raw)
}

// feedCols is the column list scanFeed expects, in order.
const feedCols = `id, kind, url, site_url, title, original_title, icon_url, folder_id,
	etag, last_modified, last_fetched_at, next_fetch_at, interval_sec, error_count,
	last_error, created_at, ttl_sec`

// scanFeed reads one row selected with feedCols.
func scanFeed(r rowScanner) (Feed, error) {
	var (
		f                                                  Feed
		kind                                               string
		siteURL, origTitle, iconURL, etag, lastMod, lastEr sql.NullString
		folderID, lastFetched, interval                    sql.NullInt64
		next, created                                      int64
	)
	err := r.Scan(&f.ID, &kind, &f.URL, &siteURL, &f.Title, &origTitle, &iconURL, &folderID,
		&etag, &lastMod, &lastFetched, &next, &interval, &f.ErrorCount, &lastEr, &created, &f.TTLSec)
	if err != nil {
		return Feed{}, err
	}
	f.Kind = Kind(kind)
	f.SiteURL = siteURL.String
	f.OriginalTitle = origTitle.String
	f.IconURL = iconURL.String
	f.FolderID = folderID.Int64
	f.ETag = etag.String
	f.LastModified = lastMod.String
	f.LastFetchedAt = timeFromNullUnix(lastFetched)
	f.NextFetchAt = timeFromUnixOrZero(next)
	f.IntervalSec = int(interval.Int64)
	f.LastError = lastEr.String
	f.CreatedAt = timeFromUnix(created)
	return f, nil
}

// queryFeeds runs a query selecting feedCols and collects the rows.
func queryFeeds(ctx context.Context, q queryer, query string, args ...any) ([]Feed, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Feed
	for rows.Next() {
		f, err := scanFeed(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func getFeed(ctx context.Context, q queryer, id int64) (Feed, error) {
	f, err := scanFeed(q.QueryRowContext(ctx, `SELECT `+feedCols+` FROM feeds WHERE id = ?`, id))
	if err != nil {
		return Feed{}, classify(err)
	}
	return f, nil
}

// ListFeeds returns all feeds ordered by title (case-insensitive).
func (s *SQLite) ListFeeds(ctx context.Context) ([]Feed, error) {
	feeds, err := queryFeeds(ctx, s.db,
		`SELECT `+feedCols+` FROM feeds ORDER BY title COLLATE NOCASE, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list feeds: %w", err)
	}
	return feeds, nil
}

// GetFeed returns one feed.
func (s *SQLite) GetFeed(ctx context.Context, id int64) (Feed, error) {
	f, err := getFeed(ctx, s.db, id)
	if err != nil {
		return Feed{}, fmt.Errorf("store: get feed %d: %w", id, err)
	}
	return f, nil
}

// GetFeedByURL returns the feed subscribed at url (exact match).
func (s *SQLite) GetFeedByURL(ctx context.Context, url string) (Feed, error) {
	f, err := scanFeed(s.db.QueryRowContext(ctx, `SELECT `+feedCols+` FROM feeds WHERE url = ?`, url))
	if err != nil {
		return Feed{}, fmt.Errorf("store: get feed by url %s: %w", redactURL(url), classify(err))
	}
	return f, nil
}

// CreateFeed atomically inserts the feed and all its entries. The
// initialUnread entries with the newest PublishedAt are unread (ties: the
// earlier position in entries counts as newer); all other entries are stored
// read with read_at = f.FetchedAt. Entries sharing a GUID are collapsed (last
// one wins). Returns ErrConflict if the URL exists, ErrNotFound if FolderID
// does not exist and ErrInvalid for an unknown kind, an empty URL or an entry
// without a GUID. An empty Title falls back to OriginalTitle, then to URL
// (without its credentials).
func (s *SQLite) CreateFeed(ctx context.Context, f NewFeed, entries []NewEntry, initialUnread int) (Feed, error) {
	if err := validateNewFeed(f); err != nil {
		return Feed{}, fmt.Errorf("store: create feed: %w", err)
	}
	fetchedAt := f.FetchedAt
	if fetchedAt.IsZero() {
		fetchedAt = s.now()
	}
	list, err := prepareEntries(entries, fetchedAt)
	if err != nil {
		return Feed{}, fmt.Errorf("store: create feed %s: %w", redactURL(f.URL), err)
	}
	unread := newestN(list, initialUnread)

	var out Feed
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var id int64
		err := tx.QueryRowContext(ctx,
			`INSERT INTO feeds (kind, url, site_url, title, original_title, icon_url, folder_id,
				etag, last_modified, last_fetched_at, next_fetch_at, created_at, ttl_sec)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			 RETURNING id`,
			string(f.Kind), f.URL, nullStr(f.SiteURL), feedTitle(f), nullStr(f.OriginalTitle),
			nullStr(f.IconURL), nullID(f.FolderID), nullStr(f.ETag), nullStr(f.LastModified),
			fetchedAt.Unix(), unixOrZero(f.NextFetchAt), s.now().Unix(), max(f.TTLSec, 0),
		).Scan(&id)
		if err != nil {
			return classify(err)
		}
		if err := insertInitialEntries(ctx, tx, id, list, unread, fetchedAt); err != nil {
			return err
		}
		out, err = getFeed(ctx, tx, id)
		return err
	})
	if err != nil {
		return Feed{}, fmt.Errorf("store: create feed %s: %w", redactURL(f.URL), err)
	}
	return out, nil
}

func validateNewFeed(f NewFeed) error {
	if f.Kind != KindRSS && f.Kind != KindYouTube {
		return fmt.Errorf("unknown feed kind %q: %w", f.Kind, ErrInvalid)
	}
	if strings.TrimSpace(f.URL) == "" {
		return fmt.Errorf("empty feed URL: %w", ErrInvalid)
	}
	if f.FolderID < 0 {
		return fmt.Errorf("invalid folder id %d: %w", f.FolderID, ErrInvalid)
	}
	return nil
}

// feedTitle picks the stored title: the user's, else the feed's, else its URL
// without its credentials.
func feedTitle(f NewFeed) string {
	for _, t := range []string{f.Title, f.OriginalTitle} {
		if t = strings.TrimSpace(t); t != "" {
			return t
		}
	}
	return safeurl.String(f.URL) // titles are shown and logged
}

// insertInitialEntries stores the entries of a newly created feed. Entries
// are inserted from last to first so that, among equal published_at values,
// the earlier slice position gets the higher id and sorts first in the grid.
func insertInitialEntries(ctx context.Context, tx *sql.Tx, feedID int64, list []NewEntry, unread map[int]bool, fetchedAt time.Time) error {
	stmt, err := tx.PrepareContext(ctx, insertEntrySQL)
	if err != nil {
		return fmt.Errorf("prepare entry insert: %w", err)
	}
	defer stmt.Close()
	for i := len(list) - 1; i >= 0; i-- {
		readAt := any(fetchedAt.Unix())
		if unread[i] {
			readAt = nil
		}
		if _, err := stmt.ExecContext(ctx, entryInsertArgs(feedID, list[i], fetchedAt, !unread[i], readAt)...); err != nil {
			return fmt.Errorf("insert entry %q: %w", list[i].GUID, classify(err))
		}
	}
	return nil
}

// RenameFeed sets the user-visible title. The title is trimmed; empty titles
// are rejected with ErrInvalid.
func (s *SQLite) RenameFeed(ctx context.Context, id int64, title string) error {
	title = strings.TrimSpace(title)
	if title == "" {
		return fmt.Errorf("store: rename feed %d: empty title: %w", id, ErrInvalid)
	}
	if err := execOne(ctx, s.db, "feed", id, `UPDATE feeds SET title = ? WHERE id = ?`, title, id); err != nil {
		return fmt.Errorf("store: rename feed: %w", err)
	}
	return nil
}

// MoveFeed sets the feed's folder; folderID 0 = uncategorized. It returns
// ErrNotFound if either the feed or the folder does not exist.
func (s *SQLite) MoveFeed(ctx context.Context, id, folderID int64) error {
	if folderID < 0 {
		return fmt.Errorf("store: move feed %d: invalid folder id %d: %w", id, folderID, ErrInvalid)
	}
	if err := execOne(ctx, s.db, "feed", id,
		`UPDATE feeds SET folder_id = ? WHERE id = ?`, nullID(folderID), id); err != nil {
		return fmt.Errorf("store: move feed %d to folder %d: %w", id, folderID, err)
	}
	return nil
}

// SetFeedInterval sets the per-feed poll interval in seconds; 0 = the
// default interval of the feed's kind. Values below config.MinPollInterval
// are rejected with ErrInvalid.
func (s *SQLite) SetFeedInterval(ctx context.Context, id int64, sec int) error {
	if sec < 0 || (sec > 0 && time.Duration(sec)*time.Second < config.MinPollInterval) {
		return fmt.Errorf("store: set interval of feed %d: %ds is below the minimum %s: %w",
			id, sec, config.MinPollInterval, ErrInvalid)
	}
	var v any
	if sec > 0 {
		v = sec
	}
	if err := execOne(ctx, s.db, "feed", id, `UPDATE feeds SET interval_sec = ? WHERE id = ?`, v, id); err != nil {
		return fmt.Errorf("store: set feed interval: %w", err)
	}
	return nil
}

// DeleteFeed removes the feed, all its entries (including later and favourite
// ones) and their entry_tags rows, via ON DELETE CASCADE.
func (s *SQLite) DeleteFeed(ctx context.Context, id int64) error {
	if err := execOne(ctx, s.db, "feed", id, `DELETE FROM feeds WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete feed: %w", err)
	}
	return nil
}

// FeedEntryStats counts the feed's entries for the unsubscribe dialog.
func (s *SQLite) FeedEntryStats(ctx context.Context, id int64) (FeedEntryStats, error) {
	var st FeedEntryStats
	err := s.db.QueryRowContext(ctx,
		`SELECT
			(SELECT count(*) FROM entries WHERE feed_id = f.id),
			(SELECT count(*) FROM entries WHERE feed_id = f.id AND (is_later = 1 OR is_favourite = 1))
		 FROM feeds f WHERE f.id = ?`, id).Scan(&st.Total, &st.Saved)
	if err != nil {
		return FeedEntryStats{}, fmt.Errorf("store: entry stats of feed %d: %w", id, classify(err))
	}
	return st, nil
}

// UpdateFeedURL stores a permanently redirected feed URL. Returns ErrConflict
// if another feed already uses that URL and ErrInvalid if it is empty.
func (s *SQLite) UpdateFeedURL(ctx context.Context, id int64, url string) error {
	if strings.TrimSpace(url) == "" {
		return fmt.Errorf("store: update url of feed %d: empty URL: %w", id, ErrInvalid)
	}
	if err := execOne(ctx, s.db, "feed", id, `UPDATE feeds SET url = ? WHERE id = ?`, url, id); err != nil {
		return fmt.Errorf("store: update url of feed %d to %s: %w", id, redactURL(url), err)
	}
	return nil
}

// SetFeedIcon stores an icon found for the feed outside its document.
// Returns ErrNotFound if the feed does not exist and ErrInvalid if iconURL
// is empty.
func (s *SQLite) SetFeedIcon(ctx context.Context, id int64, iconURL string) error {
	if strings.TrimSpace(iconURL) == "" {
		return fmt.Errorf("store: set icon of feed %d: empty URL: %w", id, ErrInvalid)
	}
	if err := execOne(ctx, s.db, "feed", id, `UPDATE feeds SET icon_url = ? WHERE id = ?`, iconURL, id); err != nil {
		return fmt.Errorf("store: set icon of feed %d: %w", id, err)
	}
	return nil
}
