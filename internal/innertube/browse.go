package innertube

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/giacomomicoli/feedreader/internal/fetch"
)

// BrowseURL answers with a page of YouTube's site as data: for a channel
// and a tab's params, the channel's tabs, the requested one with its
// content, newest first.
const BrowseURL = "https://www.youtube.com/youtubei/v1/browse?prettyPrint=false"

// Tab is a tab of a channel's page: the params a browse request names it
// with (a protobuf, base64url and percent-encoded, as YouTube's web
// client sends it), which the answer repeats in the tab's endpoint.
type Tab struct {
	Name   string // for logs
	Params string
}

// The tabs the fallback reads: a channel's regular videos (no Shorts, no
// live streams) and its Shorts, the same split as its videos-only and
// Shorts-only upload lists, and its live streams (recordings of past ones,
// as well as those on air or upcoming), which only the channel feed lists.
var (
	TabVideos = Tab{Name: "videos", Params: "EgZ2aWRlb3PyBgQKAjoA"}
	TabShorts = Tab{Name: "shorts", Params: "EgZzaG9ydHPyBgUKA5oBAA%3D%3D"}
	TabLive   = Tab{Name: "live", Params: "EgdzdHJlYW1z8gYECgJ6AA%3D%3D"}
)

// BrowseRequest returns the POST asking for tab of the channel channelID
// as profile p.
func BrowseRequest(p Profile, channelID string, tab Tab) fetch.Request {
	r := post(BrowseURL, struct {
		Context  requestContext `json:"context"`
		BrowseID string         `json:"browseId"`
		Params   string         `json:"params"`
	}{newContext(p), channelID, tab.Params})
	r.MaxBytes = MaxBrowseBytes
	return r
}

// ParseBrowse returns the IDs of the videos listed in tab wantParams of a
// browse answer served with contentType, in the answer's order (newest
// first), valid (VideoIDPattern), without repeats, at most max.
//
// Only the tab whose endpoint names wantParams and that the answer marks
// selected is read: the page's header (which may show a trailer or a live
// video) and the other tabs are not. A channel without that tab gets its
// Home tab, whose shelves can list other channels' videos: that is
// ErrNoTab, with no IDs. A list continues on further pages
// (continuationItemRenderer), which are never asked for. An answer that
// carries an error alert (a channel that does not exist) or an error
// object is ErrAlert; a web page is ErrBlocked; anything else that is not
// a JSON document of at most MaxBrowseBytes is ErrTooLarge or ErrFormat.
//
// It allocates about nothing but the IDs, whatever the answer: everything
// outside the tab's content is skipped by Walker, and the content is
// scanned for "videoId" members without decoding it.
func ParseBrowse(body []byte, contentType, wantParams string, max int) ([]string, error) {
	const step = "browse answer"
	switch {
	case IsWebPage(contentType):
		return nil, fmt.Errorf("%w: %s is a web page", ErrBlocked, step)
	case !IsJSON(contentType):
		return nil, formatError(step, "content type is not JSON")
	case len(body) > MaxBrowseBytes:
		return nil, fmt.Errorf("%w: %s", ErrTooLarge, step)
	case !json.Valid(body):
		return nil, formatError(step, "invalid JSON")
	}
	var (
		found, alert, apiError bool
		contentBeg, contentEnd int
	)
	w := NewWalker(body)
	w.Object(func(k []byte) {
		switch {
		case equalShort(k, "contents"):
			found, contentBeg, contentEnd = findTab(w, wantParams)
		case equalShort(k, "alerts"):
			alert = alert || hasErrorAlert(w)
		case equalShort(k, "error"):
			apiError = true
		}
	})
	switch {
	case alert:
		return nil, fmt.Errorf("%w: %s is an error alert", ErrAlert, step)
	case apiError:
		return nil, fmt.Errorf("%w: %s is an error", ErrAlert, step)
	case !found:
		return nil, fmt.Errorf("%w: %s", ErrNoTab, step)
	}
	return scanVideoIDs(body[contentBeg:contentEnd], max), nil
}

// findTab reads the contents member of a browse answer and returns where
// the content of the selected tab whose endpoint params are wantParams
// lies in the input.
func findTab(w *Walker, wantParams string) (found bool, beg, end int) {
	w.Object(func(k []byte) {
		if !equalShort(k, "twoColumnBrowseResultsRenderer") {
			return
		}
		w.Object(func(k []byte) {
			if !equalShort(k, "tabs") {
				return
			}
			w.Array(func() {
				w.Object(func(k []byte) {
					if found || !equalShort(k, "tabRenderer") {
						return
					}
					if ok, b, e := readTab(w, wantParams); ok {
						found, beg, end = true, b, e
					}
				})
			})
		})
	})
	return found, beg, end
}

// readTab reads one tabRenderer and returns where its content lies when it
// is selected and names wantParams.
func readTab(w *Walker, wantParams string) (ok bool, beg, end int) {
	selected, match := false, false
	beg = -1
	w.Object(func(k []byte) {
		switch {
		case equalShort(k, "selected"):
			if c := w.Next(); c != '"' && c != '{' && c != '[' {
				selected = string(w.Literal()) == "true"
			}
		case equalShort(k, "endpoint"):
			w.Object(func(k []byte) {
				if !equalShort(k, "browseEndpoint") {
					return
				}
				w.Object(func(k []byte) {
					if equalShort(k, "params") && w.Next() == '"' {
						match = equalShort(w.Str(), wantParams)
					}
				})
			})
		case equalShort(k, "content"):
			w.Next()
			beg = w.Pos()
			w.Skip()
			end = w.Pos()
		}
	})
	return selected && match && beg >= 0, beg, end
}

// hasErrorAlert reads the alerts member of a browse answer and reports
// whether one of its alerts (any renderer) has type ERROR.
func hasErrorAlert(w *Walker) bool {
	alert := false
	// The callbacks are made once, not per alert: an answer of a million
	// alerts must not cost a closure each.
	member := func(k []byte) {
		if equalShort(k, "type") && w.Next() == '"' && equalShort(w.Str(), "ERROR") {
			alert = true
		}
	}
	renderer := func([]byte) { w.Object(member) }
	element := func() { w.Object(renderer) }
	w.Array(element)
	return alert
}

// scanVideoIDs returns the values of the "videoId" members in content, a
// JSON value json.Valid accepted, in order: valid IDs only, without
// repeats, at most max. It reads members wherever they are nested without
// decoding anything else.
func scanVideoIDs(content []byte, max int) []string {
	var ids []string
	seen := make(map[string]bool, max)
	s := NewWalker(content)
	for len(ids) < max {
		c := s.Next()
		if c == 0 {
			break
		}
		if c != '"' {
			s.i++
			continue
		}
		if !equalShort(s.Str(), "videoId") || s.Next() != ':' {
			continue
		}
		s.i++
		if s.Next() != '"' {
			continue
		}
		v := s.Str()
		if !isVideoID(v) || seen[string(v)] { // the lookup does not allocate
			continue
		}
		id := string(v)
		seen[id] = true
		ids = append(ids, id)
	}
	return ids
}

// isVideoID reports whether b, a JSON string's escaped contents, is a
// video ID as written (VideoIDPattern; an escaped ID is not taken).
func isVideoID(b []byte) bool {
	if len(b) != videoIDLen {
		return false
	}
	for _, c := range b {
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// equalShort reports whether raw, the escaped contents of a JSON string (a
// key or a value), decodes to want, an ASCII string of at most keyBufBytes
// bytes, as KeyIs does but without allocating: an escaped string is
// decoded into a buffer on the stack, so an answer of millions of escaped
// keys costs nothing.
func equalShort(raw []byte, want string) bool {
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw) == want
	}
	if len(raw) > 6*len(want) { // longer than any escaping of want
		return false
	}
	var buf [keyBufBytes]byte
	n := 0
	for i := 0; i < len(raw); {
		if n == len(buf) {
			return false
		}
		c := raw[i]
		if c != '\\' {
			buf[n] = c
			n, i = n+1, i+1
			continue
		}
		if i+1 >= len(raw) {
			return false
		}
		switch raw[i+1] {
		case 'u':
			r, ok := hex4(raw[i+2:])
			if !ok || r >= 0x80 { // want is ASCII
				return false
			}
			buf[n] = byte(r)
			n, i = n+1, i+6
		case '"', '\\', '/':
			buf[n] = raw[i+1]
			n, i = n+1, i+2
		default:
			return false // a control escape is in no string equalShort is asked about
		}
	}
	return string(buf[:n]) == want
}

// keyBufBytes is the longest string equalShort decodes.
const keyBufBytes = 64
