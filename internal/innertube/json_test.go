package innertube

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// FuzzUnquote: Unquote decodes like encoding/json, and its cut prefix
// clips to the same text as the whole string.
func FuzzUnquote(f *testing.F) {
	for _, s := range []string{`plain`, `a\nb\t\"\\\/`, `\u00e9\ud83d\ude00`, `\ud800x`, `\udc00\ud83d`, "\xff\xfe", `\u0000`, `\ud83d\u0041`} {
		f.Add([]byte(`"`+s+`"`), 3)
	}
	f.Fuzz(func(t *testing.T, lit []byte, max int) {
		var want string
		lit = bytes.TrimSpace(lit)
		if len(lit) < 2 || !json.Valid(lit) || json.Unmarshal(lit, &want) != nil {
			return
		}
		contents := lit[1 : len(lit)-1]
		if got := Unquote(contents, len(lit)*utf8.UTFMax); got != want {
			t.Fatalf("Unquote(%q) = %q, want %q", contents, got, want)
		}
		max = min(len(lit), max&0xff)
		got := Unquote(contents, max)
		if !strings.HasPrefix(want, got) || string(ClipUTF8Bytes([]byte(got), max)) != string(ClipUTF8Bytes([]byte(want), max)) {
			t.Fatalf("Unquote(%q, %d) = %q; whole %q", contents, max, got, want)
		}
	})
}

func TestTruthy(t *testing.T) {
	for lit, want := range map[string]bool{
		"true": true, "1": true, "-2": true, "0.001": true, "1e-400": true, "3E2": true,
		"false": false, "null": false, "0": false, "-0": false, "0.0": false, "0e5": false, "": false, "x1": false,
	} {
		if got := Truthy([]byte(lit)); got != want {
			t.Errorf("Truthy(%q) = %v", lit, got)
		}
	}
}

func TestKeyIs(t *testing.T) {
	for raw, want := range map[string]bool{
		`utf8`: true, `utf\u0038`: true, `\u0075tf8`: true, `UTF8`: false, `utf8 `: false,
		`utf\u00388`: false, `ut`: false, `utf\ud800`: false,
	} {
		if got := KeyIs([]byte(raw), "utf8"); got != want {
			t.Errorf("KeyIs(%q) = %v", raw, got)
		}
	}
}

// TestWalker_TerminatesOnInvalidInput: the walker only ever sees
// documents json.Valid accepted, but must not loop or read out of bounds
// on anything else.
func TestWalker_TerminatesOnInvalidInput(t *testing.T) {
	for _, doc := range []string{``, `{`, `[`, `"`, `{"a`, `{"a":`, `{"a":}`, `{:}`, `[,,]`, `{]`, `[}`, `{"a" 1}`, `}}}`, `{"a":"\`, `{,"a":[1,]}`, `x`} {
		w := NewWalker([]byte(doc))
		w.Object(func([]byte) { w.Array(func() { w.Skip() }) })
		w.Skip()
		if w.Pos() > len(doc) {
			t.Errorf("%q: read to %d", doc, w.Pos())
		}
	}
}
