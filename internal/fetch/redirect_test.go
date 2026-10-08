package fetch

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hop is one redirect served by redirectServer.
type hop struct {
	status   int
	location string
}

// redirectServer redirects the paths in routes and serves sampleFeed on
// every other path. Header assertions run on the final, non-redirect hop.
func redirectServer(t *testing.T, routes map[string]hop) *httptest.Server {
	t.Helper()
	return newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if h, ok := routes[r.URL.Path]; ok {
			w.Header().Set("Location", h.location)
			w.WriteHeader(h.status)
			return
		}
		w.Header().Set("Content-Type", "application/atom+xml")
		fmt.Fprint(w, sampleFeed)
	})
}

func TestFetch_RedirectsTrackPermanentURL(t *testing.T) {
	tests := []struct {
		name      string
		start     string // path; "" = /a
		routes    map[string]hop
		wantFinal string // path
		wantPerm  string // path; "" = not moved
	}{
		{
			name:      "301 then 301 moves to the end of the chain",
			routes:    map[string]hop{"/a": {301, "/b"}, "/b": {301, "/c"}},
			wantFinal: "/c", wantPerm: "/c",
		},
		{
			name:      "301 then 302 moves only to the 302 source",
			routes:    map[string]hop{"/a": {301, "/b"}, "/b": {302, "/c"}},
			wantFinal: "/c", wantPerm: "/b",
		},
		{
			name:      "302 only is not a move",
			routes:    map[string]hop{"/a": {302, "/c"}},
			wantFinal: "/c", wantPerm: "",
		},
		{
			name:      "308 is a move",
			routes:    map[string]hop{"/a": {308, "/c"}},
			wantFinal: "/c", wantPerm: "/c",
		},
		{
			name:      "301 after a temporary hop does not count",
			routes:    map[string]hop{"/a": {302, "/b"}, "/b": {301, "/c"}},
			wantFinal: "/c", wantPerm: "",
		},
		{
			name:      "303 and 307 are temporary",
			routes:    map[string]hop{"/a": {303, "/b"}, "/b": {307, "/c"}},
			wantFinal: "/c", wantPerm: "",
		},
		{
			name:      "relative Location is resolved",
			start:     "/dir/a",
			routes:    map[string]hop{"/dir/a": {301, "c"}},
			wantFinal: "/dir/c", wantPerm: "/dir/c",
		},
		{
			name:  "exactly five hops are allowed",
			start: "/r0",
			routes: map[string]hop{
				"/r0": {301, "/r1"}, "/r1": {301, "/r2"}, "/r2": {301, "/r3"},
				"/r3": {301, "/r4"}, "/r4": {301, "/r5"},
			},
			wantFinal: "/r5", wantPerm: "/r5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := redirectServer(t, tt.routes)
			c := newTestClient(t, 1<<20)
			start := tt.start
			if start == "" {
				start = "/a"
			}

			res, err := c.Fetch(t.Context(), Request{URL: srv.URL + start})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if res.FinalURL != srv.URL+tt.wantFinal {
				t.Errorf("FinalURL = %q, want %q", res.FinalURL, srv.URL+tt.wantFinal)
			}
			wantPerm := ""
			if tt.wantPerm != "" {
				wantPerm = srv.URL + tt.wantPerm
			}
			if res.PermanentURL != wantPerm {
				t.Errorf("PermanentURL = %q, want %q", res.PermanentURL, wantPerm)
			}
			if string(res.Body) != sampleFeed {
				t.Errorf("Body = %q", res.Body)
			}
		})
	}
}

func TestFetch_RedirectKeepsUserAgentAndValidators(t *testing.T) {
	const etag = `"e1"`
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/new", http.StatusMovedPermanently)
			return
		}
		if r.Header.Get("User-Agent") != testUA || r.Header.Get("If-None-Match") != etag {
			t.Errorf("after redirect: UA=%q If-None-Match=%q", r.Header.Get("User-Agent"), r.Header.Get("If-None-Match"))
		}
		w.WriteHeader(http.StatusNotModified)
	})
	c := newTestClient(t, 1<<20)

	res, err := c.Fetch(t.Context(), Request{URL: srv.URL + "/old", ETag: etag})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !res.NotModified || res.PermanentURL != srv.URL+"/new" {
		t.Errorf("NotModified=%v PermanentURL=%q, want true/%s/new", res.NotModified, res.PermanentURL, srv.URL)
	}
}

func TestFetch_RedirectErrors(t *testing.T) {
	tests := []struct {
		name    string
		routes  map[string]hop
		wantErr error
		wantMsg string // suffix after "fetch <url>/a: "; {srv} = server URL
	}{
		{
			name:    "loop",
			routes:  map[string]hop{"/a": {301, "/b"}, "/b": {302, "/a"}},
			wantErr: ErrRedirects,
			wantMsg: "redirect loop via {srv}/a",
		},
		{
			name:    "self loop",
			routes:  map[string]hop{"/a": {302, "/a"}},
			wantErr: ErrRedirects,
			wantMsg: "redirect loop via {srv}/a",
		},
		{
			name: "six hops",
			routes: map[string]hop{
				"/a": {301, "/r1"}, "/r1": {301, "/r2"}, "/r2": {301, "/r3"},
				"/r3": {301, "/r4"}, "/r4": {301, "/r5"}, "/r5": {301, "/r6"},
			},
			wantErr: ErrRedirects,
			wantMsg: "more than 5 redirects",
		},
		{
			name:    "javascript: target",
			routes:  map[string]hop{"/a": {302, "javascript:alert(1)"}},
			wantErr: ErrScheme,
			wantMsg: "refused redirect to a javascript: URL; only http and https URLs are supported",
		},
		{
			name:    "file: target",
			routes:  map[string]hop{"/a": {301, "file:///etc/passwd"}},
			wantErr: ErrScheme,
			wantMsg: "refused redirect to a file: URL; only http and https URLs are supported",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := redirectServer(t, tt.routes)
			c := newTestClient(t, 1<<20)

			res, err := c.Fetch(t.Context(), Request{URL: srv.URL + "/a"})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Fetch = %+v, %v; want %v", res, err, tt.wantErr)
			}
			msg := strings.ReplaceAll(tt.wantMsg, "{srv}", srv.URL)
			if want := "fetch " + srv.URL + "/a: " + msg; err.Error() != want {
				t.Errorf("message = %q, want %q", err.Error(), want)
			}
		})
	}
}
