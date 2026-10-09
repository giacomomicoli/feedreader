package e2e

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
	"github.com/giacomomicoli/feedreader/internal/web"
)

// waitTimeout bounds every wait for background work (scheduler fetches).
const waitTimeout = 10 * time.Second

// app is the real application stack, wired as in cmd/feedreader, with the
// scheduler running and the web handler behind an httptest server.
type app struct {
	t   *testing.T
	st  *store.SQLite
	srv *httptest.Server
	hc  *http.Client
}

// startApp opens a fresh database in a temp dir and starts the scheduler
// and the web UI. Everything is stopped in reverse order at cleanup.
func startApp(t *testing.T) *app {
	t.Helper()
	cfg := config.Default()
	cfg.DataDir = t.TempDir()
	cfg.Listen = "127.0.0.1:0"
	log := slog.New(slog.NewTextHandler(t.Output(), &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := store.OpenSQLite(cfg.DBPath())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	fetcher := fetch.New(cfg)
	scheduler := sched.New(st, fetcher, cfg, log.With("component", "sched"))
	handler, err := web.New(web.Deps{
		Config:   cfg,
		Store:    st,
		Sched:    scheduler,
		Resolver: resolve.New(scheduler.ResolveFetcher()),
		Log:      log.With("component", "web"),
	})
	if err != nil {
		_ = st.Close()
		t.Fatalf("web.New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	schedDone := make(chan error, 1)
	go func() { schedDone <- scheduler.Run(ctx) }()
	srv := httptest.NewServer(handler)
	t.Cleanup(func() {
		srv.Close()
		cancel()
		select {
		case err := <-schedDone:
			if err != nil {
				t.Errorf("scheduler Run: %v", err)
			}
		case <-time.After(waitTimeout):
			t.Errorf("scheduler did not stop within %s", waitTimeout)
		}
		if err := st.Close(); err != nil {
			t.Errorf("close store: %v", err)
		}
	})

	return &app{
		t:   t,
		st:  st,
		srv: srv,
		hc: &http.Client{
			Timeout: waitTimeout,
			// Redirects are part of what is being tested.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// page is a response from the app, with its HTML parsed.
type page struct {
	status int
	header http.Header
	body   string
	doc    *html.Node
}

// requestOpts describe how the browser sends a request.
type requestOpts struct {
	htmx    bool   // send HX-Request like htmx does
	site    string // Sec-Fetch-Site; "" = same-origin
	current string // page the request is made from (HX-Current-URL)
}

// visit loads a full page, as a browser navigation does.
func (a *app) visit(t *testing.T, path string) *page {
	t.Helper()
	return a.do(t, http.MethodGet, path, nil, requestOpts{})
}

// hxGet is an htmx GET (hx-get).
func (a *app) hxGet(t *testing.T, path string) *page {
	t.Helper()
	return a.do(t, http.MethodGet, path, nil, requestOpts{htmx: true})
}

// hxPost is an htmx form submission (hx-post) from the page at current.
func (a *app) hxPost(t *testing.T, path string, form url.Values, current string) *page {
	t.Helper()
	return a.do(t, http.MethodPost, path, form, requestOpts{htmx: true, current: current})
}

func (a *app) do(t *testing.T, method, path string, form url.Values, o requestOpts) *page {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequest(method, a.srv.URL+path, body)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	site := o.site
	if site == "" {
		site = "same-origin"
	}
	req.Header.Set("Sec-Fetch-Site", site)
	if o.htmx {
		req.Header.Set("HX-Request", "true")
		if o.current != "" {
			req.Header.Set("HX-Current-URL", a.srv.URL+o.current)
		}
	}
	resp, err := a.hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("%s %s: read body: %v", method, path, err)
	}
	doc, err := html.Parse(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("%s %s: parse HTML: %v", method, path, err)
	}
	return &page{status: resp.StatusCode, header: resp.Header, body: string(raw), doc: doc}
}

// expect fails the test unless the response has the given status.
func (p *page) expect(t *testing.T, status int) *page {
	t.Helper()
	if p.status != status {
		t.Fatalf("status = %d, want %d; body:\n%s", p.status, status, p.body)
	}
	return p
}

// --- HTML inspection ---

// card is what the test reads back from a rendered entry card.
type card struct {
	id     int64
	title  string
	href   string
	thumb  string
	status string // "Unread", "Read", "Unwatched", "Watched"
	// summary is where the Summary link leads: the entry page, which htmx
	// loads into the entry dialog; "" without a link.
	summary string
	read    bool
	later   bool
	fav     bool
	tags    []string
}

// cards returns the entry cards in document order.
func (p *page) cards(t *testing.T) []card {
	t.Helper()
	var out []card
	for _, n := range findAll(p.doc, func(n *html.Node) bool { return isElem(n, "article") && hasClass(n, "card") }) {
		id, err := strconv.ParseInt(strings.TrimPrefix(attr(n, "id"), "entry-"), 10, 64)
		if err != nil {
			t.Fatalf("card without entry id: %q", attr(n, "id"))
		}
		c := card{id: id, read: hasClass(n, "is-read")}
		if h := findFirst(n, elemClass("h3", "card-title")); h != nil {
			c.title = textOf(h)
			if a := findFirst(h, elem("a")); a != nil {
				c.href = attr(a, "href")
			}
		}
		if th := findFirst(n, elemClass("a", "card-thumb")); th != nil {
			if img := findFirst(th, elem("img")); img != nil {
				c.thumb = attr(img, "src")
			}
		}
		if s := findFirst(n, elemClass("span", "status")); s != nil {
			c.status = textOf(s)
		}
		if a := findFirst(n, elemClass("a", "card-summary")); a != nil {
			c.summary = attr(a, "href")
			if hx := attr(a, "hx-get"); hx != c.summary || attr(a, "hx-target") != "#entry-dialog" {
				t.Errorf("Summary link of entry %d: href %q, hx-get %q, hx-target %q", id, c.summary, hx, attr(a, "hx-target"))
			}
		}
		for _, f := range findAll(n, elemClass("span", "flag")) {
			if hasClass(f, "flag-fav") {
				c.fav = true
			} else {
				c.later = true
			}
		}
		if ul := findFirst(n, elemClass("ul", "card-tags")); ul != nil {
			for _, a := range findAll(ul, elem("a")) {
				c.tags = append(c.tags, textOf(a))
			}
		}
		out = append(out, c)
	}
	return out
}

// cardTitles lists the titles of cs.
func cardTitles(cs []card) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.title
	}
	return out
}

// loadMore returns the hx-get URL of the "Load more" button, or "".
func (p *page) loadMore() string {
	div := findFirst(p.doc, elemClass("div", "load-more"))
	if div == nil {
		return ""
	}
	if a := findFirst(div, elem("a")); a != nil {
		return attr(a, "hx-get")
	}
	return ""
}

// badge returns the number shown by the badge with the given element id
// (0 when empty) and whether it is an out-of-band swap. ok is false when
// the badge is absent.
func (p *page) badge(id string) (n int, oob, ok bool) {
	b := byID(p.doc, id)
	if b == nil {
		return 0, false, false
	}
	if txt := textOf(b); txt != "" {
		var err error
		if n, err = strconv.Atoi(txt); err != nil {
			return -1, false, true
		}
	}
	return n, attr(b, "hx-swap-oob") == "true", true
}

// sidebarTags returns the sidebar tag list as name → count.
func (p *page) sidebarTags() map[string]string {
	ul := byID(p.doc, "sidebar-tags")
	if ul == nil {
		return nil
	}
	out := map[string]string{}
	for _, a := range findAll(ul, elemClass("a", "tag-row")) {
		name, count := "", ""
		if s := findFirst(a, elemClass("span", "nav-label")); s != nil {
			name = textOf(s)
		}
		if s := findFirst(a, elemClass("span", "count")); s != nil {
			count = textOf(s)
		}
		out[name] = count
	}
	return out
}

// formValues collects the successful controls of the first form whose
// hx-post is action, the way a browser serializes it: text and hidden
// inputs, checked radios, and selected options.
func (p *page) formValues(t *testing.T, action string) url.Values {
	t.Helper()
	form := findFirst(p.doc, func(n *html.Node) bool { return isElem(n, "form") && attr(n, "hx-post") == action })
	if form == nil {
		t.Fatalf("no form posting to %s in:\n%s", action, p.body)
	}
	v := url.Values{}
	for _, in := range findAll(form, elem("input")) {
		name := attr(in, "name")
		if name == "" {
			continue
		}
		switch attr(in, "type") {
		case "radio", "checkbox":
			if hasAttr(in, "checked") {
				v.Add(name, attr(in, "value"))
			}
		default:
			v.Add(name, attr(in, "value"))
		}
	}
	for _, sel := range findAll(form, elem("select")) {
		opts := findAll(sel, elem("option"))
		chosen := ""
		if len(opts) > 0 {
			chosen = attr(opts[0], "value")
		}
		for _, o := range opts {
			if hasAttr(o, "selected") {
				chosen = attr(o, "value")
			}
		}
		v.Set(attr(sel, "name"), chosen)
	}
	return v
}

func findAll(n *html.Node, match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	for d := range n.Descendants() {
		if match(d) {
			out = append(out, d)
		}
	}
	return out
}

func findFirst(n *html.Node, match func(*html.Node) bool) *html.Node {
	for d := range n.Descendants() {
		if match(d) {
			return d
		}
	}
	return nil
}

func byID(n *html.Node, id string) *html.Node {
	return findFirst(n, func(d *html.Node) bool { return d.Type == html.ElementNode && attr(d, "id") == id })
}

func isElem(n *html.Node, tag string) bool { return n.Type == html.ElementNode && n.Data == tag }

func elem(tag string) func(*html.Node) bool {
	return func(n *html.Node) bool { return isElem(n, tag) }
}

func elemClass(tag, class string) func(*html.Node) bool {
	return func(n *html.Node) bool { return isElem(n, tag) && hasClass(n, class) }
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

func hasAttr(n *html.Node, key string) bool {
	for _, a := range n.Attr {
		if a.Key == key {
			return true
		}
	}
	return false
}

func hasClass(n *html.Node, class string) bool {
	for _, c := range strings.Fields(attr(n, "class")) {
		if c == class {
			return true
		}
	}
	return false
}

// textOf returns the visible text of n with whitespace collapsed.
func textOf(n *html.Node) string {
	var b strings.Builder
	for d := range n.Descendants() {
		if d.Type == html.TextNode {
			b.WriteString(d.Data)
			b.WriteByte(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// eventually polls cond until it holds or waitTimeout passes.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", waitTimeout, what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
