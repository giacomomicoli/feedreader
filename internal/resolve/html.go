package resolve

import (
	"errors"
	"io"
	"slices"
	"strings"

	"golang.org/x/net/html"
)

// tag is a start tag reported by scanTags.
type tag struct {
	name  string            // lower-cased element name
	attrs map[string]string // lower-cased names; the first occurrence wins
}

// scanTags streams the HTML in r and calls visit for every start or
// self-closing tag whose name is in names, until visit returns false or the
// input ends. Nothing is buffered beyond the current token, so large pages
// (a YouTube channel page is over 1.5 MB) cost one pass and little memory.
// The contents of raw-text elements (script, style, textarea, …) are never
// reported as tags.
func scanTags(r io.Reader, names []string, visit func(tag) bool) error {
	z := html.NewTokenizer(r)
	for {
		switch z.Next() {
		case html.ErrorToken:
			if err := z.Err(); !errors.Is(err, io.EOF) {
				return err
			}
			return nil
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if !slices.Contains(names, string(name)) {
				continue
			}
			t := tag{name: string(name), attrs: map[string]string{}}
			for hasAttr {
				var k, v []byte
				k, v, hasAttr = z.TagAttr()
				if _, dup := t.attrs[string(k)]; !dup {
					t.attrs[string(k)] = string(v)
				}
			}
			if !visit(t) {
				return nil
			}
		}
	}
}

// relTokens splits a rel attribute into lower-cased keywords, separated by
// ASCII whitespace as HTML requires.
func relTokens(rel string) []string {
	fields := strings.FieldsFunc(rel, isHTMLSpace)
	for i, f := range fields {
		fields[i] = strings.ToLower(f)
	}
	return fields
}

// hasRel reports whether the rel attribute contains keyword.
func hasRel(rel, keyword string) bool {
	return slices.Contains(relTokens(rel), keyword)
}

// isHTMLSpace reports ASCII whitespace as defined by the HTML standard.
func isHTMLSpace(r rune) bool {
	switch r {
	case ' ', '\t', '\n', '\f', '\r':
		return true
	}
	return false
}

// trimHTMLSpace strips leading and trailing ASCII whitespace, as browsers do
// for URL-valued attributes.
func trimHTMLSpace(s string) string {
	return strings.TrimFunc(s, isHTMLSpace)
}
