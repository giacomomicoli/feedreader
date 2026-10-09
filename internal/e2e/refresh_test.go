package e2e

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/html"

	"github.com/giacomomicoli/feedreader/internal/config"
)

// TestRefresh_ReloadsTheGridWhenDone: Refresh on the card grid fetches every
// source in the background; the page then checks, as htmx does, until those
// fetches are done, and the reloaded grid shows the new entries. The blog
// fetch is held until a check has said it is still pending, so the checks
// must follow the real scheduler and store.
func TestRefresh_ReloadsTheGridWhenDone(t *testing.T) {
	site := newFeedSite(t)
	a := startApp(t)
	blogID := a.subscribe(t, site.url(blogFeedPath))
	videosID := a.subscribe(t, site.url(videosFeedPath+videosFeedQuery))
	// Click in a later second than the subscriptions: feeds subscribed in
	// the second of the click are not waited for.
	a.waitPastCreation(t, blogID, videosID)
	site.publish(publishedLater, "Post 8 (updated)")
	release := site.holdBlogFeed(t)
	blogBefore, videosBefore := site.requests(blogFeedPath), site.requests(videosFeedPath)

	home := a.visit(t, "/").expect(t, http.StatusOK)
	if idle := byID(home.doc, "refresh-poll"); idle == nil || hasAttr(idle, "hx-get") {
		t.Fatalf("the page lacks the idle refresh check:\n%s", home.body)
	}
	p := a.hxPost(t, "/refresh", home.formValues(t, "/refresh"), "/").expect(t, http.StatusOK)
	if !strings.Contains(textOf(p.doc), "Refresh started") {
		t.Errorf("refresh answer = %s", p.body)
	}
	check := pendingCheck(p)
	if check == nil || attr(check, "hx-swap-oob") != "true" {
		t.Fatalf("the refresh answer starts no check:\n%s", p.body)
	}

	// Follow the checks like htmx: wait the delay, ask, swap the answer in
	// place of the check. The blog fetch goes on once a check is pending.
	pending := 0
	for n := 1; check != nil; n++ {
		if n > config.RefreshMaxChecks {
			t.Fatalf("more than %d checks", config.RefreshMaxChecks)
		}
		trigger := attr(check, "hx-trigger")
		delay, err := time.ParseDuration(strings.TrimPrefix(trigger, "load delay:"))
		if err != nil || !strings.HasPrefix(trigger, "load delay:") {
			t.Fatalf("check trigger %q: %v", trigger, err)
		}
		time.Sleep(delay)
		p = a.do(t, http.MethodGet, attr(check, "hx-get"), nil, requestOpts{htmx: true, current: "/"}).
			expect(t, http.StatusOK)
		if check = pendingCheck(p); check != nil {
			pending++
			release()
		}
	}
	if pending == 0 {
		t.Fatal("the first check says the refresh is done while the blog fetch is held")
	}

	if idle := byID(p.doc, "refresh-poll"); idle == nil || hasAttr(idle, "hx-swap-oob") {
		t.Errorf("the last check does not leave the idle check in its place:\n%s", p.body)
	}
	if n := byID(p.doc, "notice"); n == nil || attr(n, "hx-swap-oob") != "innerHTML" || textOf(n) != "Refresh done." {
		t.Errorf("the last check does not say the refresh is done:\n%s", p.body)
	}
	if v := byID(p.doc, "scope-view"); v == nil || attr(v, "hx-swap-oob") != "true" {
		t.Fatalf("the last check does not reload the grid:\n%s", p.body)
	}
	cs := p.cards(t)
	blogUnread := config.InitialUnread + publishedLater // the newest posts, then the new ones
	unread := blogUnread + videoCount
	if want := []string{"Post 10", "Post 9", "Post 8 (updated)"}; len(cs) != unread || !slices.Equal(cardTitles(cs)[:len(want)], want) {
		t.Errorf("reloaded grid = %q, want %d unread cards starting with %q", cardTitles(cs), unread, want)
	}
	expectBadges(t, p, true, map[string]int{
		"badge-all": unread, feedBadge(blogID): blogUnread, feedBadge(videosID): videoCount,
	})
	if n := site.requests(blogFeedPath) - blogBefore; n != 1 {
		t.Errorf("blog fetched %d times by one refresh, want 1", n)
	}
	if n := site.requests(videosFeedPath) - videosBefore; n != 1 {
		t.Errorf("videos fetched %d times by one refresh, want 1", n)
	}
}

// subscribe adds the feed at feedURL through the add flow, without a folder,
// and returns its id.
func (a *app) subscribe(t *testing.T, feedURL string) int64 {
	t.Helper()
	p := a.hxPost(t, "/add/resolve", url.Values{"url": {feedURL}}, "/add").expect(t, http.StatusOK)
	form := p.formValues(t, "/add/subscribe")
	return redirectedFeed(t, a.hxPost(t, "/add/subscribe", form, "/add").expect(t, http.StatusOK))
}

// waitPastCreation sleeps until the second after the latest creation time
// of the given feeds (stored times have one-second precision).
func (a *app) waitPastCreation(t *testing.T, ids ...int64) {
	t.Helper()
	var latest int64
	for _, id := range ids {
		f, err := a.st.GetFeed(t.Context(), id)
		if err != nil {
			t.Fatalf("get feed %d: %v", id, err)
		}
		latest = max(latest, f.CreatedAt.Unix())
	}
	time.Sleep(time.Until(time.Unix(latest+1, 0)))
}

// pendingCheck returns the refresh check a response schedules, or nil.
func pendingCheck(p *page) *html.Node {
	c := byID(p.doc, "refresh-poll")
	if c == nil || attr(c, "hx-get") == "" {
		return nil
	}
	return c
}
