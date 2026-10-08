package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ListTags returns all tags with Count, ordered by name (case-insensitive).
// Tags that no longer label any entry are listed with Count 0.
func (s *SQLite) ListTags(ctx context.Context) ([]Tag, error) {
	tags, err := queryTags(ctx, s.db,
		`SELECT t.id, t.name, count(et.entry_id)
		 FROM tags t LEFT JOIN entry_tags et ON et.tag_id = t.id
		 GROUP BY t.id
		 ORDER BY t.name_key, t.id`)
	if err != nil {
		return nil, fmt.Errorf("store: list tags: %w", err)
	}
	return tags, nil
}

// GetTag returns one tag; Count is not filled.
func (s *SQLite) GetTag(ctx context.Context, id int64) (Tag, error) {
	t := Tag{ID: id}
	if err := s.db.QueryRowContext(ctx, `SELECT name FROM tags WHERE id = ?`, id).Scan(&t.Name); err != nil {
		return Tag{}, fmt.Errorf("store: get tag %d: %w", id, classify(err))
	}
	return t, nil
}

// SuggestTags returns up to limit tags whose name starts with prefix
// (case-insensitive, see foldName), ordered by name, for autocomplete. Count
// is not filled. A non-positive limit returns no tags.
func (s *SQLite) SuggestTags(ctx context.Context, prefix string, limit int) ([]Tag, error) {
	if limit <= 0 {
		return nil, nil
	}
	// Folding is per rune, so a name starts with prefix case-insensitively
	// exactly when its key starts with the prefix's key. Keys hold no ASCII
	// upper case, so LIKE's ASCII case folding cannot widen the match.
	tags, err := queryTags(ctx, s.db,
		`SELECT id, name, 0 FROM tags WHERE name_key LIKE ? ESCAPE '\' ORDER BY name_key, id LIMIT ?`,
		likePrefix(foldName(strings.TrimSpace(prefix))), limit)
	if err != nil {
		return nil, fmt.Errorf("store: suggest tags for %q: %w", prefix, err)
	}
	return tags, nil
}

// likePrefix escapes LIKE wildcards in p and appends '%'.
func likePrefix(p string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(p) + "%"
}

func queryTags(ctx context.Context, q queryer, query string, args ...any) ([]Tag, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Tag
	for rows.Next() {
		var t Tag
		if err := rows.Scan(&t.ID, &t.Name, &t.Count); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddEntryTag attaches the tag named name (trimmed; created if missing,
// matched case-insensitively by its foldName key, keeping the existing
// spelling) to the entry.
// Idempotent. Returns ErrInvalid for an empty name and ErrNotFound if the
// entry does not exist. Count is not filled.
func (s *SQLite) AddEntryTag(ctx context.Context, entryID int64, name string) (Tag, error) {
	name, err := cleanName("tag", name)
	if err != nil {
		return Tag{}, fmt.Errorf("store: tag entry %d: %w", entryID, err)
	}
	key := foldName(name)
	var t Tag
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM entries WHERE id = ?`, entryID).Scan(&one); err != nil {
			return fmt.Errorf("entry %d: %w", entryID, classify(err))
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO tags (name, name_key) VALUES (?, ?) ON CONFLICT (name_key) DO NOTHING`,
			name, key); err != nil {
			return fmt.Errorf("create tag: %w", err)
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT id, name FROM tags WHERE name_key = ?`, key).Scan(&t.ID, &t.Name); err != nil {
			return fmt.Errorf("look up tag: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO entry_tags (entry_id, tag_id) VALUES (?, ?) ON CONFLICT DO NOTHING`,
			entryID, t.ID); err != nil {
			return fmt.Errorf("attach tag: %w", classify(err))
		}
		return nil
	})
	if err != nil {
		return Tag{}, fmt.Errorf("store: tag entry %d with %q: %w", entryID, name, err)
	}
	return t, nil
}

// RemoveEntryTag detaches a tag from an entry; the tag itself is kept.
// Removing a tag the entry does not carry is a no-op; ErrNotFound is returned
// only when the entry or the tag does not exist.
func (s *SQLite) RemoveEntryTag(ctx context.Context, entryID, tagID int64) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM entry_tags WHERE entry_id = ? AND tag_id = ?`, entryID, tagID)
	if err != nil {
		return fmt.Errorf("store: untag entry %d: %w", entryID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: untag entry %d: %w", entryID, err)
	}
	if n > 0 {
		return nil
	}
	var entryOK, tagOK bool
	err = s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM entries WHERE id = ?), EXISTS (SELECT 1 FROM tags WHERE id = ?)`,
		entryID, tagID).Scan(&entryOK, &tagOK)
	switch {
	case err != nil:
		return fmt.Errorf("store: untag entry %d: %w", entryID, err)
	case !entryOK:
		return fmt.Errorf("store: untag entry %d: entry: %w", entryID, ErrNotFound)
	case !tagOK:
		return fmt.Errorf("store: untag entry %d: tag %d: %w", entryID, tagID, ErrNotFound)
	}
	return nil
}

// DeleteTag removes the tag and its entry_tags rows (ON DELETE CASCADE);
// entries are kept.
func (s *SQLite) DeleteTag(ctx context.Context, id int64) error {
	if err := execOne(ctx, s.db, "tag", id, `DELETE FROM tags WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete tag: %w", err)
	}
	return nil
}
