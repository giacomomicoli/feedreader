package parse

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/giacomomicoli/feedreader/internal/config"
)

func TestExcerpt_VisibleTextOnly(t *testing.T) {
	cases := []struct {
		name, in string
		n        int
		want     string
	}{
		{"tags stripped", `<p>Hello <b>bold</b> world</p>`, 100, "Hello bold world"},
		{"entities decoded", `Fish &amp; chips &lt;3 &#8364;5 &nbsp;now`, 100, "Fish & chips <3 €5 now"},
		{"whitespace collapsed", "  a \n\n\t b   c  ", 100, "a b c"},
		{"block boundaries separate words", `<p>one</p><p>two</p><ul><li>three</li><li>four</li></ul>a<br>b`, 100, "one two three four a b"},
		{"inline boundaries do not", `in<b>line</b>`, 100, "inline"},
		{"invisible elements skipped", `<style>p{}</style><script>var x="<p>"</script>visible<template>t</template><svg><text>s</text></svg><!-- c -->`, 100, "visible"},
		{"exact length not truncated", "abcde", 5, "abcde"},
		{"truncated with ellipsis", "abcdef", 5, "abcde…"},
		{"trailing space trimmed before ellipsis", "abcd efgh", 5, "abcd…"},
		{"runes never split", "àèìòù€😀ab", 6, "àèìòù€…"},
		{"emoji counted as one rune", "😀😀😀", 2, "😀😀…"},
		{"zero n", "abc", 0, ""},
		{"negative n", "abc", -1, ""},
		{"empty", "", 10, ""},
		{"plain text input", "just text", 100, "just text"},
		{"unclosed markup", `<p>open <b>never closed`, 100, "open never closed"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Excerpt(c.in, c.n)
			checkEqual(t, "Excerpt", got, c.want)
			if !utf8.ValidString(got) {
				t.Errorf("invalid UTF-8: %q", got)
			}
		})
	}
}

func TestExcerpt_SummaryMaxChars(t *testing.T) {
	long := "<p>" + strings.Repeat("é", config.SummaryMaxChars+50) + "</p>"
	got := Excerpt(long, config.SummaryMaxChars)
	if n := utf8.RuneCountInString(got); n != config.SummaryMaxChars+1 {
		t.Errorf("rune count = %d, want %d (+ ellipsis)", n, config.SummaryMaxChars+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("missing ellipsis: %q", got)
	}
	short := "<p>" + strings.Repeat("x", config.SummaryMaxChars) + "</p>"
	if got := Excerpt(short, config.SummaryMaxChars); strings.HasSuffix(got, "…") {
		t.Errorf("exactly %d runes must not be truncated", config.SummaryMaxChars)
	}
}

func TestPlainText(t *testing.T) {
	cases := map[string]string{
		"Simple":                        "Simple",
		"  multi \n  line\ttitle ":      "multi line title",
		"AT&amp;T":                      "AT&T",
		"<b>Bold</b> move":              "Bold move",
		"a < b and c > d":               "a < b and c > d",
		"x<y":                           "x<y",
		"<script>alert(1)</script>Safe": "Safe",
		"Tom & Jerry":                   "Tom & Jerry",
		"ctrl\x00chars\x1f":             "ctrl chars",
	}
	for in, want := range cases {
		checkEqual(t, "plainText("+in+")", plainText(in), want)
	}
}

func TestTextToHTML(t *testing.T) {
	cases := map[string]string{
		"":                   "",
		"plain":              "plain",
		`<b>"x" & 'y'</b>`:   "&lt;b&gt;&#34;x&#34; &amp; &#39;y&#39;&lt;/b&gt;",
		"a\nb":               "a<br>b",
		"a\r\nb\rc":          "a<br>b<br>c",
		"a\n\n\n\nb":         "a<br><br>b",
		"  \n trimmed  \n  ": "trimmed",
		"bell\x07":           "bell",
	}
	for in, want := range cases {
		checkEqual(t, "textToHTML("+in+")", textToHTML(in), want)
	}
}

func TestLooksLikeHTML(t *testing.T) {
	html := []string{"<p>x</p>", "a <br/> b", "<img src=x>", "AT&amp;T", "&#8364;", "&#x20AC;", `<a href="x">`}
	text := []string{"plain", "a < b", "x<y", "Tom & Jerry", "1 > 0", "&", "<3", "email <me@example.com"}
	for _, s := range html {
		if !looksLikeHTML(s) {
			t.Errorf("looksLikeHTML(%q) = false", s)
		}
	}
	for _, s := range text {
		if looksLikeHTML(s) {
			t.Errorf("looksLikeHTML(%q) = true", s)
		}
	}
}

func TestTitleLooksLikeHTML(t *testing.T) {
	markup := []string{"<b>Bold</b> move", "<script>alert(1)</script>", "AT&amp;T", "&#8364;5", "x <em>y</em>"}
	text := []string{"Vec<T> vs Box<T>", "The <dialog> element", "if a<b and b>c then", "List<A> </B>",
		"Tom & Jerry", "<3", "email <me@example.com>", "<T>x</T>"}
	for _, s := range markup {
		if !titleLooksLikeHTML(s) {
			t.Errorf("titleLooksLikeHTML(%q) = false", s)
		}
	}
	for _, s := range text {
		if titleLooksLikeHTML(s) {
			t.Errorf("titleLooksLikeHTML(%q) = true", s)
		}
	}
}

func TestTitleText(t *testing.T) {
	cases := []struct {
		in   string
		kind textKind
		want string
	}{
		{"  a \n b ", kindText, "a b"},
		{"<b>x</b> &amp;", kindText, "<b>x</b> &amp;"},
		{"<b>x</b> &amp;", kindHTML, "x &"},
		{"<b>x</b> &amp;", kindMaybeHTML, "x &"},
		{"Vec<T>", kindMaybeHTML, "Vec<T>"},
		{"<script>gone</script>", kindHTML, ""},
		{strings.Repeat("a", maxTitleRunes), kindText, strings.Repeat("a", maxTitleRunes)},
		{strings.Repeat("a", maxTitleRunes+1), kindText, strings.Repeat("a", maxTitleRunes-1) + "…"},
		{"<p>" + strings.Repeat("ü ", maxTitleRunes) + "</p>", kindHTML, strings.Repeat("ü ", maxTitleRunes/2-1) + "ü…"},
	}
	for _, c := range cases {
		got := titleText(c.in, c.kind)
		checkEqual(t, fmt.Sprintf("titleText(%.30q, %d)", c.in, c.kind), got, c.want)
		if n := utf8.RuneCountInString(got); n > maxTitleRunes {
			t.Errorf("titleText(%.30q) has %d runes", c.in, n)
		}
	}
}

func TestClipRunesAndUTF8(t *testing.T) {
	checkEqual(t, "clipRunes fits", clipRunes("abc", 3), "abc")
	checkEqual(t, "clipRunes cut", clipRunes("abcd", 3), "ab…")
	checkEqual(t, "clipRunes space trimmed", clipRunes("a bcd", 3), "a…")
	checkEqual(t, "clipRunes runes", clipRunes("éèàù", 3), "éè…")
	checkEqual(t, "clipRunes zero", clipRunes("abc", 0), "")
	checkEqual(t, "clipUTF8 fits", clipUTF8("abc", 3), "abc")
	checkEqual(t, "clipUTF8 boundary", clipUTF8("aé", 2), "a")
	checkEqual(t, "clipUTF8 emoji", clipUTF8("a😀", 4), "a")
	checkEqual(t, "clipUTF8 exact", clipUTF8("a😀", 5), "a😀")
}

func TestText_KeepsLineAndParagraphBreaks(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"br is a line break", "one<br>two<br/>three</br>four", "one\ntwo\nthree\nfour"},
		{"two br leave a blank line", "one<br><br>two", "one\n\ntwo"},
		{"runs of blank lines collapsed", "one<br><br><br><br>two", "one\n\ntwo"},
		{"paragraphs set off by a blank line", "<p>one</p><p>two</p>", "one\n\ntwo"},
		{"blocks without margins start a line", "<div>one</div><div>two</div>three", "one\ntwo\nthree"},
		{"list items one per line", "<p>Intro</p><ul><li>a</li><li>b</li></ul><p>End</p>", "Intro\n\na\nb\n\nEnd"},
		{"headings", "<h2>Title</h2>text", "Title\n\ntext"},
		{"table rows and cells", "<table><tr><td>a</td><td>b</td></tr><tr><th>c</th><td>d</td></tr></table>", "a b\nc d"},
		{"br inside a paragraph", "<p>one<br></p><p>two</p>", "one\n\ntwo"},
		{"other whitespace collapsed", "  a \n\n\t b   c  ", "a b c"},
		{"no spaces around breaks", "a  <br>  b \n<p> c </p>", "a\nb\n\nc"},
		{"leading and trailing breaks dropped", "<br><p></p>\n<p>x</p><br><br>", "x"},
		{"preformatted newlines kept", "<pre>if x {\n  y()\n\n\n\n}</pre>after", "if x {\ny()\n\n}\n\nafter"},
		{"entities decoded", "Fish &amp; chips&#10;&lt;3 &#8364;5&nbsp;now", "Fish & chips <3 €5 now"},
		{"invisible elements dropped", `<style>p{}</style><script>var x="<br>"</script>visible<template><p>t</p></template><svg><text>s</text></svg><!-- <br> --><br>`, "visible"},
		{"inline markup joins words", "in<b>line</b> <a href=x>link</a>", "inline link"},
		{"images separate words", `a<img src="x">b`, "a b"},
		{"control characters", "a\x00b\x1fc\rd", "a b c d"},
		{"plain text", "just text", "just text"},
		{"empty", "", ""},
		{"unclosed markup", "<p>open <b>never<br>closed", "open never\nclosed"},
		// A YouTube description: plain text stored with <br> line breaks.
		{"text field", textToHTML("Line one\nLine two\n\n\n\nChapters:\n0:00 Intro"), "Line one\nLine two\n\nChapters:\n0:00 Intro"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Text(c.in, len(c.in)+1)
			checkEqual(t, "Text", got, c.want)
			if !utf8.ValidString(got) {
				t.Errorf("invalid UTF-8: %q", got)
			}
		})
	}
}

func TestText_Truncation(t *testing.T) {
	cases := []struct {
		name, in string
		n        int
		want     string
	}{
		{"exact length not truncated", "abc<br>de", 6, "abc\nde"},
		{"truncated with ellipsis", "abc<br>def", 6, "abc\nde…"},
		{"break before the cut trimmed", "abc<br>def", 4, "abc…"},
		{"paragraph break counts two runes", "<p>ab</p><p>cd</p>", 5, "ab\n\nc…"},
		{"runes never split", "àè<br>😀ab", 4, "àè\n😀…"},
		{"zero n", "abc", 0, ""},
		{"negative n", "abc", -1, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			checkEqual(t, "Text", Text(c.in, c.n), c.want)
		})
	}
}

func TestText_SummaryFullMaxChars(t *testing.T) {
	if config.SummaryFullMaxChars <= config.SummaryMaxChars {
		t.Fatalf("SummaryFullMaxChars %d must exceed the excerpt length %d", config.SummaryFullMaxChars, config.SummaryMaxChars)
	}
	long := "<p>" + strings.Repeat("é", config.SummaryFullMaxChars+50) + "</p>"
	got := Text(long, config.SummaryFullMaxChars)
	if n := utf8.RuneCountInString(got); n != config.SummaryFullMaxChars+1 {
		t.Errorf("rune count = %d, want %d (+ ellipsis)", n, config.SummaryFullMaxChars+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("missing ellipsis: …%q", got[max(0, len(got)-40):])
	}
	// Exactly SummaryFullMaxChars runes, line breaks included.
	exact := strings.Repeat("x\n", config.SummaryFullMaxChars/2)
	exact = exact[:config.SummaryFullMaxChars-1] + "z"
	if got := Text(textToHTML(exact), config.SummaryFullMaxChars); got != exact {
		t.Errorf("%d runes must not be truncated: …%q", config.SummaryFullMaxChars, got[max(0, len(got)-10):])
	}
}

// TestText_HostileInputIsBounded: Text renders stored summaries, which feeds
// control, and these are at most renderLimit(maxFieldBytes) bytes long.
// Whatever their markup, the output stays within the limit and the work is
// one pass over the input that stops at the limit; the tokenizer's buffer
// for one huge text token is the largest allocation.
func TestText_HostileInputIsBounded(t *testing.T) {
	const maxAlloc = 4 << 20
	size := renderLimit(maxFieldBytes)
	fill := func(unit string) string { return strings.Repeat(unit, size/len(unit)) }
	half := func(unit string) string { return strings.Repeat(unit, size/2/len(unit)) }
	cases := map[string]string{
		"only breaks":       fill("<br>"),
		"breaks and words":  fill("x<br><br><br>"),
		"deep nesting":      half("<div><p>") + "x" + half("</p></div>"),
		"unclosed script":   "<script>" + fill("<p>x"),
		"unclosed pre":      "<pre>" + half("\n") + "x" + half("\n") + "y",
		"one long word":     fill("x"),
		"escaped markup":    fill("&lt;script&gt;alert(1)&lt;/script&gt;"),
		"invalid encoding":  fill("\xff\xfe<br>"),
		"many entity names": fill("&NotEqualTilde;"),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var got string
			if alloc := allocated(func() { got = Text(in, config.SummaryFullMaxChars) }); alloc > maxAlloc {
				t.Errorf("allocated %d KB for %d KB of input", alloc>>10, len(in)>>10)
			}
			if n := utf8.RuneCountInString(got); n > config.SummaryFullMaxChars+1 {
				t.Errorf("%d runes", n)
			}
			if !utf8.ValidString(got) || strings.Contains(got, "\n\n\n") {
				t.Errorf("output %.80q", got)
			}
		})
	}
}

// TestText_SameWordsAsExcerpt: Text only adds line breaks to what Excerpt
// extracts.
func TestText_SameWordsAsExcerpt(t *testing.T) {
	for _, in := range []string{
		"<p>one <b>two</b></p><ul><li>three<li>four</ul><pre>five\nsix</pre>seven<br>eight",
		textToHTML("a\n\nb\nc"),
		`<table><tr><td>x</td></tr></table><img src="y">z`,
	} {
		lines := Text(in, len(in)+1)
		checkEqual(t, "joined lines", strings.Join(strings.Fields(lines), " "), Excerpt(in, len(in)+1))
	}
}
