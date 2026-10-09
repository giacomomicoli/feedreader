package web

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// refreshFixture is the action fixture with the server clock fixed a minute
// ahead, so that a refresh is accepted after the feeds were created (stored
// times have one-second precision).
type refreshFixture struct {
	actionFixture
	now time.Time
}

func newRefreshFixture(t *testing.T) refreshFixture {
	f := newActionFixture(t)
	now := time.Now().Add(time.Minute).Truncate(time.Second)
	f.srv.now = func() time.Time { return now }
	return refreshFixture{actionFixture: f, now: now}
}

// gridRefresh is the form of the card grid's Refresh button, on a grid
// rendered with the watermark upTo.
func gridRefresh(scope string, id int64, filter string, upTo int64) url.Values {
	v := url.Values{"scope": {scope}, "filter": {filter}, "back": {"/"}, "reload": {"grid"},
		"up_to": {strconv.FormatInt(upTo, 10)}}
	if id != 0 {
		v.Set("id", idStr(id))
	}
	return v
}

// maxEntryID is the watermark of a grid rendered now.
func (f refreshFixture) maxEntryID() int64 {
	f.t.Helper()
	id, err := f.st.MaxEntryID(context.Background())
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

// refreshCheck is a parsed refresh check element (#refresh-poll).
type refreshCheck struct {
	url   string // hx-get, unescaped
	oob   bool
	delay time.Duration
}

var refreshCheckRE = regexp.MustCompile(`<div id="refresh-poll" hidden( hx-swap-oob="true")? hx-get="([^"]+)" hx-trigger="load delay:(\d+)ms" hx-target="this" hx-swap="outerHTML"></div>`)

// refreshIdle is the refresh check placeholder with no check pending.
const refreshIdle = `<div id="refresh-poll" hidden></div>`

// findRefreshCheck returns the refresh check in body; ok is false when
// there is none.
func findRefreshCheck(t *testing.T, body string) (c refreshCheck, ok bool) {
	t.Helper()
	m := refreshCheckRE.FindAllStringSubmatch(body, -1)
	switch len(m) {
	case 0:
		return c, false
	case 1:
	default:
		t.Fatalf("%d refresh checks in one response:\n%s", len(m), body)
	}
	ms, err := strconv.Atoi(m[0][3])
	if err != nil {
		t.Fatal(err)
	}
	return refreshCheck{url: strings.ReplaceAll(m[0][2], "&amp;", "&"), oob: m[0][1] != "", delay: time.Duration(ms) * time.Millisecond}, true
}

// mustRefreshCheck is findRefreshCheck for a response that must carry one.
func mustRefreshCheck(t *testing.T, body string) refreshCheck {
	t.Helper()
	c, ok := findRefreshCheck(t, body)
	if !ok {
		t.Fatalf("no refresh check in:\n%s", body)
	}
	return c
}

// wantCheckURL asserts that c asks GET /refresh/status for scope, filter,
// since, the grid's watermark upTo and attempt.
func wantCheckURL(t *testing.T, c refreshCheck, scope url.Values, since time.Time, upTo int64, attempt int) {
	t.Helper()
	u, err := url.Parse(c.url)
	if err != nil {
		t.Fatalf("check URL %q: %v", c.url, err)
	}
	want := url.Values{"since": {strconv.FormatInt(since.Unix(), 10)}, "up_to": {strconv.FormatInt(upTo, 10)},
		"attempt": {strconv.Itoa(attempt)}}
	for k, v := range scope {
		want[k] = v
	}
	if u.Path != "/refresh/status" || !reflect.DeepEqual(u.Query(), want) {
		t.Errorf("check URL = %q, want /refresh/status?%s", c.url, want.Encode())
	}
	if c.delay != config.RefreshCheckInterval {
		t.Errorf("check delay = %s, want %s", c.delay, config.RefreshCheckInterval)
	}
}

// withAttempt returns the check URL u with its attempt counter set to n.
func withAttempt(t *testing.T, u string, n int) string {
	t.Helper()
	p, err := url.Parse(u)
	if err != nil {
		t.Fatal(err)
	}
	q := p.Query()
	q.Set("attempt", strconv.Itoa(n))
	p.RawQuery = q.Encode()
	return p.String()
}

// sameURL reports whether a and b have the same path and query values, in
// any order.
func sameURL(t *testing.T, a, b string) bool {
	t.Helper()
	ua, err := url.Parse(a)
	if err != nil {
		t.Fatal(err)
	}
	ub, err := url.Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	return ua.Path == ub.Path && reflect.DeepEqual(ua.Query(), ub.Query())
}

// fetched records a successful poll of feed id at fetchedAt with the next one
// at next, as the scheduler does, storing entries.
func (f refreshFixture) fetched(id int64, fetchedAt, next time.Time, entries ...store.NewEntry) {
	f.t.Helper()
	if _, err := f.st.RecordFetchSuccess(context.Background(), store.FetchSuccess{
		FeedID: id, FetchedAt: fetchedAt, NextFetchAt: next, Entries: entries,
	}); err != nil {
		f.t.Fatal(err)
	}
}

// freshEntry is an entry published at the time of the refresh.
func (f refreshFixture) freshEntry(title string) store.NewEntry {
	return store.NewEntry{GUID: "fresh-" + title, URL: "https://example.com/fresh/" + url.PathEscape(title),
		Title: title, PublishedAt: f.now}
}

const (
	refreshDoneText       = "Refresh done."
	refreshNothingNewText = "Refresh done — nothing new here."
	refreshStillText      = "Some sources have not been fetched yet."
	refreshStartedText    = "Refresh started — new entries show up when it is done."
)

func TestRefreshFromGridStartsReloadChecks(t *testing.T) {
	f := newRefreshFixture(t)

	// The grid's Refresh form asks for the reload and carries the grid's
	// watermark; its button has an id, so htmx gives it its focus back when
	// the grid is reloaded. The placeholder of the checks sits outside
	// #scope-view, which "Mark all as read" replaces.
	upTo := f.maxEntryID()
	page := f.get("/?scope=feed&id=" + idStr(f.yt.ID) + "&filter=all").Body.String()
	form := refreshFormRE.FindString(page)
	wantContains(t, form, `<input type="hidden" name="reload" value="grid">`,
		`<input type="hidden" name="up_to" value="`+strconv.FormatInt(upTo, 10)+`">`, `id="refresh-button"`)
	wantContains(t, page, refreshIdle)
	view := f.post("/mark-read", url.Values{"scope": {"all"}, "up_to": {"0"}}, htmx).Body.String()
	wantNotContains(t, view, `id="refresh-poll"`)

	w := f.post("/refresh", gridRefresh("feed", f.yt.ID, "all", upTo), htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), `<p class="notice">`+refreshStartedText+`</p>`) {
		t.Errorf("refresh answer does not start with the notice:\n%s", body)
	}
	c := mustRefreshCheck(t, body)
	if !c.oob {
		t.Error("the first check is not swapped out of band into its placeholder")
	}
	wantCheckURL(t, c, url.Values{"scope": {"feed"}, "id": {idStr(f.yt.ID)}, "filter": {"all"}}, f.now, upTo, 0)
	if _, ok := oobBadges(body)["badge-all"]; !ok {
		t.Error("refresh answer lacks OOB counts")
	}
	// The answer is swapped into #notice, so it must not touch it again.
	wantNotContains(t, body, `id="notice"`)
	checkCSPMarkup(t, "refresh answer", body)
	if len(f.sched.refreshed) != 1 || f.sched.refreshed[0] != f.yt.ID || f.sched.refreshAll != 0 {
		t.Fatalf("refreshed = %v, all = %d", f.sched.refreshed, f.sched.refreshAll)
	}

	// Every other scope refreshes all feeds; the filter defaults to unread.
	w = f.post("/refresh", url.Values{"scope": {"folder"}, "id": {idStr(f.folder.ID)}, "reload": {"grid"}, "up_to": {"0"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantCheckURL(t, mustRefreshCheck(t, w.Body.String()),
		url.Values{"scope": {"folder"}, "id": {idStr(f.folder.ID)}, "filter": {"unread"}}, f.now, 0, 0)
	if f.sched.refreshAll != 1 {
		t.Errorf("RefreshAll calls = %d, want 1", f.sched.refreshAll)
	}

	// The feed settings page has no grid to reload.
	w = f.post("/refresh", url.Values{"scope": {"feed"}, "id": {idStr(f.yt.ID)}, "back": {"/feeds/" + idStr(f.yt.ID)}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), "Refresh started — reload in a moment.")
	if _, ok := findRefreshCheck(t, w.Body.String()); ok {
		t.Errorf("refresh without reload starts checks:\n%s", w.Body.String())
	}
	if strings.Contains(f.get("/feeds/"+idStr(f.yt.ID)).Body.String(), `name="reload"`) {
		t.Error("the feed settings Refresh asks for a grid reload")
	}

	// Without htmx the form posts redirect back, as before.
	w = f.post("/refresh", gridRefresh("all", 0, "unread", upTo))
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}

	// An unknown reload value, or a grid refresh without a valid
	// watermark, is rejected before anything is refreshed.
	calls := len(f.sched.refreshed) + f.sched.refreshAll
	for name, change := range map[string]url.Values{
		"unknown reload":      {"reload": {"page"}},
		"no up_to":            {"up_to": {""}},
		"up_to not a number":  {"up_to": {"latest"}},
		"negative up_to":      {"up_to": {"-1"}},
		"up_to with fraction": {"up_to": {"1.5"}},
		"up_to overflowing":   {"up_to": {"99999999999999999999"}},
	} {
		bad := gridRefresh("all", 0, "unread", upTo)
		for k, v := range change {
			if v[0] == "" {
				bad.Del(k)
			} else {
				bad[k] = v
			}
		}
		if w := f.post("/refresh", bad, htmx); w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want %d", name, w.Code, http.StatusBadRequest)
		}
	}
	if n := len(f.sched.refreshed) + f.sched.refreshAll; n != calls {
		t.Errorf("a rejected refresh reached the scheduler")
	}
}

// refreshFormRE extracts the card grid's Refresh form from a page.
var refreshFormRE = regexp.MustCompile(`(?s)<form method="post" action="/refresh".*?</form>`)

// startRefresh posts the Refresh of a grid rendered just before and returns
// its first check.
func (f refreshFixture) startRefresh(scope string, id int64, filter string) refreshCheck {
	f.t.Helper()
	w := f.post("/refresh", gridRefresh(scope, id, filter, f.maxEntryID()), htmx)
	wantStatus(f.t, w, http.StatusOK)
	return mustRefreshCheck(f.t, w.Body.String())
}

// check performs one refresh check, as htmx does from the page at "/".
func (f refreshFixture) check(u string) *httptest.ResponseRecorder {
	f.t.Helper()
	return f.get(u, htmx, withHeader("HX-Current-URL", "http://"+testHost+"/"))
}

// wantNextCheck asserts that w is only the next check, attempt n.
func wantNextCheck(t *testing.T, w *httptest.ResponseRecorder, prev string, n int) refreshCheck {
	t.Helper()
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	c := mustRefreshCheck(t, body)
	if c.oob {
		t.Error("the next check replaces the current one in place, not out of band")
	}
	if want := withAttempt(t, prev, n); !sameURL(t, c.url, want) {
		t.Errorf("next check URL = %q, want %q", c.url, want)
	}
	if rest := strings.TrimSpace(refreshCheckRE.ReplaceAllString(body, "")); rest != "" {
		t.Errorf("a pending check answers with more than the next check: %q", rest)
	}
	checkCSPMarkup(t, "next check", body)
	return c
}

// wantReloaded asserts that w ends the checks: the idle placeholder, the
// final notice, the reloaded scope view and the sidebar counts, all out of
// band but the placeholder.
func wantReloaded(t *testing.T, w *httptest.ResponseRecorder, notice string) string {
	t.Helper()
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), refreshIdle) {
		t.Errorf("the last check does not replace itself with the idle placeholder:\n%s", body)
	}
	if _, ok := findRefreshCheck(t, body); ok {
		t.Errorf("the last check schedules another one:\n%s", body)
	}
	wantContains(t, body,
		`<div id="notice" hx-swap-oob="innerHTML"><p class="notice">`+notice,
		`<div id="scope-view" class="scope-view" hx-swap-oob="true">`,
		`id="sidebar-tags" hx-swap-oob="true"`,
	)
	if n := strings.Count(body, `id="notice"`); n != 1 {
		t.Errorf("the last check writes #notice %d times, want once (no notice-clear after it)", n)
	}
	if _, ok := oobBadges(body)["badge-all"]; !ok {
		t.Error("the last check lacks OOB counts")
	}
	checkCSPMarkup(t, "last check", body)
	return body
}

func TestRefreshCheckWaitsForEveryRefreshedFeedThenReloadsGrid(t *testing.T) {
	f := newRefreshFixture(t)
	c := f.startRefresh("all", 0, "unread")
	later := f.now.Add(config.DefaultPollInterval)

	// Nothing fetched since the click: check again.
	c = wantNextCheck(t, f.check(c.url), c.url, 1)

	// One feed is done, the other one is not.
	f.fetched(f.yt.ID, f.now.Add(time.Second), later, f.freshEntry("Fresh video"))
	c = wantNextCheck(t, f.check(c.url), c.url, 2)

	// The blog was being fetched at the click: that fetch ends after it but
	// keeps the refresh's schedule, so the blog is fetched once more.
	if err := f.st.RecordNotModified(context.Background(), f.blog.ID, f.now.Add(time.Second), f.now); err != nil {
		t.Fatal(err)
	}
	c = wantNextCheck(t, f.check(c.url), c.url, 3)

	// A failed fetch is a fetch too.
	if err := f.st.RecordFetchError(context.Background(), f.blog.ID, "HTTP 404", f.now.Add(2*time.Second), later); err != nil {
		t.Fatal(err)
	}
	body := wantReloaded(t, f.check(c.url), refreshDoneText)
	// The scope view is the first page of the scope with its filter, like a
	// fresh load: the new entry first, a new watermark for Mark all as read.
	view := body[strings.Index(body, `<div id="scope-view"`):]
	ids := cardIDs(view)
	if len(ids) == 0 || ids[0] != idStr(f.entries(f.yt.ID)[0].ID) {
		t.Errorf("reloaded grid = %v, want the new entry first", ids)
	}
	wantContains(t, view, "Fresh video", `name="filter" value="unread"`)
	m := upToRE.FindStringSubmatch(view)
	maxID, err := f.st.MaxEntryID(context.Background())
	if err != nil || m == nil || m[1] != strconv.FormatInt(maxID, 10) {
		t.Errorf("reloaded watermark = %v, want %d (%v)", m, maxID, err)
	}
	if got := oobBadges(body)["badge-feed-"+idStr(f.yt.ID)]; got != "4" {
		t.Errorf("OOB badge of the refreshed feed = %q, want 4", got)
	}
}

func TestRefreshCheckOfOneFeedWaitsForThatFeedOnly(t *testing.T) {
	f := newRefreshFixture(t)
	c := f.startRefresh("feed", f.yt.ID, "all")
	c = wantNextCheck(t, f.check(c.url), c.url, 1)
	f.fetched(f.yt.ID, f.now, f.now.Add(config.DefaultYouTubePollInterval), f.freshEntry("Fresh video"))
	body := wantReloaded(t, f.check(c.url), refreshDoneText)
	wantContains(t, body, `<h1 class="page-title">Channel</h1>`, `name="filter" value="all"`, "Fresh video",
		`id="refresh-button"`)
	if got, want := len(cardIDs(body)), len(f.entries(f.yt.ID)); got != want {
		t.Errorf("reloaded feed grid has %d cards, want %d (filter all)", got, want)
	}
}

func TestRefreshCheckGivesUpAfterMaxChecks(t *testing.T) {
	f := newRefreshFixture(t)
	c := f.startRefresh("all", 0, "unread")
	last := config.RefreshMaxChecks - 1
	next := wantNextCheck(t, f.check(withAttempt(t, c.url, last-1)), c.url, last)
	// Nothing fetched, nothing new: the grid stays.
	body := wantQuiet(t, f.check(next.url), refreshStillText)
	wantNotContains(t, body, refreshDoneText)

	// One feed brought an entry, the other one is still due: the grid is
	// reloaded with what has arrived.
	f.fetched(f.yt.ID, f.now, f.now.Add(config.DefaultPollInterval), f.freshEntry("Fresh video"))
	body = wantReloaded(t, f.check(next.url), refreshStillText)
	wantNotContains(t, body, refreshDoneText)
	wantContains(t, body, "Fresh video")
}

func TestRefreshCheckFollowsUnsubscribes(t *testing.T) {
	f := newRefreshFixture(t)
	c := f.startRefresh("all", 0, "unread")
	f.fetched(f.yt.ID, f.now, f.now.Add(config.DefaultPollInterval))
	c = wantNextCheck(t, f.check(c.url), c.url, 1)
	// A feed unsubscribed meanwhile is not waited for.
	if err := f.st.DeleteFeed(context.Background(), f.blog.ID); err != nil {
		t.Fatal(err)
	}
	wantQuiet(t, f.check(c.url), refreshNothingNewText)

	// The checks of a feed that is gone end with the usual error.
	c = f.startRefresh("feed", f.yt.ID, "unread")
	if err := f.st.DeleteFeed(context.Background(), f.yt.ID); err != nil {
		t.Fatal(err)
	}
	w := f.check(c.url)
	wantStatus(t, w, http.StatusNotFound)
	if w.Header().Get("HX-Retarget") != "#notice" {
		t.Errorf("HX-Retarget = %q, want #notice", w.Header().Get("HX-Retarget"))
	}
	if _, ok := findRefreshCheck(t, w.Body.String()); ok {
		t.Error("an error answer schedules another check")
	}
}

// wantQuiet asserts that w ends the checks without reloading the grid: the
// idle placeholder, the final notice and the sidebar counts, but no scope
// view, so the pages, menus, typed text and focus on screen stay as they are.
func wantQuiet(t *testing.T, w *httptest.ResponseRecorder, notice string) string {
	t.Helper()
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), refreshIdle) {
		t.Errorf("the last check does not replace itself with the idle placeholder:\n%s", body)
	}
	if _, ok := findRefreshCheck(t, body); ok {
		t.Errorf("the last check schedules another one:\n%s", body)
	}
	wantContains(t, body,
		`<div id="notice" hx-swap-oob="innerHTML"><p class="notice">`+notice,
		`id="sidebar-tags" hx-swap-oob="true"`,
	)
	wantNotContains(t, body, `id="scope-view"`, `<article class="card`)
	if n := strings.Count(body, `id="notice"`); n != 1 {
		t.Errorf("the last check writes #notice %d times, want once (no notice-clear after it)", n)
	}
	if _, ok := oobBadges(body)["badge-all"]; !ok {
		t.Error("the last check lacks OOB counts")
	}
	checkCSPMarkup(t, "last check", body)
	return body
}

func TestRefreshCheckWithNothingNewLeavesTheGridAlone(t *testing.T) {
	f := newRefreshFixture(t)
	later := f.now.Add(config.DefaultPollInterval)
	if err := f.st.SetFavourite(context.Background(), f.video.ID, true); err != nil {
		t.Fatal(err)
	}

	// Every feed fetched, nothing new: the grid on screen is still current.
	c := f.startRefresh("all", 0, "unread")
	f.fetched(f.yt.ID, f.now, later)
	f.fetched(f.blog.ID, f.now, later)
	wantQuiet(t, f.check(c.url), refreshNothingNewText)

	// A fetch can bring entries, but never a favourite: Favourites is not
	// reloaded, while the counts show what arrived.
	c = f.startRefresh("favourites", 0, "unread")
	f.fetched(f.yt.ID, f.now.Add(time.Second), later, f.freshEntry("Fresh video"))
	f.fetched(f.blog.ID, f.now.Add(time.Second), later)
	body := wantQuiet(t, f.check(c.url), refreshNothingNewText)
	if got := oobBadges(body)["badge-feed-"+idStr(f.yt.ID)]; got != "4" {
		t.Errorf("OOB badge of the refreshed feed = %q, want 4", got)
	}
}

func TestRefreshCheckReloadsEntriesStoredBeforeTheClick(t *testing.T) {
	f := newRefreshFixture(t)
	later := f.now.Add(config.DefaultPollInterval)
	// The grid was rendered, then a scheduled poll stored an entry; the
	// refresh itself brings nothing. The grid does not have that entry yet.
	upTo := f.maxEntryID()
	f.fetched(f.yt.ID, f.now.Add(-time.Second), f.now, f.freshEntry("Polled video"))
	w := f.post("/refresh", gridRefresh("all", 0, "unread", upTo), htmx)
	wantStatus(t, w, http.StatusOK)
	c := mustRefreshCheck(t, w.Body.String())
	f.fetched(f.yt.ID, f.now, later)
	f.fetched(f.blog.ID, f.now, later)
	body := wantReloaded(t, f.check(c.url), refreshDoneText)
	wantContains(t, body, "Polled video")
}

func TestRefreshedSince(t *testing.T) {
	since := time.Now().Truncate(time.Second)
	before, after := since.Add(-time.Second), since.Add(time.Second)
	next := since.Add(config.DefaultPollInterval)
	cases := []struct {
		name string
		f    store.Feed
		want bool
	}{
		{"not fetched since the click", store.Feed{CreatedAt: before, LastFetchedAt: before, NextFetchAt: since}, false},
		{"never fetched", store.Feed{CreatedAt: before, NextFetchAt: since}, false},
		{"fetched after the click", store.Feed{CreatedAt: before, LastFetchedAt: after, NextFetchAt: next}, true},
		{"fetched within the second of the click", store.Feed{CreatedAt: before, LastFetchedAt: since, NextFetchAt: next}, true},
		{"fetch failed after the click", store.Feed{CreatedAt: before, LastFetchedAt: after, NextFetchAt: next,
			ErrorCount: 1, LastError: "HTTP 500"}, true},
		{"in flight at the click, refresh still due", store.Feed{CreatedAt: before, LastFetchedAt: after, NextFetchAt: since}, false},
		{"in flight at the click, refresh overdue", store.Feed{CreatedAt: before, LastFetchedAt: after, NextFetchAt: before}, false},
		{"subscribed after the click", store.Feed{CreatedAt: after, LastFetchedAt: before, NextFetchAt: next}, true},
		{"subscribed within the second of the click", store.Feed{CreatedAt: since, LastFetchedAt: before, NextFetchAt: next}, true},
	}
	for _, c := range cases {
		if got := refreshedSince(c.f, since); got != c.want {
			t.Errorf("%s: refreshedSince = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRefreshCheckRejectsInvalidParameters(t *testing.T) {
	f := newRefreshFixture(t)
	since := strconv.FormatInt(f.now.Unix(), 10)
	tooOld := strconv.FormatInt(f.now.Add(-refreshSinceWindow-time.Second).Unix(), 10)
	tooNew := strconv.FormatInt(f.now.Add(refreshSinceWindow+time.Second).Unix(), 10)
	valid := url.Values{"scope": {"all"}, "filter": {"unread"}, "since": {since}, "up_to": {"0"}, "attempt": {"0"}}
	cases := []struct {
		name   string
		change url.Values // replaces valid's values; "" deletes one
		status int
	}{
		{"no since", url.Values{"since": {""}}, http.StatusBadRequest},
		{"since not a number", url.Values{"since": {"yesterday"}}, http.StatusBadRequest},
		{"since with a fraction", url.Values{"since": {since + ".5"}}, http.StatusBadRequest},
		{"since too old", url.Values{"since": {tooOld}}, http.StatusBadRequest},
		{"since in the future", url.Values{"since": {tooNew}}, http.StatusBadRequest},
		{"since overflowing", url.Values{"since": {"99999999999999999999"}}, http.StatusBadRequest},
		{"largest since", url.Values{"since": {strconv.FormatInt(math.MaxInt64, 10)}}, http.StatusBadRequest},
		{"smallest since", url.Values{"since": {strconv.FormatInt(math.MinInt64, 10)}}, http.StatusBadRequest},
		{"no up_to", url.Values{"up_to": {""}}, http.StatusBadRequest},
		{"up_to not a number", url.Values{"up_to": {"latest"}}, http.StatusBadRequest},
		{"up_to with a fraction", url.Values{"up_to": {"1.5"}}, http.StatusBadRequest},
		{"negative up_to", url.Values{"up_to": {"-1"}}, http.StatusBadRequest},
		{"up_to overflowing", url.Values{"up_to": {"99999999999999999999"}}, http.StatusBadRequest},
		{"no attempt", url.Values{"attempt": {""}}, http.StatusBadRequest},
		{"attempt not a number", url.Values{"attempt": {"one"}}, http.StatusBadRequest},
		{"negative attempt", url.Values{"attempt": {"-1"}}, http.StatusBadRequest},
		{"attempt past the limit", url.Values{"attempt": {strconv.Itoa(config.RefreshMaxChecks)}}, http.StatusBadRequest},
		{"huge attempt", url.Values{"attempt": {"99999999999999999999"}}, http.StatusBadRequest},
		{"unknown scope", url.Values{"scope": {"everything"}}, http.StatusBadRequest},
		{"unknown filter", url.Values{"filter": {"new"}}, http.StatusBadRequest},
		{"feed scope without id", url.Values{"scope": {"feed"}}, http.StatusBadRequest},
		{"missing feed", url.Values{"scope": {"feed"}, "id": {"99999"}}, http.StatusNotFound},
		{"missing folder", url.Values{"scope": {"folder"}, "id": {"99999"}}, http.StatusNotFound},
	}
	for _, c := range cases {
		q := url.Values{}
		for k, v := range valid {
			q[k] = v
		}
		for k, v := range c.change {
			if v[0] == "" {
				q.Del(k)
			} else {
				q[k] = v
			}
		}
		w := f.check("/refresh/status?" + q.Encode())
		if w.Code != c.status {
			t.Errorf("%s: status %d, want %d", c.name, w.Code, c.status)
		}
		if w.Header().Get("HX-Retarget") != "#notice" {
			t.Errorf("%s: HX-Retarget = %q, want #notice", c.name, w.Header().Get("HX-Retarget"))
		}
		if _, ok := findRefreshCheck(t, w.Body.String()); ok {
			t.Errorf("%s: an invalid check schedules another one", c.name)
		}
	}
	// The limit itself, a since at the edge of the window and any
	// watermark are valid.
	for _, change := range []url.Values{
		{"attempt": {strconv.Itoa(config.RefreshMaxChecks - 1)}},
		{"since": {strconv.FormatInt(f.now.Add(-refreshSinceWindow).Unix(), 10)}},
		{"up_to": {strconv.FormatInt(math.MaxInt64, 10)}},
	} {
		q := url.Values{}
		for k, v := range valid {
			q[k] = v
		}
		for k, v := range change {
			q[k] = v
		}
		wantStatus(t, f.check("/refresh/status?"+q.Encode()), http.StatusOK)
	}

	// Without htmx (opened by hand): the page of the scope.
	w := f.get("/refresh/status?scope=feed&id=" + idStr(f.yt.ID) + "&filter=all&since=" + since + "&up_to=0&attempt=0")
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/?scope=feed&id="+idStr(f.yt.ID)+"&filter=all" {
		t.Errorf("Location = %q", loc)
	}
	wantStatus(t, f.get("/refresh/status?scope=nope"), http.StatusBadRequest)
}

// snapshot is everything a refresh check could change.
type snapshot struct {
	feeds   []store.Feed
	entries []store.EntryView
	counts  store.UnreadCounts
	folders []store.Folder
	tags    []store.Tag
}

func (f refreshFixture) snapshot() snapshot {
	f.t.Helper()
	ctx := context.Background()
	var s snapshot
	var err error
	if s.feeds, err = f.st.ListFeeds(ctx); err != nil {
		f.t.Fatal(err)
	}
	page, err := f.st.ListEntries(ctx, store.ListQuery{Scope: store.Scope{Kind: store.ScopeAll}, Limit: 1000})
	if err != nil {
		f.t.Fatal(err)
	}
	s.entries = page.Entries
	if s.counts, err = f.st.UnreadCounts(ctx); err != nil {
		f.t.Fatal(err)
	}
	if s.folders, err = f.st.ListFolders(ctx); err != nil {
		f.t.Fatal(err)
	}
	if s.tags, err = f.st.ListTags(ctx); err != nil {
		f.t.Fatal(err)
	}
	return s
}

func TestRefreshCheckNeverChangesState(t *testing.T) {
	f := newRefreshFixture(t)
	c := f.startRefresh("all", 0, "unread")
	if _, err := f.st.AddEntryTag(context.Background(), f.video.ID, "kept"); err != nil {
		t.Fatal(err)
	}
	refreshes := len(f.sched.refreshed) + f.sched.refreshAll
	before := f.snapshot()
	for name, w := range map[string]*httptest.ResponseRecorder{
		"pending":   f.check(c.url),
		"limit":     f.check(withAttempt(t, c.url, config.RefreshMaxChecks-1)),
		"invalid":   f.check(withAttempt(t, c.url, -1)),
		"no htmx":   f.get(c.url),
		"post":      f.post(c.url, nil, htmx),
		"post form": f.post("/refresh/status", url.Values{"scope": {"all"}, "since": {"0"}, "attempt": {"0"}}, htmx),
	} {
		if strings.HasPrefix(name, "post") && w.Code == http.StatusOK {
			t.Errorf("%s: a POST to the check succeeded", name)
		}
	}
	if after := f.snapshot(); !reflect.DeepEqual(before, after) {
		t.Errorf("refresh checks changed state:\nbefore %+v\nafter  %+v", before, after)
	}
	if n := len(f.sched.refreshed) + f.sched.refreshAll; n != refreshes {
		t.Errorf("refresh checks asked the scheduler for %d more refreshes", n-refreshes)
	}
}
