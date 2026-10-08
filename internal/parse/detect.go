package parse

import (
	"bytes"
	"encoding/json"
	"strings"
)

// format is a feed document format.
type format int

const (
	formatUnknown format = iota
	formatRSS            // RSS 0.9x/2.0 and RSS 1.0 (RDF)
	formatAtom
	formatJSON
)

func (f format) String() string {
	switch f {
	case formatRSS:
		return "RSS"
	case formatAtom:
		return "Atom"
	case formatJSON:
		return "JSON Feed"
	}
	return "unknown"
}

// jsonFeedMediaType is the JSON Feed media type, used as a hint for JSON
// documents that lack the (required) version member.
const jsonFeedMediaType = "application/feed+json"

// sniffFormat cheaply identifies the feed format of body from its first
// element (XML) or its version member (JSON). The Content-Type is only a
// hint and never turns a non-feed body (e.g. HTML) into a feed.
func sniffFormat(body []byte, contentType string) format {
	if enc, rest := splitBOM(body); enc != "" {
		body = rest
		if enc != encUTF8 {
			body = transcode(rest, enc)
		}
	}
	body = bytes.TrimLeft(body, " \t\r\n")
	if len(body) == 0 {
		return formatUnknown
	}
	switch body[0] {
	case '<':
		return xmlRootFormat(body)
	case '{':
		if isJSONFeed(body, mediaType(contentType) == jsonFeedMediaType) {
			return formatJSON
		}
	}
	return formatUnknown
}

// xmlRootFormat skips the prolog (XML declaration, processing instructions,
// comments, doctype, whitespace) and maps the root element's local name:
// rss and rdf:RDF → RSS, feed → Atom.
func xmlRootFormat(b []byte) format {
	for {
		b = bytes.TrimLeft(b, " \t\r\n")
		switch {
		case len(b) < 2 || b[0] != '<':
			return formatUnknown
		case bytes.HasPrefix(b, []byte("<?")):
			b = skipPast(b, "?>")
		case bytes.HasPrefix(b, []byte("<!--")):
			b = skipPast(b, "-->")
		case b[1] == '!':
			b = skipDeclaration(b)
		default:
			return rootFormat(elementName(b[1:]))
		}
	}
}

// skipPast returns b after the first occurrence of end, or nil.
func skipPast(b []byte, end string) []byte {
	i := bytes.Index(b, []byte(end))
	if i < 0 {
		return nil
	}
	return b[i+len(end):]
}

// skipDeclaration skips a markup declaration such as <!DOCTYPE …>,
// including a bracketed internal subset and quoted strings.
func skipDeclaration(b []byte) []byte {
	depth := 0
	var quote byte
	for i := 2; i < len(b); i++ {
		c := b[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[':
			depth++
		case c == ']':
			depth--
		case c == '>' && depth <= 0:
			return b[i+1:]
		}
	}
	return nil
}

// elementName returns the tag name at the start of b.
func elementName(b []byte) string {
	end := bytes.IndexAny(b, " \t\r\n/>")
	if end < 0 {
		end = len(b)
	}
	return string(b[:end])
}

// rootFormat maps a (possibly prefixed) root element name to a format.
func rootFormat(name string) format {
	if i := strings.LastIndexByte(name, ':'); i >= 0 {
		name = name[i+1:]
	}
	switch strings.ToLower(name) {
	case "rss", "rdf":
		return formatRSS
	case "feed":
		return formatAtom
	}
	return formatUnknown
}

// isJSONFeed reports whether b is a JSON object whose "version" contains
// "jsonfeed.org". With mediaHint (Content-Type application/feed+json) an
// object with an "items" member is accepted without a version. The object
// is streamed so that detection usually stops at the first member.
func isJSONFeed(b []byte, mediaHint bool) bool {
	dec := json.NewDecoder(bytes.NewReader(b))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	hasItems := false
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return false
		}
		key, _ := tok.(string)
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			return false
		}
		switch key {
		case "version":
			var v string
			if json.Unmarshal(val, &v) == nil && strings.Contains(strings.ToLower(v), "jsonfeed.org") {
				return true
			}
		case "items":
			hasItems = true
		}
	}
	return mediaHint && hasItems
}
