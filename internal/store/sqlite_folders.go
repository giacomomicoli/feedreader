package store

import (
	"context"
	"fmt"
	"strings"
)

// cleanName trims a user-supplied folder or tag name and rejects empty ones.
func cleanName(what, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", fmt.Errorf("%s name is empty: %w", what, ErrInvalid)
	}
	return name, nil
}

// ListFolders returns all folders ordered by position, then name
// (case-insensitive).
func (s *SQLite) ListFolders(ctx context.Context) ([]Folder, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, position FROM folders ORDER BY position, name_key, id`)
	if err != nil {
		return nil, fmt.Errorf("store: list folders: %w", err)
	}
	defer rows.Close()
	var out []Folder
	for rows.Next() {
		var f Folder
		if err := rows.Scan(&f.ID, &f.Name, &f.Position); err != nil {
			return nil, fmt.Errorf("store: list folders: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list folders: %w", err)
	}
	return out, nil
}

// GetFolder returns one folder.
func (s *SQLite) GetFolder(ctx context.Context, id int64) (Folder, error) {
	f := Folder{ID: id}
	err := s.db.QueryRowContext(ctx,
		`SELECT name, position FROM folders WHERE id = ?`, id).Scan(&f.Name, &f.Position)
	if err != nil {
		return Folder{}, fmt.Errorf("store: get folder %d: %w", id, classify(err))
	}
	return f, nil
}

// CreateFolder appends a folder after the existing ones. The name is trimmed;
// it returns ErrConflict if the name is taken (case-insensitive, see
// foldName) and ErrInvalid if it is empty.
func (s *SQLite) CreateFolder(ctx context.Context, name string) (Folder, error) {
	name, err := cleanName("folder", name)
	if err != nil {
		return Folder{}, fmt.Errorf("store: create folder: %w", err)
	}
	f := Folder{Name: name}
	err = s.db.QueryRowContext(ctx,
		`INSERT INTO folders (name, name_key, position)
		 VALUES (?, ?, COALESCE((SELECT MAX(position) FROM folders), -1) + 1)
		 RETURNING id, position`, name, foldName(name)).Scan(&f.ID, &f.Position)
	if err != nil {
		return Folder{}, fmt.Errorf("store: create folder %q: %w", name, classify(err))
	}
	return f, nil
}

// RenameFolder renames a folder. The name is trimmed; it returns ErrConflict
// if the name is taken (case-insensitive, see foldName), ErrNotFound if the
// folder does not exist and ErrInvalid if the name is empty. Changing only
// the case of the current name is allowed.
func (s *SQLite) RenameFolder(ctx context.Context, id int64, name string) error {
	name, err := cleanName("folder", name)
	if err != nil {
		return fmt.Errorf("store: rename folder %d: %w", id, err)
	}
	if err := execOne(ctx, s.db, "folder", id,
		`UPDATE folders SET name = ?, name_key = ? WHERE id = ?`, name, foldName(name), id); err != nil {
		return fmt.Errorf("store: rename folder %d to %q: %w", id, name, err)
	}
	return nil
}

// DeleteFolder removes the folder; its feeds become uncategorized (ON DELETE
// SET NULL). It returns ErrNotFound if the folder does not exist.
func (s *SQLite) DeleteFolder(ctx context.Context, id int64) error {
	if err := execOne(ctx, s.db, "folder", id, `DELETE FROM folders WHERE id = ?`, id); err != nil {
		return fmt.Errorf("store: delete folder: %w", err)
	}
	return nil
}

// DeleteFolderIfEmpty removes the folder only while no feed is in it, in one
// statement, so a feed moved into it concurrently is never made
// uncategorized. deleted is false when the folder is in use or does not
// exist. The add flow uses it to roll back a folder it created for a
// subscription that then failed.
func (s *SQLite) DeleteFolderIfEmpty(ctx context.Context, id int64) (deleted bool, err error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM folders WHERE id = ? AND NOT EXISTS (SELECT 1 FROM feeds WHERE folder_id = ?)`, id, id)
	if err != nil {
		return false, fmt.Errorf("store: delete folder %d if empty: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("store: delete folder %d if empty: %w", id, err)
	}
	return n == 1, nil
}
