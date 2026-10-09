package parse

import (
	"bytes"
	"errors"
	"html"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/microcosm-cc/bluemonday"
	xhtml "golang.org/x/net/html"
	"golang.org/x/net/html/atom"
)

// policy returns the shared allow-list policy. A built bluemonday policy is
// safe for concurrent use; building it is not, hence sync.OnceValue.
var policy = sync.OnceValue(newPolicy)

// newPolicy builds the allow-list. It mirrors bluemonday.UGCPolicy (which
// cannot be narrowed after construction) minus everything a feed reader does
// not need:
//   - no id attribute, so feed content cannot collide with the app's own DOM
//     ids and htmx targets;
//   - no map/area/usemap image maps;
//   - URLs must be absolute http/https (mailto for links): relative URLs are
//     resolved before sanitizing, and whatever is still relative is dropped
//     because it would point into this application;
//   - img keeps only src/alt/width/height/align (srcset, loading, … dropped);
//   - links get rel="nofollow noreferrer noopener" and target="_blank".
//
// Elements that are not listed (script, iframe, object, embed, form, input,
// style, svg, math, meta, base, link, …) never survive, and no event handler
// or style attribute is ever allowed.
func newPolicy() *bluemonday.Policy {
	p := bluemonday.NewPolicy()

	// Global attributes (UGCPolicy's AllowStandardAttributes without "id").
	p.AllowAttrs("dir").Matching(bluemonday.Direction).Globally()
	p.AllowAttrs("lang").Matching(regexp.MustCompile(`^[a-zA-Z]{2,20}(-[a-zA-Z0-9]{1,8})*$`)).Globally()
	p.AllowAttrs("title").Matching(bluemonday.Paragraph).Globally()

	// URL handling.
	p.RequireParseableURLs(true)
	p.AllowRelativeURLs(false)
	p.AllowURLSchemes("http", "https", "mailto")
	p.RequireNoFollowOnLinks(true)
	p.RequireNoReferrerOnLinks(true)
	p.AddTargetBlankToFullyQualifiedLinks(true) // also adds rel="noopener"

	// Sectioning, headings and text blocks.
	p.AllowElements("article", "aside", "figure", "figcaption", "section",
		"summary", "hgroup", "h1", "h2", "h3", "h4", "h5", "h6",
		"br", "div", "hr", "p", "span", "wbr", "pre")
	p.AllowAttrs("open").Matching(regexp.MustCompile(`(?i)^(|open)$`)).OnElements("details")
	p.AllowElements("details")

	// Phrasing content.
	p.AllowElements("abbr", "acronym", "b", "cite", "code", "dfn", "em", "i",
		"kbd", "mark", "s", "samp", "small", "strike", "strong", "sub", "sup",
		"tt", "u", "var", "rp", "rt", "ruby")
	p.AllowAttrs("dir").Matching(bluemonday.Direction).OnElements("bdi", "bdo")
	p.AllowAttrs("datetime").Matching(bluemonday.ISO8601).OnElements("time", "del", "ins")
	p.AllowElements("time", "del", "ins", "q", "blockquote")
	p.AllowAttrs("cite").OnElements("blockquote", "q", "del", "ins")

	// Links and images.
	p.AllowAttrs("href").OnElements("a")
	p.AllowAttrs("src").OnElements("img")
	p.AllowAttrs("alt").Matching(bluemonday.Paragraph).OnElements("img")
	p.AllowAttrs("height", "width").Matching(bluemonday.NumberOrPercent).OnElements("img")
	p.AllowAttrs("align").Matching(bluemonday.ImageAlign).OnElements("img")

	p.AllowLists()
	p.AllowTables()

	// Drop the text of elements whose content is not prose either.
	p.SkipElementsContent("svg", "math")
	p.AddSpaceWhenStrippingTag(true)
	return p
}

// urlAttrs lists, per element, the attributes holding a URL that must be
// made absolute before sanitizing.
var urlAttrs = map[atom.Atom]string{
	atom.A:          "href",
	atom.Img:        "src",
	atom.Blockquote: "cite",
	atom.Q:          "cite",
	atom.Del:        "cite",
	atom.Ins:        "cite",
}

// sanitizeHTML is Sanitize with an already parsed base.
//
// Its work is bounded: only the first maxFieldBytes of s are used, the tree
// builder only gets the part of that whose tree is known to stay within
// maxHTMLTreeNodes (treeFragment), and output outgrowing renderLimit is
// replaced by fallbackHTML. Without these bounds the tree builder turns a
// few hundred kilobytes of unclosed formatting tags into gigabytes.
func sanitizeHTML(s string, base *url.URL) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	frag, _ := treeFragment(clipUTF8(s, maxFieldBytes), maxHTMLTreeNodes)
	limit := renderLimit(len(frag))
	pre, err := absolutize(frag, base, limit)
	if err != nil {
		// The tree builder refused the input (e.g. absurd nesting depth)
		// or the tree renders far larger than its source.
		return fallbackHTML(frag, base)
	}
	out := strings.TrimSpace(policy().Sanitize(pre))
	if len(out) > limit {
		return fallbackHTML(frag, base)
	}
	return out
}

// fallbackHTML renders a fragment that cannot be rendered within the limits
// as what the UI uses of a summary: its first usable image, then its
// visible text (fallbackTextRunes at most) with its line breaks, escaped.
// It is always safe.
func fallbackHTML(s string, base *url.URL) string {
	out := textToHTML(textLines(s, fallbackTextRunes))
	var img string
	firstImage(s, func(src string) bool {
		img = absHTTP(base, src)
		return img != ""
	})
	if img != "" {
		out = policy().Sanitize(`<img src="`+html.EscapeString(img)+`"/>`) + out
	}
	return out
}

// formattingElements are the HTML formatting elements (HTML Living Standard
// section 13.2.4.3). The tree builder keeps each one in its list of active
// formatting elements until its own end tag, and re-creates every one that
// an outer element closed implicitly whenever text or an element follows:
// "<b><i>…" (or "<b x=1><b x=2>…") then thousands of "<p>x" builds a copy
// of each formatting element per paragraph.
var formattingElements = map[atom.Atom]bool{
	atom.A: true, atom.B: true, atom.Big: true, atom.Code: true,
	atom.Em: true, atom.Font: true, atom.I: true, atom.Nobr: true,
	atom.S: true, atom.Small: true, atom.Strike: true, atom.Strong: true,
	atom.Tt: true, atom.U: true,
}

// Node costs used by treeFragment.
const (
	// elementNodes bounds the nodes one start tag creates: the element and
	// the ones it implies (tbody and tr before a td, …).
	elementNodes = 3
	// adoptionNodes bounds the nodes the adoption agency algorithm creates
	// for one misnested formatting tag: at most 8 outer iterations, each
	// cloning at most 3 elements plus the formatting element.
	adoptionNodes = 8 * (3 + 1)
	// noahsArk is how many identical formatting elements (same name and
	// attributes) the list of active formatting elements keeps.
	noahsArk = 3
)

// treeFragment returns the part of the HTML fragment s that the tree builder
// may parse while creating at most budget nodes, cut at a token boundary,
// and the bound on the nodes it creates for that part. svg and math
// subtrees are left out: the policy drops them with their content anyway,
// and only outside foreign content does a plain tokenizer split the input
// the way the tree builder does.
//
// The bound is computed with a tokenizer, without building anything: every
// token creates at most a few nodes, and the formatting elements re-created
// after a token that may close elements are at most the entries of the
// active formatting list, which holds no more than noahsArk elements per
// distinct name and attributes seen so far.
func treeFragment(s string, budget int) (string, int) {
	var (
		out     bytes.Buffer
		seen    map[string]int // formatting start tags by name and attributes
		active  int            // upper bound on the active formatting list
		cost    int            // upper bound on the nodes created so far
		kept    int            // cost of the tokens in out
		foreign int            // open svg and math elements being left out
	)
	z := xhtml.NewTokenizer(strings.NewReader(s))
	for {
		tt := z.Next()
		if tt == xhtml.ErrorToken {
			return out.String(), cost
		}
		// Copy the token first: TagName and TagAttr rewrite it in place
		// (lower-casing names, unescaping values).
		mark := out.Len()
		out.Write(z.Raw())
		var (
			name    []byte
			hasAttr bool
		)
		if tt == xhtml.StartTagToken || tt == xhtml.SelfClosingTagToken || tt == xhtml.EndTagToken {
			name, hasAttr = z.TagName() // TagName can only be called once per token
		}
		a := atom.Lookup(name)
		isForeign := a == atom.Svg || a == atom.Math
		switch {
		case foreign > 0:
			switch {
			case isForeign && tt == xhtml.StartTagToken:
				foreign++
			case isForeign && tt == xhtml.EndTagToken:
				foreign--
			}
			out.Truncate(mark)
			continue
		case isForeign && tt == xhtml.StartTagToken:
			foreign = 1
			out.Truncate(mark)
			continue
		case isForeign && tt == xhtml.SelfClosingTagToken:
			out.Truncate(mark)
			continue
		}

		switch tt {
		case xhtml.StartTagToken, xhtml.SelfClosingTagToken:
			cost += elementNodes
			if formattingElements[a] {
				if a == atom.A || a == atom.Nobr {
					// Closes an open one through the adoption agency.
					cost += active + adoptionNodes
				}
				if seen == nil {
					seen = map[string]int{}
				}
				if key := formattingKey(z, name, hasAttr); seen[key] < noahsArk {
					seen[key]++
					active++
				}
			} else {
				cost += active // may close elements, formatting ones included
			}
		case xhtml.EndTagToken:
			cost += 1 + active // "</p>" and "</br>" create an element
			if formattingElements[a] {
				cost += adoptionNodes
			}
		default:
			cost++
		}
		if cost > budget {
			out.Truncate(mark)
			return out.String(), kept
		}
		kept = cost
	}
}

// formattingKey identifies the current start tag, named name, by name and
// attributes, the way the tree builder compares formatting elements.
func formattingKey(z *xhtml.Tokenizer, name []byte, hasAttr bool) string {
	var attrs []string
	for hasAttr {
		var k, v []byte
		k, v, hasAttr = z.TagAttr()
		attrs = append(attrs, strconv.Itoa(len(k))+":"+string(k)+strconv.Itoa(len(v))+":"+string(v))
	}
	slices.Sort(attrs)
	return string(name) + "\x00" + strings.Join(attrs, "")
}

// errRenderLimit stops rendering a tree whose HTML outgrew its limit.
var errRenderLimit = errors.New("rendered HTML exceeds its size limit")

// limitWriter is a bytes.Buffer that refuses to grow beyond max bytes.
type limitWriter struct {
	buf bytes.Buffer
	max int
}

func (w *limitWriter) Write(p []byte) (int, error) {
	if w.buf.Len()+len(p) > w.max {
		return 0, errRenderLimit
	}
	return w.buf.Write(p)
}

// absolutize parses s as an HTML body fragment, makes URL attributes
// absolute against base, removes images without an absolute http(s) source
// and re-serializes the tree, failing once the result exceeds limit bytes.
// Serializing a parsed tree also balances unclosed tags, so the sanitized
// result cannot leak markup into the page.
func absolutize(s string, base *url.URL, limit int) (string, error) {
	body := &xhtml.Node{Type: xhtml.ElementNode, Data: "body", DataAtom: atom.Body}
	nodes, err := xhtml.ParseFragment(strings.NewReader(s), body)
	if err != nil {
		return "", err
	}
	for _, n := range nodes {
		body.AppendChild(n) // fragment nodes are returned detached
	}
	rewrite(body, base)
	w := &limitWriter{max: limit}
	for c := body.FirstChild; c != nil; c = c.NextSibling {
		if err := xhtml.Render(w, c); err != nil {
			return "", err
		}
	}
	return w.buf.String(), nil
}

// rewrite walks the tree rooted at n, resolving URL attributes and removing
// images that would not load from an absolute http(s) URL.
func rewrite(n *xhtml.Node, base *url.URL) {
	for c := n.FirstChild; c != nil; {
		next := c.NextSibling
		if c.Type == xhtml.ElementNode && c.DataAtom == atom.Img && absHTTP(base, attr(c, "src")) == "" {
			n.RemoveChild(c)
		} else {
			rewrite(c, base)
		}
		c = next
	}
	if n.Type != xhtml.ElementNode {
		return
	}
	if n.DataAtom == atom.Plaintext {
		// <plaintext> swallows the rest of the document and cannot be
		// serialized with balanced tags; render it as preformatted text.
		n.Data, n.DataAtom = "pre", atom.Pre
	}
	key, ok := urlAttrs[n.DataAtom]
	if !ok {
		return
	}
	for i := range n.Attr {
		if n.Attr[i].Namespace == "" && n.Attr[i].Key == key {
			n.Attr[i].Val = resolveRef(base, n.Attr[i].Val)
		}
	}
}

// attr returns the value of the first attribute of n named key.
func attr(n *xhtml.Node, key string) string {
	for _, a := range n.Attr {
		if a.Namespace == "" && a.Key == key {
			return a.Val
		}
	}
	return ""
}
