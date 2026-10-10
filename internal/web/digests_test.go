package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// digestZone is the server's time zone in the digest tests, so the times
// the pages show do not depend on the machine running them.
var digestZone = time.FixedZone("TST", int(2*time.Hour/time.Second))

// sevenThirty is 07:30 as a minute of the day.
const sevenThirty = 7*minutesPerHour + minutesPerHour/2

// digestFixture is a library with a digest:
//
//	folder News: feed "Blog" (3 entries)
//	no folder:   feed "Videos" (youtube, 2 entries), feed "Other" (2 entries)
//	tag "go" on Other's newest entry
//	digest "Morning": folder News, feed Videos, tag "go"
type digestFixture struct {
	e                   *testEnv
	news                store.Folder
	blog, videos, other store.Feed
	goTag               store.Tag
	digest              store.Digest
}

func newDigestFixture(t *testing.T) *digestFixture {
	t.Helper()
	e := newTestEnv(t)
	e.srv.loc = digestZone
	ctx := context.Background()
	fx := &digestFixture{e: e}
	fx.news = e.addFolder("News")
	fx.blog = e.addFeed(store.KindRSS, "blog", "Blog", fx.news.ID, makeEntries("b", 3))
	fx.videos = e.addFeed(store.KindYouTube, "videos", "Videos", 0, makeEntries("v", 2))
	fx.other = e.addFeed(store.KindRSS, "other", "Other", 0, makeEntries("o", 2))
	var err error
	if fx.goTag, err = e.st.AddEntryTag(ctx, e.entries(fx.other.ID)[0].ID, "go"); err != nil {
		t.Fatal(err)
	}
	fx.digest = fx.addDigest("Morning", store.DigestSources{
		FolderIDs: []int64{fx.news.ID}, FeedIDs: []int64{fx.videos.ID}, TagIDs: []int64{fx.goTag.ID}})
	return fx
}

func (fx *digestFixture) addDigest(name string, src store.DigestSources) store.Digest {
	fx.e.t.Helper()
	ctx := context.Background()
	d, err := fx.e.st.CreateDigest(ctx, name)
	if err != nil {
		fx.e.t.Fatalf("create digest %s: %v", name, err)
	}
	if err := fx.e.st.SetDigestSources(ctx, d.ID, src, fx.offered(d.ID)); err != nil {
		fx.e.t.Fatalf("set sources of %s: %v", name, err)
	}
	return d
}

// offeredRE finds the fingerprint of the offered sources that the sources
// form of a digest settings page posts.
var offeredRE = regexp.MustCompile(`<input type="hidden" name="offered" value="([0-9a-f]+)">`)

// offered returns the fingerprint that digest id's settings page, rendered
// now, posts with its sources form.
func (fx *digestFixture) offered(id int64) string {
	fx.e.t.Helper()
	body := fx.e.get(digestPage(id)).Body.String()
	m := offeredRE.FindStringSubmatch(body)
	if m == nil {
		fx.e.t.Fatalf("no offered sources in the settings page:\n%s", body)
	}
	return m[1]
}

// sourcesForm is the sources form of the fixture's digest as its settings
// page, rendered now, posts it with the ids in picked.
func (fx *digestFixture) sourcesForm(picked url.Values) url.Values {
	fx.e.t.Helper()
	form := url.Values{sourceOffered: {fx.offered(fx.digest.ID)}}
	for k, v := range picked {
		form[k] = v
	}
	return form
}

func digestScope(id int64) string { return "/?scope=digest&id=" + idStr(id) }

// digestScopeAttr is digestScope as an href attribute value in a page.
func digestScopeAttr(id int64) string { return strings.ReplaceAll(digestScope(id), "&", "&amp;") }
func digestPage(id int64) string      { return "/digests/" + idStr(id) }

func (fx *digestFixture) sources() store.DigestSources {
	fx.e.t.Helper()
	src, err := fx.e.st.DigestSources(context.Background(), fx.digest.ID)
	if err != nil {
		fx.e.t.Fatal(err)
	}
	return src
}

func TestSidebarListsDigestsBetweenFavouritesAndTags(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	empty := fx.addDigest("Empty", store.DigestSources{})
	body := e.get("/").Body.String()

	// Blog 3 + Videos 2 + Other's tagged entry: all unread.
	link := `href="` + digestScopeAttr(fx.digest.ID) + `"`
	wantContains(t, body,
		`<h2 class="nav-heading">Digests</h2>`, link,
		`<span class="badge" id="badge-digest-`+idStr(fx.digest.ID)+`">6</span>`,
		`<span class="badge" id="badge-digest-`+idStr(empty.ID)+`"></span>`,
		`hx-post="/digests"`, `<label class="nav-heading" for="new-digest-name">New digest</label>`,
		`maxlength="`+strconv.Itoa(maxNameLen)+`"`,
	)
	fav := strings.Index(body, ">Favourites<")
	digests := strings.Index(body, ">Digests</h2>")
	tags := strings.Index(body, ">Tags</h2>")
	if fav < 0 || !(fav < digests && digests < tags) {
		t.Errorf("sidebar order: Favourites at %d, Digests at %d, Tags at %d", fav, digests, tags)
	}
	if strings.Index(body, ">Morning<") > strings.Index(body, ">Empty<") {
		t.Error("digests are not listed in creation order")
	}
	wantNotContains(t, body, link+` aria-current="page"`)

	page := e.get(digestScope(fx.digest.ID)).Body.String()
	wantContains(t, page, `class="nav-item digest-row is-active" `+link+` aria-current="page"`)
	settings := e.get(digestPage(fx.digest.ID)).Body.String()
	wantContains(t, settings, `class="nav-item digest-row is-active" `+link+` aria-current="page"`)

	// Without digests the section explains itself.
	wantContains(t, newTestEnv(t).get("/").Body.String(), `<h2 class="nav-heading">Digests</h2>`, "Digests combine")
}

func TestSidebarShowsADigestsIngestionTime(t *testing.T) {
	fx := newDigestFixture(t)
	if err := fx.e.st.SetDigestIngest(context.Background(), fx.digest.ID, sevenThirty, true); err != nil {
		t.Fatal(err)
	}
	wantContains(t, fx.e.get("/").Body.String(), `title="Fetched daily at 07:30"`)
}

func TestDigestScopePage(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	w := e.get(digestScope(fx.digest.ID))
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body,
		`<h1 class="page-title">Morning</h1>`, `<p class="topbar-sub">Digest</p>`,
		`<a class="btn" href="/digests/`+idStr(fx.digest.ID)+`">Digest settings</a>`,
		`<input type="hidden" name="scope" value="digest">`, `<input type="hidden" name="id" value="`+idStr(fx.digest.ID)+`">`,
		`hx-post="/mark-read"`, `id="refresh-button"`,
		`href="/?scope=digest&amp;id=`+idStr(fx.digest.ID)+`&amp;filter=all"`,
	)
	// The folder's feed, the named feed and the tagged entry; not Other's
	// untagged entry.
	if got, want := len(cardIDs(body)), 6; got != want {
		t.Errorf("cards = %d, want %d", got, want)
	}
	untagged := e.entries(fx.other.ID)[1]
	wantNotContains(t, body, `id="entry-`+idStr(untagged.ID)+`"`)

	wantStatus(t, e.get(digestScope(9999)), http.StatusNotFound)
	wantStatus(t, e.get("/?scope=digest"), http.StatusBadRequest)
	wantStatus(t, e.get("/?scope=digest&id=x"), http.StatusBadRequest)
}

func TestDigestScopeWithATime(t *testing.T) {
	fx := newDigestFixture(t)
	if err := fx.e.st.SetDigestIngest(context.Background(), fx.digest.ID, sevenThirty, true); err != nil {
		t.Fatal(err)
	}
	wantContains(t, fx.e.get(digestScope(fx.digest.ID)).Body.String(),
		`<p class="topbar-sub">Digest, fetched daily at 07:30 (TST)</p>`)
}

func TestDigestScopeWithoutSourcesSaysSo(t *testing.T) {
	fx := newDigestFixture(t)
	empty := fx.addDigest("Empty", store.DigestSources{})
	for _, filter := range []string{"", "&filter=all"} {
		body := fx.e.get(digestScope(empty.ID) + filter).Body.String()
		wantContains(t, body, "This digest has no sources yet.", "Digest settings")
		wantNotContains(t, body, "all caught up")
	}
	// Even without any subscription the digest can be set up.
	e := newTestEnv(t)
	d, err := e.st.CreateDigest(context.Background(), "Lonely")
	if err != nil {
		t.Fatal(err)
	}
	wantContains(t, e.get(digestScope(d.ID)).Body.String(), `href="/digests/`+idStr(d.ID)+`">Digest settings</a>`)
}

func TestDigestScopeLoadMoreAndMarkAllRead(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	e.addFeed(store.KindRSS, "big", "Big", fx.news.ID, makeEntries("big", config.ScopePageSize))
	all := digestScope(fx.digest.ID) + "&filter=all"
	body := e.get(all).Body.String()
	if got := len(cardIDs(body)); got != config.ScopePageSize {
		t.Fatalf("first page = %d cards, want %d", got, config.ScopePageSize)
	}
	more := loadMoreURL(body)
	if !strings.Contains(more, "scope=digest&id="+idStr(fx.digest.ID)) {
		t.Fatalf("Load more URL = %q", more)
	}
	w := e.get(more, htmx)
	wantStatus(t, w, http.StatusOK)
	if got := len(cardIDs(w.Body.String())); got == 0 {
		t.Error("Load more returned no cards")
	}

	// Mark all as read clears the digest only, up to the page's watermark.
	upTo, err := e.st.MaxEntryID(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	form := url.Values{"scope": {"digest"}, "id": {idStr(fx.digest.ID)}, "filter": {"unread"}, "up_to": {strconv.FormatInt(upTo, 10)}}
	w = e.post("/mark-read", form, htmx, withHeader("HX-Current-URL", "http://"+testHost+digestScope(fx.digest.ID)))
	wantStatus(t, w, http.StatusOK)
	badges := oobBadges(w.Body.String())
	if badges["badge-digest-"+idStr(fx.digest.ID)] != "" || badges["badge-feed-"+idStr(fx.other.ID)] != "1" {
		t.Errorf("OOB badges after mark all read = %v", badges)
	}
	if untagged := e.entries(fx.other.ID)[1]; untagged.IsRead {
		t.Error("mark all as read in the digest marked an entry outside it")
	}
}

func TestEntryActionsRefreshDigestBadgesOutOfBand(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	tagged := e.entries(fx.other.ID)[0]
	w := e.post("/entries/"+idStr(tagged.ID)+"/read", nil, htmx)
	wantStatus(t, w, http.StatusOK)
	if got := oobBadges(w.Body.String())["badge-digest-"+idStr(fx.digest.ID)]; got != "5" {
		t.Errorf("digest badge after reading its tagged entry = %q, want 5", got)
	}
}

func TestDigestRefreshFetchesEveryFeedAndIsFollowed(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	form := url.Values{"scope": {"digest"}, "id": {idStr(fx.digest.ID)}, "filter": {"unread"}, "reload": {"grid"}, "up_to": {"0"}}
	w := e.post("/refresh", form, htmx)
	wantStatus(t, w, http.StatusOK)
	if e.sched.refreshAll != 1 || len(e.sched.refreshed) != 0 {
		t.Errorf("refresh all = %d, single refreshes %v; want one refresh of every feed", e.sched.refreshAll, e.sched.refreshed)
	}
	wantContains(t, w.Body.String(), `hx-get="/refresh/status?scope=digest&amp;id=`+idStr(fx.digest.ID))

	// The checks follow every feed, as for a folder: none has been fetched
	// since a click made after they were subscribed (stored times have
	// one-second precision), so the next check is scheduled.
	since := time.Now().Add(2 * time.Second)
	status := "/refresh/status?scope=digest&id=" + idStr(fx.digest.ID) + "&filter=unread&since=" +
		strconv.FormatInt(since.Unix(), 10) + "&up_to=0&attempt=0"
	w = e.get(status, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), `id="refresh-poll"`, "attempt=1")
}

func TestDigestCreate(t *testing.T) {
	e := newTestEnv(t)
	w := e.post("/digests", url.Values{"name": {"  Morning   news "}}, htmx)
	wantStatus(t, w, http.StatusOK)
	ds, err := e.st.ListDigests(context.Background())
	if err != nil || len(ds) != 1 || ds[0].Name != "Morning news" {
		t.Fatalf("digests = %+v, %v", ds, err)
	}
	if got, want := w.Header().Get("HX-Redirect"), digestPage(ds[0].ID); got != want {
		t.Errorf("HX-Redirect = %q, want the new digest's settings %q", got, want)
	}

	for name, want := range map[string]string{
		"   ":                             "Enter a digest name.",
		strings.Repeat("x", maxNameLen+1): fmt.Sprintf("Digest names can be at most %d characters.", maxNameLen),
		"MORNING NEWS":                    "A digest with that name already exists.",
	} {
		w := e.post("/digests", url.Values{"name": {name}}, htmx)
		wantStatus(t, w, http.StatusUnprocessableEntity)
		if got := strings.TrimSpace(w.Body.String()); got != want {
			t.Errorf("create %q: inline error %q, want %q", name, got, want)
		}
	}
	wantStatus(t, e.post("/digests", url.Values{"name": {"Evening"}}), http.StatusSeeOther)
	if ds, _ := e.st.ListDigests(context.Background()); len(ds) != 2 {
		t.Errorf("digests = %d, want 2", len(ds))
	}
}

func TestDigestSettingsPage(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	ctx := context.Background()
	now := time.Now()
	for range config.WarnAfterFailures {
		if err := e.st.RecordFetchError(ctx, fx.other.ID, "HTTP 500 from the server", now, now); err != nil {
			t.Fatal(err)
		}
	}
	id := idStr(fx.digest.ID)
	w := e.get(digestPage(fx.digest.ID))
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body,
		`<h1 class="page-title">Morning</h1>`, `href="`+digestScopeAttr(fx.digest.ID)+`">View entries</a>`,
		`hx-post="/digests/`+id+`/rename"`, `value="Morning"`,
		`hx-post="/digests/`+id+`/schedule"`, `<input id="digest-ingest" name="ingest" type="time" value=""`,
		"Server time (TST)",
		`hx-post="/digests/`+id+`/sources"`,
		`name="folder" value="`+idStr(fx.news.ID)+`" checked`,
		`name="feed" value="`+idStr(fx.videos.ID)+`" checked`,
		`name="feed" value="`+idStr(fx.blog.ID)+`">`,
		`name="tag" value="`+idStr(fx.goTag.ID)+`" checked`,
		"HTTP 500 from the server", // the failing source is flagged
		`hx-get="/digests/`+id+`/delete"`,
	)
	wantNotContains(t, body, "Next ingestion")

	if err := e.st.SetDigestIngest(ctx, fx.digest.ID, sevenThirty, true); err != nil {
		t.Fatal(err)
	}
	next := sched.NextIngest(time.Now(), sevenThirty, digestZone)
	body = e.get(digestPage(fx.digest.ID)).Body.String()
	wantContains(t, body,
		`<input id="digest-ingest" name="ingest" type="time" value="07:30"`,
		"Next ingestion", next.Format(absTimeLayout),
		// Blog (through News) and Videos; the tag fetches nothing.
		"2 sources are fetched",
		`<input type="hidden" name="ingest" value="">`,
	)

	wantStatus(t, e.get("/digests/9999"), http.StatusNotFound)
	wantStatus(t, e.get("/digests/nope"), http.StatusBadRequest)
}

func TestDigestSourcesSave(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	path := digestPage(fx.digest.ID) + "/sources"
	form := fx.sourcesForm(url.Values{
		"folder": {idStr(fx.news.ID)},
		"feed":   {idStr(fx.other.ID), idStr(fx.videos.ID), idStr(fx.other.ID)},
	})
	w := e.post(path, form, htmx)
	wantStatus(t, w, http.StatusOK)
	if got := w.Header().Get("HX-Redirect"); got != digestPage(fx.digest.ID) {
		t.Errorf("HX-Redirect = %q", got)
	}
	want := store.DigestSources{FolderIDs: []int64{fx.news.ID}, FeedIDs: sortedIDs(fx.other.ID, fx.videos.ID)}
	if got := fx.sources(); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("sources = %+v, want %+v", got, want)
	}
	// No ingestion time: nothing to schedule.
	if len(e.sched.ingested) != 0 {
		t.Errorf("ScheduleIngest calls = %v, want none for a digest without a time", e.sched.ingested)
	}

	// With a time, the digest's feeds are scheduled after every save.
	if err := e.st.SetDigestIngest(context.Background(), fx.digest.ID, sevenThirty, true); err != nil {
		t.Fatal(err)
	}
	wantStatus(t, e.post(path, fx.sourcesForm(url.Values{"folder": {idStr(fx.news.ID)}, "tag": {idStr(fx.goTag.ID)}})),
		http.StatusSeeOther)
	if got, want := fmt.Sprint(e.sched.ingested), fmt.Sprint([][]int64{{fx.blog.ID}}); got != want {
		t.Errorf("ScheduleIngest calls = %s, want %s (the feeds of the folder)", got, want)
	}

	// Saving nothing clears every source.
	wantStatus(t, e.post(path, fx.sourcesForm(nil), htmx), http.StatusOK)
	if got := fx.sources(); len(got.FeedIDs)+len(got.FolderIDs)+len(got.TagIDs) != 0 {
		t.Errorf("sources after an empty save = %+v", got)
	}
}

func TestDigestSourcesValidation(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	path := digestPage(fx.digest.ID) + "/sources"
	before := fx.sources()

	w := e.post(path, fx.sourcesForm(url.Values{"feed": {idStr(fx.other.ID), "9999"}}), htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "Some of the picked sources no longer exist.")
	for _, bad := range []url.Values{{"feed": {"abc"}}, {"folder": {"-1"}}, {"tag": {"0"}}} {
		w := e.post(path, fx.sourcesForm(bad), htmx)
		wantStatus(t, w, http.StatusBadRequest)
		if w.Header().Get("HX-Retarget") != "#notice" {
			t.Errorf("%v: answer not retargeted to #notice", bad)
		}
	}
	many := fx.sourcesForm(nil)
	for i := range store.MaxDigestSources + 1 {
		many.Add("tag", strconv.Itoa(i+1))
	}
	w = e.post(path, many, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), fmt.Sprintf("at most %d sources", store.MaxDigestSources))
	if got := fx.sources(); fmt.Sprint(got) != fmt.Sprint(before) {
		t.Errorf("a rejected save changed the sources to %+v", got)
	}
	wantStatus(t, e.post("/digests/9999/sources", url.Values{}, htmx), http.StatusNotFound)
}

// staleSourcesMsg is the start of the answer to a sources form posted
// after the sources it offered changed.
const staleSourcesMsg = "The folders, sources or tags changed since this page was loaded, so nothing was saved."

// TestDigestSourcesStaleFormIsAConflict: feed, folder and tag ids are
// reused once the highest one is deleted, so a settings page left open
// while a source is deleted and another one created must not attach the
// new one. The save answers 409 with the sources form re-rendered as it is
// now (in place for htmx, as the page without JavaScript), and nothing
// changes until that form is saved.
func TestDigestSourcesStaleFormIsAConflict(t *testing.T) {
	for _, tc := range []struct {
		kind string
		// reuse deletes the fixture's source of that kind with the highest
		// id and creates another one, which gets its id and is labelled
		// label in the form.
		reuse func(t *testing.T, fx *digestFixture) (old, reused int64, label string)
	}{
		{sourceFeed, func(t *testing.T, fx *digestFixture) (int64, int64, string) {
			if err := fx.e.st.DeleteFeed(context.Background(), fx.other.ID); err != nil {
				t.Fatal(err)
			}
			f := fx.e.addFeed(store.KindRSS, "podcast", "Podcast", 0, makeEntries("p", 1))
			return fx.other.ID, f.ID, "Podcast"
		}},
		{sourceFolder, func(t *testing.T, fx *digestFixture) (int64, int64, string) {
			if err := fx.e.st.DeleteFolder(context.Background(), fx.news.ID); err != nil {
				t.Fatal(err)
			}
			return fx.news.ID, fx.e.addFolder("Podcasts").ID, "Podcasts"
		}},
		{sourceTag, func(t *testing.T, fx *digestFixture) (int64, int64, string) {
			ctx := context.Background()
			if err := fx.e.st.DeleteTag(ctx, fx.goTag.ID); err != nil {
				t.Fatal(err)
			}
			tag, err := fx.e.st.AddEntryTag(ctx, fx.e.entries(fx.blog.ID)[0].ID, "rust")
			if err != nil {
				t.Fatal(err)
			}
			return fx.goTag.ID, tag.ID, "rust"
		}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			fx := newDigestFixture(t)
			e := fx.e
			if err := e.st.SetDigestIngest(context.Background(), fx.digest.ID, sevenThirty, true); err != nil {
				t.Fatal(err)
			}
			path := digestPage(fx.digest.ID) + "/sources"
			stale := fx.sourcesForm(nil) // the settings page is rendered
			old, reused, label := tc.reuse(t, fx)
			if reused != old {
				t.Fatalf("the new %s got id %d, not the deleted one's %d", tc.kind, reused, old)
			}
			before := fx.sources()
			stale.Set(tc.kind, idStr(reused)) // the deleted source was picked

			w := e.post(path, stale, htmx)
			wantStatus(t, w, http.StatusConflict)
			if got, want := w.Header().Get("HX-Retarget")+" "+w.Header().Get("HX-Reswap"), "#digest-sources outerHTML"; got != want {
				t.Errorf("HX-Retarget and HX-Reswap = %q, want %q", got, want)
			}
			current := fx.offered(fx.digest.ID)
			body := w.Body.String()
			wantContains(t, body, `<form id="digest-sources"`, staleSourcesMsg, ">"+label+"<",
				`<input type="hidden" name="offered" value="`+current+`">`,
				`name="`+tc.kind+`" value="`+idStr(reused)+`">`) // shown, not picked
			wantNotContains(t, body, stale.Get(sourceOffered), "<html")
			checkCSPMarkup(t, "stale sources form", body)
			// Without JavaScript, the settings page with the same form.
			w = e.post(path, stale)
			wantStatus(t, w, http.StatusConflict)
			wantContains(t, w.Body.String(), `<h1 class="page-title">Morning</h1>`, staleSourcesMsg, ">"+label+"<",
				`<input type="hidden" name="offered" value="`+current+`">`)
			if got := fx.sources(); fmt.Sprint(got) != fmt.Sprint(before) {
				t.Errorf("a stale form changed the sources to %+v, want %+v", got, before)
			}
			if len(e.sched.ingested) != 0 {
				t.Errorf("ScheduleIngest calls = %v after refused saves", e.sched.ingested)
			}

			// The re-rendered form saves.
			stale.Set(sourceOffered, current)
			wantStatus(t, e.post(path, stale, htmx), http.StatusOK)
			got := fx.sources()
			picked := map[string][]int64{sourceFeed: got.FeedIDs, sourceFolder: got.FolderIDs, sourceTag: got.TagIDs}
			if !slices.Equal(picked[tc.kind], []int64{reused}) {
				t.Errorf("sources after saving the current form = %+v, want the new %s %d", got, tc.kind, reused)
			}
		})
	}
}

// TestFolderAndTagDeleteConfirmationsNameTheirDigests: deleting a folder
// or a tag also removes it from the digests that name it; the confirmation
// says which.
func TestFolderAndTagDeleteConfirmationsNameTheirDigests(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	solo, spare := e.addFolder("Solo"), e.addFolder("Spare")
	fx.addDigest("Evening", store.DigestSources{FolderIDs: []int64{fx.news.ID}, TagIDs: []int64{fx.goTag.ID}})
	night := fx.addDigest("Night", store.DigestSources{FolderIDs: []int64{solo.ID}})
	fx.addDigest("Unrelated", store.DigestSources{FeedIDs: []int64{fx.blog.ID, fx.other.ID}})

	both := "It is also removed from the digests “Morning” and “Evening”."
	for _, path := range []string{"/folders/" + idStr(fx.news.ID) + "/delete", "/tags/" + idStr(fx.goTag.ID) + "/delete"} {
		wantContains(t, e.get(path, htmx).Body.String(), both)
		wantContains(t, e.get(path).Body.String(), both) // a page without JavaScript
	}
	wantContains(t, e.get("/folders/"+idStr(solo.ID)+"/delete", htmx).Body.String(),
		"It is also removed from the digest “Night”.")
	wantNotContains(t, e.get("/folders/"+idStr(spare.ID)+"/delete", htmx).Body.String(), "digest")

	if err := e.st.RenameDigest(context.Background(), night.ID, "<b>Night</b>"); err != nil {
		t.Fatal(err)
	}
	body := e.get("/folders/"+idStr(solo.ID)+"/delete", htmx).Body.String()
	wantContains(t, body, "“&lt;b&gt;Night&lt;/b&gt;”")
	wantNotContains(t, body, "<b>Night")
}

func TestDigestScheduleSave(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	ctx := context.Background()
	path := digestPage(fx.digest.ID) + "/schedule"
	get := func() store.Digest {
		t.Helper()
		d, err := e.st.GetDigest(ctx, fx.digest.ID)
		if err != nil {
			t.Fatal(err)
		}
		return d
	}

	for in, want := range map[string]int{"07:30": sevenThirty, "00:00": 0, "23:59": store.MinutesPerDay - 1, "07:30:00": sevenThirty} {
		w := e.post(path, url.Values{"ingest": {in}}, htmx)
		wantStatus(t, w, http.StatusOK)
		if d := get(); !d.HasIngest || d.IngestMinute != want {
			t.Errorf("ingest %q stored as %+v, want minute %d", in, d, want)
		}
	}
	feeds := []int64{fx.blog.ID, fx.videos.ID}
	slices.Sort(feeds)
	if len(e.sched.ingested) != 4 || fmt.Sprint(e.sched.ingested[0]) != fmt.Sprint(feeds) {
		t.Errorf("ScheduleIngest calls = %v, want 4 with %v", e.sched.ingested, feeds)
	}
	for _, bad := range []string{"24:00", "7", "noon", "07:60", "-1:00", "07:30 PM"} {
		w := e.post(path, url.Values{"ingest": {bad}}, htmx)
		wantStatus(t, w, http.StatusUnprocessableEntity)
		wantContains(t, w.Body.String(), "Enter a time like 07:00")
	}
	wantStatus(t, e.post(path, url.Values{"ingest": {""}}), http.StatusSeeOther)
	if d := get(); d.HasIngest {
		t.Errorf("empty time kept %+v", d)
	}

	// A failed schedule is logged; the time stands.
	e.sched.ingestErr = errors.New("database is locked")
	logs := e.captureLogs()
	wantStatus(t, e.post(path, url.Values{"ingest": {"06:00"}}, htmx), http.StatusOK)
	if d := get(); d.IngestMinute != 6*minutesPerHour {
		t.Errorf("time = %+v", d)
	}
	wantContains(t, logs.String(), "database is locked")
}

func TestDigestRenameAndDelete(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	ctx := context.Background()
	base := digestPage(fx.digest.ID)
	other := fx.addDigest("Evening", store.DigestSources{})

	w := e.post(base+"/rename", url.Values{"name": {" Breakfast "}}, htmx)
	wantStatus(t, w, http.StatusOK)
	if got := w.Header().Get("HX-Redirect"); got != base {
		t.Errorf("HX-Redirect = %q", got)
	}
	if d, _ := e.st.GetDigest(ctx, fx.digest.ID); d.Name != "Breakfast" {
		t.Errorf("name = %q", d.Name)
	}
	for name, want := range map[string]string{"": "Enter a digest name.", "evening": "A digest with that name already exists."} {
		w := e.post(base+"/rename", url.Values{"name": {name}}, htmx)
		wantStatus(t, w, http.StatusUnprocessableEntity)
		if got := strings.TrimSpace(w.Body.String()); got != want {
			t.Errorf("rename to %q: %q, want %q", name, got, want)
		}
	}

	w = e.get(base+"/delete", htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), "Delete digest “Breakfast”?", "stay as they are", `action="`+base+`/delete"`)
	wantContains(t, e.get(base+"/delete").Body.String(), "Delete digest “Breakfast”?") // a page without JavaScript

	entries := len(e.entries(fx.blog.ID))
	w = e.post(base+"/delete", nil)
	wantStatus(t, w, http.StatusSeeOther)
	if _, err := e.st.GetDigest(ctx, fx.digest.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("digest after delete: %v", err)
	}
	if _, err := e.st.GetDigest(ctx, other.ID); err != nil {
		t.Errorf("other digest: %v", err)
	}
	if n := len(e.entries(fx.blog.ID)); n != entries {
		t.Errorf("entries after deleting the digest = %d, want %d", n, entries)
	}
	wantStatus(t, e.get(base), http.StatusNotFound)
	wantStatus(t, e.post(base+"/delete", nil), http.StatusNotFound)
}

// TestDigestStateChangesArePostOnly: every digest action changes state only
// on a same-origin POST.
func TestDigestStateChangesArePostOnly(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	base := digestPage(fx.digest.ID)
	forms := map[string]url.Values{
		"/digests":                               {"name": {"Night"}},
		base + "/rename":                         {"name": {"Night"}},
		base + "/schedule":                       {"ingest": {"07:00"}},
		base + "/sources":                        {},
		base + "/delete":                         {},
		"/feeds/" + idStr(fx.other.ID) + "/move": {"folder": {idStr(fx.news.ID)}},
	}
	for path, form := range forms {
		if w := e.get(path + "?" + form.Encode()); w.Code < http.StatusBadRequest && path != base+"/delete" {
			t.Errorf("GET %s = %d, want an error", path, w.Code)
		}
		w := e.post(path, form, htmx, withHeader("Sec-Fetch-Site", "cross-site"))
		wantStatus(t, w, http.StatusForbidden)
	}
	if ds, _ := e.st.ListDigests(context.Background()); len(ds) != 1 || ds[0] != fx.digest {
		t.Errorf("digests after rejected requests = %+v", ds)
	}
	if got := fx.sources(); len(got.FolderIDs) != 1 || len(got.FeedIDs) != 1 || len(got.TagIDs) != 1 {
		t.Errorf("sources after rejected requests = %+v", got)
	}
	// GET /digests/{id}/delete only asks.
	if _, err := e.st.GetDigest(context.Background(), fx.digest.ID); err != nil {
		t.Errorf("GET of the delete confirmation deleted the digest: %v", err)
	}
}

func TestDigestNamesAreEscaped(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	const hostile = `<script>alert(1)</script><img src=x onerror=alert(2)>`
	if err := e.st.RenameDigest(context.Background(), fx.digest.ID, hostile); err != nil {
		t.Fatal(err)
	}
	pages := map[string]string{
		"home":     e.get("/").Body.String(),
		"scope":    e.get(digestScope(fx.digest.ID)).Body.String(),
		"settings": e.get(digestPage(fx.digest.ID)).Body.String(),
		"confirm":  e.get(digestPage(fx.digest.ID)+"/delete", htmx).Body.String(),
	}
	for name, body := range pages {
		wantNotContains(t, body, "<script>alert", "<img src=x")
		wantContains(t, body, "&lt;script&gt;alert(1)&lt;/script&gt;")
		checkCSPMarkup(t, name, body)
	}
}

// TestFeedMoveSchedulesItsDigestIngestion: a feed moved into a folder that a
// timed digest names is scheduled for that digest's time.
func TestFeedMoveSchedulesItsDigestIngestion(t *testing.T) {
	fx := newDigestFixture(t)
	e := fx.e
	path := "/feeds/" + idStr(fx.other.ID) + "/move"
	wantStatus(t, e.post(path, url.Values{"folder": {idStr(fx.news.ID)}}, htmx), http.StatusOK)
	wantStatus(t, e.post(path, url.Values{"folder": {"0"}}, htmx), http.StatusOK)
	if got, want := fmt.Sprint(e.sched.ingested), fmt.Sprint([][]int64{{fx.other.ID}, {fx.other.ID}}); got != want {
		t.Errorf("ScheduleIngest calls = %s, want %s", got, want)
	}
	// A failed move schedules nothing.
	wantStatus(t, e.post(path, url.Values{"folder": {"9999"}}, htmx), http.StatusUnprocessableEntity)
	if len(e.sched.ingested) != 2 {
		t.Errorf("ScheduleIngest calls = %v after a rejected move", e.sched.ingested)
	}
}

func sortedIDs(ids ...int64) []int64 {
	slices.Sort(ids)
	return ids
}
