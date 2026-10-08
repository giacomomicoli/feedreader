package e2e

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// TestNoSubscriptions_SchedulerMakesNoRequests: with zero subscriptions the
// worker is idle and no HTTP request is ever made until the first feed is
// added, even when a refresh wakes the scheduler up.
func TestNoSubscriptions_SchedulerMakesNoRequests(t *testing.T) {
	site := newFeedSite(t)
	a := startApp(t)

	home := a.visit(t, "/").expect(t, http.StatusOK)
	if findFirst(home.doc, elemClass("a", "card-cta")) == nil {
		t.Errorf("empty state: no \"Add your first source\" card")
	}
	a.hxPost(t, "/refresh", url.Values{"scope": {"all"}}, "/").expect(t, http.StatusOK)

	time.Sleep(300 * time.Millisecond) // give a misbehaving scheduler the chance to fetch
	if n := site.total(); n != 0 {
		t.Fatalf("feed site received %d requests with zero subscriptions, want 0", n)
	}
}

// TestAddDirectFeedURL_FetchesTheFeedOnce: a pasted URL that already is a
// feed is used as-is and fetched once immediately: the resolver's request
// serves the preview and the subscription.
func TestAddDirectFeedURL_FetchesTheFeedOnce(t *testing.T) {
	site := newFeedSite(t)
	a := startApp(t)
	p := a.hxPost(t, "/add/resolve", url.Values{"url": {site.url(blogFeedPath)}}, "/add").expect(t, http.StatusOK)
	form := p.formValues(t, "/add/subscribe")
	if got := form.Get("feed_url"); got != site.url(blogFeedPath) {
		t.Fatalf("confirm form feed_url = %q, want %q", got, site.url(blogFeedPath))
	}
	a.hxPost(t, "/add/subscribe", form, "/add").expect(t, http.StatusOK)
	if n := site.requests(blogFeedPath); n != 1 {
		t.Errorf("feed fetched %d times to resolve, preview and subscribe it; want 1", n)
	}
}

// TestEndToEnd drives the whole application through its UI, step by step,
// like a user with htmx would. Each step depends on the previous ones.
func TestEndToEnd(t *testing.T) {
	sc := &scenario{site: newFeedSite(t), a: startApp(t), ids: map[string]int64{}}
	steps := []struct {
		name string
		run  func(*testing.T)
	}{
		{"EmptyState_AddYourFirstSource_NothingPolled", sc.emptyState},
		{"AddSource_PageWithFeedLinks_ListsCandidatesFirstPreselected", sc.resolveCandidates},
		{"AddSource_Preview_FetchesFeedOnceAndPrefillsTitle", sc.preview},
		{"AddSource_Confirm_SubscribesIntoNewFolderAndRedirectsToFeed", sc.confirm},
		{"InitialAdd_5NewestUnread_RestStoredRead", sc.initialUnread},
		{"LoadMore_RevealsOlderStoredEntriesInBlocksOf5", sc.loadMore},
		{"QuickActions_ReturnCardPlusOOBSidebarCounts", sc.quickActions},
		{"Tags_AddMatchCaseInsensitivelyAutocompleteRemove", sc.tags},
		{"Refresh_UnchangedFeed_ConditionalGetAnswered304", sc.refreshNotModified},
		{"Poll_NewItemsUnread_EarlierStateUntouched", sc.pollNewItems},
		{"Poll_PermanentRedirect_StoredURLUpdatedOnce", sc.pollMoved},
		{"Poll_RepeatedFailures_WarningAfter3_EntriesKept", sc.pollFailures},
		{"AddSource_DirectFeedURL_FewerThan5AllUnreadNoLoadMore", sc.addVideos},
		{"AddSource_AlreadySubscribed_Conflict", sc.addDuplicate},
		{"MarkAllAsRead_CurrentScopeOnly", sc.markAllRead},
		{"CrossSitePost_Rejected", sc.crossSite},
		{"Unsubscribe_DeletesFeedEntriesSavedEntriesAndEntryTags", sc.unsubscribe},
		{"Unsubscribe_FeedNeverPolledAgain", sc.notPolledAfterUnsubscribe},
		{"Unsubscribe_LastFeed_BackToEmptyState", sc.unsubscribeLast},
	}
	for _, s := range steps {
		if !t.Run(s.name, s.run) {
			return // later steps build on this one
		}
	}
}

// scenario is the state shared by the TestEndToEnd steps.
type scenario struct {
	site *feedSite
	a    *app

	candidates *page            // the "pick a feed" step of the add flow
	confirmed  url.Values       // the confirmation form of the blog
	blogID     int64            // the blog feed
	folderID   int64            // the folder created while adding the blog
	videosID   int64            // the YouTube-style channel feed
	tagID      int64            // the tag "Go"
	ids        map[string]int64 // entry id by card title
	lastAction *page            // the latest per-entry action response
}

func feedScope(id int64) string { return "/?scope=feed&id=" + strconv.FormatInt(id, 10) }

func feedBadge(id int64) string   { return "badge-feed-" + strconv.FormatInt(id, 10) }
func folderBadge(id int64) string { return "badge-folder-" + strconv.FormatInt(id, 10) }

// blogBadges are the sidebar badges while only the blog has unread entries:
// All, the blog's folder and the blog itself all count n.
func (sc *scenario) blogBadges(n int) map[string]int {
	return map[string]int{"badge-all": n, folderBadge(sc.folderID): n, feedBadge(sc.blogID): n}
}

// Unread blog entries at the stages of the scenario.
const (
	// blogUnreadAfterAdd: the newest posts start unread, the rest are
	// archived as read.
	blogUnreadAfterAdd = config.InitialUnread
	// blogUnreadAfterPoll: the posts published later arrive unread; marking
	// Post 8 read and Post 1 unread cancelled each other out.
	blogUnreadAfterPoll = blogUnreadAfterAdd + publishedLater
)

func (sc *scenario) emptyState(t *testing.T) {
	p := sc.a.visit(t, "/").expect(t, http.StatusOK)
	if csp := p.header.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("Content-Security-Policy = %q", csp)
	}
	cta := findFirst(p.doc, elemClass("a", "card-cta"))
	if cta == nil || !strings.Contains(textOf(cta), "Add your first source") {
		t.Errorf("empty state: no \"Add your first source\" card in:\n%s", p.body)
	}
	if n := sc.site.total(); n != 0 {
		t.Errorf("feed site received %d requests before any source was added", n)
	}
}

// resolveCandidates: a page advertising several feeds yields a list to pick
// from, the first one pre-selected, relative hrefs resolved.
func (sc *scenario) resolveCandidates(t *testing.T) {
	a, site := sc.a, sc.site
	a.visit(t, "/add").expect(t, http.StatusOK)
	p := a.hxPost(t, "/add/resolve", url.Values{"url": {site.url(blogPagePath)}}, "/add").expect(t, http.StatusOK)

	radios := findAll(p.doc, func(n *html.Node) bool {
		return isElem(n, "input") && attr(n, "type") == "radio" && attr(n, "name") == "feed_url"
	})
	var got []string
	for _, r := range radios {
		got = append(got, attr(r, "value"))
	}
	want := []string{site.url(blogFeedPath), site.url(blogCommentsPath)}
	if !slices.Equal(got, want) {
		t.Fatalf("candidates = %q, want %q", got, want)
	}
	if !hasAttr(radios[0], "checked") || hasAttr(radios[1], "checked") {
		t.Errorf("the first candidate must be the only pre-selected one")
	}
	if !strings.Contains(textOf(p.doc), "E2E Blog posts") {
		t.Errorf("candidate titles from <link title> missing:\n%s", p.body)
	}
	if n := site.requests(blogPagePath); n != 1 {
		t.Errorf("blog page fetched %d times, want 1", n)
	}
	if n := site.requests(blogFeedPath) + site.requests(blogCommentsPath); n != 0 {
		t.Errorf("feeds fetched %d times before one was picked, want 0", n)
	}
	sc.candidates = p
}

// preview: the feed is fetched once immediately, its title is pre-filled
// (editable).
func (sc *scenario) preview(t *testing.T) {
	a, site := sc.a, sc.site
	form := sc.candidates.formValues(t, "/add/preview")
	if got := form.Get("feed_url"); got != site.url(blogFeedPath) {
		t.Fatalf("pre-selected candidate = %q", got)
	}
	p := a.hxPost(t, "/add/preview", form, "/add").expect(t, http.StatusOK)

	v := p.formValues(t, "/add/subscribe")
	checks := map[string]string{
		"feed_url": site.url(blogFeedPath),
		"kind":     string(store.KindRSS),
		"entries":  strconv.Itoa(initialPosts),
		"title":    "E2E Blog",
		"folder":   "0",
	}
	for k, want := range checks {
		if got := v.Get(k); got != want {
			t.Errorf("confirm form %s = %q, want %q", k, got, want)
		}
	}
	text := textOf(p.doc)
	for _, want := range []string{
		fmt.Sprintf("%d entries found.", initialPosts),
		fmt.Sprintf("The %d newest will be marked unread", config.InitialUnread),
	} {
		if !strings.Contains(text, want) {
			t.Errorf("preview text lacks %q: %s", want, text)
		}
	}
	if n := site.requests(blogFeedPath); n != 1 {
		t.Errorf("feed fetched %d times by the preview, want 1", n)
	}

	// Choosing "New folder…" reveals the folder name input.
	nf := a.hxGet(t, "/add/folder-field?folder=new").expect(t, http.StatusOK)
	if findFirst(nf.doc, func(n *html.Node) bool { return isElem(n, "input") && attr(n, "name") == "new_folder" }) == nil {
		t.Errorf("no new_folder input after choosing \"New folder…\":\n%s", nf.body)
	}
	sc.confirmed = v
}

func (sc *scenario) confirm(t *testing.T) {
	a, site := sc.a, sc.site
	v := sc.confirmed
	v.Set("title", "My E2E Blog")
	v.Set("folder", "new")
	v.Set("new_folder", "Tech newsletters")
	p := a.hxPost(t, "/add/subscribe", v, "/add").expect(t, http.StatusOK)

	sc.blogID = redirectedFeed(t, p)
	f, err := a.st.GetFeed(t.Context(), sc.blogID)
	if err != nil {
		t.Fatalf("get subscribed feed: %v", err)
	}
	if f.URL != site.url(blogFeedPath) || f.Title != "My E2E Blog" || f.OriginalTitle != "E2E Blog" || f.Kind != store.KindRSS {
		t.Errorf("stored feed = url %q title %q original %q kind %q", f.URL, f.Title, f.OriginalTitle, f.Kind)
	}
	if f.ETag != `"posts-v1"` {
		t.Errorf("stored ETag = %q, want the one from the initial fetch", f.ETag)
	}
	if until := time.Until(f.NextFetchAt); until < config.DefaultPollInterval-time.Hour {
		t.Errorf("next fetch in %s, want about %s", until, config.DefaultPollInterval)
	}
	if f.FolderID == 0 {
		t.Fatalf("feed not placed in the new folder")
	}
	folder, err := a.st.GetFolder(t.Context(), f.FolderID)
	if err != nil || folder.Name != "Tech newsletters" {
		t.Fatalf("folder = %+v, %v", folder, err)
	}
	sc.folderID = folder.ID
	if n := site.requests(blogFeedPath); n != 1 {
		t.Errorf("feed fetched %d times in the whole add flow, want 1 (subscribe reuses the preview)", n)
	}
}

// redirectedFeed checks the HX-Redirect of a successful subscription and
// returns the feed id it points to.
func redirectedFeed(t *testing.T, p *page) int64 {
	t.Helper()
	loc := p.header.Get("HX-Redirect")
	u, err := url.Parse(loc)
	if err != nil || u.Path != "/" || u.Query().Get("scope") != "feed" {
		t.Fatalf("HX-Redirect = %q, want the feed scope", loc)
	}
	id, err := strconv.ParseInt(u.Query().Get("id"), 10, 64)
	if err != nil || id <= 0 {
		t.Fatalf("HX-Redirect = %q: bad feed id", loc)
	}
	return id
}

// initialUnread: the 5 newest entries are marked unread, the rest are stored
// as read.
func (sc *scenario) initialUnread(t *testing.T) {
	a, site := sc.a, sc.site
	p := a.visit(t, feedScope(sc.blogID)).expect(t, http.StatusOK)
	cs := p.cards(t)
	want := postTitles(initialPosts, initialPosts-config.InitialUnread+1)
	if got := cardTitles(cs); !slices.Equal(got, want) {
		t.Fatalf("unread cards = %q, want %q", got, want)
	}
	for _, c := range cs {
		if c.read || c.status != "Unread" {
			t.Errorf("%s: read=%v status=%q, want unread", c.title, c.read, c.status)
		}
	}
	if more := p.loadMore(); more != "" {
		t.Errorf("unread filter shows Load more (%s) with exactly %d unread", more, config.InitialUnread)
	}
	// Card content: absolute link and image from the sanitized summary.
	if c := cs[0]; c.href != site.url("/posts/8") || c.thumb != site.url("/img/8.png") {
		t.Errorf("card link %q thumb %q", c.href, c.thumb)
	}
	if strings.Contains(p.body, "xss-") {
		t.Errorf("feed script content reached the page")
	}
	expectBadges(t, p, false, sc.blogBadges(blogUnreadAfterAdd))
}

// loadMore: single-feed scope pages through stored entries in blocks of 5;
// the button disappears when nothing older is stored.
func (sc *scenario) loadMore(t *testing.T) {
	a := sc.a
	p := a.visit(t, feedScope(sc.blogID)+"&filter=all").expect(t, http.StatusOK)
	first := p.cards(t)
	if len(first) != config.FeedPageSize {
		t.Fatalf("first block has %d cards, want %d", len(first), config.FeedPageSize)
	}
	more := p.loadMore()
	if more == "" {
		t.Fatalf("no Load more with %d stored entries", initialPosts)
	}
	q := a.hxGet(t, more).expect(t, http.StatusOK)
	if strings.Contains(q.body, "<aside") {
		t.Errorf("Load more returned a full page instead of a fragment")
	}
	older := q.cards(t)
	if got, want := cardTitles(older), postTitles(initialPosts-config.FeedPageSize, 1); !slices.Equal(got, want) {
		t.Fatalf("Load more cards = %q, want %q", got, want)
	}
	for _, c := range older {
		if !c.read || c.status != "Read" {
			t.Errorf("%s: read=%v status=%q, want archived as read", c.title, c.read, c.status)
		}
	}
	if next := q.loadMore(); next != "" {
		t.Errorf("Load more still offered after the last stored entry: %s", next)
	}
	for _, c := range append(first, older...) {
		sc.ids[c.title] = c.id
	}
}

// quickActions: per-entry quick actions; every action handler returns the
// fragment it changed plus an OOB swap for the sidebar counts.
func (sc *scenario) quickActions(t *testing.T) {
	a := sc.a
	cur := feedScope(sc.blogID)
	act := func(title, action string) card {
		t.Helper()
		p := a.hxPost(t, fmt.Sprintf("/entries/%d/%s", sc.ids[title], action), url.Values{"back": {cur}}, cur).
			expect(t, http.StatusOK)
		cs := p.cards(t)
		if len(cs) != 1 || cs[0].id != sc.ids[title] {
			t.Fatalf("%s %s: response cards = %+v, want the one card", action, title, cs)
		}
		if tags := byID(p.doc, "sidebar-tags"); tags == nil || attr(tags, "hx-swap-oob") != "true" {
			t.Errorf("%s %s: no out-of-band sidebar tag list", action, title)
		}
		sc.lastAction = p
		return cs[0]
	}

	if c := act("Post 8", "read"); !c.read || c.status != "Read" {
		t.Errorf("mark read: card = %+v", c)
	}
	expectBadges(t, sc.lastAction, true, sc.blogBadges(blogUnreadAfterAdd-1))
	if c := act("Post 1", "unread"); c.read || c.status != "Unread" {
		t.Errorf("mark unread: card = %+v", c)
	}
	expectBadges(t, sc.lastAction, true, sc.blogBadges(blogUnreadAfterAdd))

	// Read later and favourite never change the read state.
	if c := act("Post 2", "later"); !c.later || !c.read {
		t.Errorf("read later: card = %+v", c)
	}
	if c := act("Post 2", "unlater"); c.later {
		t.Errorf("remove from read later: card = %+v", c)
	}
	if c := act("Post 2", "later"); !c.later {
		t.Errorf("read later again: card = %+v", c)
	}
	if c := act("Post 3", "favourite"); !c.fav || !c.read {
		t.Errorf("favourite: card = %+v", c)
	}
	expectBadges(t, sc.lastAction, true, map[string]int{"badge-all": blogUnreadAfterAdd})

	// Saved scopes list entries regardless of read state; the blog is an
	// rss feed, so its saved entries are in Read later.
	scopes := map[string][]string{
		"/?scope=readlater":  {"Post 2"},
		"/?scope=favourites": {"Post 3"},
		"/?scope=watchlater": nil,
	}
	for path, want := range scopes {
		if got := cardTitles(a.visit(t, path).expect(t, http.StatusOK).cards(t)); !slices.Equal(got, want) {
			t.Errorf("%s cards = %q, want %q", path, got, want)
		}
	}
}

// tags: "Add tag" takes free text with autocomplete on existing tags, tag
// names are unique case-insensitively, the sidebar lists tags with counts.
func (sc *scenario) tags(t *testing.T) {
	a := sc.a
	cur := feedScope(sc.blogID)
	tag := func(title, name string) *page {
		t.Helper()
		return a.hxPost(t, fmt.Sprintf("/entries/%d/tags", sc.ids[title]), url.Values{"tag": {name}, "back": {cur}}, cur).
			expect(t, http.StatusOK)
	}

	p := tag("Post 7", "Go")
	if cs := p.cards(t); len(cs) != 1 || !slices.Equal(cs[0].tags, []string{"Go"}) {
		t.Fatalf("tagged card = %+v", cs)
	}
	if got := p.sidebarTags(); got["Go"] != "1" {
		t.Errorf("sidebar tags = %v, want Go: 1", got)
	}

	s := a.hxGet(t, "/tags/suggest?tag=g").expect(t, http.StatusOK)
	if opt := findFirst(s.doc, elem("option")); opt == nil || attr(opt, "value") != "Go" {
		t.Errorf("autocomplete for \"g\" = %s, want the option Go", s.body)
	}

	p = tag("Post 6", "go") // same tag, different case
	if cs := p.cards(t); len(cs) != 1 || !slices.Equal(cs[0].tags, []string{"Go"}) {
		t.Errorf("card tagged \"go\" = %+v, want the existing tag Go", cs)
	}
	if got := p.sidebarTags(); got["Go"] != "2" || len(got) != 1 {
		t.Errorf("sidebar tags = %v, want only Go: 2", got)
	}

	tags, err := a.st.ListTags(t.Context())
	if err != nil || len(tags) != 1 {
		t.Fatalf("stored tags = %+v, %v", tags, err)
	}
	sc.tagID = tags[0].ID
	tp := a.visit(t, fmt.Sprintf("/?scope=tag&id=%d", sc.tagID)).expect(t, http.StatusOK)
	if got, want := cardTitles(tp.cards(t)), []string{"Post 7", "Post 6"}; !slices.Equal(got, want) {
		t.Errorf("tag scope cards = %q, want %q", got, want)
	}

	p = a.hxPost(t, fmt.Sprintf("/entries/%d/tags/%d/remove", sc.ids["Post 6"], sc.tagID), url.Values{"back": {cur}}, cur).
		expect(t, http.StatusOK)
	if cs := p.cards(t); len(cs) != 1 || len(cs[0].tags) != 0 {
		t.Errorf("card after removing the tag = %+v", cs)
	}
	if got := p.sidebarTags(); got["Go"] != "1" {
		t.Errorf("sidebar tags after removal = %v, want Go: 1", got)
	}
}

// refresh forces a fetch of feed id through the UI.
func (sc *scenario) refresh(t *testing.T, scope string, id int64) {
	t.Helper()
	form := url.Values{"scope": {scope}, "filter": {"unread"}, "back": {"/"}}
	if id != 0 {
		form.Set("id", strconv.FormatInt(id, 10))
	}
	p := sc.a.hxPost(t, "/refresh", form, "/").expect(t, http.StatusOK)
	if !strings.Contains(textOf(p.doc), "Refresh started") {
		t.Errorf("refresh answer = %s", p.body)
	}
	if _, oob, ok := p.badge("badge-all"); !ok || !oob {
		t.Errorf("refresh answer lacks the out-of-band sidebar counts")
	}
}

// waitPolled waits until feed id has been fetched more than before times
// and the outcome is stored (its next fetch is scheduled again).
func (sc *scenario) waitPolled(t *testing.T, id int64, path string, before int) {
	t.Helper()
	eventually(t, fmt.Sprintf("feed %d to be polled", id), func() bool {
		if sc.site.requests(path) <= before {
			return false
		}
		f, err := sc.a.st.GetFeed(t.Context(), id)
		return err == nil && f.NextFetchAt.After(time.Now().Add(time.Hour))
	})
}

// refreshNotModified: conditional GET (RFC 9110) with the stored ETag,
// identification headers, 304 leaves everything as it was.
func (sc *scenario) refreshNotModified(t *testing.T) {
	a, site := sc.a, sc.site
	before := site.requests(blogFeedPath)
	sc.refresh(t, "feed", sc.blogID)
	sc.waitPolled(t, sc.blogID, blogFeedPath, before)

	h := site.lastHit(t, blogFeedPath)
	if h.status != http.StatusNotModified || h.ifNoneMatch != `"posts-v1"` {
		t.Errorf("poll: status %d with If-None-Match %q, want 304 for \"posts-v1\"", h.status, h.ifNoneMatch)
	}
	if want := config.DefaultUserAgent(); h.userAgent != want {
		t.Errorf("User-Agent = %q, want %q", h.userAgent, want)
	}
	if h.acceptEncoding != "gzip" {
		t.Errorf("Accept-Encoding = %q, want gzip", h.acceptEncoding)
	}
	f, err := a.st.GetFeed(t.Context(), sc.blogID)
	if err != nil || f.ErrorCount != 0 || f.LastError != "" || f.LastFetchedAt.IsZero() {
		t.Errorf("feed after 304 = %+v, %v", f, err)
	}
	counts, err := a.st.UnreadCounts(t.Context())
	if err != nil || counts.ByFeed[sc.blogID] != blogUnreadAfterAdd {
		t.Errorf("unread after 304 = %+v, %v; want %d", counts, err, blogUnreadAfterAdd)
	}
}

// pollNewItems: new entries found on a poll are unread; existing ones get
// their content refreshed but keep read, later, favourite state and their
// position (sorted by published_at).
func (sc *scenario) pollNewItems(t *testing.T) {
	a, site := sc.a, sc.site
	site.publish(publishedLater, "Post 8 (updated)")
	before := site.requests(blogFeedPath)
	sc.refresh(t, "feed", sc.blogID)
	eventually(t, "the new posts to be stored", func() bool {
		c, err := a.st.UnreadCounts(t.Context())
		return err == nil && c.ByFeed[sc.blogID] == blogUnreadAfterPoll
	})
	if n := site.requests(blogFeedPath); n != before+1 {
		t.Errorf("feed fetched %d times by one refresh, want 1", n-before)
	}
	if h := site.lastHit(t, blogFeedPath); h.status != http.StatusOK || h.ifNoneMatch != `"posts-v1"` {
		t.Errorf("poll: status %d with If-None-Match %q", h.status, h.ifNoneMatch)
	}
	eventually(t, "the new ETag to be stored", func() bool {
		f, err := a.st.GetFeed(t.Context(), sc.blogID)
		return err == nil && f.ETag == `"posts-v2"`
	})

	all := sc.allCards(t, feedScope(sc.blogID)+"&filter=all")
	wantTitles := []string{"Post 10", "Post 9", "Post 8 (updated)", "Post 7", "Post 6", "Post 5", "Post 4", "Post 3", "Post 2", "Post 1"}
	if got := cardTitles(all); !slices.Equal(got, wantTitles) {
		t.Fatalf("cards after poll = %q, want %q", got, wantTitles)
	}
	type state struct{ read, later, fav bool }
	want := map[string]state{
		"Post 10":          {},
		"Post 9":           {},
		"Post 8 (updated)": {read: true}, // marked read by the user
		"Post 7":           {},
		"Post 6":           {},
		"Post 5":           {},
		"Post 4":           {},
		"Post 3":           {read: true, fav: true},
		"Post 2":           {read: true, later: true},
		"Post 1":           {}, // marked unread by the user
	}
	for _, c := range all {
		if got := (state{c.read, c.later, c.fav}); got != want[c.title] {
			t.Errorf("%s: state %+v, want %+v", c.title, got, want[c.title])
		}
		sc.ids[c.title] = c.id
	}
	if sc.ids["Post 8 (updated)"] != sc.ids["Post 8"] {
		t.Errorf("the edited post became a new entry")
	}
	if c := all[3]; !slices.Equal(c.tags, []string{"Go"}) {
		t.Errorf("Post 7 tags after poll = %q", c.tags)
	}
	p := a.visit(t, feedScope(sc.blogID)).expect(t, http.StatusOK)
	expectBadges(t, p, false, sc.blogBadges(blogUnreadAfterPoll))
}

// pollMoved: after a 301 the stored feed URL is updated once and later polls
// go straight to the new location.
func (sc *scenario) pollMoved(t *testing.T) {
	a, site := sc.a, sc.site
	site.moveFeed()
	before := site.requests(blogMovedPath)
	sc.refresh(t, "feed", sc.blogID)
	sc.waitPolled(t, sc.blogID, blogMovedPath, before)

	f, err := a.st.GetFeed(t.Context(), sc.blogID)
	if err != nil || f.URL != site.url(blogMovedPath) || f.ErrorCount != 0 {
		t.Fatalf("feed after a 301 = url %q errors %d, %v; want url %q", f.URL, f.ErrorCount, err, site.url(blogMovedPath))
	}
	if h := site.lastHit(t, blogFeedPath); h.status != http.StatusMovedPermanently {
		t.Errorf("old URL answered %d, want 301", h.status)
	}
	if h := site.lastHit(t, blogMovedPath); h.status != http.StatusNotModified || h.ifNoneMatch != `"posts-v2"` {
		t.Errorf("new URL: status %d with If-None-Match %q, want a conditional 304", h.status, h.ifNoneMatch)
	}

	oldBefore, newBefore := site.requests(blogFeedPath), site.requests(blogMovedPath)
	sc.refresh(t, "feed", sc.blogID)
	sc.waitPolled(t, sc.blogID, blogMovedPath, newBefore)
	if n := site.requests(blogFeedPath); n != oldBefore {
		t.Errorf("the old URL was requested again after the move")
	}
	counts, err := a.st.UnreadCounts(t.Context())
	if err != nil || counts.ByFeed[sc.blogID] != blogUnreadAfterPoll {
		t.Errorf("unread after the move = %+v, %v; want %d", counts, err, blogUnreadAfterPoll)
	}
}

// pollFailures: failures are counted, the sidebar warns after 3 consecutive
// ones, cached entries stay visible, the feed is never deleted, and a success
// clears the warning.
func (sc *scenario) pollFailures(t *testing.T) {
	a, site := sc.a, sc.site
	warned := func() bool {
		t.Helper()
		p := a.visit(t, feedScope(sc.blogID)).expect(t, http.StatusOK)
		if n := len(p.cards(t)); n != config.FeedPageSize {
			t.Errorf("cached entries not shown while the feed fails: %d cards", n)
		}
		row := findFirst(p.doc, func(n *html.Node) bool {
			return isElem(n, "a") && hasClass(n, "feed-row") && attr(n, "href") == feedScope(sc.blogID)
		})
		if row == nil {
			t.Fatalf("feed missing from the sidebar")
		}
		return findFirst(row, elemClass("span", "feed-warn")) != nil
	}
	errorCount := func(want int) {
		t.Helper()
		eventually(t, fmt.Sprintf("error_count %d", want), func() bool {
			f, err := a.st.GetFeed(t.Context(), sc.blogID)
			return err == nil && f.ErrorCount == want
		})
	}

	site.setGone(true)
	for i := 1; i <= config.WarnAfterFailures; i++ {
		sc.refresh(t, "feed", sc.blogID)
		errorCount(i)
		if got, want := warned(), i >= config.WarnAfterFailures; got != want {
			t.Errorf("after %d failures: warning shown = %v, want %v", i, got, want)
		}
	}
	f, err := a.st.GetFeed(t.Context(), sc.blogID)
	if err != nil || !strings.Contains(f.LastError, "404") {
		t.Errorf("last_error = %q, %v; want the HTTP 404", f.LastError, err)
	}
	settings := a.visit(t, fmt.Sprintf("/feeds/%d", sc.blogID)).expect(t, http.StatusOK)
	if !strings.Contains(textOf(settings.doc), "404") {
		t.Errorf("feed settings do not show the last error")
	}

	site.setGone(false)
	sc.refresh(t, "feed", sc.blogID)
	errorCount(0)
	if warned() {
		t.Errorf("warning still shown after a successful fetch")
	}
	counts, err := a.st.UnreadCounts(t.Context())
	if err != nil || counts.ByFeed[sc.blogID] != blogUnreadAfterPoll {
		t.Errorf("unread after failures = %+v, %v; want %d", counts, err, blogUnreadAfterPoll)
	}
}

// allCards loads a scope page and follows Load more to the end.
func (sc *scenario) allCards(t *testing.T, path string) []card {
	t.Helper()
	p := sc.a.visit(t, path).expect(t, http.StatusOK)
	cs := p.cards(t)
	for more := p.loadMore(); more != ""; more = p.loadMore() {
		p = sc.a.hxGet(t, more).expect(t, http.StatusOK)
		cs = append(cs, p.cards(t)...)
	}
	return cs
}

// addVideos: a feed URL pasted directly is used as-is; a feed with fewer than
// 5 entries starts all unread with no Load more.
func (sc *scenario) addVideos(t *testing.T) {
	a, site := sc.a, sc.site
	feedURL := site.url(videosFeedPath + videosFeedQuery)
	p := a.hxPost(t, "/add/resolve", url.Values{"url": {feedURL}}, "/add").expect(t, http.StatusOK)
	v := p.formValues(t, "/add/subscribe")
	if v.Get("feed_url") != feedURL || v.Get("entries") != strconv.Itoa(videoCount) || v.Get("title") != "E2E Channel" {
		t.Fatalf("confirm form = %v", v)
	}
	if !strings.Contains(textOf(p.doc), "All of them will be marked unread.") {
		t.Errorf("preview does not say all entries will be unread:\n%s", p.body)
	}
	sc.videosID = redirectedFeed(t, a.hxPost(t, "/add/subscribe", v, "/add").expect(t, http.StatusOK))

	vp := a.visit(t, feedScope(sc.videosID)).expect(t, http.StatusOK)
	cs := vp.cards(t)
	if got, want := cardTitles(cs), []string{"Video 3", "Video 2", "Video 1"}; !slices.Equal(got, want) {
		t.Fatalf("video cards = %q, want %q", got, want)
	}
	for _, c := range cs {
		if c.read {
			t.Errorf("%s stored read; with fewer than %d entries all are unread", c.title, config.InitialUnread)
		}
	}
	if more := vp.loadMore(); more != "" {
		t.Errorf("Load more offered for a feed with %d entries", videoCount)
	}
	if c := cs[0]; c.href != "https://www.youtube.com/watch?v=e2evideo0003" ||
		c.thumb != "https://i.ytimg.com/vi/e2evideo0003/hqdefault.jpg" {
		t.Errorf("video card link %q thumb %q", c.href, c.thumb)
	}
	f, err := a.st.GetFeed(t.Context(), sc.videosID)
	if err != nil || f.FolderID != 0 || f.Kind != store.KindRSS {
		t.Errorf("videos feed = folder %d kind %q, %v; want uncategorized rss (not a youtube.com URL)", f.FolderID, f.Kind, err)
	}
	expectBadges(t, vp, false, map[string]int{
		"badge-all": blogUnreadAfterPoll + videoCount, folderBadge(sc.folderID): blogUnreadAfterPoll,
		feedBadge(sc.blogID): blogUnreadAfterPoll, feedBadge(sc.videosID): videoCount,
	})
}

// addDuplicate: pasting the blog's old feed URL follows its permanent
// redirect and finds the existing subscription.
func (sc *scenario) addDuplicate(t *testing.T) {
	p := sc.a.hxPost(t, "/add/resolve", url.Values{"url": {sc.site.url(blogFeedPath)}}, "/add").expect(t, http.StatusConflict)
	if !strings.Contains(textOf(p.doc), "already subscribed") {
		t.Errorf("conflict answer = %s", p.body)
	}
	link := findFirst(p.doc, func(n *html.Node) bool { return isElem(n, "a") && attr(n, "href") == feedScope(sc.blogID) })
	if link == nil {
		t.Errorf("conflict answer does not link to the existing feed:\n%s", p.body)
	}
	if feeds, err := sc.a.st.ListFeeds(t.Context()); err != nil || len(feeds) != 2 {
		t.Errorf("feeds = %d, %v; want 2", len(feeds), err)
	}
}

// markAllRead: the scope-level "Mark all as read/watched".
func (sc *scenario) markAllRead(t *testing.T) {
	a := sc.a
	cur := feedScope(sc.videosID)
	// Submit the page's own form, as the browser does: besides the scope it
	// carries the watermark up to which the page's entries are marked read.
	form := a.visit(t, cur).expect(t, http.StatusOK).formValues(t, "/mark-read")
	scope := map[string]string{"scope": "feed", "id": strconv.FormatInt(sc.videosID, 10), "filter": "unread"}
	for name, want := range scope {
		if got := form.Get(name); got != want {
			t.Errorf("mark all as read form: %s = %q, want %q", name, got, want)
		}
	}
	p := a.hxPost(t, "/mark-read", form, cur).expect(t, http.StatusOK)
	view := byID(p.doc, "scope-view")
	if view == nil || len(p.cards(t)) != 0 || !strings.Contains(textOf(view), "all caught up") {
		t.Fatalf("mark all as read answer = %s", p.body)
	}
	expectBadges(t, p, true, map[string]int{
		"badge-all": blogUnreadAfterPoll, folderBadge(sc.folderID): blogUnreadAfterPoll,
		feedBadge(sc.blogID): blogUnreadAfterPoll, feedBadge(sc.videosID): 0,
	})
}

func (sc *scenario) crossSite(t *testing.T) {
	a := sc.a
	id := sc.ids["Post 10"]
	p := a.do(t, http.MethodPost, fmt.Sprintf("/entries/%d/read", id), url.Values{"back": {"/"}},
		requestOpts{htmx: true, site: "cross-site"})
	p.expect(t, http.StatusForbidden)
	e, err := a.st.GetEntry(t.Context(), id)
	if err != nil || e.IsRead {
		t.Errorf("entry after a rejected cross-site POST: read=%v, %v", e.IsRead, err)
	}
}

// unsubscribe: deletes the feed, its entries (later and favourite ones
// included, as the confirmation says) and their entry_tags.
func (sc *scenario) unsubscribe(t *testing.T) {
	a := sc.a
	settings := fmt.Sprintf("/feeds/%d", sc.blogID)
	cp := a.hxGet(t, settings+"/unsubscribe").expect(t, http.StatusOK)
	text := textOf(cp.doc)
	for _, want := range []string{
		fmt.Sprintf("all %d entries", initialPosts+publishedLater),
		"2 entries saved in Read later or Favourites, which will be deleted too",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("confirmation lacks %q: %s", want, text)
		}
	}

	p := a.hxPost(t, settings+"/unsubscribe", url.Values{}, settings).expect(t, http.StatusOK)
	if loc := p.header.Get("HX-Redirect"); loc != "/" {
		t.Errorf("HX-Redirect = %q, want /", loc)
	}

	a.visit(t, feedScope(sc.blogID)).expect(t, http.StatusNotFound)
	a.hxPost(t, fmt.Sprintf("/entries/%d/favourite", sc.ids["Post 3"]), url.Values{"back": {"/"}}, "/").
		expect(t, http.StatusNotFound)
	if _, err := a.st.GetFeed(t.Context(), sc.blogID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetFeed after unsubscribe: %v, want ErrNotFound", err)
	}
	for title, id := range sc.ids {
		if _, err := a.st.GetEntry(t.Context(), id); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("entry %q still stored: %v", title, err)
		}
	}
	tags, err := a.st.ListTags(t.Context())
	if err != nil || len(tags) != 1 || tags[0].Count != 0 {
		t.Errorf("tags after unsubscribe = %+v, %v; want Go with no entries", tags, err)
	}
	for _, path := range []string{"/?scope=favourites", "/?scope=readlater", fmt.Sprintf("/?scope=tag&id=%d&filter=all", sc.tagID)} {
		if cs := a.visit(t, path).expect(t, http.StatusOK).cards(t); len(cs) != 0 {
			t.Errorf("%s still lists %q", path, cardTitles(cs))
		}
	}

	home := a.visit(t, "/?filter=all").expect(t, http.StatusOK)
	if got, want := cardTitles(home.cards(t)), []string{"Video 3", "Video 2", "Video 1"}; !slices.Equal(got, want) {
		t.Errorf("All after unsubscribe = %q, want %q", got, want)
	}
	if _, _, ok := home.badge(feedBadge(sc.blogID)); ok {
		t.Errorf("the unsubscribed feed is still in the sidebar")
	}
	// Folders are kept; their count drops to zero.
	expectBadges(t, home, false, map[string]int{"badge-all": 0, folderBadge(sc.folderID): 0})
}

// notPolledAfterUnsubscribe: a refresh of everything fetches the remaining
// feed only; re-fetching unchanged entries does not make them unread again.
func (sc *scenario) notPolledAfterUnsubscribe(t *testing.T) {
	a, site := sc.a, sc.site
	blogRequests := func() int { return site.requests(blogFeedPath) + site.requests(blogMovedPath) }
	blogBefore := blogRequests()
	videosBefore := site.requests(videosFeedPath)
	sc.refresh(t, "all", 0)
	sc.waitPolled(t, sc.videosID, videosFeedPath, videosBefore)
	if n := blogRequests(); n != blogBefore {
		t.Errorf("unsubscribed feed fetched %d more times", n-blogBefore)
	}
	counts, err := a.st.UnreadCounts(t.Context())
	if err != nil || counts.All != 0 {
		t.Errorf("unread after re-fetching read entries = %+v, %v; want 0", counts, err)
	}
}

func (sc *scenario) unsubscribeLast(t *testing.T) {
	a := sc.a
	settings := fmt.Sprintf("/feeds/%d", sc.videosID)
	a.hxPost(t, settings+"/unsubscribe", url.Values{}, settings).expect(t, http.StatusOK)
	home := a.visit(t, "/").expect(t, http.StatusOK)
	if findFirst(home.doc, elemClass("a", "card-cta")) == nil {
		t.Errorf("no \"Add your first source\" card after the last unsubscribe:\n%s", home.body)
	}
}

// expectBadges checks sidebar unread badges by element id; oob requires
// them to be out-of-band swaps (htmx action responses).
func expectBadges(t *testing.T, p *page, oob bool, want map[string]int) {
	t.Helper()
	for id, n := range want {
		got, isOOB, ok := p.badge(id)
		switch {
		case !ok:
			t.Errorf("badge %s missing", id)
		case got != n:
			t.Errorf("badge %s = %d, want %d", id, got, n)
		case oob && !isOOB:
			t.Errorf("badge %s is not an out-of-band swap", id)
		}
	}
}
