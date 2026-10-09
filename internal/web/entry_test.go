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

func TestEntryDialogShowsTheFullSummaryWithItsLineBreaks(t *testing.T) {
	e := newTestEnv(t)
	f, id := e.summaryEntry(store.KindYouTube, "Synthetic Channel", "https://www.youtube.com/watch?v=abcdefghijk", videoDescription)

	w := e.get("/entries/"+idStr(id), htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body,
		`<h2 class="entry-title" id="entry-dialog-title">Episode one</h2>`,
		`href="`+strings.ReplaceAll(feedHref(f.ID, ""), "&", "&amp;")+`"`, ">Synthetic Channel</span>",
		`<time datetime="`, ">1 hour ago</time>",
		// Plain text, escaped, with its line breaks (app.css shows them).
		">Line one\nLine two\n\nChapters:\n0:00 Intro &amp; more</div>",
		`<a class="btn btn-primary" href="https://www.youtube.com/watch?v=abcdefghijk" target="_blank" rel="noopener noreferrer">Open video</a>`,
		`<form method="dialog"><button class="btn" type="submit">Close</button></form>`,
		" autofocus>",
	)
	// The content of the dialog only: no page, no sidebar, and no counts,
	// because nothing changed.
	wantNotContains(t, body, "<!doctype html>", `id="sidebar-tags"`, `hx-swap-oob="true"`, "<br>")
	checkCSPMarkup(t, "entry dialog", body)
	if e.entry(id).IsRead {
		t.Error("opening the summary marked the entry read")
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
		">Line one\nLine two\n\nChapters:\n0:00 Intro &amp; more</div>",
		`href="https://example.com/posts/1" target="_blank" rel="noopener noreferrer">Open article</a>`,
	)
	// No dialog to close and nothing to focus on a page of its own.
	wantNotContains(t, body, `method="dialog"`, "autofocus", `id="entry-dialog-title"`)
	checkCSPMarkup(t, "entry page", body)
	if w.Header().Get("Vary") != "HX-Request" {
		t.Errorf("Vary = %q: the page and the dialog fragment share a URL", w.Header().Get("Vary"))
	}
}

func TestEntrySummaryErrors(t *testing.T) {
	e := newTestEnv(t)
	_, id := e.summaryEntry(store.KindRSS, "Synthetic Blog", "https://example.com/posts/1", videoDescription)
	cases := map[string]int{
		"/entries/abc":    http.StatusBadRequest,
		"/entries/0":      http.StatusBadRequest,
		"/entries/-1":     http.StatusBadRequest,
		"/entries/1.5":    http.StatusBadRequest,
		"/entries/424242": http.StatusNotFound,
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
	// The read-only route leaves the entry actions POST-only.
	for _, action := range []string{"read", "later", "favourite"} {
		wantStatus(t, e.get("/entries/"+idStr(id)+"/"+action), http.StatusNotFound)
	}
	if ev := e.entry(id); ev.IsRead || ev.IsLater || ev.IsFavourite {
		t.Errorf("a GET changed the entry: %+v", ev)
	}
	// "Load more" keeps its own route.
	wantStatus(t, e.get("/entries?scope=all", htmx), http.StatusOK)
}

// TestEntrySummaryIsEscapedPlainText: stored summaries are sanitized, and
// the dialog still only ever shows their visible text, escaped.
func TestEntrySummaryIsEscapedPlainText(t *testing.T) {
	e := newTestEnv(t)
	hostile := `<p>&lt;script&gt;alert(1)&lt;/script&gt; "quoted" &amp; <b>bold</b></p>` +
		`<script>alert(2)</script><img src=x onerror=alert(3)><a href="javascript:alert(4)">link</a>`
	_, id := e.summaryEntry(store.KindRSS, `<img src=x onerror=alert(5)>`, "javascript:alert(6)", hostile)
	for name, w := range map[string]*httptest.ResponseRecorder{
		"dialog": e.get("/entries/"+idStr(id), htmx),
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
		t.Errorf("dialog shows %d characters, want %d", n, config.SummaryFullMaxChars)
	}
	// The title's tooltip keeps the short excerpt.
	card := e.get("/").Body.String()
	wantContains(t, card, `title="`+strings.Repeat("é", config.SummaryMaxChars)+`…"`)
}

func TestCardSummaryLinkOpensTheEntryDialog(t *testing.T) {
	e := newTestEnv(t)
	f := e.addFeed(store.KindRSS, "blog", "Blog", 0, makeEntries("blog", 1))
	ev := e.entries(f.ID)[0]
	_, bare := e.summaryEntry(store.KindRSS, "Bare Feed", "https://example.com/bare", "<script>nothing visible</script>")

	body := e.get("/").Body.String()
	path := "/entries/" + idStr(ev.ID)
	link := `<a class="card-summary" href="` + path + `" hx-get="` + path +
		`" hx-target="#entry-dialog" hx-swap="innerHTML" aria-haspopup="dialog">Summary<span class="sr-only"> of ` +
		ev.Title + `</span></a>` // an accessible name of its own on every card
	wantContains(t, body, link, `title="Summary of blog entry 0"`)
	wantNotContains(t, body, `<details class="card-summary">`, `href="/entries/`+idStr(bare)+`"`)
	// One dialog, in the layout, outside the main pane whose parts htmx
	// replaces.
	if n := strings.Count(body, "<dialog"); n != 1 {
		t.Errorf("%d dialogs, want 1", n)
	}
	dialog := `<dialog id="entry-dialog" class="entry-dialog" aria-labelledby="entry-dialog-title"></dialog>`
	if i, j := strings.Index(body, "</main>"), strings.Index(body, dialog); j < 0 || j < i {
		t.Errorf("layout lacks %s after the main pane", dialog)
	}
	// Cards re-rendered by an action keep the link.
	wantContains(t, e.post(path+"/read", nil, htmx).Body.String(), link)

	// static/app.js opens the dialog once htmx filled it and gives focus
	// back to the link when it closes; app.css keeps the text's breaks.
	js := e.get("/static/app.js").Body.String()
	wantContains(t, js, `"htmx:afterSwap"`, `"entry-dialog"`, "showModal()", `document.addEventListener("close"`, "opener.focus()")
	// The link is remembered only when the dialog opens: a later swap into
	// the open dialog must not replace the element focus returns to.
	if !regexp.MustCompile(`if \(!isEntryDialog\(dialog\) \|\| dialog\.open\) \{\s*return;\s*\}\s*opener = [^;]+;\s*dialog\.showModal\(\);`).MatchString(js) {
		t.Error("app.js does not remember the opener only when it opens the dialog")
	}
	// htmx cancels every click on the link to make its request. A modified
	// click (Ctrl/Cmd, Shift or Alt: new tab, new window, …) is stopped on
	// the document in the capture phase, before it reaches htmx's listener
	// on the link, and keeps its default action: the browser follows href.
	modifiedClick := regexp.MustCompile(`document\.addEventListener\("click", function \(evt\) \{[^}]*` +
		`\.closest\("a\.card-summary"\)[^}]*evt\.ctrlKey \|\| evt\.metaKey \|\| evt\.shiftKey \|\| evt\.altKey[^}]*\{\s*` +
		`evt\.stopPropagation\(\);\s*\}\s*\}, true\);`)
	if !modifiedClick.MatchString(js) {
		t.Error("app.js does not stop modified clicks on the Summary link in the capture phase")
	}
	wantNotContains(t, js, "preventDefault")
	css := e.get("/static/app.css").Body.String()
	wantContains(t, css, ".entry-dialog::backdrop", "white-space: pre-line;")
}
