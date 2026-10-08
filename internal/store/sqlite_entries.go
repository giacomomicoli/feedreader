package store

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// entryViewSelect selects what scanEntryView expects. CROSS JOIN pins
// entries as the outer loop so the planner walks a published_at index in grid
// order and stops after LIMIT rows instead of sorting the whole scope.
const entryViewSelect = `SELECT e.id, e.feed_id, e.guid, e.url, e.title, e.summary_html,
		e.author, e.thumbnail_url, e.published_at, e.updated_at, e.fetched_at,
		e.is_read, e.is_later, e.is_favourite, e.read_at,
		f.title, f.kind, f.icon_url
	FROM entries e CROSS JOIN feeds f ON f.id = e.feed_id`

// scanEntryView reads one row selected with entryViewSelect. Tags are filled
// separately by attachTags.
func scanEntryView(r rowScanner) (EntryView, error) {
	var (
		v                                     EntryView
		url, summary, author, thumb, feedIcon sql.NullString
		published, fetched                    int64
		updated, readAt                       sql.NullInt64
		kind                                  string
	)
	err := r.Scan(&v.ID, &v.FeedID, &v.GUID, &url, &v.Title, &summary,
		&author, &thumb, &published, &updated, &fetched,
		&v.IsRead, &v.IsLater, &v.IsFavourite, &readAt,
		&v.FeedTitle, &kind, &feedIcon)
	if err != nil {
		return EntryView{}, err
	}
	v.URL = url.String
	v.SummaryHTML = summary.String
	v.Author = author.String
	v.ThumbnailURL = thumb.String
	v.PublishedAt = timeFromUnix(published)
	v.UpdatedAt = timeFromNullUnix(updated)
	v.FetchedAt = timeFromUnix(fetched)
	v.ReadAt = timeFromNullUnix(readAt)
	v.FeedKind = Kind(kind)
	v.FeedIconURL = feedIcon.String
	return v, nil
}

// scopeFilter returns a boolean SQL expression selecting the entries of scope
// s, plus its arguments. The expression refers to the entry as "e" and to its
// feed (already joined on f.id = e.feed_id) as "f". It is shared by
// ListEntries and MarkScopeReadUpTo so both agree on what a scope contains.
// The literal "= 1" comparisons let the planner use the partial indexes.
func scopeFilter(s Scope) (string, []any, error) {
	switch s.Kind {
	case ScopeAll:
		return "1", nil, nil
	case ScopeFolder:
		// An IN list keeps the work proportional to the folder's size.
		return "e.feed_id IN (SELECT id FROM feeds WHERE folder_id = ?)", []any{s.ID}, nil
	case ScopeFeed:
		return "e.feed_id = ?", []any{s.ID}, nil
	case ScopeWatchLater:
		return "e.is_later = 1 AND f.kind = ?", []any{string(KindYouTube)}, nil
	case ScopeReadLater:
		return "e.is_later = 1 AND f.kind = ?", []any{string(KindRSS)}, nil
	case ScopeFavourites:
		return "e.is_favourite = 1", nil, nil
	case ScopeTag:
		return "e.id IN (SELECT entry_id FROM entry_tags WHERE tag_id = ?)", []any{s.ID}, nil
	}
	return "", nil, fmt.Errorf("unknown scope %q: %w", s.Kind, ErrInvalid)
}

// ListEntries returns one page of the card grid, sorted by published_at DESC,
// id DESC, using keyset pagination: q.After excludes every entry at or before
// the cursor, so entries changing state between pages cause neither gaps nor
// duplicates. Tags for the page are loaded with one extra query. Returns
// ErrInvalid for an unknown scope or a non-positive Limit.
func (s *SQLite) ListEntries(ctx context.Context, q ListQuery) (Page, error) {
	query, args, err := listQuerySQL(q)
	if err != nil {
		return Page{}, fmt.Errorf("store: list entries: %w", err)
	}
	views, err := queryEntryViews(ctx, s.db, query, args...)
	if err != nil {
		return Page{}, fmt.Errorf("store: list entries in %s %d: %w", q.Scope.Kind, q.Scope.ID, err)
	}
	var p Page
	if len(views) > q.Limit {
		p.HasMore = true
		views = views[:q.Limit]
	}
	if len(views) > 0 {
		last := views[len(views)-1]
		p.Next = Cursor{PublishedAt: last.PublishedAt, ID: last.ID}
	}
	if err := attachTags(ctx, s.db, views); err != nil {
		return Page{}, fmt.Errorf("store: list entries: %w", err)
	}
	p.Entries = views
	return p, nil
}

// listQuerySQL builds the grid query for q. It fetches Limit+1 rows so the
// caller can tell whether another page exists.
func listQuerySQL(q ListQuery) (string, []any, error) {
	if q.Limit <= 0 {
		return "", nil, fmt.Errorf("limit %d: %w", q.Limit, ErrInvalid)
	}
	where, args, err := scopeFilter(q.Scope)
	if err != nil {
		return "", nil, err
	}
	var sb strings.Builder
	sb.WriteString(entryViewSelect)
	sb.WriteString("\n\tWHERE ")
	sb.WriteString(where)
	if q.UnreadOnly && !q.Scope.IgnoresReadFilter() {
		sb.WriteString(" AND e.is_read = 0")
	}
	if q.After != nil {
		sb.WriteString(" AND (e.published_at, e.id) < (?, ?)")
		args = append(args, q.After.PublishedAt.Unix(), q.After.ID)
	}
	sb.WriteString("\n\tORDER BY e.published_at DESC, e.id DESC LIMIT ?")
	args = append(args, q.Limit+1)
	return sb.String(), args, nil
}

func queryEntryViews(ctx context.Context, q queryer, query string, args ...any) ([]EntryView, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EntryView
	for rows.Next() {
		v, err := scanEntryView(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// attachTags fills Tags (ordered by name, case-insensitive) for every view
// with one query.
func attachTags(ctx context.Context, q queryer, views []EntryView) error {
	if len(views) == 0 {
		return nil
	}
	byID := make(map[int64]*EntryView, len(views))
	ids := make([]byte, 0, len(views)*8)
	ids = append(ids, '[')
	for i := range views {
		if i > 0 {
			ids = append(ids, ',')
		}
		ids = strconv.AppendInt(ids, views[i].ID, 10)
		byID[views[i].ID] = &views[i]
	}
	ids = append(ids, ']')

	rows, err := q.QueryContext(ctx,
		`SELECT et.entry_id, t.id, t.name
		 FROM entry_tags et JOIN tags t ON t.id = et.tag_id
		 WHERE et.entry_id IN (SELECT value FROM json_each(?))
		 ORDER BY t.name_key, t.id`, string(ids))
	if err != nil {
		return fmt.Errorf("load tags: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entryID int64
		var t Tag
		if err := rows.Scan(&entryID, &t.ID, &t.Name); err != nil {
			return fmt.Errorf("load tags: %w", err)
		}
		if v := byID[entryID]; v != nil {
			v.Tags = append(v.Tags, t)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("load tags: %w", err)
	}
	return nil
}

// GetEntry returns one entry with its feed fields and tags.
func (s *SQLite) GetEntry(ctx context.Context, id int64) (EntryView, error) {
	v, err := scanEntryView(s.db.QueryRowContext(ctx, entryViewSelect+` WHERE e.id = ?`, id))
	if err != nil {
		return EntryView{}, fmt.Errorf("store: get entry %d: %w", id, classify(err))
	}
	views := []EntryView{v}
	if err := attachTags(ctx, s.db, views); err != nil {
		return EntryView{}, fmt.Errorf("store: get entry %d: %w", id, err)
	}
	return views[0], nil
}

// SetRead marks one entry read (read_at = at, or now if at is zero) or unread
// (read_at = NULL). It never touches is_later or is_favourite.
func (s *SQLite) SetRead(ctx context.Context, id int64, read bool, at time.Time) error {
	var err error
	if read {
		err = execOne(ctx, s.db, "entry", id,
			`UPDATE entries SET is_read = 1, read_at = ? WHERE id = ?`, s.orNow(at).Unix(), id)
	} else {
		err = execOne(ctx, s.db, "entry", id,
			`UPDATE entries SET is_read = 0, read_at = NULL WHERE id = ?`, id)
	}
	if err != nil {
		return fmt.Errorf("store: set read=%t: %w", read, err)
	}
	return nil
}

// SetLater sets is_later; it never changes the read state.
func (s *SQLite) SetLater(ctx context.Context, id int64, on bool) error {
	if err := execOne(ctx, s.db, "entry", id,
		`UPDATE entries SET is_later = ? WHERE id = ?`, boolInt(on), id); err != nil {
		return fmt.Errorf("store: set later=%t: %w", on, err)
	}
	return nil
}

// SetFavourite sets is_favourite; it never changes the read state.
func (s *SQLite) SetFavourite(ctx context.Context, id int64, on bool) error {
	if err := execOne(ctx, s.db, "entry", id,
		`UPDATE entries SET is_favourite = ? WHERE id = ?`, boolInt(on), id); err != nil {
		return fmt.Errorf("store: set favourite=%t: %w", on, err)
	}
	return nil
}

// MaxEntryID returns the largest entry id stored so far, or 0 when there are
// no entries. Entry ids only grow and are never reused (AUTOINCREMENT, see
// migration 0003), and writers are serialized, so every entry committed after
// this call gets a larger id: read it before listing a scope and pass it to
// MarkScopeReadUpTo.
func (s *SQLite) MaxEntryID(ctx context.Context) (int64, error) {
	var id int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM entries`).Scan(&id); err != nil {
		return 0, fmt.Errorf("store: max entry id: %w", err)
	}
	return id, nil
}

// MarkScopeReadUpTo marks every unread entry in the scope whose id is <=
// maxID as read (read_at = at, or now if at is zero). With maxID taken from
// MaxEntryID before the page was listed, entries stored after that, which the
// user has not seen, stay unread whatever their fetched_at. A non-positive
// maxID matches nothing. Returns rows changed.
func (s *SQLite) MarkScopeReadUpTo(ctx context.Context, sc Scope, maxID int64, at time.Time) (int64, error) {
	where, args, err := scopeFilter(sc)
	if err != nil {
		return 0, fmt.Errorf("store: mark scope read: %w", err)
	}
	if maxID <= 0 {
		return 0, nil
	}
	args = append([]any{s.orNow(at).Unix(), maxID}, args...)
	res, err := s.db.ExecContext(ctx,
		`UPDATE entries AS e SET is_read = 1, read_at = ?
		 FROM feeds AS f
		 WHERE f.id = e.feed_id AND e.is_read = 0 AND e.id <= ? AND `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("store: mark %s %d read: %w", sc.Kind, sc.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: mark %s %d read: %w", sc.Kind, sc.ID, err)
	}
	return n, nil
}

// UnreadCounts computes all sidebar badges with a single query; folder and
// overall totals are summed from the per-feed rows. Each per-feed count is
// answered from the covering partial index entries_unread. Feeds and folders
// without unread entries are absent from the maps.
func (s *SQLite) UnreadCounts(ctx context.Context) (UnreadCounts, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT f.id, f.folder_id,
			(SELECT count(*) FROM entries e WHERE e.feed_id = f.id AND e.is_read = 0)
		 FROM feeds f`)
	if err != nil {
		return UnreadCounts{}, fmt.Errorf("store: unread counts: %w", err)
	}
	defer rows.Close()
	c := UnreadCounts{ByFeed: map[int64]int{}, ByFolder: map[int64]int{}}
	for rows.Next() {
		var feedID int64
		var folderID sql.NullInt64
		var n int
		if err := rows.Scan(&feedID, &folderID, &n); err != nil {
			return UnreadCounts{}, fmt.Errorf("store: unread counts: %w", err)
		}
		if n == 0 {
			continue
		}
		c.ByFeed[feedID] = n
		if folderID.Valid {
			c.ByFolder[folderID.Int64] += n
		}
		c.All += n
	}
	if err := rows.Err(); err != nil {
		return UnreadCounts{}, fmt.Errorf("store: unread counts: %w", err)
	}
	return c, nil
}

// orNow returns t, or the store clock's current time when t is zero.
func (s *SQLite) orNow(t time.Time) time.Time {
	if t.IsZero() {
		return s.now()
	}
	return t
}
