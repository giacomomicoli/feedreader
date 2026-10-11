package web

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Shorts modes: a YouTube channel is followed with all its uploads, only its
// regular videos or only its Shorts (resolve.Mode), picked on the add form's
// confirm step and on the feed settings page.

// modeOption is one option of a mode <select>.
type modeOption struct {
	Value    resolve.Mode
	Label    string
	Selected bool
}

// errPickMode is the message for a mode that is not one of the options, or
// that the feed cannot follow.
const errPickMode = "Pick one of the options."

// modeLabel names a mode in the UI.
func modeLabel(m resolve.Mode) string {
	switch m {
	case resolve.ModeVideos:
		return "Videos only, no Shorts"
	case resolve.ModeShorts:
		return "Shorts only"
	}
	return "Videos and Shorts"
}

// modeOptions returns the modes feedURL can be followed in, with selected
// (or, when selected is "", the mode feedURL follows now) selected; nil
// when feedURL does not follow a YouTube channel (see resolve.FeedMode).
func modeOptions(feedURL string, selected resolve.Mode) []modeOption {
	_, cur, ok := resolve.FeedMode(feedURL)
	if !ok {
		return nil
	}
	if selected == "" {
		selected = cur
	}
	modes := resolve.Modes()
	out := make([]modeOption, len(modes))
	for i, m := range modes {
		out[i] = modeOption{Value: m, Label: modeLabel(m), Selected: m == selected}
	}
	return out
}

// modeFeedURL returns the feed URL that follows the channel of feedURL in
// the mode named raw (a form value), or ok false when raw is not a mode or
// feedURL does not follow a YouTube channel.
func modeFeedURL(feedURL, raw string) (target string, m resolve.Mode, ok bool) {
	m, ok = resolve.ParseMode(raw)
	if !ok {
		return "", "", false
	}
	id, _, ok := resolve.FeedMode(feedURL)
	if !ok {
		return "", "", false
	}
	return resolve.ModeFeedURL(id, m), m, true
}

// modeFeedFailure is the message shown when the feed of a channel's mode
// (only its videos, only its Shorts) answered 404 or 5xx while subscribing:
// unlike a channel's feed, it may not exist at all.
func modeFeedFailure(m resolve.Mode, err error) (string, bool) {
	var se *fetch.StatusError
	if (m != resolve.ModeVideos && m != resolve.ModeShorts) || !errors.As(err, &se) ||
		(se.StatusCode != http.StatusNotFound && se.StatusCode < http.StatusInternalServerError) {
		return "", false
	}
	what := "videos"
	if m == resolve.ModeShorts {
		what = "Shorts"
	}
	return fmt.Sprintf("YouTube's “%s” list of this channel answered with HTTP %d: the channel may have no %s yet, "+
		"or YouTube's feeds are down. Pick another option, or try again later.", modeLabel(m), se.StatusCode, what), true
}

// modeNote explains a mode switch next to the mode <select>.
var modeNote = fmt.Sprintf("“Videos only” and “Shorts only” follow the lists YouTube keeps of the channel's "+
	"regular videos and of its Shorts. Switching fetches the new list right away (after any rate-limit wait): "+
	"entries already stored stay, and of the videos it adds the %d newest are marked unread.", config.InitialUnread)

// addModeNote explains the mode <select> of the add form.
const addModeNote = "“Videos only” and “Shorts only” follow the lists YouTube keeps of the channel's " +
	"regular videos and of its Shorts. " +
	"You can change this later in the source's settings."

// handleFeedMode switches what a YouTube channel subscription follows.
func (s *server) handleFeedMode(w http.ResponseWriter, r *http.Request) {
	f, err := s.feedForPath(r)
	if err == nil {
		err = parseForm(r)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	m, ok := resolve.ParseMode(r.PostForm.Get("mode"))
	if !ok {
		s.formError(w, r, errPickMode)
		return
	}
	_, err = s.sched.SetMode(r.Context(), f.ID, m)
	switch {
	case errors.Is(err, store.ErrConflict):
		msg := fmt.Sprintf("You already follow this channel's “%s” as another source.", modeLabel(m))
		if id, _, ok := resolve.FeedMode(f.URL); ok {
			if other, gerr := s.store.GetFeedByURL(r.Context(), resolve.ModeFeedURL(id, m)); gerr == nil {
				msg = fmt.Sprintf("You already follow this channel's “%s” as “%s”.", modeLabel(m), other.Title)
			}
		}
		s.formErrorStatus(w, r, http.StatusConflict, msg)
		return
	case errors.Is(err, store.ErrInvalid):
		s.formError(w, r, "Videos and Shorts can be picked only for YouTube channels.")
		return
	case err != nil:
		s.fail(w, r, fmt.Errorf("set mode of feed %d: %w", f.ID, err))
		return
	}
	s.redirect(w, r, settingsHref(f.ID))
}
