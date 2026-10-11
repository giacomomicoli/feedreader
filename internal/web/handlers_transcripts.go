package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
	"github.com/giacomomicoli/feedreader/internal/transcript"
)

// Video transcripts in the UI: the status line and "Fetch transcript"
// button under the entry drawer's tabs, the lazily loaded Transcription
// tab, the global switch at the bottom of the sidebar, and the per-source
// switch on a YouTube feed's settings page. Every text is plain and
// escaped by html/template; nothing here is ever template.HTML.

// formChecked is the value a checked switch posts.
const formChecked = "1"

// day is how long the queue age of a transcript is shown in.
const day = 24 * time.Hour

// isVideo reports whether e is a YouTube video whose transcript can be
// fetched: an entry of a YouTube feed whose URL is a video's.
func isVideo(e store.EntryView) bool {
	if e.FeedKind != store.KindYouTube {
		return false
	}
	_, ok := transcript.VideoID(e.URL)
	return ok
}

// transcriptStatusView is the status line under the entry drawer's tabs,
// with the button that queues the transcript when the rules allow it.
type transcriptStatusView struct {
	EntryID int64
	Text    string
	Button  string // the button's label; "" = no button
	Error   string // why the last request was not queued, or ""
}

// aboutTime renders a time the worker will act at, in the server's zone:
// "15:04" today, "Mon 15:04" on another day.
func aboutTime(t, now time.Time, loc *time.Location) string {
	t, now = t.In(loc), now.In(loc)
	if y, m, d := t.Date(); y == now.Year() && m == now.Month() && d == now.Day() {
		return t.Format("15:04")
	}
	return t.Format("Mon 15:04")
}

// newTranscriptStatus says where an entry's transcript stands, given the
// stored state, the switches and the time now.
func newTranscriptStatus(id int64, st store.TranscriptState, sw sched.TranscriptSwitches, now time.Time, loc *time.Location) *transcriptStatusView {
	v := &transcriptStatusView{EntryID: id}
	if st.Status == store.StatusOK {
		v.Text = "Transcript fetched."
		return v
	}
	if !sw.On() {
		switch {
		case !sw.Server:
			v.Text = "Transcripts are disabled on this server (FR_TRANSCRIPTS=false)."
		case !sw.Global:
			v.Text = "Video transcripts are off; turn them on at the bottom of the sidebar."
		default:
			v.Text = "Transcripts are off for this source; turn them on in its feed settings."
		}
		if st.Status == store.StatusPending {
			v.Text += " Its transcript stays queued until then."
		}
		return v
	}
	tries := func(n int) string { return fmt.Sprintf("try %d of %d", n, config.TranscriptMaxAttempts) }
	switch st.Status {
	case store.StatusNone:
		v.Text, v.Button = "Transcript not requested.", "Fetch transcript"
	case store.StatusPending:
		v.Button = "Check now"
		due := st.DueAt
		switch {
		case st.PausedUntil.After(now) && !st.PausedUntil.Before(due):
			v.Text = "YouTube is limiting requests; transcripts resume about " + aboutTime(st.PausedUntil, now, loc) + "."
		case st.Reason == store.ReasonNoCaptions:
			v.Text = "No captions yet; checking again about " + aboutTime(due, now, loc) + "."
		case st.Reason == store.ReasonNotReady:
			v.Text = "Not aired yet; next check about " + aboutTime(due, now, loc) + "."
		case st.Reason == store.ReasonUnavailable:
			v.Text = "YouTube would not provide it; " + tries(st.Attempts+1) + " about " + aboutTime(due, now, loc) + "."
		case st.Reason == store.ReasonError:
			v.Text = "Could not be fetched; " + tries(st.Attempts+1) + " about " + aboutTime(due, now, loc) + "."
		case !due.After(now):
			v.Text, v.Button = "Queued; fetching soon.", ""
		default:
			v.Text = "Queued; next check about " + aboutTime(due, now, loc) + "."
		}
	case store.StatusNoCaptions:
		v.Text, v.Button = "No captions found (checked twice).", "Check again"
	case store.StatusFailed:
		v.Button = "Try again"
		switch st.Reason {
		case store.ReasonTooLarge:
			v.Text = "Too long to fetch; trying again will most likely fail the same way."
		case store.ReasonUnavailable:
			v.Text = fmt.Sprintf("YouTube would not provide it (tried %d times).", st.Attempts)
		case store.ReasonQueuedLong:
			v.Text = fmt.Sprintf("Gave up after %d days in the queue.", int(config.TranscriptMaxQueued/day))
		case store.ReasonNotAVideo:
			v.Text, v.Button = "Not a video.", ""
		default:
			v.Text = fmt.Sprintf("Could not be fetched (tried %d times).", st.Attempts)
		}
	default:
		v.Text = "Transcript state unknown."
	}
	return v
}

// transcriptStatusOf builds the status line of entry e, a video.
func (s *server) transcriptStatusOf(ctx context.Context, e store.EntryView) (*transcriptStatusView, error) {
	st, err := s.store.TranscriptStateOf(ctx, e.ID)
	if err != nil {
		return nil, fmt.Errorf("transcript state of entry %d: %w", e.ID, err)
	}
	sw, err := s.sched.TranscriptSwitchesOf(ctx, e.FeedID)
	if err != nil {
		return nil, fmt.Errorf("transcript switches of feed %d: %w", e.FeedID, err)
	}
	return newTranscriptStatus(e.ID, st, sw, s.now(), s.loc), nil
}

// handleEntryTranscript is the Transcription tab (GET
// /entries/{id}/transcript): the transcript alone for htmx, which loads it
// the first time the tab is shown, and the entry page with that tab
// selected otherwise. 404 unless the transcript was fetched.
func (s *server) handleEntryTranscript(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	t, err := s.store.GetTranscript(r.Context(), id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "This entry has no transcript.")
			return
		}
		s.fail(w, r, fmt.Errorf("get transcript of entry %d: %w", id, err))
		return
	}
	if isHTMX(r) {
		s.renderFragment(w, r, http.StatusOK, "entry-transcript", newTranscriptView(t), false)
		return
	}
	s.renderEntry(w, r, id, tabTranscript, newTranscriptView(t))
}

// handleTranscriptFetch queues an entry's transcript at the user's request
// (POST /entries/{id}/transcript/fetch): the new status line plus the
// sidebar counts for htmx, a 303 to the entry page otherwise. When the
// rules refuse it (not a video, a switch off, already fetched) the status
// line comes back with a 409 saying why.
func (s *server) handleTranscriptFetch(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err == nil {
		err = parseForm(r)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	reqErr := s.sched.RequestTranscript(ctx, id)
	var refusal string
	switch {
	case reqErr == nil:
	case errors.Is(reqErr, sched.ErrNotAVideo):
		refusal = "Not queued: this entry is not a YouTube video."
	case errors.Is(reqErr, sched.ErrTranscriptsOff):
		refusal = "Not queued: transcripts are off for this entry."
	case errors.Is(reqErr, store.ErrConflict):
		refusal = "Not queued: the transcript is already fetched."
	default:
		s.fail(w, r, fmt.Errorf("request transcript of entry %d: %w", id, reqErr))
		return
	}
	if !isHTMX(r) {
		if refusal != "" {
			s.writeError(w, r, http.StatusConflict, refusal)
			return
		}
		http.Redirect(w, r, entryHref(id), http.StatusSeeOther)
		return
	}
	e, err := s.store.GetEntry(ctx, id)
	if err != nil {
		s.fail(w, r, fmt.Errorf("get entry %d: %w", id, err))
		return
	}
	v := &transcriptStatusView{EntryID: id}
	if isVideo(e) {
		if v, err = s.transcriptStatusOf(ctx, e); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	status := http.StatusOK
	if refusal != "" {
		v.Error, status = refusal, http.StatusConflict
	}
	s.renderFragment(w, r, status, "transcript-status", v, true)
}

func entryHref(id int64) string { return "/entries/" + strconv.FormatInt(id, 10) }

// transcriptsSettingView is the global switch at the bottom of the
// sidebar.
type transcriptsSettingView struct {
	Allowed bool // FR_TRANSCRIPTS
	On      bool // the global setting
	Note    string
	Saved   bool
	Error   string
}

func newTranscriptsSetting(allowed, on bool) *transcriptsSettingView {
	v := &transcriptsSettingView{Allowed: allowed, On: on}
	switch {
	case !allowed:
		v.Note = "Disabled on this server (FR_TRANSCRIPTS=false)."
	case on:
		v.Note = "On: new videos get their transcript, slowly, one at a time."
	default:
		v.Note = "Off: no transcript is requested."
	}
	return v
}

// handleTranscriptsSetting turns the global transcript switch on or off
// (POST /settings/transcripts, checkbox "transcripts"): the switch again
// plus the sidebar counts for htmx, a 303 to the home page otherwise.
// Turning it on while FR_TRANSCRIPTS=false is refused (409).
func (s *server) handleTranscriptsSetting(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	on := r.PostForm.Get("transcripts") == formChecked
	err := s.sched.SetTranscripts(ctx, on)
	status, refusal := http.StatusOK, ""
	switch {
	case err == nil:
	case errors.Is(err, sched.ErrTranscriptsOff):
		status, refusal = http.StatusConflict, "Transcripts are disabled on this server."
	default:
		s.fail(w, r, fmt.Errorf("set transcripts setting: %w", err))
		return
	}
	if !isHTMX(r) {
		if refusal != "" {
			s.writeError(w, r, status, refusal)
			return
		}
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	sw, err := s.sched.TranscriptSwitchesOf(ctx, 0)
	if err != nil {
		s.fail(w, r, fmt.Errorf("transcript switches: %w", err))
		return
	}
	v := newTranscriptsSetting(sw.Server, sw.Global)
	v.Saved, v.Error = refusal == "", refusal
	s.renderFragment(w, r, status, "transcripts-setting", v, true)
}

// feedTranscriptsView is the transcript panel of a YouTube feed's settings.
type feedTranscriptsView struct {
	FeedID int64
	On     bool // the source's switch
	Note   string
	// PendingNote is set while the source's next document is stored as an
	// initial add (it waits for its feed's first answer, or was switched
	// between videos and Shorts), whose videos get no transcript.
	PendingNote string
	Saved       bool
}

func newFeedTranscripts(f store.Feed, sw sched.TranscriptSwitches) *feedTranscriptsView {
	v := &feedTranscriptsView{FeedID: f.ID, On: sw.Source}
	// The next document of a pending feed is an initial add, which queues
	// no transcripts: for a subscription that never answered (waitingFeed),
	// and after a switch between videos and Shorts, whose new list is
	// fetched as if newly added.
	switch {
	case waitingFeed(f):
		v.PendingNote = "The videos of its feed's first answer arrive as when you add a source: they get no transcript."
	case f.InitialPending:
		v.PendingNote = "The videos of the new list's first answer arrive as when you add a source: they get no transcript. " +
			"Videos already stored are not affected."
	}
	switch {
	case !sw.Source:
		v.Note = "Off for this source: its videos get no transcript."
	case !sw.Server:
		v.Note = "On for this source, but transcripts are disabled on this server (FR_TRANSCRIPTS=false)."
	case !sw.Global:
		v.Note = "On for this source, but video transcripts are off globally (bottom of the sidebar)."
	default:
		v.Note = "On: new videos of this source get their transcript."
	}
	return v
}

// handleFeedTranscripts turns a YouTube source's transcript switch on or
// off (POST /feeds/{id}/transcripts, checkbox "transcripts").
func (s *server) handleFeedTranscripts(w http.ResponseWriter, r *http.Request) {
	f, err := s.feedForPath(r)
	if err == nil {
		err = parseForm(r)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if f.Kind != store.KindYouTube {
		s.writeError(w, r, http.StatusNotFound, "Only YouTube sources have transcripts.")
		return
	}
	ctx := r.Context()
	on := r.PostForm.Get("transcripts") == formChecked
	if err := s.sched.SetFeedTranscripts(ctx, f.ID, on); err != nil {
		s.fail(w, r, fmt.Errorf("set transcripts of feed %d: %w", f.ID, err))
		return
	}
	if !isHTMX(r) {
		http.Redirect(w, r, settingsHref(f.ID), http.StatusSeeOther)
		return
	}
	sw, err := s.sched.TranscriptSwitchesOf(ctx, f.ID)
	if err != nil {
		s.fail(w, r, fmt.Errorf("transcript switches of feed %d: %w", f.ID, err))
		return
	}
	v := newFeedTranscripts(f, sw)
	v.Saved = true
	s.renderFragment(w, r, http.StatusOK, "feed-transcripts", v, true)
}
