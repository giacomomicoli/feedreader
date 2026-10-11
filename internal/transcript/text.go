package transcript

import (
	"html"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ellipsis ends a transcript that was cut at the limit.
const ellipsis = "…"

// speakerMarker starts a cue spoken by someone new, by caption convention
// (automatic captions also flag it).
const speakerMarker = ">>"

// paragraphBreak separates paragraphs: one blank line.
const paragraphBreak = "\n\n"

// sentenceEnds are runes that end a sentence, a paragraph may end after.
const sentenceEnds = ".!?…。！？"

// formattingTags are the styling tags uploaded caption files may carry
// (SRT and WebVTT: <i>, <b>, <u>, <font color=…>, <c.class>, <v Name>, …),
// matched case-insensitively. Other text in angle brackets ("Vec<T>",
// "<script>") is kept as it is: the text is plain and always rendered
// escaped.
var formattingTags = []string{"b", "i", "u", "s", "c", "v", "font", "lang", "ruby", "rt", "span"}

// textBuilder accumulates the plain text of caption cues, at most limit
// runes kept (see String). Within a cue segments are concatenated; cues are
// joined by a space, or by a paragraph break at a change of speaker or once
// the paragraph is long (paragraphSoftRunes after the end of a sentence,
// paragraphHardRunes regardless). Whitespace and control characters
// collapse into single spaces, and invisible format characters are dropped
// (see dropped), so cue text cannot reorder or hide what follows.
type textBuilder struct {
	b        strings.Builder
	limit    int
	n        int  // runes written
	para     int  // runes written in the current paragraph
	last     rune // the last rune written
	space    bool // a space is owed before the next visible rune
	cueStart bool // the next visible rune is the first of a cue
	speaker  bool // the current cue starts with a change of speaker
}

func newTextBuilder(limit int) *textBuilder { return &textBuilder{limit: limit} }

// full reports whether more than limit runes have been written; nothing
// more is then accepted.
func (t *textBuilder) full() bool { return t.n > t.limit }

// startCue begins a new cue.
func (t *textBuilder) startCue() {
	t.cueStart, t.speaker = true, false
	if t.n > 0 {
		t.space = true
	}
}

// writeSegment appends the text of one segment of the current cue;
// speaker marks a change of speaker.
func (t *textBuilder) writeSegment(s string, speaker bool) {
	if t.full() {
		return
	}
	if speaker {
		t.speaker = true
	}
	s = cleanSegment(clipUTF8(s, t.budget()))
	if t.cueStart && strings.HasPrefix(strings.TrimLeftFunc(s, invisible), speakerMarker) {
		t.speaker = true
	}
	for _, r := range s {
		if t.full() {
			return
		}
		if invisible(r) {
			if t.n > 0 && !dropped(r) {
				t.space = true
			}
			continue
		}
		switch {
		case t.n == 0:
		case t.cueStart && t.newParagraph():
			t.b.WriteString(paragraphBreak)
			t.n += utf8.RuneCountInString(paragraphBreak)
			t.para = 0
		case t.space:
			t.b.WriteByte(' ')
			t.n++
			t.para++
		}
		t.cueStart, t.space = false, false
		t.b.WriteRune(r)
		t.n++
		t.para++
		t.last = r
	}
}

// newParagraph reports whether the cue starting now begins a paragraph.
func (t *textBuilder) newParagraph() bool {
	switch {
	case t.speaker, t.para >= paragraphHardRunes:
		return true
	default:
		return t.para >= paragraphSoftRunes && strings.ContainsRune(sentenceEnds, t.last)
	}
}

// budget is how many bytes of a segment can still matter: every rune left
// in the limit at its longest. The rest is not decoded or cleaned.
func (t *textBuilder) budget() int {
	return (t.limit - t.n + 1) * utf8.UTFMax
}

// String returns the text: when more than limit runes were written, the
// first limit-1 of them, trailing whitespace trimmed, and an ellipsis.
func (t *textBuilder) String() string {
	s := t.b.String()
	if !t.full() {
		return s
	}
	if t.limit <= 0 {
		return ""
	}
	runes := 0
	for i := range s {
		if runes == t.limit-1 {
			return strings.TrimRightFunc(s[:i], unicode.IsSpace) + ellipsis
		}
		runes++
	}
	return s
}

// cleanSegment turns segment text into plain text: formatting tags are
// dropped, then character references decoded ("&amp;" → "&"), the order
// that undoes markup escaped inside it.
func cleanSegment(s string) string {
	return html.UnescapeString(stripFormatting(s))
}

// stripFormatting drops the formattingTags of s, in one pass.
func stripFormatting(s string) string {
	i := strings.IndexByte(s, '<')
	if i < 0 {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for ; i >= 0; i = strings.IndexByte(s, '<') {
		b.WriteString(s[:i])
		s = s[i:]
		n := formattingTagLen(s)
		if n == 0 {
			b.WriteByte('<')
			n = 1
		}
		s = s[n:]
	}
	b.WriteString(s)
	return b.String()
}

// formattingTagLen returns the length of the start or end tag of one of
// formattingTags that s starts with, or 0: "<i>", "</font>",
// "<font color=\"#fff\">", "<c.yellow>", "<v Narrator>".
func formattingTagLen(s string) int {
	name := 1 // after '<'
	if name < len(s) && s[name] == '/' {
		name++
	}
	end := name
	for end < len(s) && ('a' <= s[end]|0x20 && s[end]|0x20 <= 'z') {
		end++
	}
	if !slices.ContainsFunc(formattingTags, func(t string) bool { return strings.EqualFold(t, s[name:end]) }) {
		return 0
	}
	rest := strings.IndexAny(s[end:], "<>")
	switch {
	case rest < 0 || s[end+rest] != '>':
		return 0 // not closed before the next tag
	case rest > 0 && !strings.ContainsRune(" \t\n\r\f.", rune(s[end])):
		return 0 // a longer name, such as <script>
	}
	return end + rest + 1
}

// invisible reports whether r is whitespace, a control character or a
// character dropped from the text: never written as is.
func invisible(r rune) bool {
	return unicode.IsSpace(r) || unicode.IsControl(r) || dropped(r)
}

// Zero-width non-joiner and joiner: format characters that some scripts
// (Persian, Indic scripts, emoji sequences) need to render words right.
const (
	zwnj = '\u200c'
	zwj  = '\u200d'
)

// Emoji tag characters (U+E0020–U+E007F): the format characters that spell
// a subdivision flag (England, Scotland, Wales) after U+1F3F4.
const (
	firstTag = '\U000e0020'
	lastTag  = '\U000e007f'
)

// prependedMarks are the format characters that show a mark over the
// number following them: Arabic number signs (U+0600–U+0605, U+0890,
// U+0891, U+08E2), the Arabic end of ayah (U+06DD), the Syriac abbreviation
// mark (U+070F) and the Kaithi number signs (U+110BD, U+110CD).
var prependedMarks = unicode.RangeTable{
	R16: []unicode.Range16{
		{Lo: 0x0600, Hi: 0x0605, Stride: 1},
		{Lo: 0x06dd, Hi: 0x070f, Stride: 0x070f - 0x06dd},
		{Lo: 0x0890, Hi: 0x0891, Stride: 1},
		{Lo: 0x08e2, Hi: 0x08e2, Stride: 1},
	},
	R32: []unicode.Range32{
		{Lo: 0x110bd, Hi: 0x110cd, Stride: 0x110cd - 0x110bd},
	},
}

// dropped reports whether r is left out of the text altogether: an
// invisible format character (Unicode category Cf: zero-width spaces, word
// joiners, soft hyphens, byte order marks, direction marks, interlinear
// annotation, …), including the bidirectional controls, which could
// reorder the text displayed after them. The format characters that change
// what is shown are kept: ZWNJ and ZWJ, prependedMarks and the emoji tag
// characters.
func dropped(r rune) bool {
	switch {
	case bidiControl(r):
		return true
	case !unicode.Is(unicode.Cf, r), r == zwnj, r == zwj, firstTag <= r && r <= lastTag:
		return false
	}
	return !unicode.Is(&prependedMarks, r)
}

// bidiControl reports whether r is a bidirectional embedding, override or
// isolate control (U+202A–U+202E, U+2066–U+2069), which could reorder the
// text displayed after it.
func bidiControl(r rune) bool {
	return '\u202a' <= r && r <= '\u202e' || '\u2066' <= r && r <= '\u2069'
}

// clipUTF8 returns the longest prefix of s of at most n bytes that does not
// end inside a UTF-8 sequence.
func clipUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	if n <= 0 {
		return ""
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}
