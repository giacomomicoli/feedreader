package resolve

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/fetch"
)

// fakeFetcher serves canned responses by exact URL and records every
// request. Unknown URLs answer 404, like a real site without that path.
type fakeFetcher struct {
	mu     sync.Mutex
	routes map[string]route
	calls  []fetch.Request
	// onFetch, when set, runs before each request is answered (after it is
	// recorded); tests use it to cancel the context mid-resolution.
	onFetch func(n int)
}

// route is one canned answer.
type route struct {
	res *fetch.Result
	err error
}

func newFake() *fakeFetcher { return &fakeFetcher{routes: map[string]route{}} }

func (f *fakeFetcher) Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, r)
	n := len(f.calls)
	rt, ok := f.routes[r.URL]
	hook := f.onFetch
	f.mu.Unlock()

	if hook != nil {
		hook(n)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !ok {
		return nil, &fetch.StatusError{URL: r.URL, StatusCode: http.StatusNotFound}
	}
	if rt.err != nil {
		return nil, rt.err
	}
	res := *rt.res
	if res.FinalURL == "" {
		res.FinalURL = r.URL
	}
	return &res, nil
}

// serve registers a 200 response.
func (f *fakeFetcher) serve(rawURL, contentType, body string) *fetch.Result {
	res := &fetch.Result{StatusCode: http.StatusOK, ContentType: contentType, Body: []byte(body)}
	f.routes[rawURL] = route{res: res}
	return res
}

// fail registers an error response.
func (f *fakeFetcher) fail(rawURL string, err error) { f.routes[rawURL] = route{err: err} }

// urls returns the requested URLs in order.
func (f *fakeFetcher) urls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.URL
	}
	return out
}

func (f *fakeFetcher) requests() []fetch.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fetch.Request(nil), f.calls...)
}

// Synthetic feed documents, one per supported format.
const (
	rssDoc = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel><title>Blog</title><link>https://blog.example/</link>
<item><title>Post</title><link>https://blog.example/post</link></item></channel></rss>`
	atomDoc = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>Blog</title><id>urn:x</id>
<entry><id>urn:1</id><title>Post</title></entry></feed>`
	jsonFeedDoc = `{"version":"https://jsonfeed.org/version/1.1","title":"Blog","items":[{"id":"1","content_text":"x"}]}`
)

const (
	ctHTML = "text/html; charset=utf-8"
	ctRSS  = "application/rss+xml; charset=utf-8"
	ctAtom = "application/atom+xml"
	ctJSON = "application/feed+json"
)

// htmlPage wraps head markup in a minimal document.
func htmlPage(head string) string {
	return "<!DOCTYPE html><html><head><title>Site</title>" + head +
		"</head><body><p>Hello</p></body></html>"
}

// errUnreachable is a transport-level failure (the host does not resolve).
var errUnreachable = &net.DNSError{Err: "no such host", Name: "down.example", IsNotFound: true}

// resolveOK resolves raw and fails the test on error.
func resolveOK(t *testing.T, f *fakeFetcher, raw string) *Result {
	t.Helper()
	res, err := New(f).Resolve(context.Background(), raw)
	if err != nil {
		t.Fatalf("Resolve(%q): unexpected error: %v (requests: %v)", raw, err, f.urls())
	}
	if res == nil || len(res.Candidates) == 0 {
		t.Fatalf("Resolve(%q): no candidates", raw)
	}
	return res
}

// resolveErr resolves raw and requires an error matching want.
func resolveErr(t *testing.T, f *fakeFetcher, raw string, want error) error {
	t.Helper()
	res, err := New(f).Resolve(context.Background(), raw)
	if !errors.Is(err, want) {
		t.Fatalf("Resolve(%q) = %+v, %v; want error %v", raw, res, err, want)
	}
	if res != nil {
		t.Fatalf("Resolve(%q) returned a result with an error: %+v", raw, res)
	}
	return err
}

// candidateURLs lists the candidate URLs of res.
func candidateURLs(res *Result) []string {
	out := make([]string, len(res.Candidates))
	for i, c := range res.Candidates {
		out[i] = c.URL
	}
	return out
}
