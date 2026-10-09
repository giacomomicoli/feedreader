package parse

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	xhtml "golang.org/x/net/html"

	"github.com/giacomomicoli/feedreader/internal/config"
)

// safeElements is the complete set of elements the policy may emit.
var safeElements = map[string]bool{
	"a": true, "abbr": true, "acronym": true, "article": true, "aside": true,
	"b": true, "bdi": true, "bdo": true, "blockquote": true, "br": true,
	"caption": true, "cite": true, "code": true, "col": true, "colgroup": true,
	"dd": true, "del": true, "details": true, "dfn": true, "div": true,
	"dl": true, "dt": true, "em": true, "figcaption": true, "figure": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"hgroup": true, "hr": true, "i": true, "img": true, "ins": true,
	"kbd": true, "li": true, "mark": true, "ol": true, "p": true, "pre": true,
	"q": true, "rp": true, "rt": true, "ruby": true, "s": true, "samp": true,
	"section": true, "small": true, "span": true, "strike": true,
	"strong": true, "sub": true, "summary": true, "sup": true, "table": true,
	"tbody": true, "td": true, "tfoot": true, "th": true, "thead": true,
	"time": true, "tr": true, "tt": true, "u": true, "ul": true, "var": true,
	"wbr": true,
}

// forbiddenAttrs must never be emitted (event handlers are checked by
// prefix).
var forbiddenAttrs = map[string]bool{
	"style": true, "id": true, "class": true, "srcset": true, "formaction": true,
	"action": true, "background": true, "poster": true, "data": true,
	"xmlns": true, "http-equiv": true, "content": true, "name": true,
}

// assertSafeHTML re-tokenizes sanitizer output and checks every element,
// attribute and URL against the allow-list, independently of bluemonday.
func assertSafeHTML(t *testing.T, s string) {
	t.Helper()
	z := xhtml.NewTokenizer(strings.NewReader(s))
	for {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			return
		}
		if tt != xhtml.StartTagToken && tt != xhtml.SelfClosingTagToken && tt != xhtml.EndTagToken {
			if tt == xhtml.CommentToken || tt == xhtml.DoctypeToken {
				t.Errorf("unexpected %v token in %q", tt, s)
			}
			continue
		}
		tok := z.Token()
		if !safeElements[tok.Data] {
			t.Errorf("element <%s> not allowed in %q", tok.Data, s)
		}
		for _, a := range tok.Attr {
			key := strings.ToLower(a.Key)
			if strings.HasPrefix(key, "on") || forbiddenAttrs[key] || a.Namespace != "" {
				t.Errorf("attribute %q not allowed on <%s> in %q", a.Key, tok.Data, s)
			}
			if key == "href" || key == "src" || key == "cite" {
				assertSafeURL(t, tok.Data, a.Val)
			}
		}
	}
}

// assertSafeURL requires an absolute http(s) URL (or mailto: on links).
func assertSafeURL(t *testing.T, element, raw string) {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Errorf("unparseable URL %q on <%s>", raw, element)
		return
	}
	switch {
	case (u.Scheme == "http" || u.Scheme == "https") && u.Host != "":
	case u.Scheme == "mailto" && element == "a":
	default:
		t.Errorf("unsafe URL %q on <%s>", raw, element)
	}
}

func TestSanitize_XSSCorpus(t *testing.T) {
	corpus := []string{
		`<script>alert(1)</script>`,
		`<SCRIPT SRC=https://evil.example.com/x.js></SCRIPT>`,
		`<scr<script>ipt>alert(1)</script>`,
		`<img src=x onerror=alert(1)>`,
		`<img src="x" onerror="alert(1)">`,
		`<img """><script>alert(1)</script>">`,
		`<IMG SRC=JaVaScRiPt:alert('XSS')>`,
		`<img src="javascript:alert(1)">`,
		`<img src="data:image/svg+xml;base64,PHN2ZyBvbmxvYWQ9YWxlcnQoMSk+">`,
		`<img src=x onerror="&#0000106&#0000097&#0000118&#0000097&#0000115&#0000099&#0000114&#0000105&#0000112&#0000116&#0000058alert(1)">`,
		`<img srcset="https://evil.example.com/a.png 1x" src="https://ok.example.com/b.png">`,
		`<a href="javascript:alert(1)">x</a>`,
		`<a href="JaVaScRiPt:alert(1)">x</a>`,
		`<a href=" javascript:alert(1)">x</a>`,
		`<a href="jav&#x09;ascript:alert(1)">x</a>`,
		`<a href="jav&#x0A;ascript:alert(1)">x</a>`,
		`<a href="&#106;&#97;&#118;&#97;&#115;&#99;&#114;&#105;&#112;&#116;&#58;alert(1)">x</a>`,
		`<a href="&#x6A;&#x61;&#x76;&#x61;&#x73;&#x63;&#x72;&#x69;&#x70;&#x74;&#x3A;alert(1)">x</a>`,
		`<a href="javascript&colon;alert(1)">x</a>`,
		`<a href="vbscript:msgbox(1)">x</a>`,
		`<a href="data:text/html;base64,PHNjcmlwdD5hbGVydCgxKTwvc2NyaXB0Pg==">x</a>`,
		`<a href="file:///etc/passwd">x</a>`,
		`<a href=https://ok.example.com/ onmouseover=alert(1)>x</a>`,
		`<a href="https://ok.example.com/" style="position:fixed;top:0;left:0;width:100%;height:100%">overlay</a>`,
		`<svg onload=alert(1)>`,
		`<svg><script>alert(1)</script></svg>`,
		`<svg><a xlink:href="javascript:alert(1)"><text>x</text></a></svg>`,
		`<svg><style><img src=x onerror=alert(1)></style></svg>`,
		`<math><mtext><table><mglyph><style><img src=x onerror=alert(1)>`,
		`<math href="javascript:alert(1)">click</math>`,
		`<iframe src="https://evil.example.com/"></iframe>`,
		`<iframe srcdoc="<script>alert(1)</script>"></iframe>`,
		`<object data="evil.swf"><param name=x value=y></object>`,
		`<embed src="evil.swf">`,
		`<form action="https://evil.example.com/"><input name="password" type="password"><button formaction="javascript:alert(1)">go</button></form>`,
		`<style>@import "https://evil.example.com/x.css";</style>`,
		`<p style="background:url(javascript:alert(1))">x</p>`,
		`<div style="behavior: url(x.htc)">x</div>`,
		`<link rel="stylesheet" href="https://evil.example.com/x.css">`,
		`<meta http-equiv="refresh" content="0;url=javascript:alert(1)">`,
		`<base href="https://evil.example.com/">`,
		`<body onload=alert(1)>`,
		`<details open ontoggle=alert(1)>`,
		`<video><source onerror="alert(1)"></video>`,
		`<audio src=x onerror=alert(1)>`,
		`<input autofocus onfocus=alert(1)>`,
		`<select autofocus onfocus=alert(1)>`,
		`<textarea autofocus onfocus=alert(1)>`,
		`<keygen autofocus onfocus=alert(1)>`,
		`<marquee onstart=alert(1)>`,
		`<isindex type=image src=1 onerror=alert(1)>`,
		`<table background="javascript:alert(1)"><tr><td>x</td></tr></table>`,
		`<blockquote cite="javascript:alert(1)">q</blockquote>`,
		`<noscript><p title="</noscript><img src=x onerror=alert(1)>">`,
		`<template><script>alert(1)</script></template>`,
		`<xmp><script>alert(1)</script></xmp>`,
		`<plaintext><script>alert(1)</script>`,
		`<!--<img src=x onerror=alert(1)>-->`,
		`<![CDATA[<script>alert(1)</script>]]>`,
		`<p id="sidebar-counts" class="evil">id clash</p>`,
		`<div id="x" hx-get="/feeds/1/delete" hx-trigger="load">htmx</div>`,
		`<a href="https://ok.example.com/" target="_self" rel="opener">x</a>`,
		`"><script>alert(1)</script>`,
		`'><img src=x onerror=alert(1)>`,
		`<<script>script>alert(1)</script>`,
		`<b>unclosed <i>tags <a href="https://ok.example.com/">everywhere`,
		`<img src="https://ok.example.com/a.png" width="100" height="50%" alt="ok">`,
	}
	for _, in := range corpus {
		out := Sanitize(in, "https://feed.example.com/blog/")
		assertSafeHTML(t, out)
		low := strings.ToLower(out)
		for _, bad := range []string{"<script", "javascript", "vbscript", "data:", "alert(1)>", "onerror=", "onload=", "<iframe", "<style", "<svg", "evil.example.com/x.css", "hx-get"} {
			if strings.Contains(low, bad) {
				t.Errorf("Sanitize(%q) = %q, contains %q", in, out, bad)
			}
		}
	}
}

func TestSanitize_ResolvesRelativeURLsBeforeSanitizing(t *testing.T) {
	const base = "https://example.com/blog/posts/feed.xml"
	cases := []struct{ in, want string }{
		{`<a href="/about">a</a>`, `href="https://example.com/about"`},
		{`<a href="next.html">n</a>`, `href="https://example.com/blog/posts/next.html"`},
		{`<a href="../up.html">u</a>`, `href="https://example.com/blog/up.html"`},
		{`<a href="//cdn.example.net/x">p</a>`, `href="https://cdn.example.net/x"`},
		{`<a href="?page=2">q</a>`, `href="https://example.com/blog/posts/feed.xml?page=2"`},
		{`<img src="img/a.png">`, `src="https://example.com/blog/posts/img/a.png"`},
		{`<img src="/a b.png">`, `src="https://example.com/a%20b.png"`},
		{`<blockquote cite="/source">q</blockquote>`, `cite="https://example.com/source"`},
		{`<a href="mailto:me@example.com">m</a>`, `href="mailto:me@example.com"`},
		{`<a href="https://other.example.org/x?a=1&amp;b=2">o</a>`, `href="https://other.example.org/x?a=1&amp;b=2"`},
	}
	for _, c := range cases {
		out := Sanitize(c.in, base)
		if !strings.Contains(out, c.want) {
			t.Errorf("Sanitize(%q) = %q, want it to contain %q", c.in, out, c.want)
		}
		assertSafeHTML(t, out)
	}

	t.Run("without a usable base relative URLs are dropped", func(t *testing.T) {
		for _, base := range []string{"", "relative/path", "javascript:alert(1)"} {
			out := Sanitize(`<a href="/x">link</a><img src="/i.png" alt="i">`, base)
			if out != "link" {
				t.Errorf("Sanitize with base %q = %q, want %q", base, out, "link")
			}
		}
	})
}

func TestSanitize_LinksGetNofollowNoopenerNoreferrerAndTargetBlank(t *testing.T) {
	out := Sanitize(`<a href="https://example.com/" rel="author opener" target="_self" referrerpolicy="unsafe-url">x</a>`, "")
	z := xhtml.NewTokenizer(strings.NewReader(out))
	if z.Next() != xhtml.StartTagToken {
		t.Fatalf("no start tag in %q", out)
	}
	tok := z.Token()
	attrs := map[string]string{}
	for _, a := range tok.Attr {
		attrs[a.Key] = a.Val
	}
	rel := strings.Fields(attrs["rel"])
	slices.Sort(rel)
	checkEqual(t, "rel", strings.Join(rel, " "), "nofollow noopener noreferrer")
	checkEqual(t, "target", attrs["target"], "_blank")
	if _, ok := attrs["referrerpolicy"]; ok {
		t.Errorf("referrerpolicy kept: %q", out)
	}
}

func TestSanitize_ImagesNeedAbsoluteHTTPSource(t *testing.T) {
	cases := map[string]string{
		`<img alt="no src">`:                                                     "",
		`<img src="data:image/png;base64,iVBORw0KGgo=">`:                         "",
		`<img src="javascript:alert(1)">`:                                        "",
		`<img src="ftp://example.com/a.png">`:                                    "",
		`<img src="/a.png" srcset="/b.png 2x" loading=lazy style="x" class="c">`: `<img src="https://example.com/a.png"/>`,
		`<p>before<img src="cid:123">after</p>`:                                  "<p>beforeafter</p>",
	}
	for in, want := range cases {
		checkEqual(t, "Sanitize("+in+")", Sanitize(in, "https://example.com/feed"), want)
	}
}

func TestSanitize_BalancesUnclosedTags(t *testing.T) {
	cases := map[string]string{
		`<b>bold`:                 `<b>bold</b>`,
		`<p>one<p>two`:            `<p>one</p><p>two</p>`,
		`<ul><li>a<li>b`:          `<ul><li>a</li><li>b</li></ul>`,
		`<table><tr><td>cell`:     `<table><tbody><tr><td>cell</td></tr></tbody></table>`,
		`</div></p>stray closers`: `<p></p>stray closers`,
		`<plaintext><b>x</b>`:     `<pre>&lt;b&gt;x&lt;/b&gt;</pre>`,
		`<blockquote>q <em>open`:  `<blockquote>q <em>open</em></blockquote>`,
	}
	for in, want := range cases {
		checkEqual(t, "Sanitize("+in+")", Sanitize(in, ""), want)
	}
	out := Sanitize(`<a href="https://example.com/">never closed`, "")
	if !strings.HasSuffix(out, "never closed</a>") {
		t.Errorf("unclosed link not closed: %q", out)
	}
}

func TestSanitize_KeepsSafeFormatting(t *testing.T) {
	in := `<h2 title="t">Head</h2><p dir="rtl" lang="en">Some <em>em</em> <strong>strong</strong> <code>code</code></p>` +
		`<ul><li>one</li></ul><ol><li>two</li></ol><blockquote>quote</blockquote><pre>pre  text</pre>` +
		`<table><thead><tr><th>h</th></tr></thead><tbody><tr><td>d</td></tr></tbody></table><hr><br>`
	out := Sanitize(in, "")
	for _, want := range []string{`<h2 title="t">Head</h2>`, `<p dir="rtl" lang="en">`, "<em>em</em>", "<strong>strong</strong>",
		"<code>code</code>", "<ul><li>one</li></ul>", "<ol><li>two</li></ol>", "<blockquote>quote</blockquote>",
		"<pre>pre  text</pre>", "<th>h</th>", "<td>d</td>", "<hr/>", "<br/>"} {
		if !strings.Contains(out, want) {
			t.Errorf("Sanitize dropped %q: %s", want, out)
		}
	}
	assertSafeHTML(t, out)
}

func TestSanitize_EmptyInput(t *testing.T) {
	for _, in := range []string{"", "   ", "\n\t", "<script>only()</script>", "<!-- just a comment -->"} {
		checkEqual(t, "Sanitize("+in+")", Sanitize(in, "https://example.com/"), "")
	}
}

func TestSanitize_DeepNestingFallsBackToText(t *testing.T) {
	in := strings.Repeat("<div>", 2000) + "deep <script>x()</script>" + strings.Repeat("</div>", 2000)
	out := Sanitize(in, "")
	if !strings.Contains(out, "deep") {
		t.Errorf("text lost: %.80q", out)
	}
	assertSafeHTML(t, out)
}

// formattingRun returns 3 copies of each formatting start tag the tree
// builder reconstructs.
func formattingRun() string {
	var b strings.Builder
	for _, tag := range []string{"b", "i", "u", "s", "em", "strong", "small", "big", "tt", "font", "nobr", "code", "strike"} {
		b.WriteString(strings.Repeat("<"+tag+">", 3))
	}
	return b.String()
}

func TestSanitize_FormattingAmplificationIsBounded(t *testing.T) {
	var distinct strings.Builder
	for i := range 480 {
		fmt.Fprintf(&distinct, "<b x=%d>", i)
	}
	cases := map[string]string{
		// 1 MB that the tree builder turned into an 86 MB summary (1.5 GB RSS).
		"formatting tags reopened in every paragraph": `<img src="/first.png"><p>` + formattingRun() + strings.Repeat("<p>x", 250_000),
		// Distinct attributes defeat the "three identical elements" cap.
		"distinct attributes": `<img src="/first.png"><p>` + distinct.String() + strings.Repeat("<p>x", 250_000),
		"short field":         `<img src="/first.png"><p>` + formattingRun() + strings.Repeat("<p>x", 2000),
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var out string
			alloc := allocated(func() { out = Sanitize(in, "https://example.com/feed") })
			if max := renderLimit(maxFieldBytes); len(out) > max {
				t.Errorf("output is %d bytes, want at most %d", len(out), max)
			}
			if alloc > 64<<20 {
				t.Errorf("allocated %d MB", alloc>>20)
			}
			if !strings.HasPrefix(out, `<img src="https://example.com/first.png"/>`) {
				t.Errorf("first image lost: %.120q", out)
			}
			if !strings.Contains(out, "x") {
				t.Errorf("text lost: %.120q", out)
			}
			assertSafeHTML(t, out)
		})
	}
}

func TestParse_HostileSummary_BoundedSummaryAndThumbnail(t *testing.T) {
	desc := `<img src="/first.png"><p>` + formattingRun() + strings.Repeat("<p>x", 250_000)
	doc := `<rss version="2.0"><channel><title>t</title><item><guid>1</guid><title>t</title><description><![CDATA[` +
		desc + `]]></description></item></channel></rss>`
	var f *Feed
	alloc := allocated(func() { f = mustParse(t, []byte(doc), "", "https://example.com/feed") })
	if alloc > 64<<20 {
		t.Errorf("allocated %d MB for a %d KB feed", alloc>>20, len(doc)>>10)
	}
	e := f.Entries[0]
	if max := renderLimit(maxFieldBytes); len(e.SummaryHTML) > max {
		t.Errorf("SummaryHTML is %d bytes, want at most %d", len(e.SummaryHTML), max)
	}
	checkEqual(t, "ThumbnailURL", e.ThumbnailURL, "https://example.com/first.png")
	checkEqual(t, "excerpt", Excerpt(e.SummaryHTML, 5), "x x x…")
}

// TestSanitize_FallbackKeepsWhatTheEntryDialogShows: a summary whose HTML
// cannot be rendered within the limits is stored as its visible text, and
// keeps as much of it as the entry dialog shows, line breaks included.
func TestSanitize_FallbackKeepsWhatTheEntryDialogShows(t *testing.T) {
	const paragraphs = 2000
	in := `<p>` + formattingRun() + strings.Repeat("<p>0123456789", paragraphs)
	out := Sanitize(in, "https://example.com/feed")
	if strings.Contains(out, "<p>") {
		t.Fatalf("rendered as HTML, not through the fallback: %.120q", out)
	}
	got := Text(out, config.SummaryFullMaxChars)
	if n := utf8.RuneCountInString(got); n < config.SummaryFullMaxChars || !strings.HasSuffix(got, ellipsis) {
		t.Errorf("fallback keeps %d runes of text, want the %d the entry dialog shows", n, config.SummaryFullMaxChars)
	}
	if !strings.HasPrefix(got, "0123456789\n\n0123456789") {
		t.Errorf("fallback lost the paragraph breaks: %.60q", got)
	}
	checkEqual(t, "excerpt", Excerpt(out, len("0123456789 0123456789")), "0123456789 0123456789…")
	assertSafeHTML(t, out)
}

func TestSanitize_LongInputIsCut(t *testing.T) {
	in := "<p>start</p>" + strings.Repeat("<p>Lorem ipsum dolor sit amet.</p>", 100_000) + "<p>end</p>"
	out := Sanitize(in, "")
	if max := renderLimit(maxFieldBytes); len(out) > max {
		t.Errorf("output is %d bytes, want at most %d", len(out), max)
	}
	if !strings.HasPrefix(out, "<p>start</p>") || strings.Contains(out, "end") {
		t.Errorf("want the beginning of the input only: %.60q … %.60q", out, out[max(0, len(out)-60):])
	}
	assertSafeHTML(t, out)
}

func TestSanitize_LinkHeavyContentKeepsItsBeginningAsHTML(t *testing.T) {
	var b strings.Builder
	b.WriteString(`<p><img src="/hero.jpg"></p>`)
	for i := range 2000 {
		fmt.Fprintf(&b, `<p>Para <em>%d</em> with <a href="/l/%d">a link</a>.</p>`, i, i)
	}
	out := Sanitize(b.String(), "https://example.com/")
	for _, want := range []string{`<img src="https://example.com/hero.jpg"/>`, `<p>Para <em>0</em> with <a href="https://example.com/l/0"`} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q: %.200q", want, out)
		}
	}
}

func TestSanitize_ForeignContentLeftOut(t *testing.T) {
	out := Sanitize(`<p>before</p><svg><style><b x=1></style><p>inside</svg><math><mi>m</mi></math><p>after</p>`, "")
	checkEqual(t, "Sanitize", out, "<p>before</p><p>after</p>")
}

func TestTextHTML_Bounded(t *testing.T) {
	for name, in := range map[string]string{
		"long text":        strings.Repeat("line\n", 1<<20),
		"escaping blow-up": strings.Repeat(`"`, maxFieldBytes),
	} {
		out := textHTML(in)
		if max := renderLimit(maxFieldBytes); len(out) > max {
			t.Errorf("%s: output is %d bytes, want at most %d", name, len(out), max)
		}
	}
	checkEqual(t, "short text", textHTML("a <b>\nc"), "a &lt;b&gt;<br>c")
}

func TestTreeFragment_KeepsMarkupVerbatim(t *testing.T) {
	// The tokenizer lower-cases names and unescapes attribute values in
	// place: treeFragment must pass the original markup on, not the
	// rewritten tokens, or the tree builder would see markup it never costed.
	for _, in := range []string{
		`<B TITLE="Tom &amp; Jerry">x</B><p class="&quot;&gt;">y`,
		`<a href="/q?a=1&amp;b=2">l</a>`,
		"text &amp; <!-- c --> <br/>",
	} {
		frag, _ := treeFragment(in, maxHTMLTreeNodes)
		checkEqual(t, "treeFragment", frag, in)
	}
	frag, _ := treeFragment(`<p>a</p><svg><b x=1>s</b></svg><p>b</p>`, maxHTMLTreeNodes)
	checkEqual(t, "treeFragment without svg", frag, `<p>a</p><p>b</p>`)
	frag, bound := treeFragment(strings.Repeat("<p>x</p>", 100), 50)
	if bound > 50 || !strings.HasPrefix(strings.Repeat("<p>x</p>", 100), frag) || frag == "" {
		t.Errorf("treeFragment with budget 50 = %q (bound %d), want a non-empty prefix", frag, bound)
	}
}
