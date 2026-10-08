package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

func TestSecurityHeadersOnEveryResponse(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeed(store.KindRSS, "a", "A", 0, makeEntries("a", 1))
	responses := map[string]*httptest.ResponseRecorder{
		"home":       e.get("/"),
		"static":     e.get("/static/app.css"),
		"not found":  e.get("/missing"),
		"fragment":   e.post("/entries/"+idStr(e.entries(f.ID)[0].ID)+"/read", nil, htmx),
		"redirect":   e.post("/entries/"+idStr(e.entries(f.ID)[0].ID)+"/unread", nil),
		"cross-site": e.post("/refresh", nil, withHeader("Sec-Fetch-Site", "cross-site")),
		"bad host":   e.get("/", withHost("rebind.attacker.example")),
	}
	want := map[string]string{
		"Content-Security-Policy": "default-src 'self'; img-src 'self' https: http: data:; style-src 'self'; script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'",
		"Referrer-Policy":         "no-referrer",
		"X-Content-Type-Options":  "nosniff",
		"X-Frame-Options":         "DENY",
	}
	for name, w := range responses {
		for k, v := range want {
			if got := w.Header().Get(k); got != v {
				t.Errorf("%s: %s = %q, want %q", name, k, got, v)
			}
		}
	}
}

func TestCrossSitePostIsRejected(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeed(store.KindRSS, "a", "A", 0, makeEntries("a", 1))
	id := e.entries(f.ID)[0].ID
	path := "/entries/" + idStr(id) + "/read"

	w := e.post(path, nil, htmx, withHeader("Sec-Fetch-Site", "cross-site"))
	wantStatus(t, w, http.StatusForbidden)
	w = e.post(path, nil, withHeader("Origin", "https://evil.example"))
	wantStatus(t, w, http.StatusForbidden)
	if e.entry(id).IsRead {
		t.Fatal("cross-site request changed state")
	}
	wantStatus(t, e.post("/folders", url.Values{"name": {"x"}}, withHeader("Sec-Fetch-Site", "cross-site")), http.StatusForbidden)
	wantStatus(t, e.post("/feeds/"+idStr(f.ID)+"/unsubscribe", nil, withHeader("Sec-Fetch-Site", "cross-site")), http.StatusForbidden)

	// Same-origin requests and safe methods pass.
	wantStatus(t, e.post(path, nil, htmx, withHeader("Sec-Fetch-Site", "same-origin")), http.StatusOK)
	wantStatus(t, e.get("/", withHeader("Sec-Fetch-Site", "cross-site")), http.StatusOK)
}

func TestRequestBodyIsCapped(t *testing.T) {
	e := newTestEnv(t)
	w := e.post("/folders", url.Values{"name": {strings.Repeat("x", maxRequestBody+1)}}, htmx)
	wantStatus(t, w, http.StatusRequestEntityTooLarge)
	if folders, _ := e.st.ListFolders(context.Background()); len(folders) != 0 {
		t.Errorf("oversized request created %+v", folders)
	}
}

func TestPanicIsRecoveredWithoutLeakingDetails(t *testing.T) {
	e := newTestEnv(t)
	h := e.srv.observe(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("secret database password")
	}))
	for _, opts := range [][]reqOpt{nil, {htmx}} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		for _, o := range opts {
			o(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		wantStatus(t, w, http.StatusInternalServerError)
		wantNotContains(t, w.Body.String(), "secret", "goroutine")
	}
}

func TestEntryXSSIsEscapedAndJavascriptURLsNeutralized(t *testing.T) {
	e := newTestEnv(t)
	now := time.Now().Add(-time.Hour)
	f, err := e.st.CreateFeed(context.Background(), store.NewFeed{
		Kind: store.KindRSS, URL: "https://evil.example/feed", Title: `<img src=x onerror=alert(1)>`,
		IconURL: "javascript:alert(2)", FetchedAt: now,
	}, []store.NewEntry{{
		GUID: "x", Title: "<script>alert(1)</script>", URL: "javascript:alert(1)",
		ThumbnailURL: "javascript:alert(3)", PublishedAt: now,
		SummaryHTML: `<p>"quoted" <b>bold</b></p><img src="data:image/svg+xml,<svg onload=alert(4)>">`,
	}}, 5)
	if err != nil {
		t.Fatal(err)
	}
	ev := e.entries(f.ID)[0]
	cases := []struct {
		name       string
		w          *httptest.ResponseRecorder
		titleShown bool
		entryShown bool
	}{
		{"home", e.get("/"), true, true},
		{"card fragment", e.post("/entries/"+idStr(ev.ID)+"/read", nil, htmx), true, true},
		{"settings", e.get("/feeds/" + idStr(f.ID)), true, false},
	}
	for _, c := range cases {
		body := c.w.Body.String()
		wantStatus(t, c.w, http.StatusOK)
		wantNotContains(t, body, "<script>alert", "javascript:", "<img src=x", "data:image/svg")
		checkCSPMarkup(t, c.name, body) // no on* attributes, no inline scripts
		if c.titleShown {
			wantContains(t, body, "&lt;img src=x onerror=alert(1)&gt;")
		}
		if c.entryShown {
			wantContains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;")
		}
	}
}

// TestPagesHaveNoInlineScriptsStylesOrEval walks every page's markup: CSP
// forbids inline scripts, style attributes and hx-on handlers.
func TestPagesHaveNoInlineScriptsStylesOrEval(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fo := e.addFolder("Folder")
	f := e.addFeed(store.KindYouTube, "yt", "Channel", fo.ID, makeEntries("y", 7))
	tag, err := e.st.AddEntryTag(ctx, e.entries(f.ID)[0].ID, "tagged")
	if err != nil {
		t.Fatal(err)
	}
	pages := []string{
		"/", "/?filter=all", "/?scope=folder&id=" + idStr(fo.ID), "/?scope=tag&id=" + idStr(tag.ID),
		"/?scope=feed&id=" + idStr(f.ID), "/add", "/feeds/" + idStr(f.ID),
		"/feeds/" + idStr(f.ID) + "/unsubscribe", "/folders/" + idStr(fo.ID) + "/delete", "/missing",
	}
	for _, p := range pages {
		w := e.get(p)
		checkCSPMarkup(t, p, w.Body.String())
	}
	checkCSPMarkup(t, "empty", newTestEnv(t).get("/").Body.String())
}

func checkCSPMarkup(t *testing.T, name, body string) {
	t.Helper()
	z := html.NewTokenizer(strings.NewReader(body))
	for {
		tt := z.Next()
		if tt == html.ErrorToken {
			if z.Err() != io.EOF {
				t.Errorf("%s: tokenizer: %v", name, z.Err())
			}
			return
		}
		if tt != html.StartTagToken && tt != html.SelfClosingTagToken {
			continue
		}
		tok := z.Token()
		if tok.Data == "style" {
			t.Errorf("%s: inline <style> element", name)
		}
		hasSrc := false
		for _, a := range tok.Attr {
			switch {
			case a.Key == "style":
				t.Errorf("%s: style attribute on <%s>", name, tok.Data)
			case strings.HasPrefix(a.Key, "on"):
				t.Errorf("%s: event handler %s on <%s>", name, a.Key, tok.Data)
			case strings.HasPrefix(a.Key, "hx-on"), a.Key == "hx-vals" && strings.HasPrefix(a.Val, "js:"):
				t.Errorf("%s: eval-based htmx attribute %s", name, a.Key)
			case a.Key == "src":
				hasSrc = true
			}
		}
		if tok.Data == "script" && !hasSrc {
			t.Errorf("%s: inline <script>", name)
		}
	}
}

func TestStaticAssetsAreVersionedAndCached(t *testing.T) {
	e := newTestEnv(t)
	body := e.get("/").Body.String()
	for _, name := range []string{"app.css", "app.js", "htmx.min.js", "favicon.svg"} {
		versioned := e.srv.static.url(name)
		if !strings.Contains(body, versioned) {
			t.Errorf("page does not reference %s", versioned)
		}
		w := e.get(versioned)
		wantStatus(t, w, http.StatusOK)
		if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
			t.Errorf("%s Cache-Control = %q", name, cc)
		}
		etag := w.Header().Get("ETag")
		if etag == "" {
			t.Fatalf("%s: no ETag", name)
		}
		wantStatus(t, e.get("/static/"+name, withHeader("If-None-Match", etag)), http.StatusNotModified)
	}
	for _, name := range []string{"htmx.min.js", "app.js"} {
		if ct := e.get("/static/" + name).Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/javascript") {
			t.Errorf("%s Content-Type = %q", name, ct)
		}
	}
	if cc := e.get("/static/app.css").Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("unversioned asset cached as immutable: %q", cc)
	}
	// Browsers ask for /favicon.ico on their own; it is the SVG icon.
	fav := e.get("/favicon.ico")
	wantStatus(t, fav, http.StatusOK)
	if ct := fav.Header().Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("/favicon.ico Content-Type = %q", ct)
	}
	if cc := fav.Header().Get("Cache-Control"); strings.Contains(cc, "immutable") {
		t.Errorf("/favicon.ico cached as immutable: %q", cc)
	}
	wantStatus(t, e.get("/static/nope.js"), http.StatusNotFound)
	// Path traversal is cleaned by the mux and never reaches the templates.
	w := e.get("/static/../templates/layout.html")
	if w.Code == http.StatusOK {
		t.Fatal("traversal served a file")
	}
	if loc := w.Header().Get("Location"); loc != "" {
		wantStatus(t, e.get(loc), http.StatusNotFound)
	}
}

func TestHTMXErrorsAreRetargetedToNotice(t *testing.T) {
	e := newTestEnv(t)
	w := e.post("/entries/424242/read", nil, htmx)
	wantStatus(t, w, http.StatusNotFound)
	if w.Header().Get("HX-Retarget") != "#notice" || w.Header().Get("HX-Reswap") != "innerHTML" {
		t.Errorf("headers = %v", w.Header())
	}
	wantContains(t, w.Body.String(), `<p class="notice notice-error">`)

	// The same error without htmx is a full page.
	w = e.get("/?scope=feed&id=424242")
	wantStatus(t, w, http.StatusNotFound)
	wantContains(t, w.Body.String(), "<!doctype html>", "Not found")
}

// brokenStore fails the queries the home page needs, as a dead disk would.
type brokenStore struct{ store.Store }

func (brokenStore) ListEntries(context.Context, store.ListQuery) (store.Page, error) {
	return store.Page{}, errors.New("sqlite: disk I/O error at /var/lib/feedreader/feedreader.db")
}

func (brokenStore) UnreadCounts(context.Context) (store.UnreadCounts, error) {
	return store.UnreadCounts{}, errors.New("sqlite: disk I/O error")
}

func TestStoreFailureIsGeneric500WithoutDetails(t *testing.T) {
	e := newTestEnv(t)
	e.addFeed(store.KindRSS, "a", "A", 0, makeEntries("a", 1))
	srv, err := newServer(Deps{Store: brokenStore{e.st}, Sched: e.sched, Resolver: e.res})
	if err != nil {
		t.Fatal(err)
	}
	h := srv.handler()
	for _, opts := range [][]reqOpt{nil, {htmx}} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.Host = testHost
		for _, o := range opts {
			o(r)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		wantStatus(t, w, http.StatusInternalServerError)
		wantContains(t, w.Body.String(), "Something went wrong")
		wantNotContains(t, w.Body.String(), "sqlite", "/var/lib", "No sources yet")
	}
}

func TestNewRequiresCollaborators(t *testing.T) {
	if _, err := New(Deps{}); err == nil {
		t.Fatal("New accepted missing dependencies")
	}
	e := newTestEnv(t)
	h, err := New(Deps{Store: e.st, Sched: e.sched, Resolver: e.res})
	if err != nil || h == nil {
		t.Fatalf("New = %v, %v", h, err)
	}
}

// TestDNSRebindingIsBlockedByHostAllowlist: after DNS rebinding, a page on
// attacker.example talks to this server and is same-origin with itself, so
// CrossOriginProtection lets it through. Only the Host header gives it away.
func TestDNSRebindingIsBlockedByHostAllowlist(t *testing.T) {
	e := newTestEnv(t)
	cfg := config.Default()
	cfg.Listen = "0.0.0.0:8080"
	srv, err := newServer(Deps{Config: cfg, Store: e.st, Sched: e.sched, Resolver: e.res,
		AllowedHosts: []string{" Feeds.LAN.example. ", ""}})
	if err != nil {
		t.Fatal(err)
	}
	e.srv, e.h = srv, srv.handler()
	logs := e.captureLogs()
	f := e.addFeed(store.KindRSS, "secret", "Secret reading list", 0, makeEntries("s", 2))
	entryPath := "/entries/" + idStr(e.entries(f.ID)[0].ID) + "/read"

	const evil = "rebind.attacker.example:8080"
	rebound := []reqOpt{withHost(evil), withHeader("Origin", "http://"+evil), withHeader("Sec-Fetch-Site", "same-origin")}
	for name, w := range map[string]*httptest.ResponseRecorder{
		"GET /":           e.get("/", rebound...),
		"GET settings":    e.get("/feeds/"+idStr(f.ID), rebound...),
		"POST unsub":      e.post("/feeds/"+idStr(f.ID)+"/unsubscribe", nil, rebound...),
		"POST htmx read":  e.post(entryPath, nil, append(rebound, htmx)...),
		"GET static file": e.get("/static/app.css", rebound...),
	} {
		if w.Code != http.StatusMisdirectedRequest {
			t.Errorf("%s with Host %s: status %d, want 421", name, evil, w.Code)
		}
		wantNotContains(t, w.Body.String(), "Secret reading list", "<!doctype html>")
	}
	if _, err := e.st.GetFeed(context.Background(), f.ID); err != nil {
		t.Fatalf("rebound request unsubscribed the feed: %v", err)
	}
	if e.entries(f.ID)[0].IsRead {
		t.Error("rebound request changed state")
	}
	wantContains(t, logs.String(), "request for unknown host rejected", "host="+evil)

	// localhost, IP literals (which cannot be rebound) and the configured
	// name, compared without port, case or trailing dot.
	for _, host := range []string{"localhost:8080", "LOCALHOST", "localhost.:8080", "127.0.0.1:8080", "[::1]:8080",
		"[::1]", "192.0.2.10:8080", "[2001:db8::1]:8080", "feeds.lan.example", "FEEDS.lan.example.:443"} {
		if w := e.get("/", withHost(host)); w.Code != http.StatusOK {
			t.Errorf("Host %q: status %d, want 200", host, w.Code)
		}
	}
	for _, host := range []string{"", "lan.example", "feeds.lan.example.attacker.example", "localhost.attacker.example",
		"127.0.0.1.nip.io", "0.0.0.0.attacker.example"} {
		if w := e.get("/", withHost(host)); w.Code != http.StatusMisdirectedRequest {
			t.Errorf("Host %q: status %d, want 421", host, w.Code)
		}
	}
	// Same-origin actions through the proxy's name still work.
	w := e.post(entryPath, nil, htmx, withHost("feeds.lan.example"), withHeader("Origin", "https://feeds.lan.example"),
		withHeader("Sec-Fetch-Site", "same-origin"))
	wantStatus(t, w, http.StatusOK)
}

func TestAllowedHostsIncludeListenName(t *testing.T) {
	for listen, want := range map[string][]string{
		"127.0.0.1:8080":   {"localhost", "127.0.0.1"},
		"feeds.local:8080": {"localhost", "feeds.local"},
		"[::]:8080":        {"localhost", "::"},
		":8080":            {"localhost"},
	} {
		got := allowedHosts(listen, nil)
		if len(got) != len(want) {
			t.Errorf("allowedHosts(%q) = %v, want %v", listen, got, want)
		}
		for _, h := range want {
			if !got[h] {
				t.Errorf("allowedHosts(%q) = %v, lacks %q", listen, got, h)
			}
		}
	}
}

// TestHTMXSwapsOnlyErrorsTheAppRetargets checks the page's htmx
// responseHandling against the responses the app really sends: its own
// errors carry HX-Retarget and are swapped (into #notice); 409/422 answers
// swap in place; any other error, such as a proxy's empty 502 while the
// service restarts, must not replace the card or grid it was aimed at.
func TestHTMXSwapsOnlyErrorsTheAppRetargets(t *testing.T) {
	f := newActionFixture(t)
	page := f.get("/").Body.String()
	m := regexp.MustCompile(`<meta name="htmx-config" content='([^']+)'>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatal("no htmx-config meta")
	}
	var cfg struct {
		ResponseHandling []struct {
			Code  string
			Swap  bool
			Error bool
		}
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(m[1])), &cfg); err != nil {
		t.Fatal(err)
	}
	// htmx uses the first rule whose code, as an unanchored RegExp,
	// matches the status.
	rule := func(status int) (swap, isErr bool) {
		for _, r := range cfg.ResponseHandling {
			if ok, err := regexp.MatchString(r.Code, strconv.Itoa(status)); err != nil {
				t.Fatalf("bad code pattern %q: %v", r.Code, err)
			} else if ok {
				return r.Swap, r.Error
			}
		}
		return false, false
	}

	broken, err := newServer(Deps{Store: brokenStore{f.st}, Sched: f.sched, Resolver: f.res})
	if err != nil {
		t.Fatal(err)
	}
	f.stubPreview(blogFeedURL, "Blog", store.KindRSS, 1)
	f.post("/add/subscribe", url.Values{"feed_url": {blogFeedURL}, "folder": {"0"}}, htmx)
	appErrors := map[string]*httptest.ResponseRecorder{
		"400": f.post("/entries/abc/read", nil, htmx),
		"404": f.post("/entries/99999/read", nil, htmx),
		"413": f.post("/folders", url.Values{"name": {strings.Repeat("x", maxRequestBody+1)}}, htmx),
		"422": f.post(f.entryPath("tags"), url.Values{"tag": {" "}}, htmx),
		"500": func() *httptest.ResponseRecorder {
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.Host = testHost
			htmx(r)
			w := httptest.NewRecorder()
			broken.handler().ServeHTTP(w, r)
			return w
		}(),
	}
	for name, w := range appErrors {
		if strconv.Itoa(w.Code) != name || w.Header().Get("HX-Retarget") != "#notice" {
			t.Fatalf("%s: status %d, HX-Retarget %q", name, w.Code, w.Header().Get("HX-Retarget"))
		}
		if swap, _ := rule(w.Code); !swap {
			t.Errorf("the app's own %d message is not swapped into #notice", w.Code)
		}
	}
	f.res.results["https://blog.example.com/"] = &resolve.Result{Candidates: []resolve.Candidate{{URL: blogFeedURL}}}
	inPlace := map[string]*httptest.ResponseRecorder{
		"409": f.post("/add/resolve", url.Values{"url": {"https://blog.example.com/"}}, htmx), // already subscribed
		"422": f.post("/folders", url.Values{"name": {" "}}, htmx),
	}
	for name, w := range inPlace {
		if strconv.Itoa(w.Code) != name || w.Header().Get("HX-Retarget") != "" {
			t.Fatalf("in-place %s: status %d, HX-Retarget %q", name, w.Code, w.Header().Get("HX-Retarget"))
		}
		if swap, isErr := rule(w.Code); !swap || isErr {
			t.Errorf("in-place %d answer: swap %v, error %v", w.Code, swap, isErr)
		}
	}
	// Errors the app does not write itself.
	notApp := []int{
		f.post(f.entryPath("read"), nil, htmx, withHeader("Sec-Fetch-Site", "cross-site")).Code, // 403
		f.get("/", withHost("rebind.attacker.example")).Code,                                    // 421
		http.StatusUnauthorized, http.StatusMethodNotAllowed, http.StatusTooManyRequests,
		http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout,
	}
	for _, code := range notApp {
		if swap, isErr := rule(code); swap || !isErr {
			t.Errorf("status %d: swap %v, error %v; want no swap, error", code, swap, isErr)
		}
	}
	for code, want := range map[int]bool{http.StatusOK: true, http.StatusNoContent: false} {
		if swap, _ := rule(code); swap != want {
			t.Errorf("status %d: swap %v, want %v", code, swap, want)
		}
	}
	// static/app.js keeps unretargeted errors out of the page and reports
	// them in #notice instead.
	js := f.get("/static/app.js").Body.String()
	wantContains(t, page, `<script src="`+f.srv.static.url("app.js")+`" defer></script>`)
	wantContains(t, js, "htmx:beforeSwap", "HX-Retarget", "shouldSwap = false", "htmx:responseError", "htmx:sendError", `getElementById("notice")`)
}
