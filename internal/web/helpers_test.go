package web

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// fakeSched records refreshes and implements Preview/Subscribe against the
// real store, so subscribed feeds show up in the UI exactly as in production.
type fakeSched struct {
	st store.Store

	mu           sync.Mutex
	refreshed    []int64
	refreshAll   int
	previews     map[string]*sched.Preview
	previewErr   map[string]error
	previewCalls []string
	entries      map[string][]store.NewEntry
	subscribeErr error
	subs         []sched.Subscription
	// onSubscribe runs inside Subscribe before it returns, standing in for
	// whatever other requests do while the feed is being fetched.
	onSubscribe func(sched.Subscription)

	rescheduled   []int64
	rescheduleSec []int // the feed's stored interval when Reschedule ran
	rescheduleErr error

	ingested  [][]int64 // the feed ids of each ScheduleIngest call
	ingestErr error
}

func (f *fakeSched) ScheduleIngest(_ context.Context, feedIDs []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ingested = append(f.ingested, slices.Clone(feedIDs))
	return f.ingestErr
}

func (f *fakeSched) Reschedule(ctx context.Context, id int64) error {
	fd, err := f.st.GetFeed(ctx, id)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rescheduled = append(f.rescheduled, id)
	f.rescheduleSec = append(f.rescheduleSec, fd.IntervalSec)
	return f.rescheduleErr
}

func (f *fakeSched) Refresh(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshed = append(f.refreshed, id)
	return nil
}

func (f *fakeSched) RefreshAll(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refreshAll++
	return nil
}

func (f *fakeSched) Preview(_ context.Context, feedURL string) (*sched.Preview, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.previewCalls = append(f.previewCalls, feedURL)
	if err := f.previewErr[feedURL]; err != nil {
		return nil, err
	}
	p, ok := f.previews[feedURL]
	if !ok {
		return nil, fmt.Errorf("fake preview: %w", resolve.ErrNoFeed)
	}
	return p, nil
}

func (f *fakeSched) Subscribe(ctx context.Context, sub sched.Subscription) (store.Feed, error) {
	f.mu.Lock()
	f.subs = append(f.subs, sub)
	err := f.subscribeErr
	p := f.previews[sub.FeedURL]
	entries := f.entries[sub.FeedURL]
	hook := f.onSubscribe
	f.mu.Unlock()
	if hook != nil {
		hook(sub)
	}
	if err != nil {
		return store.Feed{}, err
	}
	kind, title := store.KindRSS, ""
	if p != nil {
		kind, title = p.Kind, p.Title
	}
	now := time.Now()
	return f.st.CreateFeed(ctx, store.NewFeed{
		Kind: kind, URL: sub.FeedURL, Title: sub.Title, OriginalTitle: title,
		FolderID: sub.FolderID, FetchedAt: now.Add(-time.Minute), NextFetchAt: now.Add(time.Hour),
	}, entries, config.InitialUnread)
}

// fakeResolver answers from fixed tables.
type fakeResolver struct {
	mu      sync.Mutex
	results map[string]*resolve.Result
	errs    map[string]error
	calls   []string
}

func (f *fakeResolver) Resolve(_ context.Context, raw string) (*resolve.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, raw)
	if err := f.errs[raw]; err != nil {
		return nil, err
	}
	if r, ok := f.results[raw]; ok {
		return r, nil
	}
	return nil, resolve.ErrNoFeed
}

// testEnv is a server backed by a real SQLite store in a temp dir.
type testEnv struct {
	t     *testing.T
	st    *store.SQLite
	sched *fakeSched
	res   *fakeResolver
	srv   *server
	h     http.Handler
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "web.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fs := &fakeSched{st: st, previews: map[string]*sched.Preview{}, previewErr: map[string]error{},
		entries: map[string][]store.NewEntry{}}
	fr := &fakeResolver{results: map[string]*resolve.Result{}, errs: map[string]error{}}
	cfg := config.Default()
	srv, err := newServer(Deps{Config: cfg, Store: st, Sched: fs, Resolver: fr})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	return &testEnv{t: t, st: st, sched: fs, res: fr, srv: srv, h: srv.handler()}
}

// makeEntries builds n entries, the newest last, published hourly up to
// two hours ago.
func makeEntries(prefix string, n int) []store.NewEntry {
	out := make([]store.NewEntry, n)
	base := time.Now().Add(-time.Duration(n+2) * time.Hour)
	for i := range out {
		out[i] = store.NewEntry{
			GUID:        fmt.Sprintf("%s-%d", prefix, i),
			URL:         fmt.Sprintf("https://example.com/%s/%d", prefix, i),
			Title:       fmt.Sprintf("%s entry %d", prefix, i),
			SummaryHTML: fmt.Sprintf("<p>Summary of %s entry %d</p>", prefix, i),
			PublishedAt: base.Add(time.Duration(i) * time.Hour),
		}
	}
	return out
}

// addFeed subscribes a feed directly through the store.
func (e *testEnv) addFeed(kind store.Kind, slug, title string, folderID int64, entries []store.NewEntry) store.Feed {
	e.t.Helper()
	now := time.Now()
	f, err := e.st.CreateFeed(context.Background(), store.NewFeed{
		Kind: kind, URL: "https://example.com/" + slug + "/feed.xml", Title: title, OriginalTitle: title,
		FolderID: folderID, FetchedAt: now.Add(-time.Hour), NextFetchAt: now.Add(time.Hour),
	}, entries, config.InitialUnread)
	if err != nil {
		e.t.Fatalf("create feed %s: %v", slug, err)
	}
	return f
}

func (e *testEnv) addFolder(name string) store.Folder {
	e.t.Helper()
	f, err := e.st.CreateFolder(context.Background(), name)
	if err != nil {
		e.t.Fatalf("create folder %s: %v", name, err)
	}
	return f
}

// entries returns all entries of a feed, newest first.
func (e *testEnv) entries(feedID int64) []store.EntryView {
	e.t.Helper()
	p, err := e.st.ListEntries(context.Background(), store.ListQuery{
		Scope: store.Scope{Kind: store.ScopeFeed, ID: feedID}, Limit: 1000})
	if err != nil {
		e.t.Fatalf("list entries: %v", err)
	}
	return p.Entries
}

func (e *testEnv) entry(id int64) store.EntryView {
	e.t.Helper()
	v, err := e.st.GetEntry(context.Background(), id)
	if err != nil {
		e.t.Fatalf("get entry %d: %v", id, err)
	}
	return v
}

type reqOpt func(*http.Request)

func htmx(r *http.Request) { r.Header.Set("HX-Request", "true") }

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

// testHost is the Host test requests carry unless withHost says otherwise:
// the default listen address, as a browser on the same machine sends it.
const testHost = "localhost:8080"

func withHost(h string) reqOpt { return func(r *http.Request) { r.Host = h } }

// captureLogs sends the server's log output, debug level included, to the
// returned buffer from now on.
func (e *testEnv) captureLogs() *bytes.Buffer {
	var buf bytes.Buffer
	e.srv.log = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &buf
}

// do performs a request against the full handler (middleware included).
func (e *testEnv) do(method, target string, form url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	e.t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	r := httptest.NewRequest(method, target, body)
	r.Host = testHost
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, o := range opts {
		o(r)
	}
	w := httptest.NewRecorder()
	e.h.ServeHTTP(w, r)
	return w
}

func (e *testEnv) get(target string, opts ...reqOpt) *httptest.ResponseRecorder {
	return e.do(http.MethodGet, target, nil, opts...)
}

func (e *testEnv) post(target string, form url.Values, opts ...reqOpt) *httptest.ResponseRecorder {
	if form == nil {
		form = url.Values{}
	}
	return e.do(http.MethodPost, target, form, opts...)
}

func wantStatus(t *testing.T, w *httptest.ResponseRecorder, status int) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("status = %d, want %d; body:\n%s", w.Code, status, w.Body.String())
	}
}

func wantContains(t *testing.T, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(body, s) {
			t.Errorf("body does not contain %q\nbody:\n%s", s, body)
		}
	}
}

func wantNotContains(t *testing.T, body string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(body, s) {
			t.Errorf("body unexpectedly contains %q", s)
		}
	}
}

var oobBadgeRE = regexp.MustCompile(`<span class="badge" id="([a-z0-9-]+)" hx-swap-oob="true">(\d*)</span>`)

// oobBadges extracts out-of-band badge values ("" = zero) by element id.
func oobBadges(body string) map[string]string {
	out := map[string]string{}
	for _, m := range oobBadgeRE.FindAllStringSubmatch(body, -1) {
		out[m[1]] = m[2]
	}
	return out
}

var cardRE = regexp.MustCompile(`<article class="card[^"]*" id="entry-(\d+)"`)

// cardIDs lists the entry ids of the cards in a body, in order.
func cardIDs(body string) []string {
	var ids []string
	for _, m := range cardRE.FindAllStringSubmatch(body, -1) {
		ids = append(ids, m[1])
	}
	return ids
}

var hxGetMoreRE = regexp.MustCompile(`hx-get="(/entries\?[^"]+)"`)

// loadMoreURL returns the htmx URL of the "Load more" button, or "".
func loadMoreURL(body string) string {
	m := hxGetMoreRE.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return strings.ReplaceAll(m[1], "&amp;", "&")
}

func idStr(id int64) string { return fmt.Sprint(id) }
