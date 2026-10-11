package transcript

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/safeurl"
)

// testVideoID is the video every synthetic fixture describes.
const testVideoID = "vid00000001"

// Content types YouTube answers with.
const (
	jsonUTF8 = "application/json; charset=UTF-8"
	htmlUTF8 = "text/html; charset=UTF-8"
)

// fixture returns testdata/name.
func fixture(t testing.TB, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// renamed returns fixture name with every member key from renamed to to,
// as when YouTube renames a field of its answers.
func renamed(t testing.TB, name, from, to string) []byte {
	t.Helper()
	b := fixture(t, name)
	key := []byte(`"` + from + `":`)
	if !bytes.Contains(b, key) {
		t.Fatalf("%s has no member %s", name, from)
	}
	return bytes.ReplaceAll(b, key, []byte(`"`+to+`":`))
}

// reply is one canned answer of fakeYouTube.
type reply struct {
	res *fetch.Result
	err error
}

func jsonReply(body []byte) reply {
	return reply{res: &fetch.Result{StatusCode: http.StatusOK, ContentType: jsonUTF8, Body: body}}
}

func errReply(err error) reply { return reply{err: err} }

// fakeYouTube answers the player request and the caption track request
// with canned replies and records every request. Anything else is a 404.
type fakeYouTube struct {
	mu       sync.Mutex
	calls    []fetch.Request
	player   reply
	captions reply
	// onFetch, when set, runs before request n (1-based) is answered.
	onFetch func(n int)
}

func (f *fakeYouTube) Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, r)
	n := len(f.calls)
	hook := f.onFetch
	f.mu.Unlock()
	if hook != nil {
		hook(n)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var rep reply
	switch {
	case r.URL == playerURL:
		rep = f.player
	case strings.HasPrefix(r.URL, captionEndpoint+"?"):
		rep = f.captions
	default:
		return nil, &fetch.StatusError{URL: safeurl.String(r.URL), StatusCode: http.StatusNotFound}
	}
	if rep.err != nil {
		return nil, rep.err
	}
	if rep.res == nil {
		return nil, &fetch.StatusError{URL: safeurl.String(r.URL), StatusCode: http.StatusNotFound}
	}
	res := *rep.res
	if res.FinalURL == "" {
		res.FinalURL = r.URL
	}
	return &res, nil
}

func (f *fakeYouTube) requests() []fetch.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fetch.Request(nil), f.calls...)
}

// okYouTube serves the dubbed player fixture and the manual JSON captions.
func okYouTube(t testing.TB) *fakeYouTube {
	return &fakeYouTube{
		player:   jsonReply(fixture(t, "player_ok_dubbed.json")),
		captions: jsonReply(fixture(t, "json3_manual.json")),
	}
}
