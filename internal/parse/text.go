package parse

import (
	"html"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// ellipsis is appended to truncated excerpts.
const ellipsis = "…"

// hiddenElements are elements whose text content is never visible.
var hiddenElements = map[atom.Atom]bool{
	atom.Head: true, atom.Iframe: true, atom.Math: true, atom.Noscript: true,
	atom.Object: true, atom.Script: true, atom.Style: true, atom.Svg: true,
	atom.Template: true, atom.Title: true,
}

// breakElements are elements that separate words visually, so their tags
// become a space in extracted text ("<p>a</p><p>b</p>" → "a b").
var breakElements = map[atom.Atom]bool{
	atom.Address: true, atom.Article: true, atom.Aside: true, atom.Blockquote: true,
	atom.Br: true, atom.Caption: true, atom.Dd: true, atom.Details: true,
	atom.Div: true, atom.Dl: true, atom.Dt: true, atom.Figcaption: true,
	atom.Figure: true, atom.Footer: true, atom.H1: true, atom.H2: true,
	atom.H3: true, atom.H4: true, atom.H5: true, atom.H6: true,
	atom.Header: true, atom.Hr: true, atom.Img: true, atom.Li: true,
	atom.Main: true, atom.Nav: true, atom.Ol: true, atom.P: true,
	atom.Pre: true, atom.Section: true, atom.Summary: true, atom.Table: true,
	atom.Td: true, atom.Th: true, atom.Tr: true, atom.Ul: true,
}

// htmlish matches a complete tag or a character/entity reference, which is
// how markup-carrying text (RSS description, untyped Atom summary) is told
// apart from plain text.
var htmlish = regexp.MustCompile(`(?i)</?[a-z][a-z0-9-]*(\s[^<>]*)?/?>|&(#[0-9]{1,8}|#x[0-9a-f]{1,8}|[a-z][a-z0-9]{1,31});`)

// looksLikeHTML reports whether s contains markup rather than plain text.
func looksLikeHTML(s string) bool {
	return htmlish.MatchString(s)
}

// entityRefish matches a character or entity reference.
var entityRefish = regexp.MustCompile(`(?i)&(#[0-9]{1,8}|#x[0-9a-f]{1,8}|[a-z][a-z0-9]{1,31});`)

// titleLooksLikeHTML is looksLikeHTML for titles and names, which often
// mention things in angle brackets ("Vec<T>", "The <dialog> element",
// "if a<b and b>c"): only a character or entity reference, or a known HTML
// element closed by its own end tag, counts as markup.
func titleLooksLikeHTML(s string) bool {
	if entityRefish.MatchString(s) {
		return true
	}
	if !strings.Contains(s, "</") {
		return false
	}
	z := xhtml.NewTokenizer(strings.NewReader(s))
	var open map[atom.Atom]bool
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return false
		case xhtml.StartTagToken:
			if a := tagAtom(z); a != 0 {
				if open == nil {
					open = map[atom.Atom]bool{}
				}
				open[a] = true
			}
		case xhtml.EndTagToken:
			if a := tagAtom(z); a != 0 && open[a] {
				return true
			}
		}
	}
}

// textBuilder accumulates visible text with whitespace collapsed on the fly
// and stops accepting runes once more than limit runes were written.
type textBuilder struct {
	b       strings.Builder
	n       int  // runes written
	limit   int  // < 0: unlimited
	pending bool // a space is owed before the next visible rune
}

// full reports whether more than limit runes have been written.
func (t *textBuilder) full() bool { return t.limit >= 0 && t.n > t.limit }

// space records a word break; leading and repeated breaks are dropped.
func (t *textBuilder) space() {
	if t.n > 0 {
		t.pending = true
	}
}

// write appends s, collapsing whitespace and control characters.
func (t *textBuilder) write(s string) {
	for _, r := range s {
		if t.full() {
			return
		}
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			t.space()
			continue
		}
		if t.pending {
			t.b.WriteByte(' ')
			t.n++
			t.pending = false
		}
		t.b.WriteRune(r)
		t.n++
	}
}

// visibleText extracts the text a browser would show for the HTML fragment
// s: tags are dropped, entities decoded, whitespace collapsed and invisible
// elements (script, style, svg, …) skipped. With limit >= 0 extraction stops
// once more than limit runes were collected.
func visibleText(s string, limit int) string {
	t := textBuilder{limit: limit}
	z := xhtml.NewTokenizer(strings.NewReader(s))
	hidden := 0
	for !t.full() {
		switch z.Next() {
		case xhtml.ErrorToken:
			return t.b.String()
		case xhtml.TextToken:
			if hidden == 0 {
				t.write(string(z.Text()))
			}
		case xhtml.StartTagToken:
			a := tagAtom(z)
			if hiddenElements[a] {
				hidden++
			} else if breakElements[a] {
				t.space()
			}
		case xhtml.EndTagToken:
			a := tagAtom(z)
			if hiddenElements[a] {
				if hidden > 0 {
					hidden--
				}
			} else if breakElements[a] {
				t.space()
			}
		case xhtml.SelfClosingTagToken:
			if breakElements[tagAtom(z)] {
				t.space()
			}
		}
	}
	return t.b.String()
}

// tagAtom returns the atom of the current tag token.
func tagAtom(z *xhtml.Tokenizer) atom.Atom {
	name, _ := z.TagName()
	return atom.Lookup(name)
}

// excerpt is Excerpt: visible text truncated to n runes plus an ellipsis.
func excerpt(s string, n int) string {
	if n <= 0 {
		return ""
	}
	text := visibleText(s, n)
	if utf8.RuneCountInString(text) <= n {
		return text
	}
	cut := 0
	for i := range text {
		if n == 0 {
			cut = i
			break
		}
		n--
	}
	return strings.TrimRightFunc(text[:cut], unicode.IsSpace) + ellipsis
}

// plainText turns a title-like field of unknown kind (RSS titles, author
// names) into plain text: see titleText with kindMaybeHTML.
func plainText(s string) string {
	return titleText(s, kindMaybeHTML)
}

// titleText turns a title-like field into plain text of at most
// maxTitleRunes runes, whitespace collapsed and "…" ending a cut. Markup is
// stripped (entities decoded, invisible elements dropped) when kind is
// kindHTML, or kindMaybeHTML and titleLooksLikeHTML; otherwise the text is
// kept literally ("if x<y then", "Vec<T>" stay intact).
func titleText(s string, kind textKind) string {
	var text string
	if kind == kindHTML || kind == kindMaybeHTML && titleLooksLikeHTML(s) {
		text = visibleText(s, maxTitleRunes)
	} else {
		t := textBuilder{limit: maxTitleRunes}
		t.write(s)
		text = t.b.String()
	}
	return clipRunes(text, maxTitleRunes)
}

// clipRunes returns s when it has at most n runes, else its first n-1 runes
// (trailing space trimmed) followed by an ellipsis: at most n runes in all.
func clipRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	runes, cut := 0, 0
	for i := range s {
		if runes == n-1 {
			cut = i
		}
		if runes == n {
			return strings.TrimRightFunc(s[:cut], unicode.IsSpace) + ellipsis
		}
		runes++
	}
	return s
}

// clipUTF8 returns the longest prefix of s of at most n bytes that does not
// end inside a UTF-8 sequence.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for i := 0; i < utf8.UTFMax-1 && n > 0 && !utf8.RuneStart(s[n]); i++ {
		n--
	}
	return s[:n]
}

// textHTML is textToHTML for a feed field, bounded like sanitizeHTML: only
// the first maxFieldBytes of s are used, and when escaping makes the result
// outgrow renderLimit, the text is cut to fallbackTextRunes runes.
func textHTML(s string) string {
	s = clipUTF8(s, maxFieldBytes)
	out := textToHTML(s)
	if len(out) > renderLimit(len(s)) {
		out = textToHTML(clipRunes(s, fallbackTextRunes))
	}
	return out
}

// textToHTML renders plain text as safe HTML: everything is escaped, line
// breaks become <br> and runs of blank lines are collapsed to one.
func textToHTML(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	s = strings.Map(func(r rune) rune {
		if r != '\n' && r != '\t' && unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var b strings.Builder
	blank := 0
	for i, line := range strings.Split(s, "\n") {
		line = strings.TrimRightFunc(line, unicode.IsSpace)
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		if i > 0 {
			b.WriteString("<br>")
		}
		b.WriteString(html.EscapeString(line))
	}
	return b.String()
}

// firstImageSrc returns the src of the first <img> in an HTML fragment, or "".
func firstImageSrc(s string) string {
	src, _ := firstImage(s, func(string) bool { return true })
	return src
}

// firstImage returns the src of the first <img> in an HTML fragment that ok
// accepts.
func firstImage(s string, ok func(src string) bool) (string, bool) {
	if !strings.Contains(s, "<img") {
		return "", false
	}
	z := xhtml.NewTokenizer(strings.NewReader(s))
	for {
		switch z.Next() {
		case xhtml.ErrorToken:
			return "", false
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) != "img" {
				continue
			}
			for hasAttr {
				var key, val []byte
				key, val, hasAttr = z.TagAttr()
				if string(key) == "src" {
					if src := string(val); ok(src) {
						return src, true
					}
					break
				}
			}
		}
	}
}
