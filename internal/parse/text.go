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

// textBreak is the kind of break an element's tags leave in extracted text.
// Excerpt turns every kind into a space; Text keeps line breaks.
type textBreak int

const (
	// wordBreak is a space: images, table cells.
	wordBreak textBreak = iota + 1
	// newLine is one more line break: <br>.
	newLine
	// lineBreak starts a new line: blocks without vertical margins, such
	// as <div>, <li> and <tr>.
	lineBreak
	// paragraphBreak leaves a blank line: blocks with vertical margins,
	// such as <p>, lists and headings.
	paragraphBreak
)

// maxNewlines is the most line breaks Text writes in a row: one blank line.
const maxNewlines = 2

// breakElements are elements that separate words visually, so their tags
// become a break in extracted text ("<p>a</p><p>b</p>" → "a b" in Excerpt,
// "a\n\nb" in Text). The kind follows a browser's default rendering.
var breakElements = map[atom.Atom]textBreak{
	atom.Img: wordBreak, atom.Td: wordBreak, atom.Th: wordBreak,

	atom.Br: newLine,

	atom.Address: lineBreak, atom.Article: lineBreak, atom.Aside: lineBreak,
	atom.Caption: lineBreak, atom.Dd: lineBreak, atom.Details: lineBreak,
	atom.Div: lineBreak, atom.Dt: lineBreak, atom.Figcaption: lineBreak,
	atom.Footer: lineBreak, atom.Header: lineBreak, atom.Li: lineBreak,
	atom.Main: lineBreak, atom.Nav: lineBreak, atom.Section: lineBreak,
	atom.Summary: lineBreak, atom.Table: lineBreak, atom.Tr: lineBreak,

	atom.Blockquote: paragraphBreak, atom.Dl: paragraphBreak, atom.Figure: paragraphBreak,
	atom.H1: paragraphBreak, atom.H2: paragraphBreak, atom.H3: paragraphBreak,
	atom.H4: paragraphBreak, atom.H5: paragraphBreak, atom.H6: paragraphBreak,
	atom.Hr: paragraphBreak, atom.Ol: paragraphBreak, atom.P: paragraphBreak,
	atom.Pre: paragraphBreak, atom.Ul: paragraphBreak,
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
// and stops accepting runes once more than limit runes were written. Breaks
// are only written in front of the next visible rune, so leading and
// trailing ones are dropped and the strongest of a run wins.
type textBuilder struct {
	b        strings.Builder
	n        int  // runes written
	limit    int  // < 0: unlimited
	lines    bool // write owed line breaks as such, not as one space
	pending  bool // a space is owed before the next visible rune
	newlines int  // line breaks owed before the next visible rune
}

// full reports whether more than limit runes have been written.
func (t *textBuilder) full() bool { return t.limit >= 0 && t.n > t.limit }

// space records a word break; leading and repeated breaks are dropped.
func (t *textBuilder) space() {
	if t.n > 0 {
		t.pending = true
	}
}

// brk records a break of kind k. Line breaks add up to at most
// maxNewlines, so runs of blank lines collapse to one.
func (t *textBuilder) brk(k textBreak) {
	if t.n == 0 {
		return
	}
	switch k {
	case wordBreak:
		t.pending = true
	case newLine:
		t.newlines = min(t.newlines+1, maxNewlines)
	case lineBreak:
		t.newlines = max(t.newlines, 1)
	case paragraphBreak:
		t.newlines = maxNewlines
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
		switch {
		case t.lines && t.newlines > 0:
			for range t.newlines {
				t.b.WriteByte('\n')
			}
			t.n += t.newlines
		case t.pending || t.newlines > 0:
			t.b.WriteByte(' ')
			t.n++
		}
		t.pending, t.newlines = false, 0
		t.b.WriteRune(r)
		t.n++
	}
}

// visibleText extracts the text a browser would show for the HTML fragment
// s: tags are dropped, entities decoded, whitespace collapsed and invisible
// elements (script, style, svg, …) skipped. The breaks of breakElements and
// the newlines inside <pre> become a space, or line breaks with lines set.
// With limit >= 0 extraction stops once more than limit runes were
// collected.
func visibleText(s string, limit int, lines bool) string {
	t := textBuilder{limit: limit, lines: lines}
	z := xhtml.NewTokenizer(strings.NewReader(s))
	hidden, pre := 0, 0
	for !t.full() {
		switch z.Next() {
		case xhtml.ErrorToken:
			return t.b.String()
		case xhtml.TextToken:
			if hidden == 0 {
				writeText(&t, string(z.Text()), pre > 0)
			}
		case xhtml.StartTagToken:
			a := tagAtom(z)
			if hiddenElements[a] {
				hidden++
				break
			}
			if a == atom.Pre {
				pre++
			}
			t.brk(breakElements[a])
		case xhtml.EndTagToken:
			a := tagAtom(z)
			if hiddenElements[a] {
				if hidden > 0 {
					hidden--
				}
				break
			}
			if a == atom.Pre && pre > 0 {
				pre--
			}
			t.brk(breakElements[a]) // "</br>" is a line break too
		case xhtml.SelfClosingTagToken:
			t.brk(breakElements[tagAtom(z)])
		}
	}
	return t.b.String()
}

// writeText writes a text token; in preformatted text its newlines are line
// breaks.
func writeText(t *textBuilder, s string, preformatted bool) {
	for preformatted {
		line, rest, found := strings.Cut(s, "\n")
		if !found {
			break
		}
		t.write(line)
		t.brk(newLine)
		s = rest
	}
	t.write(s)
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
	return truncate(visibleText(s, n, false), n)
}

// textLines is Text: visible text with its line breaks, truncated to n runes
// plus an ellipsis.
func textLines(s string, n int) string {
	if n <= 0 {
		return ""
	}
	return truncate(visibleText(s, n, true), n)
}

// truncate returns text when it has at most n runes, else its first n runes,
// trailing whitespace trimmed, followed by an ellipsis.
func truncate(text string, n int) string {
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
		text = visibleText(s, maxTitleRunes, false)
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
