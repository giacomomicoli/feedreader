package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/store"
)

// actionFixture: a YouTube feed with 3 unread entries in a folder plus an
// uncategorized blog with 2 unread entries.
type actionFixture struct {
	*testEnv
	folder store.Folder
	yt     store.Feed
	blog   store.Feed
	video  store.EntryView
}

func newActionFixture(t *testing.T) actionFixture {
	e := newTestEnv(t)
	fo := e.addFolder("Videos")
	yt := e.addFeed(store.KindYouTube, "yt", "Channel", fo.ID, makeEntries("yt", 3))
	blog := e.addFeed(store.KindRSS, "blog", "Blog", 0, makeEntries("blog", 2))
	return actionFixture{testEnv: e, folder: fo, yt: yt, blog: blog, video: e.entries(yt.ID)[0]}
}

func (f actionFixture) entryPath(action string) string {
	return "/entries/" + idStr(f.video.ID) + "/" + action
}

// wantCardWithCounts asserts the response is the re-rendered card followed
// by out-of-band badges for All, every folder and every feed, and the tags.
func (f actionFixture) wantCardWithCounts(t *testing.T, body string, all, folder, yt, blog string) {
	t.Helper()
	if !strings.HasPrefix(strings.TrimSpace(body), `<article class="card`) || !strings.Contains(body, `id="entry-`+idStr(f.video.ID)+`"`) {
		t.Fatalf("response does not start with the card:\n%s", body)
	}
	got := oobBadges(body)
	want := map[string]string{
		"badge-all":                          all,
		"badge-folder-" + idStr(f.folder.ID): folder,
		"badge-feed-" + idStr(f.yt.ID):       yt,
		"badge-feed-" + idStr(f.blog.ID):     blog,
	}
	for id, v := range want {
		if got[id] != v {
			t.Errorf("OOB %s = %q, want %q (all: %v)", id, got[id], v, got)
		}
	}
	wantContains(t, body, `id="sidebar-tags" hx-swap-oob="true"`)
}

func TestMarkWatchedAndUnwatchedUpdateDBCardAndSidebarCounts(t *testing.T) {
	f := newActionFixture(t)
	w := f.post(f.entryPath("read"), nil, htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	f.wantCardWithCounts(t, body, "4", "2", "2", "2")
	wantContains(t, body, `class="card is-read"`, ">Watched<", ">Mark unwatched</button>", `action="`+f.entryPath("unread")+`"`)
	ev := f.entry(f.video.ID)
	if !ev.IsRead || ev.ReadAt.IsZero() {
		t.Fatalf("entry not marked read: %+v", ev.Entry)
	}

	w = f.post(f.entryPath("unread"), nil, htmx)
	wantStatus(t, w, http.StatusOK)
	body = w.Body.String()
	f.wantCardWithCounts(t, body, "5", "3", "3", "2")
	wantContains(t, body, ">Unwatched<", ">Mark watched</button>")
	if f.entry(f.video.ID).IsRead {
		t.Fatal("entry still read")
	}
}

func TestWatchLaterNeverChangesReadState(t *testing.T) {
	f := newActionFixture(t)
	if err := f.st.SetRead(context.Background(), f.video.ID, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	w := f.post(f.entryPath("later"), nil, htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	f.wantCardWithCounts(t, body, "4", "2", "2", "2")
	wantContains(t, body, ">Remove from Watch later</button>", `title="In Watch later"`)
	ev := f.entry(f.video.ID)
	if !ev.IsLater || !ev.IsRead {
		t.Fatalf("later=%v read=%v, want both true", ev.IsLater, ev.IsRead)
	}

	// Marking read again does not clear is_later; removal is explicit.
	f.post(f.entryPath("unread"), nil, htmx)
	f.post(f.entryPath("read"), nil, htmx)
	if !f.entry(f.video.ID).IsLater {
		t.Fatal("marking read cleared is_later")
	}
	wantStatus(t, f.post(f.entryPath("unlater"), nil, htmx), http.StatusOK)
	if f.entry(f.video.ID).IsLater {
		t.Fatal("is_later not removed")
	}
}

func TestFavouriteToggle(t *testing.T) {
	f := newActionFixture(t)
	w := f.post(f.entryPath("favourite"), nil, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), ">Remove from favourites</button>", `class="flag flag-fav"`)
	if !f.entry(f.video.ID).IsFavourite {
		t.Fatal("not favourite")
	}
	body := f.get("/?scope=favourites").Body.String()
	if got := cardIDs(body); len(got) != 1 || got[0] != idStr(f.video.ID) {
		t.Errorf("Favourites scope = %v", got)
	}
	w = f.post(f.entryPath("unfavourite"), nil, htmx)
	wantContains(t, w.Body.String(), ">Add to favourites</button>")
	if f.entry(f.video.ID).IsFavourite {
		t.Fatal("still favourite")
	}
}

func TestAddAndRemoveTagUpdatesCardAndSidebarTagList(t *testing.T) {
	f := newActionFixture(t)
	w := f.post(f.entryPath("tags"), url.Values{"tag": {"  Go   talks "}}, htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	f.wantCardWithCounts(t, body, "5", "3", "3", "2")
	ev := f.entry(f.video.ID)
	if len(ev.Tags) != 1 || ev.Tags[0].Name != "Go talks" {
		t.Fatalf("tags = %+v", ev.Tags)
	}
	tagID := ev.Tags[0].ID
	tagHref := "/?scope=tag&amp;id=" + idStr(tagID)
	wantContains(t, body,
		`<ul class="card-tags"><li><a href="`+tagHref+`">Go talks</a>`,
		`action="/entries/`+idStr(f.video.ID)+`/tags/`+idStr(tagID)+`/remove"`,
	)
	// The OOB tag list carries the new tag with its count.
	oob := body[strings.Index(body, `id="sidebar-tags" hx-swap-oob="true"`):]
	wantContains(t, oob, ">Go talks<", `<span class="count">1</span>`)

	// Adding the same tag in another case is idempotent.
	f.post(f.entryPath("tags"), url.Values{"tag": {"go TALKS"}}, htmx)
	if n := len(f.entry(f.video.ID).Tags); n != 1 {
		t.Fatalf("tags after duplicate add = %d", n)
	}

	w = f.post("/entries/"+idStr(f.video.ID)+"/tags/"+idStr(tagID)+"/remove", nil, htmx)
	wantStatus(t, w, http.StatusOK)
	wantNotContains(t, w.Body.String(), `class="card-tags"`)
	if n := len(f.entry(f.video.ID).Tags); n != 0 {
		t.Fatalf("tags after remove = %d", n)
	}
}

func TestEmptyTagIsRejectedIntoNotice(t *testing.T) {
	f := newActionFixture(t)
	w := f.post(f.entryPath("tags"), url.Values{"tag": {"   "}}, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	if got := w.Header().Get("HX-Retarget"); got != "#notice" {
		t.Errorf("HX-Retarget = %q, want #notice (the card must not be replaced by an error)", got)
	}
	wantContains(t, w.Body.String(), "Enter a tag name.")
	w = f.post(f.entryPath("tags"), url.Values{"tag": {strings.Repeat("x", maxNameLen+1)}}, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
}

func TestTagAutocompleteSuggestsExistingTags(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	for _, name := range []string{"golang", "Gophers", "rust"} {
		if _, err := f.st.AddEntryTag(ctx, f.video.ID, name); err != nil {
			t.Fatal(err)
		}
	}
	for _, target := range []string{"/tags/suggest?q=go", "/tags/suggest?tag=GO"} {
		w := f.get(target, htmx)
		wantStatus(t, w, http.StatusOK)
		body := w.Body.String()
		wantContains(t, body, `<option value="golang"></option>`, `<option value="Gophers"></option>`)
		wantNotContains(t, body, "rust")
	}
	if body := f.get("/tags/suggest?q=", htmx).Body.String(); strings.Contains(body, "<option") {
		t.Errorf("empty prefix suggested %q", body)
	}
	// The card's tag input is wired to the autocomplete.
	wantContains(t, f.get("/").Body.String(),
		`hx-get="/tags/suggest" hx-trigger="input changed delay:250ms" hx-target="#tag-options-`+idStr(f.video.ID)+`"`,
		`list="tag-options-`+idStr(f.video.ID)+`"`, `<datalist id="tag-options-`+idStr(f.video.ID)+`">`)
}

func TestEntryActionErrors(t *testing.T) {
	f := newActionFixture(t)
	cases := []struct {
		path string
		want int
	}{
		{"/entries/99999/read", http.StatusNotFound},
		{"/entries/abc/read", http.StatusBadRequest},
		{"/entries/0/read", http.StatusBadRequest},
		{f.entryPath("explode"), http.StatusNotFound},
		{"/entries/99999/tags", http.StatusNotFound},
		{"/entries/" + idStr(f.video.ID) + "/tags/99999/remove", http.StatusNotFound},
	}
	for _, c := range cases {
		form := url.Values{"tag": {"x"}}
		if w := f.post(c.path, form, htmx); w.Code != c.want {
			t.Errorf("POST %s = %d, want %d", c.path, w.Code, c.want)
		}
	}
	if w := f.get(f.entryPath("read")); w.Code == http.StatusOK {
		t.Errorf("GET on an action endpoint must not succeed, got %d", w.Code)
	}
}

func TestNonHTMXActionRedirectsBack(t *testing.T) {
	f := newActionFixture(t)
	back := "/?scope=feed&id=" + idStr(f.yt.ID) + "&filter=all"
	w := f.post(f.entryPath("read"), url.Values{"back": {back}})
	wantStatus(t, w, http.StatusSeeOther)
	if got := w.Header().Get("Location"); got != back {
		t.Errorf("Location = %q, want %q", got, back)
	}
	if !f.entry(f.video.ID).IsRead {
		t.Error("entry not marked read")
	}
	for _, evil := range []string{"//evil.example/x", "https://evil.example/", "/\\evil.example", "javascript:alert(1)"} {
		w := f.post(f.entryPath("unread"), url.Values{"back": {evil}})
		if got := w.Header().Get("Location"); got != "/" {
			t.Errorf("back=%q redirected to %q, want /", evil, got)
		}
	}
}

// upToRE extracts the "Mark all as read" watermark from a rendered page.
var upToRE = regexp.MustCompile(`name="up_to" value="(\d+)"`)

func TestMarkAllAsReadMarksScopeUpToPageWatermarkAndRerendersGrid(t *testing.T) {
	f := newActionFixture(t)
	ctx := context.Background()
	scope := url.Values{"scope": {"folder"}, "id": {idStr(f.folder.ID)}, "filter": {"unread"}}
	withScope := func(v url.Values) url.Values {
		for k, vv := range scope {
			v[k] = vv
		}
		return v
	}

	// A page rendered before the entries were stored sweeps nothing away.
	wantStatus(t, f.post("/mark-read", withScope(url.Values{"up_to": {"0"}}), htmx), http.StatusOK)
	if f.entry(f.video.ID).IsRead {
		t.Fatal("entries stored after the page was rendered were marked read")
	}

	// The page carries the largest entry id at render time. An entry stored
	// after that stays unread, even when its fetch time is older than the
	// page (a slow poll, or a clock that was set back).
	page := f.get("/?scope=folder&id=" + idStr(f.folder.ID) + "&filter=unread").Body.String()
	m := upToRE.FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no up_to watermark in the page:\n%s", page)
	}
	late := makeEntries("late", 1)
	if _, err := f.st.RecordFetchSuccess(ctx, store.FetchSuccess{
		FeedID: f.yt.ID, FetchedAt: time.Now().Add(-3 * time.Hour), NextFetchAt: time.Now().Add(time.Hour),
		Entries: late,
	}); err != nil {
		t.Fatal(err)
	}

	w := f.post("/mark-read", withScope(url.Values{"up_to": {m[1]}}), htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	if !strings.HasPrefix(strings.TrimSpace(body), `<div id="scope-view"`) {
		t.Fatalf("response is not the scope view:\n%s", body)
	}
	wantContains(t, body, `name="up_to"`, late[0].Title)
	got := oobBadges(body)
	if got["badge-all"] != "3" || got["badge-folder-"+idStr(f.folder.ID)] != "1" ||
		got["badge-feed-"+idStr(f.yt.ID)] != "1" || got["badge-feed-"+idStr(f.blog.ID)] != "2" {
		t.Errorf("OOB badges after mark all = %v", got)
	}
	for _, ev := range f.entries(f.yt.ID) {
		if want := ev.GUID != late[0].GUID; ev.IsRead != want {
			t.Errorf("entry %q read = %v, want %v", ev.Title, ev.IsRead, want)
		}
	}
	for _, ev := range f.entries(f.blog.ID) {
		if ev.IsRead {
			t.Errorf("entry %q outside the scope was marked read", ev.Title)
		}
	}

	// Validation.
	bad := url.Values{"scope": {"all"}}
	wantStatus(t, f.post("/mark-read", bad, htmx), http.StatusBadRequest)
	bad.Set("up_to", "yesterday")
	wantStatus(t, f.post("/mark-read", bad, htmx), http.StatusBadRequest)
	bad.Set("up_to", "-1")
	wantStatus(t, f.post("/mark-read", bad, htmx), http.StatusBadRequest)
	missing := url.Values{"scope": {"feed"}, "id": {"9999"}, "up_to": {"1"}}
	wantStatus(t, f.post("/mark-read", missing, htmx), http.StatusNotFound)

	// Without htmx: 303 back to the scope.
	plain := url.Values{"scope": {"all"}, "filter": {"all"}, "up_to": {m[1]}}
	w = f.post("/mark-read", plain)
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/?scope=all&filter=all" {
		t.Errorf("Location = %q", loc)
	}
}

func TestRefreshFeedOrAllShowsNoticeAndCounts(t *testing.T) {
	f := newActionFixture(t)
	w := f.post("/refresh", url.Values{"scope": {"feed"}, "id": {idStr(f.yt.ID)}}, htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body, "Refresh started — reload in a moment.")
	if _, ok := oobBadges(body)["badge-all"]; !ok {
		t.Error("refresh response lacks OOB counts")
	}
	if len(f.sched.refreshed) != 1 || f.sched.refreshed[0] != f.yt.ID || f.sched.refreshAll != 0 {
		t.Fatalf("refreshed = %v, all = %d", f.sched.refreshed, f.sched.refreshAll)
	}

	for _, sc := range []string{"all", "folder", "favourites"} {
		form := url.Values{"scope": {sc}}
		if sc == "folder" {
			form.Set("id", idStr(f.folder.ID))
		}
		wantStatus(t, f.post("/refresh", form, htmx), http.StatusOK)
	}
	if f.sched.refreshAll != 3 {
		t.Errorf("RefreshAll calls = %d, want 3", f.sched.refreshAll)
	}
	wantStatus(t, f.post("/refresh", url.Values{"scope": {"feed"}, "id": {"9999"}}, htmx), http.StatusNotFound)
	w = f.post("/refresh", url.Values{"scope": {"all"}, "back": {"/?scope=readlater"}})
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != "/?scope=readlater" {
		t.Errorf("Location = %q", loc)
	}
}

func TestOOBTagListKeepsActiveTagHighlighted(t *testing.T) {
	f := newActionFixture(t)
	tag, err := f.st.AddEntryTag(context.Background(), f.video.ID, "keep")
	if err != nil {
		t.Fatal(err)
	}
	cur := "http://feeds.lan/?scope=tag&id=" + idStr(tag.ID)
	body := f.post(f.entryPath("read"), nil, htmx, withHeader("HX-Current-URL", cur)).Body.String()
	wantContains(t, body, `class="nav-item tag-row is-active" href="/?scope=tag&amp;id=`+idStr(tag.ID)+`" aria-current="page"`)
}

// noticeClear is the out-of-band swap that empties #notice.
const noticeClear = `<div id="notice" hx-swap-oob="innerHTML"></div>`

// TestSuccessfulActionsClearStaleNotice: an error shown in #notice must not
// linger next to the result of a later, successful action.
func TestSuccessfulActionsClearStaleNotice(t *testing.T) {
	f := newActionFixture(t)
	for i := 0; i < 25; i++ { // enough for a "Load more" in the All scope
		f.addFeed(store.KindRSS, "more"+strconv.Itoa(i), "More", 0, makeEntries("m"+strconv.Itoa(i), 1))
	}
	failed := []*httptest.ResponseRecorder{
		f.post("/entries/99999/read", nil, htmx),
		f.post(f.entryPath("tags"), url.Values{"tag": {" "}}, htmx),
		f.post("/folders", url.Values{"name": {" "}}, htmx), // inline form error
	}
	for _, w := range failed {
		if w.Code < 400 || strings.Contains(w.Body.String(), noticeClear) {
			t.Errorf("failed request (%d) clears the notice", w.Code)
		}
	}
	more := loadMoreURL(f.get("/?filter=all").Body.String())
	if more == "" {
		t.Fatal("no Load more")
	}
	succeeded := map[string]*httptest.ResponseRecorder{
		"card action": f.post(f.entryPath("read"), nil, htmx),
		"add tag":     f.post(f.entryPath("tags"), url.Values{"tag": {"ok"}}, htmx),
		"load more":   f.get(more, htmx),
		"mark all":    f.post("/mark-read", url.Values{"scope": {"all"}, "filter": {"all"}, "up_to": {"0"}}, htmx),
		"confirm":     f.get("/folders/"+idStr(f.folder.ID)+"/delete", htmx),
		"add step":    f.post("/add/resolve", url.Values{"url": {"https://nothing.example"}}, htmx), // 422: no clear
	}
	for name, w := range succeeded {
		cleared := strings.Count(w.Body.String(), noticeClear) == 1
		if want := w.Code < 300; cleared != want {
			t.Errorf("%s (%d): notice cleared = %v, want %v", name, w.Code, cleared, want)
		}
	}
	// Refresh writes its own notice; tag autocomplete runs per keystroke.
	for name, w := range map[string]*httptest.ResponseRecorder{
		"refresh": f.post("/refresh", url.Values{"scope": {"all"}}, htmx),
		"suggest": f.get("/tags/suggest?q=o", htmx),
	} {
		wantStatus(t, w, http.StatusOK)
		if strings.Contains(w.Body.String(), `id="notice"`) {
			t.Errorf("%s response touches #notice:\n%s", name, w.Body.String())
		}
	}
}
