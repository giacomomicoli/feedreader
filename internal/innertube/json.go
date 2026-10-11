package innertube

import (
	"bytes"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Walker reads a JSON document that json.Valid accepted, without
// allocating: it visits the members and elements a caller asks for, skips
// everything else, and hands out string contents as slices of the input.
//
// encoding/json cannot visit the elements of a nested array this way: a
// Decoder per array, a slice per array or a Token per value all allocate
// for every element, tens of times the size of a hostile document of tiny
// elements. Syntax (and nesting depth) is left to json.Valid, so the walker
// only tracks where values begin and end; on input json.Valid refused it
// still stays within the input and terminates, but its results are
// meaningless.
type Walker struct {
	b []byte
	i int
	// Stop is set by a caller to end the walk: Next then returns 0.
	Stop bool
}

// NewWalker returns a Walker at the start of b.
func NewWalker(b []byte) *Walker { return &Walker{b: b} }

// Pos returns the offset in the input of the next byte to read: where the
// value that comes next begins (after Next), or where the one just read
// ends.
func (w *Walker) Pos() int { return w.i }

// Next returns the next byte that is not whitespace, or 0 at the end or
// once the walk is stopped.
func (w *Walker) Next() byte {
	for !w.Stop && w.i < len(w.b) {
		switch c := w.b[w.i]; c {
		case ' ', '\t', '\n', '\r':
			w.i++
		default:
			return c
		}
	}
	return 0
}

// Str consumes the string that comes next and returns its contents
// between the quotes, still escaped.
func (w *Walker) Str() []byte {
	if w.Next() != '"' {
		w.Skip()
		return nil
	}
	start := w.i + 1
	for w.i = start; w.i < len(w.b); w.i++ {
		switch w.b[w.i] {
		case '\\':
			w.i++
		case '"':
			w.i++
			return w.b[start : w.i-1]
		}
	}
	w.i = len(w.b) // unterminated (invalid JSON)
	return w.b[min(start, len(w.b)):]
}

// Literal consumes the number, true, false or null that comes next and
// returns it.
func (w *Walker) Literal() []byte {
	w.Next()
	start := w.i
	for w.i < len(w.b) && strings.IndexByte(",:]}\"[{ \t\n\r", w.b[w.i]) < 0 {
		w.i++
	}
	return w.b[start:w.i]
}

// Skip consumes the value that comes next, whatever it is.
func (w *Walker) Skip() {
	switch w.Next() {
	case 0:
	case '"':
		w.Str()
	case '{', '[':
		depth := 0
		for w.i < len(w.b) {
			switch w.b[w.i] {
			case '"':
				w.Str()
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
			w.i++
			if depth == 0 {
				return
			}
		}
	default:
		if len(w.Literal()) == 0 {
			w.i++ // a stray delimiter (invalid JSON): step over it
		}
	}
}

// Object calls member for each member of the object that comes next, with
// its key still escaped; member consumes the value or leaves it to be
// skipped. Anything but an object is skipped.
func (w *Walker) Object(member func(key []byte)) {
	if w.Next() != '{' {
		w.Skip()
		return
	}
	w.i++
	for {
		switch w.Next() {
		case '}':
			w.i++
			return
		case ',':
			w.i++
		case '"':
			key := w.Str()
			if w.Next() == ':' {
				w.i++
			}
			w.Value(func() { member(key) })
		default:
			w.Skip() // 0 (end or stopped) or invalid JSON
			if w.Next() == 0 {
				return
			}
		}
	}
}

// Array calls element for each element of the array that comes next;
// element consumes the value or leaves it to be skipped. Anything but an
// array is skipped.
func (w *Walker) Array(element func()) {
	if w.Next() != '[' {
		w.Skip()
		return
	}
	w.i++
	for {
		switch w.Next() {
		case ']':
			w.i++
			return
		case ',':
			w.i++
		case 0:
			return
		default:
			w.Value(element)
		}
	}
}

// Value runs read on the value that comes next and skips the value if read
// did not consume it.
func (w *Walker) Value(read func()) {
	w.Next()
	start := w.i
	read()
	if w.i == start && !w.Stop {
		w.Skip()
	}
}

// KeyIs reports whether the escaped object key raw is want.
func KeyIs(raw []byte, want string) bool {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw) == want
	}
	return Unquote(raw, len(want)+1) == want
}

// Truthy reports whether a JSON literal is true or a non-zero number: its
// mantissa holds a non-zero digit.
func Truthy(lit []byte) bool {
	if string(lit) == "true" {
		return true
	}
	if len(lit) == 0 || lit[0] != '-' && (lit[0] < '0' || lit[0] > '9') {
		return false
	}
	if e := bytes.IndexAny(lit, "eE"); e >= 0 {
		lit = lit[:e]
	}
	return bytes.ContainsAny(lit, "123456789")
}

// Unquote decodes s, the escaped contents of a JSON string, as
// encoding/json does (invalid UTF-8 and unpaired surrogates become
// U+FFFD), but only about its first max bytes: when the decoded string is
// longer, the result is a prefix of it, cut at a rune boundary, that
// ClipUTF8Bytes cuts to max bytes exactly as it cuts the whole string.
func Unquote(s []byte, max int) string {
	if bytes.IndexByte(s, '\\') < 0 && utf8.Valid(s) {
		return string(ClipUTF8Bytes(s, max))
	}
	var b strings.Builder
	b.Grow(min(len(s), max+utf8.UTFMax))
	for i := 0; i < len(s) && b.Len() < max; {
		c := s[i]
		if c != '\\' {
			r, size := utf8.DecodeRune(s[i:])
			b.WriteRune(r) // utf8.RuneError for an invalid byte
			i += size
			continue
		}
		if i+1 >= len(s) {
			break
		}
		esc := s[i+1]
		i += 2
		switch esc {
		case 'b':
			b.WriteByte('\b')
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'u':
			r, ok := hex4(s[i:])
			if !ok {
				return b.String()
			}
			i += 4
			if utf16.IsSurrogate(r) {
				r2, ok := rune(0), false
				if i+1 < len(s) && s[i] == '\\' && s[i+1] == 'u' {
					r2, ok = hex4(s[i+2:])
				}
				if dec := utf16.DecodeRune(r, r2); ok && dec != utf8.RuneError {
					r = dec
					i += 6
				} else {
					r = utf8.RuneError
				}
			}
			b.WriteRune(r)
		default: // '"', '\\', '/'
			b.WriteByte(esc)
		}
	}
	return b.String()
}

// hex4 decodes the four hex digits a \u escape is followed by.
func hex4(s []byte) (rune, bool) {
	if len(s) < 4 {
		return 0, false
	}
	var r rune
	for _, c := range s[:4] {
		switch {
		case '0' <= c && c <= '9':
			c -= '0'
		case 'a' <= c && c <= 'f':
			c -= 'a' - 10
		case 'A' <= c && c <= 'F':
			c -= 'A' - 10
		default:
			return 0, false
		}
		r = r<<4 | rune(c)
	}
	return r, true
}

// ClipUTF8Bytes returns the longest prefix of s of at most n bytes that
// does not end inside a UTF-8 sequence (s itself when it is that short).
func ClipUTF8Bytes(s []byte, n int) []byte {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return nil
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
