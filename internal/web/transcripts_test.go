package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// serverTranscripts sets FR_TRANSCRIPTS for the env's server and scheduler.
func (e *testEnv) serverTranscripts(on bool) {
	cfg := config.Default()
	cfg.Transcripts = on
	e.srv.cfg = cfg
	e.sched.Scheduler = sched.New(e.st, nil, cfg, nil)
}

// videoFeed subscribes a YouTube feed with one video (stored by the initial
// add, so its transcript was never queued) and returns the feed and entry.
func (e *testEnv) videoFeed() (store.Feed, int64) {
	e.t.Helper()
	return e.summaryEntry(store.KindYouTube, "Synthetic Channel", "https://www.youtube.com/watch?v=abcdefghijk", videoDescription)
}

func TestNewTranscriptStatus(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, loc)
	on := sched.TranscriptSwitches{Server: true, Global: true, Source: true}
	later := now.Add(2 * time.Hour)
	tomorrow := now.Add(20 * time.Hour)
	for _, tc := range []struct {
		name   string
		st     store.TranscriptState
		sw     sched.TranscriptSwitches
		text   string
		button string
	}{
		{"never queued", store.TranscriptState{}, on, "Transcript not requested.", "Fetch transcript"},
		{"queued, due", store.TranscriptState{Status: store.StatusPending, DueAt: now}, on, "Queued; fetching soon.", ""},
		{"queued, later", store.TranscriptState{Status: store.StatusPending, DueAt: later}, on,
			"Queued; next check about 14:00.", "Check now"},
		{"no captions yet", store.TranscriptState{Status: store.StatusPending, Reason: store.ReasonNoCaptions, Rechecked: true, DueAt: later},
			on, "No captions yet; checking again about 14:00.", "Check now"},
		{"not aired", store.TranscriptState{Status: store.StatusPending, Reason: store.ReasonNotReady, DueAt: tomorrow},
			on, "Not aired yet; next check about Fri 08:00.", "Check now"},
		{"error", store.TranscriptState{Status: store.StatusPending, Reason: store.ReasonError, Attempts: 1, DueAt: later},
			on, "Could not be fetched; try 2 of 3 about 14:00.", "Check now"},
		{"unavailable", store.TranscriptState{Status: store.StatusPending, Reason: store.ReasonUnavailable, Attempts: 2, DueAt: later},
			on, "YouTube would not provide it; try 3 of 3 about 14:00.", "Check now"},
		{"paused", store.TranscriptState{Status: store.StatusPending, Reason: store.ReasonRefused, DueAt: later, PausedUntil: later},
			on, "YouTube is limiting requests; transcripts resume about 14:00.", "Check now"},
		{"paused, refused elsewhere", store.TranscriptState{Status: store.StatusPending, DueAt: now, PausedUntil: later},
			on, "YouTube is limiting requests; transcripts resume about 14:00.", "Check now"},
		{"no captions", store.TranscriptState{Status: store.StatusNoCaptions, Reason: store.ReasonNoCaptions}, on,
			"No captions found (checked twice).", "Check again"},
		{"too large", store.TranscriptState{Status: store.StatusFailed, Reason: store.ReasonTooLarge, Attempts: 1}, on,
			"Too long to fetch; trying again will most likely fail the same way.", "Try again"},
		{"gave up", store.TranscriptState{Status: store.StatusFailed, Reason: store.ReasonExhausted, Attempts: 3}, on,
			"Could not be fetched (tried 3 times).", "Try again"},
		{"unavailable, gave up", store.TranscriptState{Status: store.StatusFailed, Reason: store.ReasonUnavailable, Attempts: 3}, on,
			"YouTube would not provide it (tried 3 times).", "Try again"},
		{"queued too long", store.TranscriptState{Status: store.StatusFailed, Reason: store.ReasonQueuedLong}, on,
			"Gave up after 7 days in the queue.", "Try again"},
		{"not a video", store.TranscriptState{Status: store.StatusFailed, Reason: store.ReasonNotAVideo}, on, "Not a video.", ""},
		{"fetched", store.TranscriptState{Status: store.StatusOK}, sched.TranscriptSwitches{}, "Transcript fetched.", ""},
		{"server off", store.TranscriptState{}, sched.TranscriptSwitches{Global: true, Source: true},
			"Transcripts are disabled on this server (FR_TRANSCRIPTS=false).", ""},
		{"global off", store.TranscriptState{}, sched.TranscriptSwitches{Server: true, Source: true},
			"Video transcripts are off; turn them on at the bottom of the sidebar.", ""},
		{"source off, queued", store.TranscriptState{Status: store.StatusPending, DueAt: now},
			sched.TranscriptSwitches{Server: true, Global: true},
			"Transcripts are off for this source; turn them on in its feed settings. Its transcript stays queued until then.", ""},
	} {
		v := newTranscriptStatus(1, tc.st, tc.sw, now, loc)
		if v.Text != tc.text || v.Button != tc.button {
			t.Errorf("%s: %q [%s], want %q [%s]", tc.name, v.Text, v.Button, tc.text, tc.button)
		}
	}
}

// TestTranscriptFetchButton: the drawer of a video offers "Fetch
// transcript" while every switch is on; the button queues it (status line
// back, plus the sidebar counts), works without JavaScript (303 back to the
// entry), and is refused for what the rules do not allow.
func TestTranscriptFetchButton(t *testing.T) {
	e := newTestEnv(t)
	f, id := e.videoFeed()
	path := "/entries/" + idStr(id)

	// Off globally (the default): no button, and the POST is refused.
	body := e.get(path, htmx).Body.String()
	wantContains(t, body, "Video transcripts are off; turn them on at the bottom of the sidebar.")
	wantNotContains(t, body, "transcript/fetch")
	w := e.post(path+"/transcript/fetch", nil, htmx)
	wantStatus(t, w, http.StatusConflict)
	wantContains(t, w.Body.String(), "Not queued: transcripts are off for this entry.")
	if st, _ := e.st.TranscriptStateOf(context.Background(), id); st.Status != store.StatusNone {
		t.Fatalf("a refused request queued the transcript: %+v", st)
	}

	e.transcriptsOn()
	body = e.get(path, htmx).Body.String()
	form := `<form method="post" action="` + path + `/transcript/fetch" hx-post="` + path + `/transcript/fetch" hx-target="#transcript-status-` +
		idStr(id) + `" hx-swap="outerHTML">`
	wantContains(t, body, "Transcript not requested.", form, ">Fetch transcript</button>")

	w = e.post(path+"/transcript/fetch", nil, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), `<div class="transcript-status" id="transcript-status-`+idStr(id)+`" role="status" tabindex="-1">`,
		"Queued; fetching soon.", `hx-swap-oob="true"`)
	checkCSPMarkup(t, "status", w.Body.String())
	if st, _ := e.st.TranscriptStateOf(context.Background(), id); st.Status != store.StatusPending {
		t.Fatalf("state after the button = %+v, want pending", st)
	}
	// Without JavaScript: back to the entry page.
	w = e.post(path+"/transcript/fetch", nil)
	wantStatus(t, w, http.StatusSeeOther)
	if loc := w.Header().Get("Location"); loc != path {
		t.Errorf("Location = %q, want %q", loc, path)
	}

	// Off for the source: refused.
	mustSet(t, e.sched.SetFeedTranscripts(context.Background(), f.ID, false))
	wantStatus(t, e.post(path+"/transcript/fetch", nil, htmx), http.StatusConflict)
	mustSet(t, e.sched.SetFeedTranscripts(context.Background(), f.ID, true))

	// An article is not a video; a fetched transcript is not fetched again.
	_, post := e.summaryEntry(store.KindRSS, "Synthetic Blog", "https://www.youtube.com/watch?v=abcdefghijk", videoDescription)
	w = e.post("/entries/"+idStr(post)+"/transcript/fetch", nil, htmx)
	wantStatus(t, w, http.StatusConflict)
	wantContains(t, w.Body.String(), "Not queued: this entry is not a YouTube video.")
	e2 := newTestEnv(t)
	done := e2.polledVideo(store.TranscriptResult{Outcome: store.TranscriptOK, Transcript: store.Transcript{Text: "x"}})
	w = e2.post("/entries/"+idStr(done)+"/transcript/fetch", nil, htmx)
	wantStatus(t, w, http.StatusConflict)
	wantContains(t, w.Body.String(), "Not queued: the transcript is already fetched.")
	wantStatus(t, e.post("/entries/424242/transcript/fetch", nil, htmx), http.StatusNotFound)

	// A cross-site POST never gets through.
	wantStatus(t, e.post(path+"/transcript/fetch", nil, htmx, withHeader("Sec-Fetch-Site", "cross-site")), http.StatusForbidden)
}

func mustSet(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// TestTranscriptsSettingInTheSidebar: the global switch sits at the bottom
// of the sidebar, off by default; saving it answers with the switch and the
// counts (htmx) or a 303 home; with FR_TRANSCRIPTS=false it is disabled
// and cannot be turned on.
func TestTranscriptsSettingInTheSidebar(t *testing.T) {
	e := newTestEnv(t)
	body := e.get("/").Body.String()
	wantContains(t, body,
		`<form class="setting-switch" id="transcripts-setting" method="post" action="/settings/transcripts" hx-post="/settings/transcripts" hx-target="this" hx-swap="outerHTML">`,
		`<input type="checkbox" name="transcripts" value="1" aria-describedby="transcripts-note">`,
		"Off: no transcript is requested.", ">Save</button>")
	if i, j := strings.Index(body, `id="new-digest-name"`), strings.Index(body, `id="transcripts-setting"`); j < i {
		t.Error("the transcript switch is not below the New digest form")
	}
	checkCSPMarkup(t, "home", body)

	w := e.post("/settings/transcripts", url.Values{"transcripts": {"1"}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), `value="1" checked`, "Saved. On: new videos get their transcript", `hx-swap-oob="true"`)
	if on, _ := e.st.TranscriptsSetting(context.Background()); !on {
		t.Fatal("the setting is still off")
	}
	w = e.post("/settings/transcripts", url.Values{}, htmx) // unchecked: no field
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), "Saved. Off: no transcript is requested.")
	if on, _ := e.st.TranscriptsSetting(context.Background()); on {
		t.Fatal("the setting is still on")
	}
	w = e.post("/settings/transcripts", url.Values{"transcripts": {"1"}})
	wantStatus(t, w, http.StatusSeeOther)
	if on, _ := e.st.TranscriptsSetting(context.Background()); !on {
		t.Fatal("a form post without JavaScript did not turn it on")
	}
	wantStatus(t, e.post("/settings/transcripts", url.Values{"transcripts": {"0"}}, htmx, withHeader("Sec-Fetch-Site", "cross-site")),
		http.StatusForbidden)
	if on, _ := e.st.TranscriptsSetting(context.Background()); !on {
		t.Fatal("a cross-site POST changed the setting")
	}

	// FR_TRANSCRIPTS=false: disabled, and turning it on is refused.
	e.serverTranscripts(false)
	mustSet(t, e.st.SetTranscriptsSetting(context.Background(), false, time.Now()))
	body = e.get("/").Body.String()
	wantContains(t, body, `value="1" disabled aria-describedby="transcripts-note"`,
		"Disabled on this server (FR_TRANSCRIPTS=false).")
	wantNotContains(t, body[strings.Index(body, `id="transcripts-setting"`):], ">Save</button>")
	w = e.post("/settings/transcripts", url.Values{"transcripts": {"1"}}, htmx)
	wantStatus(t, w, http.StatusConflict)
	wantContains(t, w.Body.String(), "Transcripts are disabled on this server.")
	if on, _ := e.st.TranscriptsSetting(context.Background()); on {
		t.Fatal("turned on although FR_TRANSCRIPTS=false")
	}
}

// TestFeedTranscriptsPanel: a YouTube source's settings have its own
// transcript switch, with a line saying whether its transcripts are really
// fetched and why not; other sources have none.
func TestFeedTranscriptsPanel(t *testing.T) {
	e := newTestEnv(t)
	f, _ := e.videoFeed()
	blog := e.addFeed(store.KindRSS, "blog", "Blog", 0, makeEntries("blog", 1))
	path := "/feeds/" + idStr(f.ID)

	body := e.get(path).Body.String()
	wantContains(t, body, `<section class="panel" id="feed-transcripts">`,
		`hx-post="`+path+`/transcripts" hx-target="#feed-transcripts" hx-swap="outerHTML"`,
		`<input type="checkbox" name="transcripts" value="1" checked aria-describedby="feed-transcripts-note">`,
		"On for this source, but video transcripts are off globally (bottom of the sidebar).")
	checkCSPMarkup(t, "feed settings", body)
	wantNotContains(t, e.get("/feeds/"+idStr(blog.ID)).Body.String(), "feed-transcripts")

	e.transcriptsOn()
	wantContains(t, e.get(path).Body.String(), "On: new videos of this source get their transcript.")
	w := e.post(path+"/transcripts", url.Values{}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantContains(t, w.Body.String(), `value="1" aria-describedby="feed-transcripts-note"`,
		"Saved. Off for this source: its videos get no transcript.", `hx-swap-oob="true"`)
	if on, _ := e.st.FeedTranscripts(context.Background(), f.ID); on {
		t.Fatal("the source's switch is still on")
	}
	w = e.post(path+"/transcripts", url.Values{"transcripts": {"1"}})
	wantStatus(t, w, http.StatusSeeOther)
	if on, _ := e.st.FeedTranscripts(context.Background(), f.ID); !on {
		t.Fatal("a form post without JavaScript did not turn it on")
	}
	wantStatus(t, e.post("/feeds/"+idStr(blog.ID)+"/transcripts", url.Values{"transcripts": {"1"}}, htmx), http.StatusNotFound)
	wantStatus(t, e.post("/feeds/424242/transcripts", url.Values{}, htmx), http.StatusNotFound)

	e.serverTranscripts(false)
	wantContains(t, e.get(path).Body.String(), "On for this source, but transcripts are disabled on this server (FR_TRANSCRIPTS=false).")
}

// TestFeedTranscriptsPanel_PendingFeed: a source waiting for its feed's
// first answer says that the videos of that answer get no transcript (they
// arrive as an initial add).
func TestFeedTranscriptsPanel_PendingFeed(t *testing.T) {
	e := newTestEnv(t)
	now := time.Now()
	f, err := e.st.CreateFeed(context.Background(), store.NewFeed{Kind: store.KindYouTube,
		URL: "https://www.youtube.com/feeds/videos.xml?channel_id=UCsynthetic333333333333", Title: "Waiting",
		FetchedAt: now, NextFetchAt: now.Add(time.Hour), InitialPending: true, LastError: "HTTP 404"}, nil, config.InitialUnread)
	if err != nil {
		t.Fatal(err)
	}
	e.transcriptsOn()
	const note = "first answer arrive as when you add a source: they get no transcript."
	wantContains(t, e.get("/feeds/"+idStr(f.ID)).Body.String(), note)
	e2 := newTestEnv(t)
	g, _ := e2.videoFeed()
	wantNotContains(t, e2.get("/feeds/"+idStr(g.ID)).Body.String(), note)
}
