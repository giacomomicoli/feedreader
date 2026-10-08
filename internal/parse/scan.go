package parse

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/url"
	"strings"

	"golang.org/x/net/html/charset"
)

// Namespaces gofeed treats as native RSS and Atom vocabulary (the rest are
// extensions). They mirror gofeed's rss and atom parsers.
const (
	atom10Namespace   = "http://www.w3.org/2005/Atom"
	atom03Namespace   = "http://purl.org/atom/ns#"
	rdfNamespace      = "http://www.w3.org/1999/02/22-rdf-syntax-ns#"
	rss10Namespace    = "http://purl.org/rss/1.0/"
	rss09Namespace    = "http://channel.netscape.com/rdf/simple/0.9/"
	rss09AltNamespace = "http://my.netscape.com/rdf/simple/0.9/"
	xmlNamespace      = "http://www.w3.org/XML/1998/namespace"
)

// tooComplex wraps ErrFeedTooComplex with the limit that was hit.
func tooComplex(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrFeedTooComplex, fmt.Sprintf(format, args...))
}

// xmlScan is what a structure scan of an RSS or Atom document learned,
// beyond the limits it enforces: details gofeed drops, in gofeed's item
// order. The scan never builds entries; gofeed still parses the document.
type xmlScan struct {
	feedTitleType string     // Atom: type attribute of the feed's <title>
	items         []*xmlItem // RSS items or Atom entries as gofeed lists them
}

// xmlItem is the scan of one RSS item or Atom entry. Bases are the xml:base
// in scope (nil when none), computed the way gofeed's pull parser does.
type xmlItem struct {
	base        *url.URL // at the item element
	summaryBase *url.URL // at its description (RSS) or summary (Atom)
	contentBase *url.URL // at its content:encoded (RSS) or content (Atom)
	hasID       bool     // Atom: has an <id>
	id          string   // Atom: text of its last <id>, trimmed, never resolved
	titleType   string   // Atom: type attribute of its <title>
	summaryType string   // Atom: type attribute of its <summary>
}

// xmlRole is what an open element is to the scan.
type xmlRole uint8

const (
	roleOther xmlRole = iota
	roleRoot
	roleChannel // RSS <channel>
	roleItem    // RSS <item> or Atom <entry>
	roleID      // Atom entry <id>
)

// xmlFrame is an open element.
type xmlFrame struct {
	role       xmlRole
	namespaces int      // namespace declarations it carries
	base       *url.URL // xml:base in scope
}

// scanXML checks an RSS or Atom document against the structural limits
// before gofeed parses it, and collects the details listed in xmlScan.
//
// It tokenizes the same bytes the same way gofeed does (control bytes
// dropped, then a non-strict encoding/xml decoder with the same charset
// reader), so what it counts is what gofeed would build. A syntax error ends
// the scan without an error: gofeed reads through an identical decoder and
// cannot get past that point either.
func scanXML(b []byte, f format) (*xmlScan, error) {
	b = dropXMLControlBytes(b)
	if xmlNodeBound(b, maxXMLNodes) > maxXMLNodes {
		return nil, tooComplex("more than %d elements and attributes", maxXMLNodes)
	}
	d := xml.NewDecoder(bytes.NewReader(b))
	d.Strict = false
	d.CharsetReader = charset.NewReaderLabel

	var (
		s                       xmlScan
		stack                   []xmlFrame
		channelItems, rootItems []*xmlItem
		cur                     *xmlItem // item being scanned
		id                      strings.Builder
		inID                    bool // inside cur's <id>, child elements included
		rootSpace               string
		items, inScopeNS        int
	)
	for {
		tok, err := d.Token()
		if err != nil {
			break // io.EOF or a syntax error (see above)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			decls := namespaceDecls(t.Attr)
			inScopeNS += decls
			switch {
			case len(stack) >= maxXMLDepth:
				return nil, tooComplex("elements nested deeper than %d", maxXMLDepth)
			case inScopeNS > maxXMLNamespaces:
				return nil, tooComplex("more than %d namespace declarations in scope", maxXMLNamespaces)
			}

			parent, parentBase := roleOther, (*url.URL)(nil)
			if n := len(stack); n > 0 {
				parent, parentBase = stack[n-1].role, stack[n-1].base
			}
			base := xmlBase(parentBase, t.Attr)
			space := strings.TrimSpace(t.Name.Space)
			name := strings.ToLower(t.Name.Local)
			role := roleOther
			switch {
			case len(stack) == 0:
				role, rootSpace = roleRoot, space
			case f == formatRSS && (parent == roleRoot || parent == roleChannel) && isNativeRSS(space, rootSpace):
				switch {
				case parent == roleRoot && name == "channel":
					// gofeed keeps only the last channel.
					role, channelItems = roleChannel, nil
				case name == "item":
					role, cur = roleItem, &xmlItem{base: base, summaryBase: base, contentBase: base}
					if parent == roleRoot {
						rootItems = append(rootItems, cur)
					} else {
						channelItems = append(channelItems, cur)
					}
				}
			case f == formatAtom && parent == roleRoot && isNativeAtom(space):
				switch name {
				case "entry":
					role, cur = roleItem, &xmlItem{base: base, summaryBase: base, contentBase: base}
					s.items = append(s.items, cur)
				case "title":
					s.feedTitleType = typeAttr(t.Attr)
				}
			case f == formatAtom && parent == roleItem && isNativeAtom(space):
				switch name {
				case "id":
					role, inID = roleID, true
					cur.hasID = true
					id.Reset()
				case "title":
					cur.titleType = typeAttr(t.Attr)
				case "summary":
					cur.summaryType, cur.summaryBase = typeAttr(t.Attr), base
				case "content":
					cur.contentBase = base
				}
			case f == formatRSS && parent == roleItem:
				switch {
				case name == "encoded":
					cur.contentBase = base
				case name == "description" && isNativeRSS(space, rootSpace):
					cur.summaryBase = base
				}
			}
			if role == roleItem {
				if items++; items > maxFeedItems {
					return nil, tooComplex("more than %d items", maxFeedItems)
				}
			}
			stack = append(stack, xmlFrame{role: role, namespaces: decls, base: base})

		case xml.EndElement:
			n := len(stack)
			if n == 0 {
				continue
			}
			top := stack[n-1]
			stack = stack[:n-1]
			inScopeNS -= top.namespaces
			switch top.role {
			case roleID:
				cur.id, inID = strings.TrimSpace(id.String()), false
			case roleItem:
				cur = nil
			}

		case xml.CharData:
			if inID {
				id.Write(t)
			}
		}
	}
	if f == formatRSS {
		// gofeed lists the channel's items, then those outside it.
		s.items = append(channelItems, rootItems...)
	}
	return &s, nil
}

// xmlNodeBound returns an upper bound on the elements and attributes of the
// XML document b, counting no further once it exceeds max. It runs before
// encoding/xml sees b, because the decoder builds a whole start tag,
// attributes included, before anything can count them: one tag with
// millions of attributes costs gigabytes.
//
// The count is an upper bound of what the decoder produces: every start tag
// and every word inside one (outside quoted values) counts, while comments,
// CDATA sections, processing instructions, declarations and end tags,
// delimited as the decoder delimits them, do not. Markup characters are
// ASCII in every encoding the decoder is given here (UTF-16 is transcoded
// beforehand), and b must already be without the control bytes gofeed
// drops (dropXMLControlBytes).
func xmlNodeBound(b []byte, max int) int {
	n := 0
	for {
		i := bytes.IndexByte(b, '<')
		if i < 0 {
			return n
		}
		b = b[i+1:]
		switch {
		case bytes.HasPrefix(b, []byte("!--")):
			b = afterXML(b[3:], "-->")
		case bytes.HasPrefix(b, []byte("![CDATA[")):
			b = afterXML(b[8:], "]]>")
		case bytes.HasPrefix(b, []byte("?")):
			b = afterXML(b[1:], "?>")
		case bytes.HasPrefix(b, []byte("!")):
			b = afterDeclaration(b[1:])
		case bytes.HasPrefix(b, []byte("/")):
			b = afterXML(b, ">")
		default:
			var words int
			words, b = startTagWords(b)
			if n += words; n > max {
				return n
			}
		}
	}
}

// afterXML returns b after the first occurrence of end, or nil.
func afterXML(b []byte, end string) []byte {
	i := bytes.Index(b, []byte(end))
	if i < 0 {
		return nil
	}
	return b[i+len(end):]
}

// afterDeclaration returns b, which follows "<!" and does not start a
// comment or CDATA section, after the declaration (<!DOCTYPE …>), scanning
// it exactly as encoding/xml does: '>' inside quotes or nested <…> does not
// end it, nested comments are skipped, and its first byte is taken as is.
func afterDeclaration(b []byte) []byte {
	var quote byte
	depth := 0
	for i := 1; i < len(b); {
		c := b[i]
		i++
		if quote == 0 && c == '>' && depth == 0 {
			return b[i:]
		}
	handle:
		switch {
		case c == quote:
			quote = 0
		case quote != 0:
		case c == '"' || c == '\'':
			quote = c
		case c == '>':
			depth--
		case c == '<':
			const comment = "!--"
			j := 0
			for ; j < len(comment); j++ {
				if i >= len(b) {
					return nil
				}
				c = b[i]
				i++
				if c != comment[j] {
					break
				}
			}
			if j < len(comment) {
				depth++
				goto handle
			}
			end := bytes.Index(b[i:], []byte("-->"))
			if end < 0 {
				return nil
			}
			i += end + len("-->")
		}
	}
	return nil
}

// startTagWords counts the words of the start tag b begins with (just after
// its '<'): the element name and one per attribute; an unquoted value (a
// non-strict decoder accepts name characters) is part of its attribute. It
// returns the count and b after the tag.
func startTagWords(b []byte) (int, []byte) {
	n := 0
	for i := 0; i < len(b); {
		switch c := b[i]; {
		case c == '>':
			return n, b[i+1:]
		case c == '"' || c == '\'':
			j := bytes.IndexByte(b[i+1:], c)
			if j < 0 {
				return n, nil
			}
			i += j + 2
		case c == '=':
			for i++; i < len(b) && isXMLSpace(b[i]); i++ {
			}
			for ; i < len(b) && isUnquotedValueByte(b[i]); i++ {
			}
		case c == '/' || isXMLSpace(c):
			i++
		default:
			n++
			for ; i < len(b) && !isWordEnd(b[i]); i++ {
			}
		}
	}
	return n, nil
}

// isXMLSpace reports whether c is XML white space.
func isXMLSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// isUnquotedValueByte reports whether a non-strict encoding/xml decoder
// accepts c in an unquoted attribute value.
func isUnquotedValueByte(c byte) bool {
	return 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == ':' || c == '-'
}

// isWordEnd reports whether c ends a word inside a tag.
func isWordEnd(c byte) bool {
	return isXMLSpace(c) || c == '=' || c == '>' || c == '/' || c == '"' || c == '\''
}

// isNativeRSS mirrors gofeed's RSS namespace test; the root element's own
// namespace counts as native.
func isNativeRSS(space, rootSpace string) bool {
	switch space {
	case "", "rss", "rdf", rdfNamespace, rss10Namespace, rss09Namespace, rss09AltNamespace:
		return true
	}
	return rootSpace != "" && space == rootSpace
}

// isNativeAtom mirrors gofeed's Atom namespace test.
func isNativeAtom(space string) bool {
	return space == "" || space == atom10Namespace || space == atom03Namespace
}

// namespaceDecls counts the xmlns and xmlns:prefix attributes in attrs.
func namespaceDecls(attrs []xml.Attr) int {
	n := 0
	for _, a := range attrs {
		if a.Name.Space == "xmlns" || (a.Name.Space == "" && a.Name.Local == "xmlns") {
			n++
		}
	}
	return n
}

// typeAttr returns the type attribute of an Atom text construct the way
// gofeed reads it (encoding/xml unmarshalling a type,attr field: the last
// attribute with that local name, whatever its namespace); "" when absent.
func typeAttr(attrs []xml.Attr) string {
	t := ""
	for _, a := range attrs {
		if a.Name.Local == "type" {
			t = a.Value
		}
	}
	return t
}

// xmlBase returns the xml:base in scope for an element with attrs whose
// parent has base parent, the way gofeed's pull parser computes it: a
// nested xml:base resolves against its parent, and an empty or unparseable
// one is ignored. The result may still be relative.
func xmlBase(parent *url.URL, attrs []xml.Attr) *url.URL {
	for _, a := range attrs {
		if a.Name.Local != "base" || a.Name.Space != xmlNamespace {
			continue
		}
		if a.Value == "" {
			return parent
		}
		u, err := url.Parse(a.Value)
		if err != nil {
			return parent
		}
		if parent != nil {
			u = parent.ResolveReference(u)
		}
		return u
	}
	return parent
}

// dropXMLControlBytes returns b without the C0 control bytes XML forbids
// (all but tab, LF and CR), exactly as gofeed filters its input before
// decoding; b itself when it has none.
func dropXMLControlBytes(b []byte) []byte {
	i := bytes.IndexFunc(b, func(r rune) bool { return r < 0x20 && r != '\t' && r != '\n' && r != '\r' })
	if i < 0 {
		return b
	}
	out := make([]byte, i, len(b))
	copy(out, b[:i])
	for _, c := range b[i:] {
		if c >= 0x20 || c == '\t' || c == '\n' || c == '\r' {
			out = append(out, c)
		}
	}
	return out
}

// jsonFrame is an open JSON object or array.
type jsonFrame struct {
	object  bool
	wantKey bool   // object: the next token is a key (or the closing brace)
	key     string // object: key of the value being read
	items   bool   // array: the feed's top-level "items"
}

// scanJSON checks a JSON Feed against maxJSONValues and maxFeedItems before
// gofeed decodes it: decoding allocates a struct for every item and
// attachment, about 500 times the size of a minimal "{}". Only the first
// JSON value is scanned, as gofeed decodes nothing else; a syntax error ends
// the scan, since encoding/json then rejects the document before allocating.
func scanJSON(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	var stack []jsonFrame
	values, items := 0, 0
	for {
		tok, err := d.Token()
		if err != nil {
			return nil
		}
		n := len(stack)
		if n > 0 && stack[n-1].wantKey {
			if key, ok := tok.(string); ok {
				stack[n-1].key, stack[n-1].wantKey = key, false
				continue
			}
		}
		if delim, ok := tok.(json.Delim); ok && (delim == '}' || delim == ']') {
			if stack = stack[:n-1]; len(stack) == 0 {
				return nil
			}
			continue
		}
		if values++; values > maxJSONValues {
			return tooComplex("more than %d JSON values", maxJSONValues)
		}
		inItems := false
		if n > 0 {
			top := &stack[n-1]
			if top.items {
				if items++; items > maxFeedItems {
					return tooComplex("more than %d items", maxFeedItems)
				}
			}
			// encoding/json matches member names case-insensitively.
			inItems = n == 1 && top.object && strings.EqualFold(top.key, "items")
			top.wantKey = top.object
		}
		delim, ok := tok.(json.Delim)
		if !ok {
			if n == 0 {
				return nil // a scalar document
			}
			continue
		}
		stack = append(stack, jsonFrame{object: delim == '{', wantKey: delim == '{', items: delim == '[' && inItems})
	}
}
