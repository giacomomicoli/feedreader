package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/store"
)

func TestFeedSettingsPageShowsFetchStatusAndForms(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fo := e.addFolder("News")
	f := e.addFeed(store.KindRSS, "blog", "Blog", fo.ID, makeEntries("b", 2))
	now := time.Now()
	if err := e.st.RecordFetchError(ctx, f.ID, "HTTP 500 from https://example.com/blog/feed.xml", now, now.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := e.st.RenameFeed(ctx, f.ID, "Renamed"); err != nil {
		t.Fatal(err)
	}

	w := e.get("/feeds/" + idStr(f.ID))
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body,
		`<h1 class="page-title">Renamed</h1>`, "The feed calls itself “Blog”.",
		`hx-post="/feeds/`+idStr(f.ID)+`/rename"`, `hx-post="/feeds/`+idStr(f.ID)+`/move"`, `hx-post="/feeds/`+idStr(f.ID)+`/interval"`,
		`<option value="`+idStr(fo.ID)+`" selected>News</option>`,
		`placeholder="`+formatInterval(config.DefaultPollInterval)+` (default)"`, "at least "+formatInterval(config.MinPollInterval),
		"Last fetch", "Next fetch", "in 1 hour", "Failures in a row", "HTTP 500 from https://example.com/blog/feed.xml",
		`href="https://example.com/blog/feed.xml" target="_blank" rel="noopener noreferrer"`,
		"Refresh now", `hx-get="/feeds/`+idStr(f.ID)+`/unsubscribe"`,
	)
	wantStatus(t, e.get("/feeds/9999"), http.StatusNotFound)
	wantStatus(t, e.get("/feeds/nope"), http.StatusBadRequest)
}

// TestFeedSettingsShowTheDefaultIntervalOfTheFeedsKind: a YouTube feed
// without an override is polled at the YouTube default, so that is the
// default its settings page offers.
func TestFeedSettingsShowTheDefaultIntervalOfTheFeedsKind(t *testing.T) {
	e := newTestEnv(t)
	blog := e.addFeed(store.KindRSS, "blog", "Blog", 0, makeEntries("b", 1))
	videos := e.addFeed(store.KindYouTube, "videos", "Videos", 0, makeEntries("v", 1))
	check := func(f store.Feed, want time.Duration) {
		t.Helper()
		w := e.get("/feeds/" + idStr(f.ID))
		wantStatus(t, w, http.StatusOK)
		wantContains(t, w.Body.String(),
			`placeholder="`+formatInterval(want)+` (default)"`, "use the default of "+formatInterval(want)+".")
	}
	check(blog, config.DefaultPollInterval)
	check(videos, config.DefaultYouTubePollInterval)

	e.srv.cfg.PollInterval = 2 * config.DefaultPollInterval
	e.srv.cfg.PollIntervalYouTube = 2 * config.DefaultYouTubePollInterval
	check(blog, e.srv.cfg.PollInterval)
	check(videos, e.srv.cfg.PollIntervalYouTube)
}

func TestFeedRenameMoveAndInterval(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	fo := e.addFolder("Later")
	f := e.addFeed(store.KindRSS, "blog", "Blog", 0, makeEntries("b", 1))
	base := "/feeds/" + idStr(f.ID)

	w := e.post(base+"/rename", url.Values{"title": {" New  name "}}, htmx)
	wantStatus(t, w, http.StatusOK)
	if w.Header().Get("HX-Redirect") != base {
		t.Errorf("HX-Redirect = %q", w.Header().Get("HX-Redirect"))
	}
	w = e.post(base+"/rename", url.Values{"title": {"   "}}, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	if strings.TrimSpace(w.Body.String()) != "Enter a title." || w.Header().Get("HX-Retarget") != "" {
		t.Errorf("inline error = %q (retarget %q)", w.Body.String(), w.Header().Get("HX-Retarget"))
	}

	wantStatus(t, e.post(base+"/move", url.Values{"folder": {idStr(fo.ID)}}, htmx), http.StatusOK)
	wantStatus(t, e.post(base+"/move", url.Values{"folder": {"9999"}}, htmx), http.StatusUnprocessableEntity)
	got, _ := e.st.GetFeed(ctx, f.ID)
	if got.Title != "New name" || got.FolderID != fo.ID {
		t.Fatalf("feed = %q folder %d", got.Title, got.FolderID)
	}
	wantStatus(t, e.post(base+"/move", url.Values{"folder": {"0"}}), http.StatusSeeOther)
	if got, _ := e.st.GetFeed(ctx, f.ID); got.FolderID != 0 {
		t.Errorf("feed not moved out of folder")
	}

	for in, want := range map[string]int{"90": 5400, "2h": 7200, "1h30m": 5400, "": 0} {
		wantStatus(t, e.post(base+"/interval", url.Values{"interval": {in}}, htmx), http.StatusOK)
		if got, _ := e.st.GetFeed(ctx, f.ID); got.IntervalSec != want {
			t.Errorf("interval %q stored as %d, want %d", in, got.IntervalSec, want)
		}
	}
	for _, bad := range []string{"1", "4m", "soon", "-5", "99999999999999999"} {
		w := e.post(base+"/interval", url.Values{"interval": {bad}}, htmx)
		if w.Code != http.StatusUnprocessableEntity {
			t.Errorf("interval %q: status %d, want 422", bad, w.Code)
		}
	}
	if !strings.Contains(e.post(base+"/interval", url.Values{"interval": {"4m"}}, htmx).Body.String(), "at least 5m") {
		t.Error("minimum interval not explained")
	}
}

// TestIntervalChangeReschedulesFeed: a shorter per-feed interval must take
// effect now, not after the schedule computed with the old one fires.
func TestIntervalChangeReschedulesFeed(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	f := e.addFeed(store.KindRSS, "blog", "Blog", 0, makeEntries("b", 1))
	base := "/feeds/" + idStr(f.ID)

	wantStatus(t, e.post(base+"/interval", url.Values{"interval": {"30m"}}, htmx), http.StatusOK)
	wantStatus(t, e.post(base+"/interval", url.Values{"interval": {""}}), http.StatusSeeOther) // back to the default
	wantStatus(t, e.post(base+"/interval", url.Values{"interval": {"4m"}}, htmx), http.StatusUnprocessableEntity)
	// Rescheduled after each stored change, with the new interval in place.
	if got, want := fmt.Sprint(e.sched.rescheduled), fmt.Sprint([]int64{f.ID, f.ID}); got != want {
		t.Errorf("rescheduled = %s, want %s", got, want)
	}
	if got := fmt.Sprint(e.sched.rescheduleSec); got != "[1800 0]" {
		t.Errorf("interval seen by Reschedule = %s, want [1800 0]", got)
	}

	// A failed reschedule is logged; the stored interval stands and the
	// next poll uses it.
	e.sched.rescheduleErr = errors.New("database is locked")
	logs := e.captureLogs()
	wantStatus(t, e.post(base+"/interval", url.Values{"interval": {"2h"}}, htmx), http.StatusOK)
	if got, _ := e.st.GetFeed(ctx, f.ID); got.IntervalSec != 7200 {
		t.Errorf("interval = %d, want 7200", got.IntervalSec)
	}
	wantContains(t, logs.String(), "could not reschedule feed", "database is locked")
}

func TestUnsubscribeConfirmationStatesSavedEntriesAndDeletesEverything(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	f := e.addFeed(store.KindRSS, "blog", "Blog", 0, makeEntries("b", 8))
	all := e.entries(f.ID)
	if err := e.st.SetLater(ctx, all[0].ID, true); err != nil {
		t.Fatal(err)
	}
	if err := e.st.SetFavourite(ctx, all[1].ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := e.st.AddEntryTag(ctx, all[2].ID, "keep"); err != nil {
		t.Fatal(err)
	}
	path := "/feeds/" + idStr(f.ID) + "/unsubscribe"

	// Step 1 (htmx): inline confirmation with the counts.
	w := e.get(path, htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body, "Unsubscribe from “Blog”?", "all 8 entries",
		"That includes 2 entries saved in Read later or Favourites, which will be deleted too.",
		`<form method="post" action="`+path+`"`, "Cancel")
	wantNotContains(t, body, "confirm(", "<script")
	if _, err := e.st.GetFeed(ctx, f.ID); err != nil {
		t.Fatal("the first step must not delete anything")
	}
	// Step 1 without JS is a full page.
	wantContains(t, e.get(path).Body.String(), "<!doctype html>", "all 8 entries")

	// Step 2.
	w = e.post(path, nil)
	wantStatus(t, w, http.StatusSeeOther)
	if _, err := e.st.GetFeed(ctx, f.ID); err == nil {
		t.Fatal("feed still exists")
	}
	if _, err := e.st.GetEntry(ctx, all[0].ID); err == nil {
		t.Error("saved entry survived unsubscribe")
	}
	wantStatus(t, e.post(path, nil, htmx), http.StatusNotFound)
}

func TestUnsubscribeConfirmationWithNothingSaved(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeed(store.KindYouTube, "yt", "Channel", 0, makeEntries("y", 1))
	body := e.get("/feeds/"+idStr(f.ID)+"/unsubscribe", htmx).Body.String()
	wantContains(t, body, "all 1 entry", "None of them are in Watch later or Favourites.")
}

func TestFolderCreateRenameDelete(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()

	w := e.post("/folders", url.Values{"name": {"Tech  newsletters"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	folders, _ := e.st.ListFolders(ctx)
	if len(folders) != 1 || folders[0].Name != "Tech newsletters" {
		t.Fatalf("folders = %+v", folders)
	}
	fo := folders[0]
	if w.Header().Get("HX-Redirect") != "/?scope=folder&id="+idStr(fo.ID) {
		t.Errorf("HX-Redirect = %q", w.Header().Get("HX-Redirect"))
	}
	w = e.post("/folders", url.Values{"name": {"tech newsletters"}}, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "A folder with that name already exists.")
	wantStatus(t, e.post("/folders", url.Values{"name": {""}}, htmx), http.StatusUnprocessableEntity)
	// Without htmx a validation error is a full page.
	plain := e.post("/folders", url.Values{"name": {""}})
	wantStatus(t, plain, http.StatusUnprocessableEntity)
	wantContains(t, plain.Body.String(), "<!doctype html>", "Enter a folder name.")

	// Folder scope top bar offers rename and delete.
	f := e.addFeed(store.KindRSS, "a", "A", fo.ID, makeEntries("a", 2))
	page := e.get("/?scope=folder&id=" + idStr(fo.ID)).Body.String()
	wantContains(t, page, `hx-post="/folders/`+idStr(fo.ID)+`/rename"`, `hx-get="/folders/`+idStr(fo.ID)+`/delete"`, ">Delete folder<")

	wantStatus(t, e.post("/folders/"+idStr(fo.ID)+"/rename", url.Values{"name": {"Reading"}}, htmx), http.StatusOK)
	if got, _ := e.st.GetFolder(ctx, fo.ID); got.Name != "Reading" {
		t.Errorf("name = %q", got.Name)
	}
	other := e.addFolder("Other")
	wantStatus(t, e.post("/folders/"+idStr(other.ID)+"/rename", url.Values{"name": {"reading"}}, htmx), http.StatusUnprocessableEntity)
	wantStatus(t, e.post("/folders/9999/rename", url.Values{"name": {"x"}}, htmx), http.StatusNotFound)

	confirm := e.get("/folders/"+idStr(fo.ID)+"/delete", htmx)
	wantStatus(t, confirm, http.StatusOK)
	wantContains(t, confirm.Body.String(), "Delete folder “Reading”?", "Its feed stays subscribed",
		`action="/folders/`+idStr(fo.ID)+`/delete"`)

	w = e.post("/folders/"+idStr(fo.ID)+"/delete", nil)
	wantStatus(t, w, http.StatusSeeOther)
	if _, err := e.st.GetFolder(ctx, fo.ID); err == nil {
		t.Fatal("folder not deleted")
	}
	if got, err := e.st.GetFeed(ctx, f.ID); err != nil || got.FolderID != 0 {
		t.Errorf("feed lost or still in folder: %v %+v", err, got)
	}
}

func TestTagDeleteRemovesTagFromEntriesOnly(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	f := e.addFeed(store.KindRSS, "a", "A", 0, makeEntries("a", 3))
	all := e.entries(f.ID)
	var tag store.Tag
	for _, ev := range all[:2] {
		var err error
		if tag, err = e.st.AddEntryTag(ctx, ev.ID, "later-reading"); err != nil {
			t.Fatal(err)
		}
	}
	page := e.get("/?scope=tag&id=" + idStr(tag.ID)).Body.String()
	if got := len(cardIDs(page)); got != 2 {
		t.Errorf("tag scope cards = %d, want 2", got)
	}
	wantContains(t, page, `hx-get="/tags/`+idStr(tag.ID)+`/delete"`)

	confirm := e.get("/tags/" + idStr(tag.ID) + "/delete").Body.String()
	wantContains(t, confirm, "Delete tag “later-reading”?", "It is removed from 2 entries. The entries themselves are kept.")

	w := e.post("/tags/"+idStr(tag.ID)+"/delete", nil, htmx)
	wantStatus(t, w, http.StatusOK)
	if w.Header().Get("HX-Redirect") != "/" {
		t.Errorf("HX-Redirect = %q", w.Header().Get("HX-Redirect"))
	}
	if _, err := e.st.GetTag(ctx, tag.ID); err == nil {
		t.Fatal("tag not deleted")
	}
	if got := len(e.entries(f.ID)); got != 3 {
		t.Errorf("entries = %d, want 3 kept", got)
	}
	wantStatus(t, e.get("/tags/"+idStr(tag.ID)+"/delete"), http.StatusNotFound)
}

// A folder created before any subscription, or a tag left behind after its
// entries' feed was unsubscribed, must still be renamable and deletable: the
// empty state replaces the grid, not the folder and tag actions.
func TestFolderAndTagActionsAvailableWithoutFeeds(t *testing.T) {
	e := newTestEnv(t)
	ctx := t.Context()

	fo, err := e.st.CreateFolder(ctx, "Early")
	if err != nil {
		t.Fatal(err)
	}
	page := e.get("/?scope=folder&id=" + idStr(fo.ID)).Body.String()
	wantContains(t, page, "Add your first source",
		`hx-post="/folders/`+idStr(fo.ID)+`/rename"`, `hx-get="/folders/`+idStr(fo.ID)+`/delete"`)
	wantNotContains(t, page, "Mark all as read")

	f := e.addFeed(store.KindRSS, "a", "A", 0, makeEntries("a", 1))
	pg, err := e.st.ListEntries(ctx, store.ListQuery{Scope: store.Scope{Kind: store.ScopeAll}, Limit: 1})
	if err != nil || len(pg.Entries) != 1 {
		t.Fatalf("list entries: %v (%d)", err, len(pg.Entries))
	}
	tag, err := e.st.AddEntryTag(ctx, pg.Entries[0].ID, "orphan")
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.DeleteFeed(ctx, f.ID); err != nil {
		t.Fatal(err)
	}
	page = e.get("/?scope=tag&id=" + idStr(tag.ID)).Body.String()
	wantContains(t, page, `hx-get="/tags/`+idStr(tag.ID)+`/delete"`)
}
