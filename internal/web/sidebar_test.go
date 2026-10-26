package web

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/store"
)

// mixedSidebar is a library with a folder of both kinds ("Mixed": a channel
// with 5 unread videos and a blog with 3 unread posts), a blog outside any
// folder (2 unread) and an empty folder.
type mixedSidebar struct {
	*testEnv
	mixed, quiet  store.Folder
	channel, blog store.Feed
	solo          store.Feed
}

func newMixedSidebar(t *testing.T) mixedSidebar {
	e := newTestEnv(t)
	m := mixedSidebar{testEnv: e, mixed: e.addFolder("Mixed"), quiet: e.addFolder("Quiet")}
	m.channel = e.addFeed(store.KindYouTube, "chan", "Some Channel", m.mixed.ID, makeEntries("vid", 6))
	m.blog = e.addFeed(store.KindRSS, "blog", "Folder Blog", m.mixed.ID, makeEntries("post", 3))
	m.solo = e.addFeed(store.KindRSS, "solo", "Solo Blog", 0, makeEntries("solo", 2))
	return m
}

// folderLink is the sidebar link to folder id narrowed to kind, as rendered.
func folderLink(id int64, kind store.Kind) string {
	return `href="/?scope=folder&amp;id=` + idStr(id) + `&amp;kind=` + string(kind) + `"`
}

// TestSidebarSplitsVideosAndBlogs: the subscriptions are split into a
// Videos and a Blogs section; a folder of both kinds is in each, with only
// its feeds of that kind, its own badge and a link narrowed to that kind;
// a folder without feeds is listed apart.
func TestSidebarSplitsVideosAndBlogs(t *testing.T) {
	m := newMixedSidebar(t)
	body := m.get("/").Body.String()
	videos := strings.Index(body, `<h2 class="nav-heading">Videos</h2>`)
	blogs := strings.Index(body, `<h2 class="nav-heading">Blogs</h2>`)
	empty := strings.Index(body, `<h2 class="nav-heading">Empty folders</h2>`)
	forms := strings.Index(body, `class="sidebar-forms"`)
	if !(0 <= videos && videos < blogs && blogs < empty && empty < forms) {
		t.Fatalf("sections out of order: Videos@%d Blogs@%d Empty folders@%d forms@%d", videos, blogs, empty, forms)
	}
	wantNotContains(t, body, `>Subscriptions</h2>`)
	in := func(from, to int, what ...string) {
		t.Helper()
		for _, w := range what {
			if i := strings.Index(body[from:to], w); i < 0 {
				t.Errorf("%q missing from the section", w)
			}
		}
	}
	out := func(from, to int, what ...string) {
		t.Helper()
		for _, w := range what {
			if strings.Contains(body[from:to], w) {
				t.Errorf("%q is in the wrong section", w)
			}
		}
	}
	mixed := idStr(m.mixed.ID)
	in(videos, blogs, folderLink(m.mixed.ID, store.KindYouTube), ">Some Channel<",
		`<span class="badge" id="badge-folder-`+mixed+`-youtube">5</span>`)
	out(videos, blogs, ">Folder Blog<", ">Solo Blog<", ">Quiet<")
	in(blogs, empty, folderLink(m.mixed.ID, store.KindRSS), ">Folder Blog<", ">Solo Blog<",
		`<span class="badge" id="badge-folder-`+mixed+`-rss">3</span>`)
	out(blogs, empty, ">Some Channel<", ">Quiet<")
	in(empty, forms, `href="/?scope=folder&amp;id=`+idStr(m.quiet.ID)+`"`, ">Quiet<",
		`<span class="badge" id="badge-folder-`+idStr(m.quiet.ID)+`"></span>`, "Empty folder")
	// Each copy of the mixed folder is open: both have unread entries.
	if n := strings.Count(body, `<details class="folder" open>`); n != 2 {
		t.Errorf("open folders = %d, want the 2 copies of Mixed", n)
	}

	// The badges sent after an action are the same ones.
	w := m.post("/entries/"+idStr(m.entries(m.blog.ID)[0].ID)+"/read", url.Values{}, htmx)
	wantStatus(t, w, http.StatusOK)
	got := oobBadges(w.Body.String())
	for id, n := range map[string]string{
		"badge-folder-" + mixed + "-youtube":         "5",
		"badge-folder-" + mixed + "-rss":             "2",
		"badge-folder-" + idStr(m.quiet.ID):          "",
		"badge-feed-" + idStr(m.solo.ID):             "2",
		"badge-folder-" + mixed:                      "absent",
		"badge-folder-" + idStr(m.quiet.ID) + "-rss": "absent",
	} {
		if v, ok := got[id]; n == "absent" && ok || n != "absent" && v != n {
			t.Errorf("OOB %s = %q (present %v), want %q", id, v, ok, n)
		}
	}
}

// TestSidebarShowsOnlyKindsWithFeeds: a library of videos alone has no Blogs
// section.
func TestSidebarShowsOnlyKindsWithFeeds(t *testing.T) {
	e := newTestEnv(t)
	e.addFeed(store.KindYouTube, "chan", "Some Channel", 0, makeEntries("vid", 2))
	body := e.get("/").Body.String()
	wantContains(t, body, `<h2 class="nav-heading">Videos</h2>`, ">Some Channel<")
	wantNotContains(t, body, `<h2 class="nav-heading">Blogs</h2>`, `<h2 class="nav-heading">Empty folders</h2>`)
}

// TestFolderNarrowedToOneKind: a folder's page narrowed to a kind lists its
// entries of that kind, says so, counts them, keeps the kind in its forms,
// and marks only them read; the unnarrowed folder page still lists all.
func TestFolderNarrowedToOneKind(t *testing.T) {
	m := newMixedSidebar(t)
	mixed := idStr(m.mixed.ID)
	page := m.get("/?scope=folder&id=" + mixed + "&kind=rss").Body.String()
	if n := len(cardIDs(page)); n != 3 {
		t.Errorf("blogs of the folder: %d cards, want 3", n)
	}
	wantContains(t, page, `<p class="topbar-sub">Folder, blogs only</p>`, `id="scope-count">3 unread</span>`,
		`<input type="hidden" name="kind" value="rss">`)
	if n := strings.Count(page, `<input type="hidden" name="kind" value="rss">`); n != 2 {
		t.Errorf("kind carried by %d forms, want Mark all as read and Refresh", n)
	}
	// Only the Blogs copy of the folder is the current page.
	if i := strings.Index(page, folderLink(m.mixed.ID, store.KindRSS)+` aria-current="page"`); i < 0 {
		t.Error("the Blogs copy of the folder is not marked current")
	}
	wantNotContains(t, page, folderLink(m.mixed.ID, store.KindYouTube)+` aria-current="page"`)
	if more := loadMoreURL(page); more != "" && !strings.Contains(more, "kind=rss") {
		t.Errorf("Load more drops the kind: %s", more)
	}

	all := m.get("/?scope=folder&id=" + mixed).Body.String()
	if n := len(cardIDs(all)); n != 8 {
		t.Errorf("whole folder: %d cards, want 8", n)
	}
	wantContains(t, all, `<p class="topbar-sub">Folder</p>`, `id="scope-count">8 unread</span>`)
	wantNotContains(t, all, `name="kind"`)

	up := upToRE.FindStringSubmatch(page)
	if up == nil {
		t.Fatal("no up_to watermark")
	}
	w := m.post("/mark-read", url.Values{"scope": {"folder"}, "id": {mixed}, "kind": {"rss"}, "filter": {"unread"}, "up_to": {up[1]}},
		htmx, withHeader("HX-Current-URL", "http://example.com/?scope=folder&id="+mixed+"&kind=rss"))
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	got := oobBadges(body)
	if got["badge-folder-"+mixed+"-rss"] != "" || got["badge-folder-"+mixed+"-youtube"] != "5" {
		t.Errorf("OOB badges after marking the folder's blogs read = %v", got)
	}
	unread := 0
	for _, ev := range m.entries(m.channel.ID) {
		if !ev.IsRead {
			unread++
		}
	}
	if unread != 5 {
		t.Errorf("%d videos unread after marking the folder's blogs read, want all 5 still", unread)
	}
	for _, ev := range m.entries(m.blog.ID) {
		if !ev.IsRead {
			t.Errorf("post %q is still unread", ev.Title)
		}
	}
}

// TestScopeKindRefused: a kind on any scope but a folder, or an unknown
// kind, is a bad request.
func TestScopeKindRefused(t *testing.T) {
	m := newMixedSidebar(t)
	for _, target := range []string{
		"/?scope=all&kind=youtube",
		"/?scope=feed&id=" + idStr(m.channel.ID) + "&kind=youtube",
		"/?scope=folder&id=" + idStr(m.mixed.ID) + "&kind=podcast",
	} {
		wantStatus(t, m.get(target), http.StatusBadRequest)
	}
}
