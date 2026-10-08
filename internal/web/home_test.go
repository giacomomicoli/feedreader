package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/store"
)

func TestHomeRendersSidebarScopesFoldersFeedsAndTags(t *testing.T) {
	e := newTestEnv(t)
	tech := e.addFolder("Tech channels")
	yt := e.addFeed(store.KindYouTube, "yt", "Go Channel", tech.ID, makeEntries("yt", 8))
	blog := e.addFeed(store.KindRSS, "blog", "Plain Blog", 0, makeEntries("blog", 3))
	ctx := context.Background()
	if _, err := e.st.AddEntryTag(ctx, e.entries(blog.ID)[0].ID, "golang"); err != nil {
		t.Fatal(err)
	}

	w := e.get("/")
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body,
		`<!doctype html>`,
		`<meta name="htmx-config" content='{"includeIndicatorStyles":false,"allowEval":false,"allowScriptTags":false`,
		`href="/add"`, "Add source",
		">All<", ">Watch later<", ">Read later<", ">Favourites<",
		`id="sidebar-tags"`, ">golang<",
		`<details class="folder">`, ">Tech channels<",
		">Go Channel<", ">Plain Blog<",
		`action="/folders"`, "New folder",
		"Mark all as read", "Refresh", ">Unread<", ">All</a>",
	)
	// Badges: 5 unread in the YouTube feed (initial add), 3 in the blog,
	// folder = sum of its feeds, All = everything.
	wantContains(t, body,
		`<span class="badge" id="badge-all">8</span>`,
		`<span class="badge" id="badge-folder-`+idStr(tech.ID)+`">5</span>`,
		`<span class="badge" id="badge-feed-`+idStr(yt.ID)+`">5</span>`,
		`<span class="badge" id="badge-feed-`+idStr(blog.ID)+`">3</span>`,
	)
	// Default scope All + Unread: every unread entry, nothing read.
	if got := len(cardIDs(body)); got != 8 {
		t.Errorf("cards = %d, want 8 unread", got)
	}
	wantNotContains(t, body, "Add your first source")
	// Folders come after tags; uncategorized feeds after folders.
	if i, j, k := strings.Index(body, `id="sidebar-tags"`), strings.Index(body, "Tech channels"), strings.Index(body, "Plain Blog"); !(i < j && j < k) {
		t.Errorf("sidebar order wrong: tags@%d folder@%d uncategorized@%d", i, j, k)
	}
}

func TestEmptyStateShowsAddFirstSourceCard(t *testing.T) {
	e := newTestEnv(t)
	w := e.get("/")
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body, `class="card-cta" href="/add"`, "Add your first source")
	wantNotContains(t, body, "Mark all as read", `<article class="card`)
	if n := strings.Count(body, "Add your first source"); n != 1 {
		t.Errorf("CTA rendered %d times, want exactly one card", n)
	}
}

func TestFolderOfActiveFeedIsExpanded(t *testing.T) {
	e := newTestEnv(t)
	fo := e.addFolder("News")
	f := e.addFeed(store.KindRSS, "news", "Daily", fo.ID, makeEntries("n", 2))
	body := e.get("/?scope=feed&id=" + idStr(f.ID)).Body.String()
	wantContains(t, body, `<details class="folder" open>`, `aria-current="page">`)
}

func TestFeedScopeLoadMorePagesInBlocksOfFive(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeed(store.KindYouTube, "yt", "Channel", 0, makeEntries("yt", 12))

	// The source grid after adding: filter=all shows the 5 newest first.
	w := e.get("/?scope=feed&id=" + idStr(f.ID) + "&filter=all")
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	all := e.entries(f.ID)
	if got := cardIDs(body); len(got) != config.FeedPageSize || got[0] != idStr(all[0].ID) {
		t.Fatalf("first page cards = %v, want %d starting with newest %d", got, config.FeedPageSize, all[0].ID)
	}
	for _, ev := range all[:config.InitialUnread] {
		if ev.IsRead {
			t.Errorf("entry %q should be unread after add", ev.Title)
		}
	}
	more := loadMoreURL(body)
	if more == "" {
		t.Fatal("no Load more button on first page")
	}

	// Load more: the next 5 older stored entries, plus a new button.
	w = e.get(more, htmx)
	wantStatus(t, w, http.StatusOK)
	page2 := w.Body.String()
	if got := cardIDs(page2); len(got) != 5 || got[0] != idStr(all[5].ID) {
		t.Fatalf("second page = %v, want 5 starting with %d", got, all[5].ID)
	}
	wantNotContains(t, page2, "<html", `id="sidebar-tags"`)
	more = loadMoreURL(page2)
	if more == "" {
		t.Fatal("no Load more button on second page")
	}

	// Last page: the remaining 2, and the button disappears.
	w = e.get(more, htmx)
	page3 := w.Body.String()
	if got := cardIDs(page3); len(got) != 2 {
		t.Fatalf("third page = %v, want 2", got)
	}
	if loadMoreURL(page3) != "" || strings.Contains(page3, "Load more") {
		t.Error("Load more button still present when stored entries are exhausted")
	}
}

func TestFewerThanFiveEntriesHasNoLoadMore(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeed(store.KindRSS, "small", "Small", 0, makeEntries("s", 3))
	body := e.get("/?scope=feed&id=" + idStr(f.ID) + "&filter=all").Body.String()
	if got := len(cardIDs(body)); got != 3 {
		t.Errorf("cards = %d, want 3", got)
	}
	wantNotContains(t, body, "Load more")
}

func TestAllScopeLoadMoreUsesBlocksOfTwenty(t *testing.T) {
	e := newTestEnv(t)
	for _, slug := range []string{"a", "b", "c", "d", "e"} {
		e.addFeed(store.KindRSS, slug, "Feed "+slug, 0, makeEntries(slug, 6))
	}
	body := e.get("/?filter=all").Body.String()
	if got := len(cardIDs(body)); got != config.ScopePageSize {
		t.Fatalf("cards = %d, want %d", got, config.ScopePageSize)
	}
	more := loadMoreURL(body)
	if more == "" {
		t.Fatal("missing Load more")
	}
	page2 := e.get(more, htmx).Body.String()
	if got := len(cardIDs(page2)); got != 10 {
		t.Errorf("second page cards = %d, want 10", got)
	}
	wantNotContains(t, page2, "Load more")
}

func TestLoadMoreWithoutHTMXFallsBackToFullPage(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeed(store.KindRSS, "a", "A", 0, makeEntries("a", 8))
	more := loadMoreURL(e.get("/?scope=feed&id=" + idStr(f.ID) + "&filter=all").Body.String())
	w := e.get(more)
	wantStatus(t, w, http.StatusSeeOther)
	loc := w.Header().Get("Location")
	if !strings.HasPrefix(loc, "/?") || !strings.Contains(loc, "after=") {
		t.Fatalf("Location = %q", loc)
	}
	full := e.get(loc)
	wantStatus(t, full, http.StatusOK)
	if got := len(cardIDs(full.Body.String())); got != 3 {
		t.Errorf("cards on fallback page = %d, want 3", got)
	}
}

func TestScopeQueryValidation(t *testing.T) {
	e := newTestEnv(t)
	e.addFeed(store.KindRSS, "a", "A", 0, makeEntries("a", 1))
	cases := map[string]int{
		"/?scope=bogus":              http.StatusBadRequest,
		"/?scope=feed":               http.StatusBadRequest,
		"/?scope=feed&id=abc":        http.StatusBadRequest,
		"/?scope=feed&id=-3":         http.StatusBadRequest,
		"/?filter=sometimes":         http.StatusBadRequest,
		"/?after=garbage":            http.StatusBadRequest,
		"/?scope=feed&id=999":        http.StatusNotFound,
		"/?scope=folder&id=999":      http.StatusNotFound,
		"/?scope=tag&id=999":         http.StatusNotFound,
		"/?scope=favourites":         http.StatusOK,
		"/?scope=watchlater&id=7":    http.StatusOK,
		"/?scope=all&filter=all":     http.StatusOK,
		"/entries?scope=bogus":       http.StatusBadRequest,
		"/no/such/page":              http.StatusNotFound,
		"/entries?scope=feed&id=999": http.StatusNotFound,
	}
	for target, want := range cases {
		w := e.get(target, htmx)
		if strings.HasPrefix(target, "/?") || strings.HasPrefix(target, "/no") {
			w = e.get(target)
		}
		if w.Code != want {
			t.Errorf("GET %s = %d, want %d", target, w.Code, want)
		}
	}
}

func TestWatchLaterAndReadLaterScopesSplitByKindRegardlessOfReadState(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	yt := e.addFeed(store.KindYouTube, "yt", "Videos", 0, makeEntries("yt", 2))
	blog := e.addFeed(store.KindRSS, "blog", "Articles", 0, makeEntries("blog", 2))
	video, article := e.entries(yt.ID)[0], e.entries(blog.ID)[0]
	for _, id := range []int64{video.ID, article.ID} {
		if err := e.st.SetLater(ctx, id, true); err != nil {
			t.Fatal(err)
		}
		if err := e.st.SetRead(ctx, id, true, time.Now()); err != nil {
			t.Fatal(err)
		}
	}

	wl := e.get("/?scope=watchlater").Body.String()
	if got := cardIDs(wl); len(got) != 1 || got[0] != idStr(video.ID) {
		t.Errorf("Watch later cards = %v, want [%d]", got, video.ID)
	}
	wantNotContains(t, wl, ">Unread</a>") // no read filter in later scopes

	rl := e.get("/?scope=readlater").Body.String()
	if got := cardIDs(rl); len(got) != 1 || got[0] != idStr(article.ID) {
		t.Errorf("Read later cards = %v, want [%d]", got, article.ID)
	}
}

func TestCardLabelsDependOnFeedKind(t *testing.T) {
	e := newTestEnv(t)
	yt := e.addFeed(store.KindYouTube, "yt", "Videos", 0, makeEntries("yt", 1))
	blog := e.addFeed(store.KindRSS, "blog", "Articles", 0, makeEntries("blog", 1))

	ytBody := e.get("/?scope=feed&id=" + idStr(yt.ID)).Body.String()
	wantContains(t, ytBody, ">Unwatched<", ">Mark watched</button>", ">Watch later</button>", ">Add to favourites</button>")
	wantNotContains(t, ytBody, ">Mark read</button>", ">Read later</button>")

	blogBody := e.get("/?scope=feed&id=" + idStr(blog.ID)).Body.String()
	wantContains(t, blogBody, `<span class="status is-unread">Unread</span>`, ">Mark read</button>", ">Read later</button>")
	wantNotContains(t, blogBody, ">Mark watched</button>", ">Watch later</button>")
}

func TestCardOpensOriginalInNewTabWithoutMarkingRead(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeed(store.KindRSS, "blog", "Blog", 0, makeEntries("blog", 1))
	ev := e.entries(f.ID)[0]
	body := e.get("/").Body.String()
	wantContains(t, body,
		`href="`+ev.URL+`" target="_blank" rel="noopener noreferrer"`,
		`title="Summary of blog entry 0"`, // summary only as tooltip / expandable area
		`<details class="card-summary">`,
	)
	// The link itself carries no htmx action: opening never marks read.
	if strings.Contains(body, `hx-post="`+ev.URL) || strings.Contains(body, `hx-get="`+ev.URL) {
		t.Error("original link triggers an htmx request")
	}
	if e.entry(ev.ID).IsRead {
		t.Error("rendering marked the entry read")
	}
}

func TestCardThumbnailFallbacks(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	now := time.Now().Add(-time.Hour)
	_, err := e.st.CreateFeed(ctx, store.NewFeed{
		Kind: store.KindRSS, URL: "https://example.com/feed", Title: "Ada Lovelace Weekly",
		IconURL: "https://example.com/icon.png", FetchedAt: now,
	}, []store.NewEntry{
		{GUID: "1", Title: "Has thumbnail", ThumbnailURL: "https://img.example.com/t.jpg", PublishedAt: now},
		{GUID: "2", Title: "Has image", SummaryHTML: `<p>x</p><img src="https://img.example.com/first.png"><img src="https://img.example.com/second.png">`, PublishedAt: now.Add(-time.Minute)},
		{GUID: "3", Title: "Uses icon", PublishedAt: now.Add(-2 * time.Minute)},
	}, 5)
	if err != nil {
		t.Fatal(err)
	}
	body := e.get("/").Body.String()
	wantContains(t, body,
		`class="thumb-image" src="https://img.example.com/t.jpg"`,
		`class="thumb-image" src="https://img.example.com/first.png"`,
		`class="thumb-icon" src="https://example.com/icon.png"`,
	)
	wantNotContains(t, body, "second.png\"") // only the first image becomes the thumbnail

	g, err := e.st.CreateFeed(ctx, store.NewFeed{Kind: store.KindRSS, URL: "https://example.org/feed",
		Title: "Grace Hopper", FetchedAt: now}, []store.NewEntry{{GUID: "x", Title: "No image", PublishedAt: now}}, 5)
	if err != nil {
		t.Fatal(err)
	}
	body = e.get("/?scope=feed&id=" + idStr(g.ID)).Body.String()
	wantContains(t, body, `<span class="thumb-initials">GH</span>`, "card-thumb "+paletteClass(g.ID))
}

func TestFutureAndPastPublishedTimesAreRelativeWithAbsoluteTooltip(t *testing.T) {
	// absTime prints the local zone's abbreviation; in zones that have none
	// it is numeric ("+04"), and html/template escapes the "+" in the
	// attribute. Pin a lettered zone so the test does not depend on TZ.
	// (Mutates a global: this test must not run in parallel.)
	oldLocal := time.Local
	time.Local = time.FixedZone("TST", 3600)
	t.Cleanup(func() { time.Local = oldLocal })

	e := newTestEnv(t)
	ctx := context.Background()
	future := time.Now().Add(49 * time.Hour)
	_, err := e.st.CreateFeed(ctx, store.NewFeed{Kind: store.KindRSS, URL: "https://example.com/f",
		Title: "F", FetchedAt: time.Now().Add(-time.Hour)},
		[]store.NewEntry{{GUID: "1", Title: "Scheduled", PublishedAt: future}}, 5)
	if err != nil {
		t.Fatal(err)
	}
	body := e.get("/").Body.String()
	wantContains(t, body, ">in 2 days</time>", `datetime="`+future.UTC().Truncate(time.Second).Format(time.RFC3339)+`"`,
		`title="`+absTime(future.Truncate(time.Second))+`"`)
}

func TestWarningIconAfterRepeatedFetchFailures(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	f := e.addFeed(store.KindRSS, "down", "Flaky", 0, makeEntries("d", 1))
	now := time.Now()
	for i := 0; i < config.WarnAfterFailures-1; i++ {
		if err := e.st.RecordFetchError(ctx, f.ID, "HTTP 503 from upstream", now, now.Add(time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	wantNotContains(t, e.get("/").Body.String(), `class="feed-warn"`)
	if err := e.st.RecordFetchError(ctx, f.ID, "HTTP 404 from upstream", now, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	wantContains(t, e.get("/").Body.String(), `<span class="feed-warn" title="HTTP 404 from upstream">`)
}
