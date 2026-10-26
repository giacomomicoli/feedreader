package web

import (
	"mime"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/store"
)

func TestEscapeMarkdownLine(t *testing.T) {
	for in, want := range map[string]string{
		"Plain words, nothing more.":    "Plain words, nothing more.",
		"Wow! a > b, (yes)":             "Wow! a > b, (yes)",
		"**bold** _em_ `code` ~~gone~~": `\*\*bold\*\* \_em\_ \` + "`" + `code\` + "`" + ` \~\~gone\~\~`,
		"[link](https://x.example)":     `\[link\](https://x.example)`,
		"![img](x.png)":                 `!\[img\](x.png)`,
		"<b>tag</b> &amp; a|b":          `\<b>tag\</b> \&amp; a\|b`,
		`back\slash`:                    `back\\slash`,
		"# not a heading":               `\# not a heading`,
		">> new speaker":                `\>> new speaker`,
		"- not a list":                  `\- not a list`,
		"+ not a list":                  `\+ not a list`,
		"=== not a rule":                `\=== not a rule`,
		"1. not a list":                 `1\. not a list`,
		"2026) not a list":              `2026\) not a list`,
		"1999 was a year":               "1999 was a year",
		"":                              "",
	} {
		if got := escapeMarkdownLine(in); got != want {
			t.Errorf("escapeMarkdownLine(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTranscriptDisposition(t *testing.T) {
	for title, want := range map[string]string{
		"Episode one": "Episode one - transcript.md",
		"Addio a BioWare, com’è Castlevania: Belmont’s Curse #97": "Addio a BioWare, com è Castlevania Belmont s Curse 97 - transcript.md",
		`../../etc/passwd`:       "etc passwd - transcript.md",
		`a<b>c:"d"/e\f|g?h*i`:    "a b c d e f g h i - transcript.md",
		"  \t\n ":                "video - transcript.md",
		"...":                    "video - transcript.md",
		strings.Repeat("x", 200): strings.Repeat("x", maxFilenameRunes) + " - transcript.md",
	} {
		d := transcriptDisposition(title)
		typ, params, err := mime.ParseMediaType(d)
		if err != nil || typ != "attachment" || params["filename"] != want {
			t.Errorf("transcriptDisposition(%q) = %q (%q, %v); want attachment named %q", title, d, params["filename"], err, want)
		}
	}
}

func TestTranscriptMarkdown(t *testing.T) {
	e := store.EntryView{
		Entry: store.Entry{Title: "Episode #1: *live*", Author: "Synthetic Channel",
			URL: "https://www.youtube.com/watch?v=abcdefghijk", PublishedAt: time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)},
		FeedTitle: "Renamed source",
	}
	got := transcriptMarkdown(e, store.Transcript{Lang: "it", Generated: true,
		Text: ">> Ciao a tutti & benvenuti\n<b>non grassetto</b>\n\n# Secondo paragrafo\n\n\n\n"})
	want := "# Episode #1: \\*live\\*\n\n" +
		"- Channel: Synthetic Channel\n" +
		"- Video: <https://www.youtube.com/watch?v=abcdefghijk>\n" +
		"- Published: " + e.PublishedAt.Local().Format(markdownDate) + "\n" +
		"- Transcript: it, auto-generated captions\n" +
		"\n\\>> Ciao a tutti \\& benvenuti\\\n\\<b>non grassetto\\</b>\n" +
		"\n\\# Secondo paragrafo\n"
	if got != want {
		t.Errorf("transcriptMarkdown =\n%s\nwant\n%s", got, want)
	}

	// Without an author the source's name stands in; an unknown language,
	// a missing date, a missing title and a URL that is not http(s) are
	// left out or replaced.
	bare := transcriptMarkdown(store.EntryView{Entry: store.Entry{URL: "javascript:alert(1)"}, FeedTitle: "Source"},
		store.Transcript{Text: "Text"})
	if want := "# Untitled video\n\n- Channel: Source\n- Transcript: authored captions\n\nText\n"; bare != want {
		t.Errorf("bare transcriptMarkdown = %q, want %q", bare, want)
	}

	// A title ending in # keeps it: it would otherwise close the heading.
	for title, want := range map[string]string{"We're number #": "# We're number \\#\n", "Tag ##": "# Tag #\\#\n"} {
		if got := transcriptMarkdown(store.EntryView{Entry: store.Entry{Title: title}}, store.Transcript{}); !strings.HasPrefix(got, want) {
			t.Errorf("heading of %q = %q, want %q", title, got, want)
		}
	}
}

// TestTranscriptDownload: a fetched transcript downloads as a Markdown file
// named after the video, linked from the Transcription tab; without one, or
// for an entry that does not exist, the download is a 404.
func TestTranscriptDownload(t *testing.T) {
	e := newTestEnv(t)
	id := e.polledVideo(store.TranscriptResult{Outcome: store.TranscriptOK,
		Transcript: store.Transcript{Lang: "en", Generated: true, Text: transcriptText}})
	sid := idStr(id)

	frag := e.get("/entries/"+sid+"/transcript", htmx).Body.String()
	wantContains(t, frag, `href="/entries/`+sid+`/transcript.md" download>`, "<span>Download</span>")

	w := e.get("/entries/" + sid + "/transcript.md")
	wantStatus(t, w, http.StatusOK)
	h := w.Result().Header
	if ct := h.Get("Content-Type"); ct != "text/markdown; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	if _, p, err := mime.ParseMediaType(h.Get("Content-Disposition")); err != nil || p["filename"] != "Episode two - transcript.md" {
		t.Errorf("Content-Disposition = %q", h.Get("Content-Disposition"))
	}
	if h.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("download served without nosniff")
	}
	wantContains(t, w.Body.String(), "# Episode two\n", "- Channel: Synthetic Channel\n",
		"- Video: <https://www.youtube.com/watch?v=bcdefghijkl>\n", "- Transcript: en, auto-generated captions\n",
		"\nHello \\& welcome\\\n\\<b>not bold\\</b> \"quoted\"\n\nSecond paragraph\n")
	if e.entry(id).IsRead {
		t.Error("downloading the transcript marked the entry read")
	}

	// The feed's other video has no transcript.
	other := int64(0)
	for _, en := range e.entries(e.entry(id).FeedID) {
		if en.ID != id {
			other = en.ID
		}
	}
	if other == 0 {
		t.Fatal("no video without a transcript")
	}
	for target, code := range map[string]int{
		"/entries/" + idStr(other) + "/transcript.md": http.StatusNotFound,
		"/entries/424242/transcript.md":               http.StatusNotFound,
		"/entries/abc/transcript.md":                  http.StatusBadRequest,
	} {
		wantStatus(t, e.get(target), code)
	}
}
