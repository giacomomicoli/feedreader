package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// videoDescription is a video description as the parser stores it: escaped
// plain text with <br> line breaks.
const videoDescription = "Line one<br>Line two<br><br>Chapters:<br>0:00 Intro &amp; more"

// summaryEntry subscribes a feed titled feedTitle with one entry, published
// an hour ago, and returns the feed and the entry id.
func (e *testEnv) summaryEntry(kind store.Kind, feedTitle, link, summary string) (store.Feed, int64) {
	e.t.Helper()
	now := time.Now().Add(-time.Hour)
	f, err := e.st.CreateFeed(context.Background(), store.NewFeed{
		Kind: kind, URL: "https://example.com/" + strings.ToLower(strings.ReplaceAll(feedTitle, " ", "-")) + "/feed.xml",
		Title: feedTitle, OriginalTitle: feedTitle, FetchedAt: now,
	}, []store.NewEntry{{GUID: "1", Title: "Episode one", URL: link, SummaryHTML: summary, PublishedAt: now}},
		config.InitialUnread)
	if err != nil {
		e.t.Fatalf("create feed %s: %v", feedTitle, err)
	}
	return f, e.entries(f.ID)[0].ID
}

func TestEntryDrawerShowsTheFullSummaryWithItsLineBreaks(t *testing.T) {
	e := newTestEnv(t)
	f, id := e.summaryEntry(store.KindYouTube, "Synthetic Channel", "https://www.youtube.com/watch?v=abcdefghijk", videoDescription)

	w := e.get("/entries/"+idStr(id), htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	sid := idStr(id)
	wantContains(t, body,
		`<h2 class="entry-title" id="entry-drawer-title">Episode one</h2>`,
		`href="`+strings.ReplaceAll(feedHref(f.ID, ""), "&", "&amp;")+`"`, ">Synthetic Channel</span>",
		`<time datetime="`, ">1 hour ago</time>",
		// Two tabs: Summary selected (and focused when the drawer opens),
		// Transcription disabled until a transcript is fetched.
		`<div class="tabs" role="tablist" aria-label="Entry details">`,
		`<a class="tab" role="tab" id="tab-summary-`+sid+`" href="/entries/`+sid+`" aria-controls="panel-summary-`+sid+
			`" aria-selected="true" tabindex="0" autofocus>Summary</a>`,
		`<span class="tab" role="tab" id="tab-transcript-`+sid+`" aria-selected="false" aria-disabled="true"`,
		`<div class="tab-panel" role="tabpanel" id="panel-summary-`+sid+`" aria-labelledby="tab-summary-`+sid+`" tabindex="0">`,
		// Plain text, escaped, with its line breaks (app.css shows them).
		">Line one\nLine two\n\nChapters:\n0:00 Intro &amp; more</div>",
		`<a class="btn btn-primary" href="https://www.youtube.com/watch?v=abcdefghijk" target="_blank" rel="noopener noreferrer">Open video</a>`,
		`<form method="dialog"><button class="btn" type="submit">Close</button></form>`,
		`<form method="dialog" class="drawer-close">`,
	)
	if n := strings.Count(body, "autofocus"); n != 1 {
		t.Errorf("%d autofocus attributes, want 1", n)
	}
	// The content of the drawer only: no page, no sidebar, and no counts,
	// because nothing changed.
	wantNotContains(t, body, "<!doctype html>", `id="sidebar-tags"`, `hx-swap-oob="true"`, "<br>", `id="panel-transcript-`)
	checkCSPMarkup(t, "entry drawer", body)
	if e.entry(id).IsRead {
		t.Error("opening the details marked the entry read")
	}
}

func TestEntryPageWithoutJavaScript(t *testing.T) {
	e := newTestEnv(t)
	_, id := e.summaryEntry(store.KindRSS, "Synthetic Blog", "https://example.com/posts/1", videoDescription)

	w := e.get("/entries/" + idStr(id))
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body,
		"<!doctype html>", "<title>Episode one · feedreader</title>", `id="sidebar-tags"`,
		`<h1 class="page-title">Episode one</h1>`,
		`href="/entries/`+idStr(id)+`" aria-controls="panel-summary-`+idStr(id)+`" aria-selected="true" tabindex="0">Summary</a>`,
		">Line one\nLine two\n\nChapters:\n0:00 Intro &amp; more</div>",
		`href="https://example.com/posts/1" target="_blank" rel="noopener noreferrer">Open article</a>`,
	)
	// No drawer to close and nothing to focus on a page of its own; an
	// article has no transcript status.
	wantNotContains(t, body, `method="dialog"`, "autofocus", `id="entry-drawer-title"`, "transcript-status")
	checkCSPMarkup(t, "entry page", body)
	if w.Header().Get("Vary") != "HX-Request" {
		t.Errorf("Vary = %q: the page and the drawer fragment share a URL", w.Header().Get("Vary"))
	}
}

func TestEntrySummaryErrors(t *testing.T) {
	e := newTestEnv(t)
	_, id := e.summaryEntry(store.KindRSS, "Synthetic Blog", "https://example.com/posts/1", videoDescription)
	cases := map[string]int{
		"/entries/abc":               http.StatusBadRequest,
		"/entries/0":                 http.StatusBadRequest,
		"/entries/-1":                http.StatusBadRequest,
		"/entries/1.5":               http.StatusBadRequest,
		"/entries/424242":            http.StatusNotFound,
		"/entries/abc/transcript":    http.StatusBadRequest,
		"/entries/424242/transcript": http.StatusNotFound,
		// An entry without a fetched transcript has no Transcription tab.
		"/entries/" + idStr(id) + "/transcript": http.StatusNotFound,
	}
	for target, want := range cases {
		w := e.get(target)
		if w.Code != want || !strings.Contains(w.Body.String(), "<!doctype html>") {
			t.Errorf("GET %s = %d, want a %d page", target, w.Code, want)
		}
		w = e.get(target, htmx)
		if w.Code != want || w.Header().Get("HX-Retarget") != "#notice" {
			t.Errorf("htmx GET %s = %d (HX-Retarget %q), want %d into #notice", target, w.Code, w.Header().Get("HX-Retarget"), want)
		}
	}
	// The read-only routes leave the actions POST-only.
	for _, action := range []string{"read", "later", "favourite", "transcript/fetch"} {
		if w := e.get("/entries/" + idStr(id) + "/" + action); w.Code != http.StatusNotFound && w.Code != http.StatusMethodNotAllowed {
			t.Errorf("GET %s = %d, want 404 or 405", action, w.Code)
		}
	}
	if ev := e.entry(id); ev.IsRead || ev.IsLater || ev.IsFavourite {
		t.Errorf("a GET changed the entry: %+v", ev)
	}
	// "Load more" keeps its own route.
	wantStatus(t, e.get("/entries?scope=all", htmx), http.StatusOK)
}

// TestEntrySummaryIsEscapedPlainText: stored summaries are sanitized, and
// the drawer still only ever shows their visible text, escaped.
func TestEntrySummaryIsEscapedPlainText(t *testing.T) {
	e := newTestEnv(t)
	hostile := `<p>&lt;script&gt;alert(1)&lt;/script&gt; "quoted" &amp; <b>bold</b></p>` +
		`<script>alert(2)</script><img src=x onerror=alert(3)><a href="javascript:alert(4)">link</a>`
	_, id := e.summaryEntry(store.KindRSS, `<img src=x onerror=alert(5)>`, "javascript:alert(6)", hostile)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"drawer": e.get("/entries/"+idStr(id), htmx),
		"page":   e.get("/entries/" + idStr(id)),
	} {
		wantStatus(t, w, http.StatusOK)
		body := w.Body.String()
		wantContains(t, body,
			">&lt;script&gt;alert(1)&lt;/script&gt; &#34;quoted&#34; &amp; bold\n\nlink</div>",
			"&lt;img src=x onerror=alert(5)&gt;",
		)
		wantNotContains(t, body, "<script>alert", "alert(2)", "alert(3)", "alert(4)", "javascript:", "<img src=x",
			`target="_blank"`) // no link to an original that is not http(s)
		checkCSPMarkup(t, name, body)
	}
}

func TestEntrySummaryIsCutAtSummaryFullMaxChars(t *testing.T) {
	e := newTestEnv(t)
	long := "<p>" + strings.Repeat("é", config.SummaryFullMaxChars+config.SummaryMaxChars) + "</p>"
	_, id := e.summaryEntry(store.KindRSS, "Synthetic Blog", "https://example.com/posts/1", long)

	body := e.get("/entries/"+idStr(id), htmx).Body.String()
	wantContains(t, body, ">"+strings.Repeat("é", config.SummaryFullMaxChars)+"…</div>")
	if n := strings.Count(body, "é"); n != config.SummaryFullMaxChars {
		t.Errorf("drawer shows %d characters, want %d", n, config.SummaryFullMaxChars)
	}
	// The title's tooltip keeps the short excerpt.
	card := e.get("/").Body.String()
	wantContains(t, card, `title="`+strings.Repeat("é", config.SummaryMaxChars)+`…"`)
}

func TestCardDetailsButtonOpensTheEntryDrawer(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeed(store.KindRSS, "blog", "Blog", 0, makeEntries("blog", 1))
	ev := e.entries(f.ID)[0]
	_, bare := e.summaryEntry(store.KindRSS, "Bare Feed", "https://example.com/bare", "<script>nothing visible</script>")

	body := e.get("/").Body.String()
	path := "/entries/" + idStr(ev.ID)
	link := `<a class="card-details" href="` + path + `" hx-get="` + path +
		`" hx-target="#entry-drawer" hx-swap="innerHTML" aria-haspopup="dialog">Details<span class="sr-only"> of ` +
		ev.Title + `</span></a>` // an accessible name of its own on every card
	wantContains(t, body, link, `title="Summary of blog entry 0"`)
	// An article without a summary has nothing to show; the old Summary
	// link is gone.
	wantNotContains(t, body, `href="/entries/`+idStr(bare)+`"`, "card-summary", "entry-dialog")
	// One drawer, in the layout, outside the main pane whose parts htmx
	// replaces.
	if n := strings.Count(body, "<dialog"); n != 1 {
		t.Errorf("%d dialogs, want 1", n)
	}
	drawer := `<dialog id="entry-drawer" class="drawer" aria-labelledby="entry-drawer-title"></dialog>`
	if i, j := strings.Index(body, "</main>"), strings.Index(body, drawer); j < 0 || j < i {
		t.Errorf("layout lacks %s after the main pane", drawer)
	}
	// Cards re-rendered by an action keep the button.
	wantContains(t, e.post(path+"/read", nil, htmx).Body.String(), link)

	// static/app.js opens the drawer once htmx filled it and gives focus
	// back to the button when it closes; app.css keeps the text's breaks.
	js := e.get("/static/app.js").Body.String()
	wantContains(t, js, `"htmx:afterSwap"`, `"entry-drawer"`, "showModal()", `document.addEventListener("close"`,
		"opener.focus()", `"tab-shown"`, `"ArrowRight"`, `"ArrowLeft"`, `"Home"`, `"End"`)
	// The button is remembered only when the drawer opens: a later swap
	// into the open drawer must not replace the element focus returns to.
	if !regexp.MustCompile(`if \(!isEntryDrawer\(drawer\) \|\| drawer\.open\) \{\s*return;\s*\}\s*opener = [^;]+;\s*drawer\.showModal\(\);`).MatchString(js) {
		t.Error("app.js does not remember the opener only when it opens the drawer")
	}
	// htmx cancels every click on the button to make its request. A
	// modified click (Ctrl/Cmd, Shift or Alt: new tab, new window, …) is
	// stopped on the document in the capture phase, before it reaches
	// htmx's listener, and keeps its default action: the browser follows
	// href.
	modifiedClick := regexp.MustCompile(`document\.addEventListener\("click", function \(evt\) \{[^}]*` +
		`\.closest\("a\.card-details"\)[^}]*evt\.ctrlKey \|\| evt\.metaKey \|\| evt\.shiftKey \|\| evt\.altKey[^}]*\{\s*` +
		`evt\.stopPropagation\(\);\s*\}\s*\}, true\);`)
	if !modifiedClick.MatchString(js) {
		t.Error("app.js does not stop modified clicks on the Details button in the capture phase")
	}
	// A click on the backdrop (the dialog element itself, outside its
	// content) closes the drawer.
	if !regexp.MustCompile(`if \(isEntryDrawer\(evt\.target\)\) \{\s*evt\.target\.close\(\);`).MatchString(js) {
		t.Error("app.js does not close the drawer on a backdrop click")
	}
	css := e.get("/static/app.css").Body.String()
	wantContains(t, css, ".drawer::backdrop", ".drawer[open]", "@starting-style", "prefers-reduced-motion",
		"white-space: pre-line;", ".tab[aria-selected=\"true\"]")
}

// transcriptsOn turns the global transcript setting on.
func (e *testEnv) transcriptsOn() {
	e.t.Helper()
	if err := e.st.SetTranscriptsSetting(context.Background(), true, time.Now()); err != nil {
		e.t.Fatal(err)
	}
}

// polledVideo stores a video found by a poll of a YouTube feed, with its
// transcript queued (the global setting turned on), records r for it unless
// r is empty (still queued), and returns the entry id.
func (e *testEnv) polledVideo(r store.TranscriptResult) int64 {
	e.t.Helper()
	return e.polledVideoWith(videoDescription, r)
}

// polledVideoWith is polledVideo for a video whose description is summary.
func (e *testEnv) polledVideoWith(summary string, r store.TranscriptResult) int64 {
	e.t.Helper()
	ctx := context.Background()
	e.transcriptsOn()
	f, _ := e.summaryEntry(store.KindYouTube, "Synthetic Channel", "https://www.youtube.com/watch?v=abcdefghijk", videoDescription)
	n, err := e.st.RecordFetchSuccess(ctx, store.FetchSuccess{FeedID: f.ID, FetchedAt: time.Now(), QueueTranscripts: true,
		Entries: []store.NewEntry{{GUID: "2", Title: "Episode two", URL: "https://www.youtube.com/watch?v=bcdefghijkl",
			SummaryHTML: summary, PublishedAt: time.Now()}}})
	if err != nil || n != 1 {
		e.t.Fatalf("poll: %d new, %v", n, err)
	}
	job, ok, err := e.st.NextTranscript(ctx)
	if err != nil || !ok {
		e.t.Fatalf("NextTranscript = %v, %v", ok, err)
	}
	if r.Outcome != "" {
		if err := e.st.RecordTranscript(ctx, job.EntryID, r); err != nil {
			e.t.Fatalf("RecordTranscript: %v", err)
		}
	}
	return job.EntryID
}

// transcriptText is a transcript as stored: plain text with line breaks,
// which may contain anything the captions said.
const transcriptText = "Hello & welcome\n<b>not bold</b> \"quoted\"\n\nSecond paragraph"

// TestTranscriptionTabIsLoadedWhenShown: with a fetched transcript the
// Transcription tab is enabled, but the drawer does not carry the text: its
// panel loads it (htmx) the first time app.js shows it. Without JavaScript
// the tab is a link to the entry page with that tab selected and the text
// in place.
func TestTranscriptionTabIsLoadedWhenShown(t *testing.T) {
	e := newTestEnv(t)
	id := e.polledVideo(store.TranscriptResult{Outcome: store.TranscriptOK,
		Transcript: store.Transcript{Lang: "en", Generated: true, Text: transcriptText}})
	sid := idStr(id)
	escaped := "Hello &amp; welcome\n&lt;b&gt;not bold&lt;/b&gt; &#34;quoted&#34;\n\nSecond paragraph</div>"

	body := e.get("/entries/"+sid, htmx).Body.String()
	wantContains(t, body,
		`<a class="tab" role="tab" id="tab-transcript-`+sid+`" href="/entries/`+sid+`/transcript" aria-controls="panel-transcript-`+sid+
			`" aria-selected="false" tabindex="-1">Transcription</a>`,
		`<div class="tab-panel" role="tabpanel" id="panel-transcript-`+sid+`" aria-labelledby="tab-transcript-`+sid+
			`" tabindex="0" hidden hx-get="/entries/`+sid+`/transcript" hx-trigger="tab-shown once" hx-swap="innerHTML">`,
		"Transcript fetched.",
	)
	wantNotContains(t, body, "Hello &amp; welcome", `aria-disabled="true"`, "transcript/fetch")
	checkCSPMarkup(t, "drawer", body)

	w := e.get("/entries/"+sid+"/transcript", htmx)
	wantStatus(t, w, http.StatusOK)
	frag := w.Body.String()
	wantContains(t, frag, ">Transcript (en, auto-generated)</p>",
		`<div class="entry-transcript-text" lang="en">`+escaped)
	wantNotContains(t, frag, "<b>not bold", `hx-swap-oob="true"`, "<!doctype html>")

	page := e.get("/entries/" + sid + "/transcript").Body.String()
	wantContains(t, page, "<!doctype html>",
		`href="/entries/`+sid+`/transcript" aria-controls="panel-transcript-`+sid+`" aria-selected="true" tabindex="0">Transcription</a>`,
		`id="panel-summary-`+sid+`" aria-labelledby="tab-summary-`+sid+`" tabindex="0" hidden>`,
		`id="panel-transcript-`+sid+`" aria-labelledby="tab-transcript-`+sid+`" tabindex="0">`, escaped)
	wantNotContains(t, page, `hx-trigger="tab-shown once"`)
	checkCSPMarkup(t, "page", page)
	if e.entry(id).IsRead {
		t.Error("opening the transcript marked the entry read")
	}
}

// TestTranscriptionTabDisabledUnlessFetched: a video still queued, without
// captions, or given up on has its Transcription tab disabled and no
// panel; its Details button is there all the same (the status line and the
// button that fetches the transcript are in the drawer).
func TestTranscriptionTabDisabledUnlessFetched(t *testing.T) {
	for name, r := range map[string]store.TranscriptResult{
		"queued":      {},
		"retrying":    {Outcome: store.TranscriptRetry, RetryAt: time.Now().Add(time.Hour), Reason: store.ReasonError, Requested: true},
		"no captions": {Outcome: store.TranscriptNoCaptions, Reason: store.ReasonNoCaptions, Requested: true},
		"failed":      {Outcome: store.TranscriptFailed, Reason: store.ReasonExhausted, Requested: true},
	} {
		e := newTestEnv(t)
		id := e.polledVideoWith("", r)
		body := e.get("/entries/"+idStr(id), htmx).Body.String()
		wantContains(t, body, `id="tab-transcript-`+idStr(id)+`" aria-selected="false" aria-disabled="true"`,
			"This entry has no summary.", `id="transcript-status-`+idStr(id)+`"`)
		if strings.Contains(body, `id="panel-transcript-`) {
			t.Errorf("%s: a Transcription panel without a transcript", name)
		}
		if grid := e.get("/").Body.String(); !strings.Contains(grid, `<a class="card-details" href="/entries/`+idStr(id)+`"`) {
			t.Errorf("%s: no Details button for a video", name)
		}
	}
}

func TestEntryTranscriptLabel(t *testing.T) {
	for _, tt := range []struct {
		tr   store.Transcript
		want string
	}{
		{store.Transcript{Lang: "de", Text: "Hallo"}, ">Transcript (de)</p>"},
		{store.Transcript{Lang: "pt-BR", Generated: true, Text: "Olá"}, ">Transcript (pt-BR, auto-generated)</p>"},
		{store.Transcript{Generated: true, Text: "Hi"}, ">Transcript (auto-generated)</p>"},
		{store.Transcript{Text: "Hi"}, ">Transcript</p>"},
	} {
		e := newTestEnv(t)
		id := e.polledVideo(store.TranscriptResult{Outcome: store.TranscriptOK, Transcript: tt.tr})
		body := e.get("/entries/"+idStr(id)+"/transcript", htmx).Body.String()
		wantContains(t, body, tt.want, ">"+tt.tr.Text+"</div>")
		if tt.tr.Lang == "" {
			wantNotContains(t, body, ` lang="`)
		}
	}
}

// TestEntryTranscriptIsEscapedPlainText: a transcript is whatever the
// captions said, so it is only ever escaped text, also its language.
func TestEntryTranscriptIsEscapedPlainText(t *testing.T) {
	e := newTestEnv(t)
	id := e.polledVideo(store.TranscriptResult{Outcome: store.TranscriptOK, Transcript: store.Transcript{
		Lang: `en" onmouseover="alert(1)`, Text: "<script>alert(2)</script><img src=x onerror=alert(3)>‮evil"}})
	for name, w := range map[string]*httptest.ResponseRecorder{
		"fragment": e.get("/entries/"+idStr(id)+"/transcript", htmx),
		"page":     e.get("/entries/" + idStr(id) + "/transcript"),
	} {
		body := w.Body.String()
		wantContains(t, body, "&lt;script&gt;alert(2)&lt;/script&gt;&lt;img src=x onerror=alert(3)&gt;")
		wantNotContains(t, body, "<script>alert", "<img src=x", `onmouseover="`)
		checkCSPMarkup(t, name, body)
	}
}
