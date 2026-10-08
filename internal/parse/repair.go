package parse

import (
	"bytes"
	"encoding/json"
	"regexp"
)

// entityRef matches a syntactically valid entity or character reference at
// the start of its input. Named references other than the five XML ones are
// kept too: the non-strict XML decoder passes them through and gofeed
// decodes them as HTML entities.
var entityRef = regexp.MustCompile(`^&([A-Za-z_:][A-Za-z0-9._:-]{0,63}|#[0-9]{1,8}|#[xX][0-9A-Fa-f]{1,8});`)

// maxEntityRefLen bounds how far entityRef needs to look.
const maxEntityRefLen = 80

// repairXML returns a best-effort well-formed version of a broken XML feed:
// the body is transcoded to UTF-8, invalid UTF-8 sequences are replaced,
// characters XML forbids are removed, and bare '&' and '<' that cannot start
// markup are escaped (outside CDATA sections, comments and processing
// instructions).
func repairXML(body []byte, contentType string) []byte {
	b := decodeXMLToUTF8(body, contentType)
	b = bytes.ToValidUTF8(b, []byte("�"))
	b = bytes.Map(func(r rune) rune {
		if isXMLChar(r) {
			return r
		}
		return -1
	}, b)
	return escapeStrayMarkup(b)
}

// isXMLChar reports whether r is allowed in an XML 1.0 document.
func isXMLChar(r rune) bool {
	switch {
	case r == 0x9 || r == 0xA || r == 0xD:
		return true
	case r >= 0x20 && r <= 0xD7FF:
		return true
	case r >= 0xE000 && r <= 0xFFFD:
		return true
	case r >= 0x10000 && r <= 0x10FFFF:
		return true
	}
	return false
}

// verbatimSections are constructs copied unchanged by escapeStrayMarkup.
var verbatimSections = []struct{ open, close string }{
	{"<![CDATA[", "]]>"},
	{"<!--", "-->"},
	{"<?", "?>"},
}

// escapeStrayMarkup escapes '&' that does not start a reference and '<'
// that cannot start a tag, comment, CDATA section or processing instruction.
func escapeStrayMarkup(b []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(b) + len(b)/64)
	for len(b) > 0 {
		i := bytes.IndexAny(b, "<&")
		if i < 0 {
			out.Write(b)
			break
		}
		out.Write(b[:i])
		b = b[i:]
		if n := verbatimLen(b); n > 0 {
			out.Write(b[:n])
			b = b[n:]
			continue
		}
		if b[0] == '&' {
			if m := entityRef.Find(b[:min(len(b), maxEntityRefLen)]); m != nil {
				out.Write(m)
				b = b[len(m):]
				continue
			}
			out.WriteString("&amp;")
		} else if len(b) > 1 && startsMarkup(b[1]) {
			out.WriteByte('<')
		} else {
			out.WriteString("&lt;")
		}
		b = b[1:]
	}
	return out.Bytes()
}

// verbatimLen returns the length of the CDATA section, comment or processing
// instruction at the start of b (to its end if unterminated), or 0.
func verbatimLen(b []byte) int {
	for _, s := range verbatimSections {
		if !bytes.HasPrefix(b, []byte(s.open)) {
			continue
		}
		end := bytes.Index(b[len(s.open):], []byte(s.close))
		if end < 0 {
			return len(b)
		}
		return len(s.open) + end + len(s.close)
	}
	return 0
}

// startsMarkup reports whether c may follow '<' in well-formed XML.
func startsMarkup(c byte) bool {
	switch {
	case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		return true
	case c == '_' || c == ':' || c == '/' || c == '!' || c == '?':
		return true
	}
	return c >= 0x80 // non-ASCII name start character
}

// repairJSON returns the first JSON value of a UTF-8 body with invalid UTF-8
// replaced, ignoring trailing garbage; nil if there is no valid value.
func repairJSON(b []byte) []byte {
	b = bytes.ToValidUTF8(b, []byte("�"))
	var raw json.RawMessage
	if err := json.NewDecoder(bytes.NewReader(b)).Decode(&raw); err != nil {
		return nil
	}
	return raw
}
