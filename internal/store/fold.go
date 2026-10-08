package store

import (
	"database/sql/driver"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"modernc.org/sqlite"
)

// foldName returns the key under which folder and tag names are unique,
// looked up and ordered (the name_key columns): name with every rune replaced
// by a canonical member of its Unicode simple case-folding orbit. Two names
// get the same key exactly when strings.EqualFold reports them equal, so
// "Città" and "CITTÀ" collide while SQLite's NOCASE, which folds only A-Z,
// would keep them apart. Callers trim the name first (cleanName).
func foldName(name string) string {
	var b strings.Builder
	b.Grow(len(name))
	for _, r := range name {
		b.WriteRune(foldRune(r))
	}
	return b.String()
}

// foldRune maps r to one fixed member of its simple case-folding orbit (the
// runes unicode.SimpleFold cycles through, which strings.EqualFold treats as
// equal): the lower-case form of the orbit's smallest rune when that form is
// in the orbit, so ASCII and most letters fold to lower case, else the
// smallest rune itself.
func foldRune(r rune) rune {
	if r < utf8.RuneSelf {
		if 'A' <= r && r <= 'Z' {
			r += 'a' - 'A'
		}
		return r
	}
	smallest := r
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		smallest = min(smallest, f)
	}
	lower := unicode.ToLower(smallest)
	for f := unicode.SimpleFold(smallest); f != smallest; f = unicode.SimpleFold(f) {
		if f == lower {
			return lower
		}
	}
	return smallest
}

// foldNameSQL is the SQL name of foldName, used by migration
// 0002_unicode_name_keys.sql to backfill name_key. It must stay registered
// while that migration exists: SQLite resolves functions when a statement is
// prepared, so even a fresh, empty database needs it. Queries pass keys
// computed in Go instead, so the schema itself never depends on it and the
// database stays usable from the stock sqlite3 shell.
const foldNameSQL = "fold_name"

func init() {
	sqlite.MustRegisterDeterministicScalarFunction(foldNameSQL, 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			switch v := args[0].(type) {
			case nil:
				return nil, nil
			case string:
				return foldName(v), nil
			case []byte:
				return foldName(string(v)), nil
			default:
				return nil, fmt.Errorf("%s: unsupported argument type %T", foldNameSQL, v)
			}
		})
}
