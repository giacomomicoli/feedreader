package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/innertube"
)

// The caption endpoint. A track's URL is signed: its query must be sent as
// YouTube wrote it, except for fmt, which the signature does not cover.
const (
	captionHost     = "www.youtube.com"
	captionPath     = "/api/timedtext"
	captionEndpoint = "https://" + captionHost + captionPath
	// captionFormat is YouTube's JSON caption format: cues ("events") of
	// plain text segments, with no XML or HTML escaping to undo (the XML
	// formats escape the text twice).
	captionFormat = "json3"
	// potMarker is the value of the exp parameter of a caption URL that
	// needs a proof-of-origin token, which only a browser can compute;
	// without one the endpoint answers an empty body.
	potMarker = "xpe"
)

// errCaptionURL is a caption URL this package will not request.
var errCaptionURL = errors.New("caption URL refused")

// captionURL returns raw, a track's URL from the player response, asking
// for captionFormat: the other parameters keep their order and spelling.
// Only an https URL of exactly captionEndpoint, with a plain query that
// names video id once (v=id) and no other video, is accepted, so a hostile answer cannot make
// the client request anything else. A URL that needs a proof-of-origin
// token is ErrBlocked.
func captionURL(raw, id string) (string, error) {
	if len(raw) > maxCaptionURLBytes {
		return "", errCaptionURL
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != captionHost || u.EscapedPath() != captionPath ||
		u.User != nil || u.Opaque != "" || u.Fragment != "" {
		return "", errCaptionURL
	}
	for i := 0; i < len(u.RawQuery); i++ {
		if !queryByte(u.RawQuery[i]) {
			return "", errCaptionURL
		}
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", errCaptionURL
	}
	if v := q["v"]; len(v) != 1 || v[0] != id {
		return "", errCaptionURL
	}
	for _, exp := range q["exp"] {
		if exp == potMarker {
			return "", fmt.Errorf("%w: the captions need a proof-of-origin token", ErrBlocked)
		}
	}
	params := make([]string, 0, strings.Count(u.RawQuery, "&")+2)
	format := "fmt=" + captionFormat
	for p := range strings.SplitSeq(u.RawQuery, "&") {
		key, _, _ := strings.Cut(p, "=")
		key, _ = url.QueryUnescape(key) // valid: ParseQuery accepted it
		switch {
		case p == "":
		case key == "fmt":
			if format != "" {
				params = append(params, format)
				format = ""
			}
		default:
			params = append(params, p)
		}
	}
	if format != "" {
		params = append(params, format)
	}
	return captionEndpoint + "?" + strings.Join(params, "&"), nil
}

// captionRequest returns the GET of a caption URL from captionURL. It does
// not follow redirects: while YouTube refuses a client it redirects the
// request to a captcha or consent page on another host, which would receive
// the signed caption URL.
func captionRequest(captionURL string) fetch.Request {
	return fetch.Request{URL: captionURL, Accept: jsonType, NoRedirects: true}
}

// queryByte reports whether c may appear in a caption URL's query as it is
// sent: unreserved characters, percent escapes and the delimiters
// YouTube's URLs use. Spaces, quotes, angle brackets, ';' and '#' may not.
func queryByte(c byte) bool {
	switch {
	case 'a' <= c && c <= 'z', 'A' <= c && c <= 'Z', '0' <= c && c <= '9':
		return true
	}
	return strings.IndexByte("-._~%&=,+:@/!$'()*", c) >= 0
}

// checkCaptionResponse classifies the answer to the caption request before
// it is parsed. The endpoint answers 200 with an empty body when it refuses
// a URL (a proof-of-origin token is required); a web page, or an answer
// from anywhere but the endpoint (from a Fetcher that followed a redirect),
// is a consent or captcha page.
func checkCaptionResponse(body []byte, contentType, finalURL string) error {
	const step = "caption track"
	final, err := url.Parse(finalURL)
	if err != nil || final.Scheme != "https" || final.Host != captionHost || final.Path != captionPath {
		return fmt.Errorf("%w: %s redirected away from %s", ErrBlocked, step, captionEndpoint)
	}
	if len(body) == 0 {
		return fmt.Errorf("%w: %s is empty", ErrBlocked, step)
	}
	switch mt, _, _ := mime.ParseMediaType(contentType); {
	case mt == "text/html":
		return fmt.Errorf("%w: %s is a web page", ErrBlocked, step)
	case mt != jsonType:
		return formatError(step, "content type is not JSON")
	case len(body) > maxCaptionBytes:
		return fmt.Errorf("%w: %s", ErrTooLarge, step)
	}
	return nil
}

// parseJSON3 returns the text of a caption track in captionFormat:
//
//	{"events":[{"tStartMs":0,"segs":[{"utf8":"text"},{"utf8":" more"}]},…]}
//
// Each event is a cue and its segments' text is concatenated; see
// textBuilder for how cues are joined. At most limit runes are kept. A
// document without cues, or whose text is blank or only sound tags such as
// "[Music]", is ErrNoCaptions, and so is one without a segment anywhere
// whose events are all recognisably caption-free: objects with only
// knownEventKeys (window setup, timing), or none. One without an events
// list, with segments none of which has a text (a utf8 string), or without
// a segment anywhere but with an event that is not an object or has
// another key (a renamed segs looks just like that) is a format error,
// naming the first unknown key (see loggedKey): a renamed field must not
// pass for a video without captions.
//
// After json.Valid has checked the syntax, an innertube.Walker visits only the
// text and speaker flags of the segments and stops once the text is full,
// so decoding allocates little beyond the text itself whatever the
// document holds. Keys match exactly once unescaped (encoding/json would
// also accept other cases).
func parseJSON3(body []byte, limit int) (string, error) {
	const step = "caption track"
	if !json.Valid(body) {
		return "", formatError(step, "invalid JSON")
	}
	w := innertube.NewWalker(body)
	if w.Next() != '{' {
		return "", formatError(step, "not a JSON object")
	}
	t := newTextBuilder(limit)
	found := readEvents(w, t)
	switch {
	case !found.events:
		return "", formatError(step, "no events")
	case found.segments && !found.texts:
		return "", formatError(step, "no segment has a text")
	case !found.segments && found.notObject:
		return "", formatError(step, "an event is not an object")
	case !found.segments && found.unknownKey != nil:
		return "", formatError(step, fmt.Sprintf("unknown event key %q", loggedKey(found.unknownKey)))
	}
	text := t.String()
	if onlyAnnotations(text) {
		return "", ErrNoCaptions
	}
	return text, nil
}

// captionShape is what readEvents found in a caption document, whatever
// the text.
type captionShape struct {
	events   bool // an events member that is a list
	segments bool // an element in the segs list of an element of such a list
	texts    bool // such a segment with a text: a utf8 string, maybe blank
	// The first sign that an event may hold captions under another name:
	// an element of the events list that is not an object, or the first
	// member key (still escaped, a slice of the document) that is neither
	// segs nor one of knownEventKeys.
	notObject  bool
	unknownKey []byte
}

// knownEventKeys are the members of a caption event besides segs: its
// timing and its caption window (as in YouTube's json3 tracks, see the
// testdata). An event with only these, or none, holds no caption; one with
// any other key and no segs may hold them under a renamed key.
var knownEventKeys = [...]string{"tStartMs", "dDurationMs", "id", "wpWinPosId", "wsWinStyleId", "wWinId", "aAppend"}

// maxEventKeyBytes is the longest of knownEventKeys: a key that decodes to
// more is unknown.
const maxEventKeyBytes = len("wsWinStyleId")

// isKnownEventKey reports whether the escaped key raw is one of
// knownEventKeys. It decodes an escaped key once, at most
// maxEventKeyBytes+1 bytes of it, so a document of many escaped keys
// costs one small string each, as innertube.KeyIs does.
func isKnownEventKey(raw []byte) bool {
	k := string(raw) // not copied: only compared
	if bytes.IndexByte(raw, '\\') >= 0 {
		k = innertube.Unquote(raw, maxEventKeyBytes+1)
	}
	for _, known := range knownEventKeys {
		if k == known {
			return true
		}
	}
	return false
}

// maxLoggedKeyBytes bounds the unknown key a format error names.
const maxLoggedKeyBytes = 32

// loggedKey returns the escaped key raw decoded and fit for a log line: at
// most maxLoggedKeyBytes bytes (then "…"), anything but ASCII letters,
// digits and _ replaced by "?".
func loggedKey(raw []byte) string {
	k := innertube.Unquote(raw, maxLoggedKeyBytes+1)
	cut := len(k) > maxLoggedKeyBytes
	if cut {
		k = clipUTF8(k, maxLoggedKeyBytes)
	}
	var b strings.Builder
	for _, r := range k {
		switch {
		case r < utf8.RuneSelf && (r == '_' || '0' <= r && r <= '9' || 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z'):
			b.WriteRune(r)
		default:
			b.WriteByte('?')
		}
	}
	if cut {
		b.WriteString("…")
	}
	return b.String()
}

// readEvents reads the cues of the caption document w is at into t, and
// reports its shape.
func readEvents(w *innertube.Walker, t *textBuilder) captionShape {
	var found captionShape
	w.Object(func(key []byte) {
		if !innertube.KeyIs(key, "events") || w.Next() != '[' {
			return
		}
		found.events = true
		w.Array(func() {
			t.startCue()
			if w.Next() != '{' {
				found.notObject = true
				return
			}
			w.Object(func(key []byte) {
				switch {
				case innertube.KeyIs(key, "segs"):
					w.Array(func() {
						found.segments = true
						if readSegment(w, t) {
							found.texts = true
						}
					})
				case found.unknownKey == nil && !isKnownEventKey(key):
					found.unknownKey = key
				}
			})
		})
	})
	return found
}

// readSegment reads one segment of a cue, {"utf8":"text"} with an optional
// "isSpeakerChange" flag (1 in automatic captions), into t, and reports
// whether it has a text. A repeated key counts once, its last value, as
// with encoding/json.
func readSegment(w *innertube.Walker, t *textBuilder) bool {
	var (
		text            []byte
		isText, speaker bool
	)
	w.Object(func(key []byte) {
		switch {
		case innertube.KeyIs(key, "utf8"):
			text, isText = nil, w.Next() == '"'
			if isText {
				text = w.Str()
			}
		case innertube.KeyIs(key, "isSpeakerChange"):
			speaker = false
			if c := w.Next(); c != '"' && c != '{' && c != '[' {
				speaker = innertube.Truthy(w.Literal())
			}
		}
	})
	t.writeSegment(innertube.Unquote(text, t.budget()), speaker)
	w.Stop = t.full()
	return isText
}

// onlyAnnotations reports whether text holds nothing but sound tags, such
// as "[Music]" or "[Applause]", musical notes and whitespace: the automatic
// captions of a music video.
func onlyAnnotations(text string) bool {
	for text != "" {
		r, size := utf8.DecodeRuneInString(text)
		switch {
		case r == '[':
			end := strings.IndexAny(text[size:], "[]")
			if end < 0 || text[size+end] != ']' {
				return false
			}
			size += end + 1
		case r != '♪' && r != '♫' && !unicode.IsSpace(r):
			return false
		}
		text = text[size:]
	}
	return true
}
