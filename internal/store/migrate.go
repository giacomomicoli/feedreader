package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"strconv"
)

// migration is one numbered schema file.
type migration struct {
	version int
	name    string
}

// migrationName matches NNNN_description.sql.
var migrationName = regexp.MustCompile(`^(\d+)_[A-Za-z0-9_-]+\.sql$`)

// migrate applies, in version order, every *.sql file in fsys whose numeric
// prefix is greater than the database's PRAGMA user_version. Each file runs
// in its own transaction together with the user_version bump, so a failing
// migration leaves the schema at the previous version. A database whose
// version is newer than the newest known migration is refused.
//
// Migrations run with foreign key enforcement off, so that they can rebuild a
// table the way SQLite documents for schema changes ALTER TABLE cannot make
// (create the new table, copy, drop the old one, rename): with enforcement on,
// DROP TABLE first deletes every row and fires the ON DELETE actions of the
// tables that reference it. PRAGMA foreign_keys is ignored inside a
// transaction, so it is switched off on a dedicated connection, every
// migration is checked with PRAGMA foreign_key_check before it commits, and
// that connection is then closed rather than returned to the pool.
func migrate(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	list, err := listMigrations(fsys)
	if err != nil {
		return err
	}
	latest := 0
	if len(list) > 0 {
		latest = list[len(list)-1].version
	}
	var current int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("migrate: read schema version: %w", err)
	}
	if current > latest {
		return fmt.Errorf("migrate: database schema version %d is newer than this binary supports (%d)", current, latest)
	}
	pending := slices.DeleteFunc(list, func(m migration) bool { return m.version <= current })
	if len(pending) == 0 {
		return nil
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	defer discardConn(conn)
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("migrate: disable foreign keys: %w", err)
	}
	for _, m := range pending {
		if err := applyMigration(ctx, conn, fsys, m); err != nil {
			return err
		}
	}
	return nil
}

// discardConn closes the driver connection behind c instead of returning it
// to the pool; connections the pool opens later get the DSN's pragmas.
func discardConn(c *sql.Conn) {
	_ = c.Raw(func(any) error { return driver.ErrBadConn })
	_ = c.Close()
}

// listMigrations returns the *.sql files of fsys sorted by version. Names
// that do not follow NNNN_description.sql, version 0 and duplicate versions
// are errors: migrations are embedded, so these are programming mistakes that
// must fail loudly at startup.
func listMigrations(fsys fs.FS) ([]migration, error) {
	names, err := fs.Glob(fsys, "*.sql")
	if err != nil {
		return nil, fmt.Errorf("migrate: list migrations: %w", err)
	}
	list := make([]migration, 0, len(names))
	seen := map[int]string{}
	for _, name := range names {
		m := migrationName.FindStringSubmatch(path.Base(name))
		if m == nil {
			return nil, fmt.Errorf("migrate: %s: file name must be NNNN_description.sql", name)
		}
		v, err := strconv.Atoi(m[1])
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("migrate: %s: version must be a positive integer", name)
		}
		if prev, dup := seen[v]; dup {
			return nil, fmt.Errorf("migrate: %s and %s share version %d", prev, name, v)
		}
		seen[v] = name
		list = append(list, migration{version: v, name: name})
	}
	slices.SortFunc(list, func(a, b migration) int { return a.version - b.version })
	return list, nil
}

// applyMigration runs one migration and sets user_version in a single write
// transaction on conn, refusing to commit if the migration left a foreign key
// pointing at a missing row. The version is re-read under the write lock so
// that two processes opening the same file cannot apply a migration twice.
func applyMigration(ctx context.Context, conn *sql.Conn, fsys fs.FS, m migration) (err error) {
	body, err := fs.ReadFile(fsys, m.name)
	if err != nil {
		return fmt.Errorf("migrate: read %s: %w", m.name, err)
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrate: %s: begin: %w", m.name, err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	var current int
	if err = tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&current); err != nil {
		return fmt.Errorf("migrate: %s: read schema version: %w", m.name, err)
	}
	if current >= m.version {
		return tx.Rollback()
	}
	if _, err = tx.ExecContext(ctx, string(body)); err != nil {
		return fmt.Errorf("migrate: %s: %w", m.name, err)
	}
	if err = checkForeignKeys(ctx, tx); err != nil {
		return fmt.Errorf("migrate: %s: %w", m.name, err)
	}
	// PRAGMA does not accept bound parameters; m.version is an int.
	if _, err = tx.ExecContext(ctx, "PRAGMA user_version = "+strconv.Itoa(m.version)); err != nil {
		return fmt.Errorf("migrate: %s: set schema version: %w", m.name, err)
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("migrate: %s: commit: %w", m.name, err)
	}
	return nil
}

// checkForeignKeys reports the first row whose foreign key references a
// missing parent row, as found by PRAGMA foreign_key_check.
func checkForeignKeys(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		var table, parent string
		var rowid sql.NullInt64
		var fkid int
		if err := rows.Scan(&table, &rowid, &parent, &fkid); err != nil {
			return fmt.Errorf("foreign key check: %w", err)
		}
		return fmt.Errorf("foreign key check: %s row %d references a missing %s row", table, rowid.Int64, parent)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("foreign key check: %w", err)
	}
	return nil
}
