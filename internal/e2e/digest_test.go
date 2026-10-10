package e2e

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/sched"
)

// TestDigest_CombinesSourcesAndIngestsAtItsTime drives a digest through the
// UI: created from the sidebar, given a source on its settings page and a
// daily ingestion time, read and marked read in its own scope, and deleted
// without touching the entries.
func TestDigest_CombinesSourcesAndIngestsAtItsTime(t *testing.T) {
	site := newFeedSite(t)
	a := startApp(t)
	subscribe := func(feedURL string) int64 {
		t.Helper()
		p := a.hxPost(t, "/add/resolve", url.Values{"url": {feedURL}}, "/add").expect(t, http.StatusOK)
		return redirectedFeed(t, a.hxPost(t, "/add/subscribe", p.formValues(t, "/add/subscribe"), "/add").expect(t, http.StatusOK))
	}
	blogID := subscribe(site.url(blogFeedPath))
	videosID := subscribe(site.url(videosFeedPath + videosFeedQuery))
	requests := site.total()

	// Created from the sidebar form, it opens its settings.
	p := a.hxPost(t, "/digests", url.Values{"name": {"Morning"}}, "/").expect(t, http.StatusOK)
	settings := p.header.Get("HX-Redirect")
	id, err := strconv.ParseInt(strings.TrimPrefix(settings, "/digests/"), 10, 64)
	if err != nil || id <= 0 {
		t.Fatalf("HX-Redirect = %q, want the new digest's settings", settings)
	}
	scope := "/?scope=digest&id=" + strconv.FormatInt(id, 10)
	badge := "badge-digest-" + strconv.FormatInt(id, 10)

	// Pick the blog as its only source, as the settings form does. The form
	// posts back what it offered, nothing else yet.
	form := a.visit(t, settings).expect(t, http.StatusOK).formValues(t, settings+"/sources")
	if len(form) != 1 || len(form["offered"]) != 1 {
		t.Fatalf("a new digest's sources form posts %v, want only what it offered", form)
	}
	form.Add("feed", strconv.FormatInt(blogID, 10))
	p = a.hxPost(t, settings+"/sources", form, settings).expect(t, http.StatusOK)
	if got := p.header.Get("HX-Redirect"); got != settings {
		t.Errorf("HX-Redirect after saving sources = %q, want %q", got, settings)
	}

	// Its scope lists the blog's unread entries only, with their badge.
	page := a.visit(t, scope).expect(t, http.StatusOK)
	if got, want := cardTitles(page.cards(t)), postTitles(initialPosts, initialPosts-config.InitialUnread+1); !slices.Equal(got, want) {
		t.Errorf("digest cards = %q, want %q", got, want)
	}
	expectBadges(t, page, false, map[string]int{badge: config.InitialUnread})

	// A daily time a couple of hours ahead, before the blog's regular poll:
	// its next fetch moves to that time; the videos are left alone.
	now := time.Now()
	at := now.Add(2 * time.Hour)
	minute := at.Hour()*int(time.Hour/time.Minute) + at.Minute()
	clock := at.Format("15:04")
	a.hxPost(t, settings+"/schedule", url.Values{"ingest": {clock}}, settings).expect(t, http.StatusOK)
	want := sched.NextIngest(now, minute, time.Local)
	blog, err := a.st.GetFeed(t.Context(), blogID)
	if err != nil || !blog.NextFetchAt.Equal(want) {
		t.Errorf("blog's next fetch = %s, %v; want the digest time %s", blog.NextFetchAt, err, want)
	}
	if videos, err := a.st.GetFeed(t.Context(), videosID); err != nil || videos.NextFetchAt.Equal(want) {
		t.Errorf("videos' next fetch = %s, %v; not in the digest, it keeps its own", videos.NextFetchAt, err)
	}
	sp := a.visit(t, settings).expect(t, http.StatusOK)
	if in := byID(sp.doc, "digest-ingest"); in == nil || attr(in, "value") != clock {
		t.Errorf("settings page does not show the time %s", clock)
	}
	if !strings.Contains(textOf(sp.doc), "1 source is fetched at that time") {
		t.Errorf("settings page does not say what is fetched:\n%s", sp.body)
	}

	// Mark all as read with the page's own form: the badge clears out of band.
	read := a.hxPost(t, "/mark-read", page.formValues(t, "/mark-read"), scope).expect(t, http.StatusOK)
	expectBadges(t, read, true, map[string]int{badge: 0, feedBadge(videosID): videoCount})

	// Deleting the digest keeps its source and the entries.
	a.hxPost(t, settings+"/delete", url.Values{}, settings).expect(t, http.StatusOK)
	a.visit(t, scope).expect(t, http.StatusNotFound)
	if n := len(a.visit(t, feedScope(blogID)+"&filter=all").expect(t, http.StatusOK).cards(t)); n != config.FeedPageSize {
		t.Errorf("blog cards after deleting the digest = %d, want %d", n, config.FeedPageSize)
	}
	// None of this fetched anything.
	if n := site.total(); n != requests {
		t.Errorf("feed site requests = %d, want %d", n, requests)
	}
}
