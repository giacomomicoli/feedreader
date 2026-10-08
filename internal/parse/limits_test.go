package parse

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"runtime"
	"strings"
	"testing"

	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/net/html/charset"
)

// allocated returns the bytes allocated while f runs.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// repeatDoc builds prefix + unit×n + suffix.
func repeatDoc(prefix, unit string, n int, suffix string) []byte {
	return []byte(prefix + strings.Repeat(unit, n) + suffix)
}

func TestParse_HostileStructure_RejectedBeforeGofeed(t *testing.T) {
	const rssItem = `<rss version="2.0" xmlns:x="urn:x"><channel><title>t</title><item><title>i</title>`
	const rssEnd = `</item></channel></rss>`
	cases := map[string][]byte{
		// Each case needed 50-200x its size in memory before the limits.
		"deeply nested unknown elements": []byte(rssItem + `<a xmlns="urn:x">` + strings.Repeat("<a>", maxXMLDepth) +
			strings.Repeat("</a>", maxXMLDepth+1) + rssEnd),
		"many nested unknown elements": []byte(rssItem + `<a xmlns="urn:x">` + strings.Repeat("<a>", 300_000) +
			strings.Repeat("</a>", 300_001) + rssEnd),
		"flat unknown elements":        repeatDoc(rssItem, `<x:a/>`, maxXMLNodes+1, rssEnd),
		"one tag, many attributes":     repeatDoc(rssItem+`<x:a`, ` a`, maxXMLNodes+1, `/>`+rssEnd),
		"valued attributes":            repeatDoc(rssItem+`<x:a`, ` b="1"`, maxXMLNodes, `/>`+rssEnd),
		"RSS items":                    repeatDoc(`<rss version="2.0"><channel><title>t</title>`, `<item/>`, maxFeedItems+1, `</channel></rss>`),
		"Atom entries":                 repeatDoc(`<feed xmlns="http://www.w3.org/2005/Atom"><title>t</title>`, `<entry/>`, maxFeedItems+1, `</feed>`),
		"JSON items":                   repeatDoc(`{"version":"https://jsonfeed.org/version/1.1","items":[{}`, `,{}`, maxFeedItems, `]}`),
		"JSON attachments":             repeatDoc(`{"version":"https://jsonfeed.org/version/1.1","items":[{"attachments":[{}`, `,{}`, maxJSONValues, `]}]}`),
		"JSON items, other key case":   repeatDoc(`{"version":"https://jsonfeed.org/version/1.1","ITEMS":[{}`, `,{}`, maxFeedItems, `]}`),
		"control byte before the bulk": repeatDoc(rssItem+"\x07", `<x:a/>`, maxXMLNodes+1, rssEnd),
	}
	var ns strings.Builder
	ns.WriteString(`<rss version="2.0"`)
	for i := range maxXMLNamespaces + 1 {
		fmt.Fprintf(&ns, ` xmlns:n%d="urn:n%d"`, i, i)
	}
	ns.WriteString(`><channel><title>t</title></channel></rss>`)
	cases["namespace declarations"] = []byte(ns.String())

	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			var err error
			alloc := allocated(func() { _, err = Parse(doc, "", "https://example.com/feed") })
			if !errors.Is(err, ErrFeedTooComplex) {
				t.Fatalf("err = %v, want ErrFeedTooComplex", err)
			}
			if limit := uint64(4*len(doc)) + 8<<20; alloc > limit {
				t.Errorf("allocated %d MB for a %d KB document", alloc>>20, len(doc)>>10)
			}
		})
	}
}

func TestParse_LargeRealisticFeedsStillParse(t *testing.T) {
	t.Run("thousands of RSS items with media", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`<rss version="2.0" xmlns:media="http://search.yahoo.com/mrss/" xmlns:content="http://purl.org/rss/1.0/modules/content/"><channel><title>Big</title>`)
		for i := range 5000 {
			fmt.Fprintf(&b, `<item><title>Item %d</title><guid isPermaLink="false">g%d</guid><link>https://big.example.com/%d</link>`+
				`<media:content url="https://cdn.example.com/%d.mp4" type="video/mp4" medium="video" width="640" height="360"/>`+
				`<media:thumbnail url="https://cdn.example.com/%d.jpg" width="480" height="360"/>`+
				`<content:encoded><![CDATA[<p>Body <b>%d</b> <a href="/x">x</a></p>]]></content:encoded></item>`, i, i, i, i, i, i)
		}
		b.WriteString(`</channel></rss>`)
		f := mustParse(t, []byte(b.String()), "", "https://big.example.com/feed")
		checkEqual(t, "entries", len(f.Entries), 5000)
		checkEqual(t, "last thumbnail", f.Entries[4999].ThumbnailURL, "https://cdn.example.com/4999.jpg")
	})
	t.Run("Atom with XHTML content near the node budget", func(t *testing.T) {
		var b strings.Builder
		b.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom"><title>X</title>`)
		for i := range 2000 {
			fmt.Fprintf(&b, `<entry><id>urn:x:%d</id><title>E%d</title><content type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml">`, i, i)
			b.WriteString(strings.Repeat(`<p>Text <em>em</em> <a href="/y">y</a></p>`, 20))
			b.WriteString(`</div></content></entry>`)
		}
		b.WriteString(`</feed>`)
		f := mustParse(t, []byte(b.String()), "", "https://x.example.com/atom")
		checkEqual(t, "entries", len(f.Entries), 2000)
	})
	t.Run("deep but sane nesting", func(t *testing.T) {
		doc := `<rss version="2.0" xmlns:x="urn:x"><channel><title>t</title><item><guid>1</guid>` +
			strings.Repeat("<x:a>", maxXMLDepth-4) + strings.Repeat("</x:a>", maxXMLDepth-4) + `</item></channel></rss>`
		checkEqual(t, "entries", len(mustParse(t, []byte(doc), "", "").Entries), 1)
	})
}

func TestXMLNodeBound(t *testing.T) {
	cases := map[string]int{
		``:                           0,
		`text only > here`:           0,
		`<a/>`:                       1,
		`<a b="1" c='2' d=e f>x</a>`: 5,
		`<a b="x > y <c d>">`:        2,
		`<!-- <a b c> --><![CDATA[<a b>]]><?pi <a>?><!DOCTYPE x><a>`: 1,
		`</a b c>`: 0,
		`<a`:       1,
		// A declaration ends at a '>' outside quotes and nested <…>.
		`<!0"><"><A>`: 1,
		`<!DOCTYPE a [<!ENTITY b '>'> <!-- > -->]><c d>`: 2,
	}
	for in, want := range cases {
		checkEqual(t, "xmlNodeBound("+in+")", xmlNodeBound([]byte(in), math.MaxInt), want)
	}
	if got := xmlNodeBound(repeatDoc("", "<a/>", 100, ""), 10); got != 11 {
		t.Errorf("xmlNodeBound stops at max+1, got %d", got)
	}
}

// decoderNodes counts the elements and attributes gofeed's XML decoder
// produces for b.
func decoderNodes(b []byte) int {
	d := xml.NewDecoder(bytes.NewReader(dropXMLControlBytes(b)))
	d.Strict = false
	d.CharsetReader = charset.NewReaderLabel
	n := 0
	for {
		tok, err := d.Token()
		if err != nil {
			return n
		}
		if se, ok := tok.(xml.StartElement); ok {
			n += 1 + len(se.Attr)
		}
	}
}

// FuzzXMLNodeBound checks that xmlNodeBound never undercounts what the
// decoder builds: the limit must hold before the decoder runs.
func FuzzXMLNodeBound(f *testing.F) {
	for _, s := range []string{
		`<rss><channel><item a="1" b c=d>x</item></channel></rss>`,
		"<!--a-\x01->b<c d e>-->",
		`<a b="<c>"><![CDATA[<d e>]]></a>`,
		`<?xml version="1.0" encoding="ISO-8859-1"?><a b='1' c="2"/>`,
		`<!DOCTYPE a [<!ENTITY x "<b c>">]><a/>`,
		`<a b=c/d e>`,
		`<!0"><"><A>`,
		`<!DOCTYPE a [<!-- > --> <!ENTITY b '>'>]><c>`,
	} {
		f.Add([]byte(s))
	}
	addFixtureSeeds(f, func(b []byte) { f.Add(b) })
	f.Fuzz(func(t *testing.T, b []byte) {
		if got, bound := decoderNodes(b), xmlNodeBound(dropXMLControlBytes(b), math.MaxInt); got > bound {
			t.Errorf("decoder built %d elements and attributes, bound %d: %q", got, bound, b)
		}
	})
}

// treeNodes counts the nodes the tree builder creates for the fragment s,
// or -1 when it rejects it.
func treeNodes(s string) int {
	body := &xhtml.Node{Type: xhtml.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := xhtml.ParseFragment(strings.NewReader(s), body)
	if err != nil {
		return -1
	}
	n := 0
	var walk func(*xhtml.Node)
	walk = func(x *xhtml.Node) {
		n++
		for c := x.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	for _, x := range nodes {
		walk(x)
	}
	return n
}

// FuzzTreeFragmentBound checks that the node bound of treeFragment is never
// below what the tree builder actually creates.
func FuzzTreeFragmentBound(f *testing.F) {
	for _, s := range []string{
		"<p><b><i><u><p>x<p>x<p>x",
		"<p><b x=1><b x=2><b x=3><b x=4><p>x<p>x</div>y",
		"<a href=1><div><div><div><div><div><div><div><div><div>x<a href=2>y<p>z</div></div><p>w",
		"<p><b x=1><table><td></b></td></table><p>x<p>x",
		"<table><b><tr><td>x</td></tr></b>y</table><p>z",
		"<b><p>x</b>y<i>z</p>w",
		"<svg><style><b x=1></style></svg><p>x",
		"<nobr>a<nobr>b<p>c<nobr>d",
		"<select><b><option>x</select><p>y",
		"</p></br><td><col><tr>",
		`<b x="&quot;&gt;&lt;p&gt;x&lt;p&gt;x">y<p>z<p>w`,
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 {
			return
		}
		frag, bound := treeFragment(s, math.MaxInt)
		if n := treeNodes(frag); n > bound {
			t.Errorf("tree builder created %d nodes, bound %d: %q", n, bound, frag)
		}
	})
}
