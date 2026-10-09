package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/safeurl"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
)

const (
	ytFeedURL   = "https://www.youtube.com/feeds/videos.xml?channel_id=UCabc"
	blogFeedURL = "https://blog.example.com/feed.xml"
)

func (e *testEnv) stubPreview(feedURL, title string, kind store.Kind, n int) {
	e.sched.previews[feedURL] = &sched.Preview{FeedURL: feedURL, Kind: kind, Title: title,
		SiteURL: "https://blog.example.com/", EntryCount: n}
	e.sched.entries[feedURL] = makeEntries("new", n)
}

func TestAddPageShowsURLForm(t *testing.T) {
	e := newTestEnv(t)
	w := e.get("/add")
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), `hx-post="/add/resolve"`, `name="url"`, "Find feeds", `id="add-flow"`)
}

func TestAddResolveErrorsAreShownInlineWithDirectFeedOption(t *testing.T) {
	e := newTestEnv(t)
	e.res.errs["nofeed.example"] = resolve.ErrNoFeed
	e.res.errs["https://www.youtube.com/@gone"] = fmt.Errorf("resolve: %w", resolve.ErrYouTubeID)
	e.res.errs["https://down.example"] = &fetch.StatusError{URL: "https://down.example", StatusCode: 503}
	e.res.errs["https://big.example"] = fetch.ErrTooLarge
	cases := map[string]string{
		"nofeed.example":                "No feed found at this URL",
		"https://www.youtube.com/@gone": "Could not resolve channel ID; paste the channel URL with /channel/UC… or the feed URL directly",
		"https://down.example":          "The site answered with HTTP 503.",
		"https://big.example":           "The response is larger than the 10 MB limit.",
		"   ":                           "Please enter a valid http(s) URL",
	}
	for input, msg := range cases {
		w := e.post("/add/resolve", url.Values{"url": {input}}, htmx)
		wantStatus(t, w, http.StatusUnprocessableEntity)
		body := w.Body.String()
		if !strings.HasPrefix(strings.TrimSpace(body), `<section id="add-flow"`) {
			t.Errorf("%q: response is not the add-flow fragment", input)
		}
		wantContains(t, body, `<p class="field-error" id="add-url-error">`+escapeText(msg)+`</p>`, `aria-invalid="true"`)
		if strings.TrimSpace(input) != "" {
			// Option to enter a feed URL directly, which skips discovery.
			wantContains(t, body, `hx-post="/add/preview"`, `name="feed_url" type="text" inputmode="url" value="`+input+`"`)
		}
	}
}

// escapeText mirrors html/template's escaping of the characters used above.
func escapeText(s string) string {
	return strings.NewReplacer("'", "&#39;", `"`, "&#34;").Replace(s)
}

func TestAddResolveMultipleCandidatesListFirstPreselected(t *testing.T) {
	e := newTestEnv(t)
	e.res.results["https://blog.example.com"] = &resolve.Result{Candidates: []resolve.Candidate{
		{URL: blogFeedURL, Title: "Posts", Type: "application/rss+xml"},
		{URL: "https://blog.example.com/comments.xml", Title: "Comments"},
		{URL: "https://blog.example.com/atom.xml"},
	}}
	w := e.post("/add/resolve", url.Values{"url": {"https://blog.example.com"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body,
		`<input type="radio" name="feed_url" value="`+blogFeedURL+`" checked required>`,
		`<input type="radio" name="feed_url" value="https://blog.example.com/comments.xml" required>`,
		">Posts<", ">Comments<", ">https://blog.example.com/atom.xml<", // untitled candidates show their URL
		"Continue", `hx-post="/add/preview"`,
	)
	if n := strings.Count(body, " checked"); n != 1 {
		t.Errorf("%d candidates pre-selected, want 1", n)
	}
	if len(e.sched.previewCalls) != 0 {
		t.Error("preview fetched before the user picked a candidate")
	}
}

func TestAddResolveSingleCandidateGoesStraightToPreview(t *testing.T) {
	e := newTestEnv(t)
	e.addFolder("Tech")
	e.res.results["youtube.com/@go"] = &resolve.Result{Candidates: []resolve.Candidate{{URL: ytFeedURL}}}
	e.stubPreview(ytFeedURL, "The Go Channel", store.KindYouTube, 15)

	w := e.post("/add/resolve", url.Values{"url": {"youtube.com/@go"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body,
		`hx-post="/add/subscribe"`,
		`name="title" type="text" value="The Go Channel"`, // editable, pre-filled
		`<option value="0">No folder</option>`, ">Tech</option>", `<option value="new">New folder…</option>`,
		`hx-get="/add/folder-field" hx-trigger="change" hx-target="#new-folder-field"`,
		"15 entries found.", fmt.Sprintf("The %d newest will be marked unread", config.InitialUnread),
		"YouTube",
	)
	if len(e.sched.previewCalls) != 1 || e.sched.previewCalls[0] != ytFeedURL {
		t.Errorf("preview calls = %v", e.sched.previewCalls)
	}
}

func TestAddChannelAvatarIsCarriedToTheSubscription(t *testing.T) {
	const avatar = "https://yt3.googleusercontent.com/synthetic-avatar"
	e := newTestEnv(t)
	e.res.results["youtube.com/@go"] = &resolve.Result{Candidates: []resolve.Candidate{{URL: ytFeedURL, IconURL: avatar}}}
	e.stubPreview(ytFeedURL, "The Go Channel", store.KindYouTube, 15)
	hidden := `<input type="hidden" name="icon_url" value="` + avatar + `">`

	w := e.post("/add/resolve", url.Values{"url": {"youtube.com/@go"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), `hx-post="/add/subscribe"`, hidden)

	// A form error keeps it.
	form := url.Values{"feed_url": {ytFeedURL}, "kind": {"youtube"}, "icon_url": {avatar}, "folder": {"new"}}
	w = e.post("/add/subscribe", form, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "Enter a name for the new folder.", hidden)

	form.Set("folder", "0")
	wantStatus(t, e.post("/add/subscribe", form, htmx), http.StatusOK)
	if len(e.sched.subs) != 1 || e.sched.subs[0].IconURL != avatar {
		t.Errorf("subscriptions = %+v; want the avatar passed on", e.sched.subs)
	}
}

func TestAddWithoutUsableAvatarSubscribesWithoutOne(t *testing.T) {
	e := newTestEnv(t)
	e.res.results["youtube.com/channel/UCabc"] = &resolve.Result{Candidates: []resolve.Candidate{{URL: ytFeedURL}}}
	e.stubPreview(ytFeedURL, "The Go Channel", store.KindYouTube, 15)
	w := e.post("/add/resolve", url.Values{"url": {"youtube.com/channel/UCabc"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantNotContains(t, w.Body.String(), `name="icon_url"`)

	for _, icon := range []string{
		"",
		"javascript:alert(1)",
		"data:image/png;base64,AAAA",
		"/relative.png",
		"https://yt3.googleusercontent.com/" + strings.Repeat("a", maxURLLen),
	} {
		e.sched.subs = nil
		form := url.Values{"feed_url": {ytFeedURL}, "kind": {"youtube"}, "icon_url": {icon}, "folder": {"new"}}
		w := e.post("/add/subscribe", form, htmx)
		wantStatus(t, w, http.StatusUnprocessableEntity)
		wantNotContains(t, w.Body.String(), `name="icon_url"`)

		form.Set("folder", "0")
		wantStatus(t, e.post("/add/subscribe", form, htmx), http.StatusOK)
		if len(e.sched.subs) != 1 || e.sched.subs[0].IconURL != "" {
			t.Errorf("icon_url %.40q: subscriptions = %+v; want no icon", icon, e.sched.subs)
		}
		f, err := e.st.GetFeedByURL(context.Background(), ytFeedURL)
		if err != nil {
			t.Fatal(err)
		}
		wantStatus(t, e.post("/feeds/"+idStr(f.ID)+"/unsubscribe", nil), http.StatusSeeOther)
	}
}

func TestAddDirectFeedURLSkipsDiscovery(t *testing.T) {
	e := newTestEnv(t)
	e.stubPreview(blogFeedURL, "Blog", store.KindRSS, 3)
	w := e.post("/add/preview", url.Values{"feed_url": {"blog.example.com/feed.xml"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), `value="Blog"`, "All of them will be marked unread.")
	if len(e.res.calls) != 0 {
		t.Errorf("resolver called for a direct feed URL: %v", e.res.calls)
	}

	w = e.post("/add/preview", url.Values{"feed_url": {"javascript:alert(1)"}}, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "Please enter a valid http(s) URL")

	e.sched.previewErr["https://notafeed.example/"] = errors.New("parse: not a recognizable RSS, Atom or JSON feed")
	w = e.post("/add/preview", url.Values{"feed_url": {"https://notafeed.example/"}}, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "Could not load this feed.", `value="https://notafeed.example/"`)
}

func TestAddAlreadySubscribedLinksToFeed(t *testing.T) {
	e := newTestEnv(t)
	existing := e.addFeed(store.KindRSS, "blog", "My Blog", 0, makeEntries("b", 1))
	e.res.results["https://example.com/blog"] = &resolve.Result{Candidates: []resolve.Candidate{{URL: existing.URL}}}

	w := e.post("/add/resolve", url.Values{"url": {"https://example.com/blog"}}, htmx)
	wantStatus(t, w, http.StatusConflict)
	wantContains(t, w.Body.String(), `You're already subscribed to <a href="/?scope=feed&amp;id=`+idStr(existing.ID)+`">My Blog</a>`)
	if len(e.sched.previewCalls) != 0 {
		t.Error("existing feed was fetched again")
	}

	// Subscribe racing with another add: ErrConflict from the scheduler.
	e.sched.subscribeErr = fmt.Errorf("sched: %w", store.ErrConflict)
	w = e.post("/add/subscribe", url.Values{"feed_url": {existing.URL}, "folder": {"0"}}, htmx)
	wantStatus(t, w, http.StatusConflict)
	wantContains(t, w.Body.String(), "already subscribed", ">My Blog</a>")
}

func TestAddNewFolderOptionRevealsNameInput(t *testing.T) {
	e := newTestEnv(t)
	w := e.get("/add/folder-field?folder=new", htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), `name="new_folder"`)
	w = e.get("/add/folder-field?folder=3", htmx)
	wantStatus(t, w, http.StatusOK)
	if strings.TrimSpace(w.Body.String()) != "" {
		t.Errorf("existing folder selection should clear the field, got %q", w.Body.String())
	}
}

func TestAddSubscribeRedirectsToNewFeedWithFiveUnreadAndLoadMore(t *testing.T) {
	e := newTestEnv(t)
	tech := e.addFolder("Tech")
	e.stubPreview(ytFeedURL, "The Go Channel", store.KindYouTube, 12)

	w := e.post("/add/subscribe", url.Values{
		"feed_url": {ytFeedURL}, "title": {"  Go videos "}, "folder": {idStr(tech.ID)},
		"kind": {"youtube"}, "entries": {"12"},
	}, htmx)
	wantStatus(t, w, http.StatusOK)
	feed, err := e.st.GetFeedByURL(context.Background(), ytFeedURL)
	if err != nil {
		t.Fatalf("feed not stored: %v", err)
	}
	if want := "/?scope=feed&id=" + idStr(feed.ID) + "&filter=all"; w.Header().Get("HX-Redirect") != want {
		t.Fatalf("HX-Redirect = %q, want %q", w.Header().Get("HX-Redirect"), want)
	}
	if feed.Title != "Go videos" || feed.FolderID != tech.ID {
		t.Errorf("feed = %q in folder %d", feed.Title, feed.FolderID)
	}
	if got := e.sched.subs[0]; got.Title != "Go videos" || got.FolderID != tech.ID {
		t.Errorf("subscription = %+v", got)
	}

	// The source's grid: the 5 newest (unread) first, Load more for older.
	body := e.get(w.Header().Get("HX-Redirect")).Body.String()
	if got := len(cardIDs(body)); got != config.FeedPageSize {
		t.Errorf("cards = %d, want %d", got, config.FeedPageSize)
	}
	if n := strings.Count(body, `<span class="status is-unread">Unwatched</span>`); n != config.InitialUnread {
		t.Errorf("unread cards = %d, want %d", n, config.InitialUnread)
	}
	if loadMoreURL(body) == "" {
		t.Error("missing Load more")
	}

	// Without htmx the same flow ends in a 303.
	e.stubPreview(blogFeedURL, "Blog", store.KindRSS, 2)
	w = e.post("/add/subscribe", url.Values{"feed_url": {blogFeedURL}, "folder": {"0"}})
	wantStatus(t, w, http.StatusSeeOther)
	if !strings.HasPrefix(w.Header().Get("Location"), "/?scope=feed&id=") {
		t.Errorf("Location = %q", w.Header().Get("Location"))
	}
}

func TestAddSubscribeNewFolderIsCreatedOrReused(t *testing.T) {
	e := newTestEnv(t)
	tech := e.addFolder("Tech")
	e.stubPreview(ytFeedURL, "Go", store.KindYouTube, 1)
	e.stubPreview(blogFeedURL, "Blog", store.KindRSS, 1)

	wantStatus(t, e.post("/add/subscribe", url.Values{"feed_url": {ytFeedURL}, "folder": {"new"}, "new_folder": {"tech"}}, htmx), http.StatusOK)
	wantStatus(t, e.post("/add/subscribe", url.Values{"feed_url": {blogFeedURL}, "folder": {"new"}, "new_folder": {"Reading"}}, htmx), http.StatusOK)

	ctx := context.Background()
	yt, _ := e.st.GetFeedByURL(ctx, ytFeedURL)
	blog, _ := e.st.GetFeedByURL(ctx, blogFeedURL)
	if yt.FolderID != tech.ID {
		t.Errorf("existing folder not reused: folder %d, want %d", yt.FolderID, tech.ID)
	}
	folders, _ := e.st.ListFolders(ctx)
	if len(folders) != 2 {
		t.Fatalf("folders = %+v", folders)
	}
	if reading, ok := findFolderByName(folders, "Reading"); !ok || blog.FolderID != reading.ID {
		t.Errorf("blog folder = %d, folders %+v", blog.FolderID, folders)
	}
}

func TestAddSubscribeValidation(t *testing.T) {
	e := newTestEnv(t)
	e.stubPreview(blogFeedURL, "Blog", store.KindRSS, 1)

	w := e.post("/add/subscribe", url.Values{"feed_url": {blogFeedURL}, "folder": {"new"}, "new_folder": {"  "}, "title": {"Mine"}}, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	body := w.Body.String()
	wantContains(t, body, "Enter a name for the new folder.", `name="new_folder"`, `value="Mine"`, `<option value="new" selected>`)

	w = e.post("/add/subscribe", url.Values{"feed_url": {blogFeedURL}, "folder": {"424242"}}, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "That folder no longer exists.")

	if len(e.sched.subs) != 0 {
		t.Errorf("subscribed despite invalid form: %+v", e.sched.subs)
	}
}

func TestAddSubscribeFailureRollsBackNewFolder(t *testing.T) {
	e := newTestEnv(t)
	e.sched.subscribeErr = &fetch.StatusError{URL: blogFeedURL, StatusCode: 500}
	w := e.post("/add/subscribe", url.Values{"feed_url": {blogFeedURL}, "folder": {"new"}, "new_folder": {"Temp"}}, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "The site answered with HTTP 500.")
	if folders, _ := e.st.ListFolders(context.Background()); len(folders) != 0 {
		t.Errorf("folder left behind: %+v", folders)
	}

	e.sched.subscribeErr = errors.New("disk on fire")
	w = e.post("/add/subscribe", url.Values{"feed_url": {blogFeedURL}, "folder": {"0"}}, htmx)
	wantStatus(t, w, http.StatusInternalServerError)
	wantNotContains(t, w.Body.String(), "disk on fire")
}

// TestFailedSubscribeKeepsNewFolderThatAnotherAddReused: "New folder…"
// creates the folder before subscribing. If another add reuses it while the
// first subscription is still fetching and that one then fails, the
// rollback must not delete the folder from under the other feed.
func TestFailedSubscribeKeepsNewFolderThatAnotherAddReused(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	const otherURL = "https://other.example/feed.xml"
	e.stubPreview(otherURL, "Other", store.KindRSS, 1)
	e.sched.subscribeErr = &fetch.StatusError{URL: blogFeedURL, StatusCode: 503}
	e.sched.onSubscribe = func(sub sched.Subscription) {
		if sub.FeedURL != blogFeedURL {
			return
		}
		// Second tab, while the first one's fetch is in flight.
		e.sched.subscribeErr = nil
		w := e.post("/add/subscribe", url.Values{"feed_url": {otherURL}, "folder": {"new"}, "new_folder": {"tech"}}, htmx)
		wantStatus(t, w, http.StatusOK)
	}

	w := e.post("/add/subscribe", url.Values{"feed_url": {blogFeedURL}, "folder": {"new"}, "new_folder": {"Tech"}}, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "The site answered with HTTP 503.")

	folders, _ := e.st.ListFolders(ctx)
	tech, ok := findFolderByName(folders, "Tech")
	if !ok {
		t.Fatalf("folder in use was rolled back; folders = %+v", folders)
	}
	other, err := e.st.GetFeedByURL(ctx, otherURL)
	if err != nil || other.FolderID != tech.ID {
		t.Errorf("other feed folder = %d (%v), want %d", other.FolderID, err, tech.ID)
	}
}

// atomicFolderStore offers the conditional delete the rollback prefers.
type atomicFolderStore struct {
	store.Store
	calls []int64
}

func (s *atomicFolderStore) DeleteFolderIfEmpty(_ context.Context, id int64) (bool, error) {
	s.calls = append(s.calls, id)
	return false, nil
}

func TestRollbackUsesAtomicConditionalDeleteWhenAvailable(t *testing.T) {
	e := newTestEnv(t)
	st := &atomicFolderStore{Store: e.st}
	srv, err := newServer(Deps{Config: config.Default(), Store: st, Sched: e.sched, Resolver: e.res})
	if err != nil {
		t.Fatal(err)
	}
	e.srv, e.h = srv, srv.handler()
	e.sched.subscribeErr = &fetch.StatusError{URL: blogFeedURL, StatusCode: 500}
	wantStatus(t, e.post("/add/subscribe", url.Values{"feed_url": {blogFeedURL}, "folder": {"new"}, "new_folder": {"Temp"}}, htmx),
		http.StatusUnprocessableEntity)
	folders, _ := e.st.ListFolders(context.Background())
	if len(st.calls) != 1 || len(folders) != 1 || st.calls[0] != folders[0].ID {
		t.Errorf("DeleteFolderIfEmpty calls = %v, folders = %+v", st.calls, folders)
	}
}

// TestFeedURLCredentialsAreRedactedInLogs: feed URLs may embed credentials,
// a user name and password or a user name alone (a token); logs are shipped
// and pasted into bug reports, unlike the database.
func TestFeedURLCredentialsAreRedactedInLogs(t *testing.T) {
	for _, secretURL := range []string{
		"https://jack:s3cret@private.example/feed.xml",
		"https://s3cret@private.example/feed.xml",
	} {
		t.Run(secretURL, func(t *testing.T) {
			e := newTestEnv(t)
			logs := e.captureLogs()
			e.stubPreview(secretURL, "Private", store.KindRSS, 1)

			wantStatus(t, e.post("/add/subscribe", url.Values{"feed_url": {secretURL}, "folder": {"0"}}, htmx), http.StatusOK)
			f, err := e.st.GetFeedByURL(context.Background(), secretURL)
			if err != nil {
				t.Fatal(err)
			}
			wantStatus(t, e.post("/feeds/"+idStr(f.ID)+"/unsubscribe", nil), http.StatusSeeOther)
			e.sched.subscribeErr = errors.New("disk on fire")
			wantStatus(t, e.post("/add/subscribe", url.Values{"feed_url": {secretURL}, "folder": {"0"}}, htmx), http.StatusInternalServerError)

			out := logs.String()
			wantContains(t, out, "msg=subscribed", "msg=unsubscribed", "disk on fire", safeurl.Placeholder+"@private.example")
			if strings.Contains(out, "s3cret") || strings.Contains(out, "jack") {
				t.Errorf("credentials in logs:\n%s", out)
			}
		})
	}
}

func TestAddDirectFeedURLWithoutHostNameIsInvalid(t *testing.T) {
	e := newTestEnv(t)
	for _, raw := range []string{"http://:8080/feed", "http://tok3n@:8080/feed"} {
		w := e.post("/add/preview", url.Values{"feed_url": {raw}}, htmx)
		wantStatus(t, w, http.StatusUnprocessableEntity)
		wantContains(t, w.Body.String(), resolve.ErrInvalidURL.Error())
		if len(e.sched.previewCalls) != 0 {
			t.Errorf("%s: previewed %v", raw, e.sched.previewCalls)
		}
	}
}
