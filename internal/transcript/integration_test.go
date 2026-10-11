package transcript

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
)

// ytOrigin is the origin of every URL the package requests.
const ytOrigin = "https://" + captionHost

// viaServer is a real *fetch.Client whose requests to YouTube go to a local
// test server instead, with final URLs mapped back.
type viaServer struct {
	c    *fetch.Client
	base string
}

func (v viaServer) Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
	if !strings.HasPrefix(r.URL, ytOrigin+"/") {
		return nil, errors.New("request to another origin: " + r.URL)
	}
	r.URL = v.base + strings.TrimPrefix(r.URL, ytOrigin)
	res, err := v.c.Fetch(ctx, r)
	if res != nil {
		res.FinalURL = ytOrigin + strings.TrimPrefix(res.FinalURL, v.base)
	}
	return res, err
}

// fakeServer is the local YouTube: it checks the player request, answers
// with the fixtures (gzip-compressed, as YouTube does) and records what it
// was asked.
type fakeServer struct {
	t           *testing.T
	mu          sync.Mutex
	playerHits  int
	captionHits int
	sorryHits   int
	query       string
	redirect    bool // answer the player request with a redirect
	// captionRedirect, when set, is where the caption request is
	// redirected to.
	captionRedirect string
}

func (s *fakeServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch r.URL.Path {
	case "/youtubei/v1/player":
		s.playerHits++
		s.checkPlayerRequest(r)
		if s.redirect {
			http.Redirect(w, r, "/sorry/index?continue=x", http.StatusFound)
			return
		}
		writeGzip(w, r, jsonUTF8, fixture(s.t, "player_ok_dubbed.json"))
	case captionPath:
		s.captionHits++
		s.query = r.URL.RawQuery
		if r.Method != http.MethodGet || r.ContentLength > 0 {
			s.t.Errorf("caption request: %s with %d bytes", r.Method, r.ContentLength)
		}
		if s.captionRedirect != "" {
			http.Redirect(w, r, s.captionRedirect, http.StatusFound)
			return
		}
		writeGzip(w, r, jsonUTF8, fixture(s.t, "json3_manual.json"))
	case "/sorry/index":
		s.sorryHits++
		w.WriteHeader(http.StatusTooManyRequests)
	default:
		http.NotFound(w, r)
	}
}

func (s *fakeServer) checkPlayerRequest(r *http.Request) {
	if r.Method != http.MethodPost || r.URL.RawQuery != "prettyPrint=false" {
		s.t.Errorf("player request: %s %s", r.Method, r.URL)
	}
	for name, want := range map[string]string{"Content-Type": jsonType, "Accept": jsonType, "User-Agent": config.DefaultUserAgent()} {
		if got := r.Header.Get(name); got != want {
			s.t.Errorf("player request %s = %q, want %q", name, got, want)
		}
	}
	for _, h := range []string{"If-None-Match", "If-Modified-Since", "Cookie", "Authorization"} {
		if v := r.Header.Get(h); v != "" {
			s.t.Errorf("player request sent %s: %q", h, v)
		}
	}
	var body struct {
		VideoID string `json:"videoId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.VideoID != testVideoID {
		s.t.Errorf("player request body: %+v, %v", body, err)
	}
}

func writeGzip(w http.ResponseWriter, r *http.Request, contentType string, body []byte) {
	w.Header().Set("Content-Type", contentType)
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Write(body)
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	zw := gzip.NewWriter(w)
	zw.Write(body)
	zw.Close()
}

func newViaServer(t *testing.T, s *fakeServer) viaServer {
	t.Helper()
	srv := httptest.NewServer(s)
	t.Cleanup(srv.Close)
	return viaServer{c: fetch.New(config.Default()), base: srv.URL}
}

func TestFetch_ThroughTheFetchClient(t *testing.T) {
	s := &fakeServer{t: t}
	got, err := New(newViaServer(t, s)).Fetch(t.Context(), testVideoID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Lang != "en" || got.Generated || !strings.HasPrefix(got.Text, "Synthetic line one") {
		t.Errorf("Fetch = %+v", got)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.playerHits != 1 || s.captionHits != 1 {
		t.Errorf("player %d, captions %d requests; want 1 each", s.playerHits, s.captionHits)
	}
	// The query reaches the server exactly as rewritten: parameter order
	// and escaping kept, only the format changed.
	want := strings.Replace(syntheticCaptionURLFromFixture(t, 1), "&fmt=srv3", "&fmt="+captionFormat, 1)
	if s.query != want[strings.Index(want, "?")+1:] {
		t.Errorf("caption query = %q", s.query)
	}
}

func TestFetch_PlayerRedirectIsBlockedAndNotFollowed(t *testing.T) {
	s := &fakeServer{t: t, redirect: true}
	_, err := New(newViaServer(t, s)).Fetch(t.Context(), testVideoID)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	var se *fetch.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusFound {
		t.Errorf("err = %v, want the 302 reachable", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sorryHits != 0 || s.captionHits != 0 {
		t.Errorf("redirect followed (%d) or captions fetched (%d)", s.sorryHits, s.captionHits)
	}
}

// TestFetch_CaptionRedirectIsBlockedAndNotFollowed: while YouTube refuses
// a client it redirects the caption request to a captcha or consent page on
// another host. The redirect is not followed, so the signed caption URL
// (the page's continue parameter) goes nowhere else: one caption request,
// and ErrBlocked.
func TestFetch_CaptionRedirectIsBlockedAndNotFollowed(t *testing.T) {
	var elsewhere atomic.Int32
	sorry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		elsewhere.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	t.Cleanup(sorry.Close)
	s := &fakeServer{t: t, captionRedirect: sorry.URL + "/sorry/index?continue=synthetic"}
	_, err := New(newViaServer(t, s)).Fetch(t.Context(), testVideoID)
	if !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
	var se *fetch.StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusFound {
		t.Errorf("err = %v, want the 302 reachable", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.playerHits != 1 || s.captionHits != 1 || elsewhere.Load() != 0 {
		t.Errorf("player %d, captions %d, other host %d requests; want 1, 1 and none",
			s.playerHits, s.captionHits, elsewhere.Load())
	}
}

// TestFetch_ResponseBodiesAreRead checks the fixtures are served whole
// through gzip (guards the test server itself).
func TestFetch_ResponseBodiesAreRead(t *testing.T) {
	s := &fakeServer{t: t}
	v := newViaServer(t, s)
	res, err := v.Fetch(t.Context(), fetch.Request{URL: ytOrigin + captionPath + "?v=" + testVideoID, Accept: jsonType})
	if err != nil {
		t.Fatal(err)
	}
	if want := fixture(t, "json3_manual.json"); string(res.Body) != string(want) {
		t.Errorf("body %d bytes, want %d", len(res.Body), len(want))
	}
}
