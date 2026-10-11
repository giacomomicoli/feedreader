package web

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
)

const (
	wantFallbackNote = "YouTube's feed isn't answering for this channel right now, so its latest videos were read from YouTube's website instead."
	fallbackField    = `<input type="hidden" name="fallback" value="1">`
)

func (e *testEnv) stubFallbackPreview(feedURL, title string, n int) {
	e.sched.previews[feedURL] = &sched.Preview{FeedURL: feedURL, Kind: store.KindYouTube, Title: title,
		EntryCount: n, Fallback: true}
	e.sched.entries[feedURL] = makeEntries("new", n)
}

// TestAddFallbackPreviewSaysWhereTheVideosCameFrom: a preview read from
// YouTube's web API is a normal preview (entry count, unread rule, mode
// choice, no subscribe-anyway consent) with a note saying so, kept when the
// form is shown again after an error.
func TestAddFallbackPreviewSaysWhereTheVideosCameFrom(t *testing.T) {
	e := newTestEnv(t)
	e.stubFallbackPreview(ytFeedURL, "Down Channel", 15)
	w := e.post("/add/preview", url.Values{"feed_url": {ytFeedURL}}, htmx)
	wantStatus(t, w, http.StatusOK)
	body := w.Body.String()
	wantContains(t, body, escapeText(wantFallbackNote), fallbackField, "15 entries found.",
		"The 5 newest will be marked unread; older ones are kept as read.", `name="title" type="text" value="Down Channel"`)
	wantNotContains(t, body, `name="allow_pending"`, escapeText(wantPendingNote))

	form := url.Values{"feed_url": {ytFeedURL}, "kind": {"youtube"}, "entries": {"15"}, "fallback": {"1"}, "folder": {"new"}}
	w = e.post("/add/subscribe", form, htmx)
	wantStatus(t, w, http.StatusUnprocessableEntity)
	wantContains(t, w.Body.String(), "Enter a name for the new folder.", escapeText(wantFallbackNote), fallbackField)
	if len(e.sched.subs) != 0 {
		t.Errorf("subscriptions = %+v", e.sched.subs)
	}
}

func TestAddNormalPreviewHasNoFallbackNote(t *testing.T) {
	e := newTestEnv(t)
	e.stubPreview(ytFeedURL, "Chan", store.KindYouTube, 3)
	w := e.post("/add/preview", url.Values{"feed_url": {ytFeedURL}}, htmx)
	wantStatus(t, w, http.StatusOK)
	wantNotContains(t, w.Body.String(), escapeText(wantFallbackNote), `name="fallback"`)
}
