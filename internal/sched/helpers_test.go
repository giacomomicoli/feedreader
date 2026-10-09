package sched

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// t0 is a fixed reference time with whole seconds (the store's precision).
var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// firstBackoff is the delay after a first failure at the default interval:
// a quick retry, at most one interval away.
var firstBackoff = min(config.DefaultPollInterval, config.FirstRetryDelay)

// secondBackoff is the delay after a second failure in a row at the default
// interval (interval × 2, capped).
var secondBackoff = min(2*config.DefaultPollInterval, config.MaxBackoff)

// longHint is a server freshness hint (RSS <ttl>, Cache-Control max-age)
// longer than the default interval and below the cap, in whole minutes like
// <ttl>.
var longHint = ((config.DefaultPollInterval + config.MaxBackoff) / 2).Truncate(time.Minute)

// testClock is a settable clock for Scheduler.now.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock(t time.Time) *testClock { return &testClock{t: t} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// fakeFetcher records requests and answers them with respond. It tracks how
// many fetches run at once, overall and per URL.
type fakeFetcher struct {
	mu        sync.Mutex
	respond   func(ctx context.Context, r fetch.Request) (*fetch.Result, error)
	calls     []fetch.Request
	active    int
	maxActive int
	perURL    map[string]int
	maxPerURL int
}

func newFetcher(respond func(ctx context.Context, r fetch.Request) (*fetch.Result, error)) *fakeFetcher {
	return &fakeFetcher{respond: respond, perURL: map[string]int{}}
}

// staticFetcher answers every request with the same result and error.
func staticFetcher(res *fetch.Result, err error) *fakeFetcher {
	return newFetcher(func(context.Context, fetch.Request) (*fetch.Result, error) { return res, err })
}

// routeFetcher answers by exact URL; unknown URLs get a 404.
func routeFetcher(routes map[string]*fetch.Result) *fakeFetcher {
	return newFetcher(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		if res, ok := routes[r.URL]; ok {
			return res, nil
		}
		return nil, &fetch.StatusError{URL: r.URL, StatusCode: 404}
	})
}

func (f *fakeFetcher) Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
	f.mu.Lock()
	f.calls = append(f.calls, r)
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	f.perURL[r.URL]++
	f.maxPerURL = max(f.maxPerURL, f.perURL[r.URL])
	respond := f.respond
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.perURL[r.URL]--
		f.mu.Unlock()
	}()
	return respond(ctx, r)
}

func (f *fakeFetcher) setRespond(respond func(ctx context.Context, r fetch.Request) (*fetch.Result, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = respond
}

func (f *fakeFetcher) Calls() []fetch.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fetch.Request(nil), f.calls...)
}

func (f *fakeFetcher) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeFetcher) Active() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.active
}

func (f *fakeFetcher) MaxActive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

func (f *fakeFetcher) MaxPerURL() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxPerURL
}

// --- responses and documents ---

// ok is a 200 response served from u.
func ok(u string, body []byte) *fetch.Result {
	return &fetch.Result{StatusCode: 200, Body: body, ContentType: "application/atom+xml; charset=utf-8",
		ETag: `"v2"`, LastModified: "Thu, 01 Oct 2026 18:00:00 GMT", FinalURL: u}
}

// notModified is a 304 response served from u.
func notModified(u string) *fetch.Result {
	return &fetch.Result{StatusCode: 304, NotModified: true, ETag: `"v1"`, FinalURL: u}
}

// item is one entry of a generated feed document. Zero times are omitted.
type item struct {
	id, title          string
	published, updated time.Time
}

// itemsNewestFirst returns n items "<prefix>0".."<prefix>n-1", each an hour
// older than the previous one.
func itemsNewestFirst(prefix string, n int, newest time.Time) []item {
	out := make([]item, n)
	for i := range out {
		id := fmt.Sprintf("%s%d", prefix, i)
		out[i] = item{id: id, title: "Title " + id, published: newest.Add(-time.Duration(i) * time.Hour)}
	}
	return out
}

// atomDoc renders an Atom 1.0 feed of site.example.
func atomDoc(title string, items ...item) []byte {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<feed xmlns=\"http://www.w3.org/2005/Atom\">\n")
	fmt.Fprintf(&b, "<title>%s</title>\n<id>urn:test:feed</id>\n<updated>%s</updated>\n", html.EscapeString(title), t0.Format(time.RFC3339))
	b.WriteString("<link rel=\"alternate\" href=\"https://site.example/\"/>\n<icon>https://site.example/icon.png</icon>\n")
	for _, it := range items {
		fmt.Fprintf(&b, "<entry><id>%s</id><title>%s</title><link href=\"https://site.example/%s\"/>",
			html.EscapeString(it.id), html.EscapeString(it.title), html.EscapeString(it.id))
		if !it.published.IsZero() {
			fmt.Fprintf(&b, "<published>%s</published>", it.published.Format(time.RFC3339))
		}
		if !it.updated.IsZero() {
			fmt.Fprintf(&b, "<updated>%s</updated>", it.updated.Format(time.RFC3339))
		}
		fmt.Fprintf(&b, "<summary>Summary of %s</summary></entry>\n", html.EscapeString(it.id))
	}
	b.WriteString("</feed>\n")
	return []byte(b.String())
}

// minutes converts d to whole minutes, the unit of RSS <ttl>.
func minutes(d time.Duration) int { return int(d / time.Minute) }

// rssDoc renders an RSS 2.0 feed; ttlMinutes 0 omits <ttl>.
func rssDoc(title string, ttlMinutes int, items ...item) []byte {
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"utf-8\"?>\n<rss version=\"2.0\"><channel>\n")
	fmt.Fprintf(&b, "<title>%s</title><link>https://site.example/</link><description>d</description>\n", html.EscapeString(title))
	if ttlMinutes > 0 {
		fmt.Fprintf(&b, "<ttl>%d</ttl>\n", ttlMinutes)
	}
	for _, it := range items {
		fmt.Fprintf(&b, "<item><guid isPermaLink=\"false\">%s</guid><title>%s</title><link>https://site.example/%s</link>",
			html.EscapeString(it.id), html.EscapeString(it.title), html.EscapeString(it.id))
		if !it.published.IsZero() {
			fmt.Fprintf(&b, "<pubDate>%s</pubDate>", it.published.Format(time.RFC1123Z))
		}
		b.WriteString("<description>Body</description></item>\n")
	}
	b.WriteString("</channel></rss>\n")
	return []byte(b.String())
}

// --- store and scheduler ---

// openStore opens a fresh SQLite database in a temporary directory.
func openStore(t *testing.T) *store.SQLite {
	t.Helper()
	st, err := store.OpenSQLite(filepath.Join(t.TempDir(), "feedreader.db"))
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return st
}

// syncBuffer is a goroutine-safe log sink.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

// testLogger logs at debug level and prints the log when the test fails.
func testLogger(t *testing.T) *slog.Logger {
	buf := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("scheduler log:\n%s", buf.String())
		}
	})
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// logRecord is one line of the scheduler's log, decoded with the fields of
// the poll log lines; the others are ignored. New is nil on a line without
// "new", which a plain int would not tell from new=0.
type logRecord struct {
	Level     slog.Level    `json:"level"`
	Msg       string        `json:"msg"`
	Feed      int64         `json:"feed"`
	URL       string        `json:"url"`
	Status    int           `json:"status"`
	New       *int          `json:"new"`
	Took      time.Duration `json:"took"`
	NextFetch time.Time     `json:"next_fetch"`
}

// logCapture is the scheduler log recorded by captureLog.
type logCapture struct {
	t   *testing.T
	buf *syncBuffer
}

// captureLog records what s logs from now on, debug level included, as JSON
// lines, and prints it when the test fails.
func captureLog(t *testing.T, s *Scheduler) *logCapture {
	t.Helper()
	buf := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("captured scheduler log:\n%s", buf.String())
		}
	})
	s.log = slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &logCapture{t: t, buf: buf}
}

// String returns the raw log.
func (c *logCapture) String() string { return c.buf.String() }

// records decodes the lines logged so far at level or above.
func (c *logCapture) records(level slog.Level) []logRecord {
	c.t.Helper()
	var out []logRecord
	dec := json.NewDecoder(strings.NewReader(c.buf.String()))
	for {
		var r logRecord
		err := dec.Decode(&r)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			c.t.Fatalf("decoding the scheduler log: %v", err)
		}
		if r.Level >= level {
			out = append(out, r)
		}
	}
}

// withMsg returns the lines logged so far, at any level, with message msg.
func (c *logCapture) withMsg(msg string) []logRecord {
	c.t.Helper()
	var out []logRecord
	for _, r := range c.records(slog.LevelDebug) {
		if r.Msg == msg {
			out = append(out, r)
		}
	}
	return out
}

// messages returns the messages of recs, in order.
func messages(recs []logRecord) []string {
	out := make([]string, len(recs))
	for i, r := range recs {
		out[i] = r.Msg
	}
	return out
}

// newTestScheduler returns a scheduler with the default config and workers
// fetch workers, on the real clock.
func newTestScheduler(t *testing.T, st store.Store, f Fetcher, workers int) *Scheduler {
	t.Helper()
	cfg := config.Default()
	cfg.FetchWorkers = workers
	return New(st, f, cfg, testLogger(t))
}

// newClockedScheduler returns a scheduler whose clock is c.
func newClockedScheduler(t *testing.T, st store.Store, f Fetcher, c *testClock) *Scheduler {
	t.Helper()
	s := newTestScheduler(t, st, f, config.DefaultFetchWorkers)
	s.now = c.Now
	return s
}

// addFeed subscribes url directly in the store (bypassing the scheduler),
// with entries derived from items and the given next fetch time.
func addFeed(t *testing.T, st store.Store, url string, next time.Time, items ...item) store.Feed {
	t.Helper()
	return addFeedOfKind(t, st, store.KindRSS, url, next, items...)
}

// addFeedOfKind is addFeed for a feed of the given kind.
func addFeedOfKind(t *testing.T, st store.Store, kind store.Kind, url string, next time.Time, items ...item) store.Feed {
	t.Helper()
	entries := make([]store.NewEntry, len(items))
	for i, it := range items {
		entries[i] = store.NewEntry{GUID: it.id, URL: "https://site.example/" + it.id, Title: it.title,
			SummaryHTML: "Summary of " + it.id, PublishedAt: it.published, UpdatedAt: it.updated}
	}
	f, err := st.CreateFeed(t.Context(), store.NewFeed{
		Kind:         kind,
		URL:          url,
		Title:        "Feed " + url,
		ETag:         `"v1"`,
		LastModified: "Wed, 30 Sep 2026 12:00:00 GMT",
		FetchedAt:    t0,
		NextFetchAt:  next,
	}, entries, config.InitialUnread)
	if err != nil {
		t.Fatalf("CreateFeed(%s): %v", url, err)
	}
	return f
}

func getFeed(t *testing.T, st store.Store, id int64) store.Feed {
	t.Helper()
	f, err := st.GetFeed(context.Background(), id)
	if err != nil {
		t.Fatalf("GetFeed(%d): %v", id, err)
	}
	return f
}

// feedEntries returns the feed's stored entries keyed by GUID.
func feedEntries(t *testing.T, st store.Store, feedID int64) map[string]store.EntryView {
	t.Helper()
	page, err := st.ListEntries(context.Background(), store.ListQuery{
		Scope: store.Scope{Kind: store.ScopeFeed, ID: feedID}, Limit: 1000})
	if err != nil {
		t.Fatalf("ListEntries(feed %d): %v", feedID, err)
	}
	out := make(map[string]store.EntryView, len(page.Entries))
	for _, e := range page.Entries {
		out[e.GUID] = e
	}
	return out
}

// unreadGUIDs returns the GUIDs of the unread entries in entries.
func unreadGUIDs(entries map[string]store.EntryView) map[string]bool {
	out := map[string]bool{}
	for g, e := range entries {
		if !e.IsRead {
			out[g] = true
		}
	}
	return out
}

func mustNoErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// waitFor polls cond until it holds, failing the test after 10 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// startRun runs s in the background. The returned stop cancels Run and
// waits for it to return; it also runs at cleanup.
func startRun(t *testing.T, s *Scheduler) (stop func() time.Duration) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	waitFor(t, "Run to start", s.running.Load)
	var (
		once    sync.Once
		elapsed time.Duration
	)
	stop = func() time.Duration {
		once.Do(func() {
			start := time.Now()
			cancel()
			select {
			case err := <-done:
				elapsed = time.Since(start)
				if err != nil {
					t.Errorf("Run: %v", err)
				}
			case <-time.After(10 * time.Second):
				t.Errorf("Run did not return after cancel")
			}
		})
		return elapsed
	}
	t.Cleanup(func() { stop() })
	return stop
}
