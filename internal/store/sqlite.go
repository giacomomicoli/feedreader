package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/giacomomicoli/feedreader/migrations"
)

// SQLite is the SQLite implementation of Store (modernc.org/sqlite, CGO-free).
//
// Every pooled connection runs in WAL mode with foreign keys enforced, a
// 10 s busy timeout and synchronous=NORMAL. Write transactions are opened
// with BEGIN IMMEDIATE so that concurrent writers queue on the busy timeout
// instead of failing with SQLITE_BUSY when a read lock is upgraded.
type SQLite struct {
	db  *sql.DB
	now func() time.Time // clock for created_at and defaults; UTC

	closeOnce sync.Once
	closeErr  error
}

var _ Store = (*SQLite)(nil)

// busyTimeoutMS is how long a connection waits for a lock held by another
// writer before giving up with SQLITE_BUSY.
const busyTimeoutMS = 10000

// OpenSQLite opens (creating if needed) the database at path, enables WAL and
// foreign keys on every connection, and applies pending migrations from the
// embedded migrations/ directory. The parent directory is created if missing.
func OpenSQLite(path string) (*SQLite, error) {
	if path == "" {
		return nil, fmt.Errorf("store: open: empty database path: %w", ErrInvalid)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
		return nil, fmt.Errorf("store: open %s: create data directory: %w", path, err)
	}
	db, err := sql.Open("sqlite", dsn(abs))
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	s := &SQLite{db: db, now: func() time.Time { return time.Now().UTC() }}
	ctx := context.Background()
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	if err := migrate(ctx, db, migrations.FS); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return s, nil
}

// dsn builds a file: URI for the absolute path abs. Using a URI (rather than
// a bare path) lets SQLite percent-decode the path, so file names containing
// '?', '#' or spaces work. The _pragma and _txlock parameters are applied by
// the driver to every new pooled connection.
func dsn(abs string) string {
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", busyTimeoutMS))
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Set("_txlock", "immediate")
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: q.Encode()}
	return u.String()
}

// Close runs PRAGMA optimize and closes the database. It is safe to call more
// than once.
func (s *SQLite) Close() error {
	s.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, optErr := s.db.ExecContext(ctx, "PRAGMA optimize")
		if optErr != nil {
			optErr = fmt.Errorf("store: optimize: %w", optErr)
		}
		closeErr := s.db.Close()
		if closeErr != nil {
			closeErr = fmt.Errorf("store: close: %w", closeErr)
		}
		s.closeErr = errors.Join(optErr, closeErr)
	})
	return s.closeErr
}

// --- transactions ---

// inTx runs fn inside a write transaction (BEGIN IMMEDIATE via the DSN),
// committing on success and rolling back on error or panic.
func (s *SQLite) inTx(ctx context.Context, fn func(tx *sql.Tx) error) (err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// execOne runs a statement that must affect exactly one row identified by
// id; zero affected rows is reported as ErrNotFound.
func execOne(ctx context.Context, x execer, what string, id int64, query string, args ...any) error {
	res, err := x.ExecContext(ctx, query, args...)
	if err != nil {
		return classify(err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%s %d: %w", what, id, ErrNotFound)
	}
	return nil
}

// execer is implemented by *sql.DB and *sql.Tx.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// queryer is implemented by *sql.DB and *sql.Tx.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// rowScanner is implemented by *sql.Row and *sql.Rows.
type rowScanner interface {
	Scan(dest ...any) error
}

// --- errors ---

// classify maps driver errors to the package sentinels while keeping the
// original error in the chain: unique and primary-key violations become
// ErrConflict, foreign-key violations (a referenced row is missing) and
// sql.ErrNoRows become ErrNotFound. Other errors are returned unchanged.
func classify(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() {
		case sqlite3.SQLITE_CONSTRAINT_UNIQUE, sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY:
			return fmt.Errorf("%w: %w", ErrConflict, err)
		case sqlite3.SQLITE_CONSTRAINT_FOREIGNKEY:
			return fmt.Errorf("%w: %w", ErrNotFound, err)
		}
	}
	return err
}

// --- value conversion ---

// unixOrZero converts t to epoch seconds for NOT NULL columns, mapping the
// zero time to 0.
func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

// nullUnix converts t to epoch seconds for nullable columns, mapping the zero
// time to NULL.
func nullUnix(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.Unix()
}

// timeFromUnix converts epoch seconds read from a NOT NULL column.
func timeFromUnix(n int64) time.Time { return time.Unix(n, 0).UTC() }

// timeFromNullUnix converts a nullable epoch column; NULL becomes the zero
// time.
func timeFromNullUnix(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return timeFromUnix(n.Int64)
}

// timeFromUnixOrZero converts a NOT NULL epoch column that uses 0 as its
// "unset" value (next_fetch_at).
func timeFromUnixOrZero(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return timeFromUnix(n)
}

// nullStr stores "" as NULL in optional text columns.
func nullStr(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// nullID stores id 0 as NULL in optional foreign-key columns.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}

// boolInt converts a Go bool to SQLite's 0/1.
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
