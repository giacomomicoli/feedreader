package web

import (
	"mime"
	"strings"
	"unicode"

	"github.com/giacomomicoli/feedreader/internal/store"
)

// The transcript download (GET /entries/{id}/transcript.md): a Markdown
// file with the video's title as its heading, a list of what is known about
// it (channel, link, publication date, caption track) and the transcript's
// paragraphs. The transcript and the title are plain text that may contain
// anything the captions or the feed said, so every character Markdown would
// read as syntax is escaped: the file shows the same text in any viewer and
// never carries a link, an image or HTML that the video did not.

// markdownDate is how the file dates a video, in the server's time zone.
const markdownDate = "2 January 2006"

// maxFilenameRunes bounds the title part of the file's name.
const maxFilenameRunes = 80

// transcriptMarkdown is the downloadable file of entry e's transcript t.
func transcriptMarkdown(e store.EntryView, t store.Transcript) string {
	var b strings.Builder
	title := oneLine(e.Title)
	if title == "" {
		title = "Untitled video"
	}
	heading := escapeMarkdownLine(title)
	if strings.HasSuffix(heading, "#") && !strings.HasSuffix(heading, `\#`) {
		// A trailing run of # would close the heading and be dropped.
		heading = heading[:len(heading)-1] + `\#`
	}
	b.WriteString("# " + heading + "\n\n")
	if ch := oneLine(e.Author); ch != "" {
		b.WriteString("- Channel: " + escapeMarkdownLine(ch) + "\n")
	} else if ch := oneLine(e.FeedTitle); ch != "" {
		b.WriteString("- Channel: " + escapeMarkdownLine(ch) + "\n")
	}
	if href := httpURL(e.URL); href != "" && !strings.ContainsAny(href, "<> \t\r\n") {
		b.WriteString("- Video: <" + href + ">\n")
	}
	if !e.PublishedAt.IsZero() {
		b.WriteString("- Published: " + e.PublishedAt.Local().Format(markdownDate) + "\n")
	}
	b.WriteString("- Transcript: " + escapeMarkdownLine(trackNote(t)) + "\n")
	for _, p := range strings.Split(strings.ReplaceAll(t.Text, "\r\n", "\n"), "\n\n") {
		var lines []string
		for _, l := range strings.Split(p, "\n") {
			if l = strings.TrimSpace(l); l != "" {
				lines = append(lines, escapeMarkdownLine(l))
			}
		}
		if len(lines) > 0 {
			// A trailing backslash is a hard line break: a line break
			// within a paragraph stays one.
			b.WriteString("\n" + strings.Join(lines, "\\\n") + "\n")
		}
	}
	return b.String()
}

// trackNote describes t's caption track: "en, auto-generated captions",
// "de, authored captions", or without the language when it is unknown.
func trackNote(t store.Transcript) string {
	kind := "authored captions"
	if t.Generated {
		kind = "auto-generated captions"
	}
	if t.Lang == "" {
		return kind
	}
	return t.Lang + ", " + kind
}

// oneLine collapses s's whitespace, line breaks included, into single
// spaces.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// markdownInline are the characters escaped wherever they are: those that
// start emphasis, code, links (and so images), HTML, autolinks, entities,
// strikethrough and table cells.
const markdownInline = "\\`*_[]<&~|"

// escapeMarkdownLine escapes line, one line of plain text, so that Markdown
// shows it as written: the inline syntax characters anywhere, and at its
// start what would make it a heading, a quote, a list item or a rule.
func escapeMarkdownLine(line string) string {
	var b strings.Builder
	for _, r := range line {
		if strings.ContainsRune(markdownInline, r) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	s := b.String()
	switch {
	case s == "":
		return s
	case strings.ContainsRune("#>-+=", rune(s[0])):
		return "\\" + s
	}
	// "1. " and "1) " start an ordered list.
	i := 0
	for i < len(s) && i < 9 && '0' <= s[i] && s[i] <= '9' {
		i++
	}
	if i > 0 && i < len(s) && (s[i] == '.' || s[i] == ')') {
		return s[:i] + "\\" + s[i:]
	}
	return s
}

// transcriptDisposition is the Content-Disposition of the download of the
// transcript of a video titled title: an attachment named after it,
// "<title> - transcript.md", reduced to letters, digits, spaces and
// "-_.,'()" so that it is a valid name on any system.
func transcriptDisposition(title string) string {
	var b strings.Builder
	n := 0
	space := false
	for _, r := range oneLine(title) {
		if n == maxFilenameRunes {
			break
		}
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) && !strings.ContainsRune("-_.,'()", r) {
			space = b.Len() > 0
			continue
		}
		if space {
			b.WriteByte(' ')
			n++
			space = false
		}
		b.WriteRune(r)
		n++
	}
	name := strings.Trim(b.String(), " .")
	if name == "" {
		name = "video"
	}
	if d := mime.FormatMediaType("attachment", map[string]string{"filename": name + " - transcript.md"}); d != "" {
		return d
	}
	return `attachment; filename="transcript.md"`
}
