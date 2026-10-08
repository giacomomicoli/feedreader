package parse

import (
	"bytes"
	"mime"
	"regexp"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
)

// Canonical (WHATWG) encoding names as returned by charset.Lookup.
const (
	encUTF8    = "utf-8"
	encUTF16LE = "utf-16le"
	encUTF16BE = "utf-16be"
)

var (
	// xmlDecl matches an XML declaration at the start of a document.
	xmlDecl = regexp.MustCompile(`^[ \t\r\n]*<\?xml[ \t\r\n][^>]*>`)
	// xmlDeclEncoding matches the encoding pseudo-attribute inside it.
	xmlDeclEncoding = regexp.MustCompile(`(encoding[ \t\r\n]*=[ \t\r\n]*)(["'])([^"']*)(["'])`)
)

// splitBOM reports the encoding announced by a byte-order mark and returns
// the body without it. enc is "" when there is no BOM.
func splitBOM(b []byte) (enc string, rest []byte) {
	switch {
	case bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}):
		return encUTF8, b[3:]
	case bytes.HasPrefix(b, []byte{0xFE, 0xFF}):
		return encUTF16BE, b[2:]
	case bytes.HasPrefix(b, []byte{0xFF, 0xFE}):
		return encUTF16LE, b[2:]
	}
	return "", b
}

// declaredEncoding returns the encoding label of the XML declaration, if the
// document has one with an encoding pseudo-attribute.
func declaredEncoding(b []byte) (string, bool) {
	decl := xmlDecl.Find(b)
	if decl == nil {
		return "", false
	}
	m := xmlDeclEncoding.FindSubmatch(decl)
	if m == nil {
		return "", false
	}
	return strings.TrimSpace(string(m[3])), true
}

// declareUTF8 rewrites the encoding of the XML declaration, if any, to
// UTF-8. It is used after the body has been transcoded, so that the XML
// decoder does not convert it a second time.
func declareUTF8(b []byte) []byte {
	loc := xmlDecl.FindIndex(b)
	if loc == nil {
		return b
	}
	decl := xmlDeclEncoding.ReplaceAll(b[loc[0]:loc[1]], []byte("${1}${2}UTF-8${4}"))
	out := make([]byte, 0, len(b)+len(decl)-(loc[1]-loc[0]))
	out = append(out, b[:loc[0]]...)
	out = append(out, decl...)
	return append(out, b[loc[1]:]...)
}

// canonicalEncoding returns the WHATWG name for label, or "" if unknown.
func canonicalEncoding(label string) string {
	if strings.TrimSpace(label) == "" {
		return ""
	}
	enc, name := charset.Lookup(label)
	if enc == nil {
		return ""
	}
	return name
}

// isUTF16 reports whether a canonical encoding name is a UTF-16 variant.
func isUTF16(name string) bool { return name == encUTF16LE || name == encUTF16BE }

// transcode converts b from the named encoding to UTF-8. Unknown encodings
// and decoder errors leave b unchanged.
func transcode(b []byte, label string) []byte {
	enc, _ := charset.Lookup(label)
	if enc == nil {
		return b
	}
	out, err := enc.NewDecoder().Bytes(b)
	if err != nil {
		return b
	}
	return out
}

// httpCharset extracts the charset parameter of an HTTP Content-Type,
// tolerating malformed headers.
func httpCharset(contentType string) string {
	if contentType == "" {
		return ""
	}
	if _, params, err := mime.ParseMediaType(contentType); err == nil {
		return strings.TrimSpace(params["charset"])
	}
	parts := strings.Split(contentType, ";")
	for _, part := range parts[1:] {
		k, v, ok := strings.Cut(part, "=")
		if ok && strings.EqualFold(strings.TrimSpace(k), "charset") {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// mediaType returns the lower-cased media type of an HTTP Content-Type.
func mediaType(contentType string) string {
	mt, _, _ := strings.Cut(contentType, ";")
	return strings.ToLower(strings.TrimSpace(mt))
}

// httpTranscoding returns the canonical name of the HTTP charset when the
// body should be transcoded from it: the charset is known and not UTF-8,
// and the body is not already evidently UTF-8 (valid UTF-8 containing
// multi-byte sequences, the typical result of a server mislabelling UTF-8
// as ISO-8859-1). It returns "" otherwise.
func httpTranscoding(contentType string, body []byte) string {
	name := canonicalEncoding(httpCharset(contentType))
	if name == "" || name == encUTF8 {
		return ""
	}
	if !isUTF16(name) && utf8.Valid(body) && hasNonASCII(body) {
		return ""
	}
	return name
}

// hasNonASCII reports whether b contains a byte >= 0x80.
func hasNonASCII(b []byte) bool {
	for _, c := range b {
		if c >= utf8.RuneSelf {
			return true
		}
	}
	return false
}

// prepareXML readies a body for the first, unrepaired parse attempt. A
// document that declares its encoding is left alone: the XML decoder honors
// the declaration through its charset reader. Otherwise a byte-order mark,
// then the HTTP charset decide, and UTF-8 is the fallback.
func prepareXML(body []byte, contentType string) []byte {
	if enc, rest := splitBOM(body); enc != "" {
		if enc != encUTF8 {
			rest = transcode(rest, enc)
		}
		return declareUTF8(rest)
	}
	if _, ok := declaredEncoding(body); ok {
		return body
	}
	if enc := httpTranscoding(contentType, body); enc != "" {
		return transcode(body, enc)
	}
	return body
}

// decodeXMLToUTF8 converts the whole body to UTF-8 for the repair attempt,
// rewriting the declaration accordingly. Precedence: byte-order mark, then
// a known declared encoding (UTF-16 only with a BOM, as a UTF-16 label on a
// BOM-less ASCII-compatible document is a mislabel), then the HTTP charset,
// then UTF-8.
func decodeXMLToUTF8(body []byte, contentType string) []byte {
	if enc, rest := splitBOM(body); enc != "" {
		if enc != encUTF8 {
			rest = transcode(rest, enc)
		}
		return declareUTF8(rest)
	}
	if label, ok := declaredEncoding(body); ok {
		if enc := canonicalEncoding(label); enc != "" && enc != encUTF8 && !isUTF16(enc) {
			return declareUTF8(transcode(body, enc))
		}
		body = declareUTF8(body)
		if utf8.Valid(body) {
			return body
		}
	}
	if enc := httpTranscoding(contentType, body); enc != "" {
		return transcode(body, enc)
	}
	return body
}

// prepareJSON converts a JSON body to UTF-8 (byte-order mark, then HTTP
// charset, then UTF-8) and strips a UTF-8 BOM, which encoding/json rejects.
func prepareJSON(body []byte, contentType string) []byte {
	if enc, rest := splitBOM(body); enc != "" {
		if enc != encUTF8 {
			rest = transcode(rest, enc)
		}
		return rest
	}
	if enc := httpTranscoding(contentType, body); enc != "" {
		return transcode(body, enc)
	}
	return body
}
