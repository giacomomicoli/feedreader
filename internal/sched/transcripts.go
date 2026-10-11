package sched

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/store"
	"github.com/giacomomicoli/feedreader/internal/transcript"
)

// Video transcripts. YouTube has no API for them and refuses clients that
// ask too fast, so they are fetched lazily and slowly: a poll of a YouTube
// feed queues the transcripts of the videos it stores for the first time
// (never the initial add of a channel, which stores all its videos at once),
// the entry's "Fetch transcript" button queues one, and RunTranscripts
// fetches the queue one video at a time, config.TranscriptDelay apart, each
// video once it is due: at once when queued, hours later when it is checked
// again. A missing transcript is a normal outcome: the entry shows without
// one.
//
// Three switches must all be on: cfg.Transcripts (FR_TRANSCRIPTS, the
// server's kill switch), the global setting (store.TranscriptsSetting,
// changed with SetTranscripts) and the source's own (store.FeedTranscripts,
// changed with SetFeedTranscripts). They are checked when a poll queues
// (the store reads the two stored ones in the poll's transaction) and each
// time the worker picks a job (the queue skips sources switched off; with
// the server's or the global switch off the worker does not read the queue
// at all). Turning a switch off freezes the queue; turning it on (the
// server's too, noticed at the worker's start) never queues the entries
// stored meanwhile, and counts the queued ones' age from then.
//
// RunTranscripts has at most one request in flight, so the scheduler makes
// at most cfg.FetchWorkers + addFetchSlots + 1 requests at once.

// Transcriber fetches the transcript of a YouTube video, the way
// *transcript.Client does: errors match transcript.ErrNoCaptions,
// transcript.ErrUnavailable, transcript.ErrNotReady, transcript.ErrBlocked
// or transcript.ErrTooLarge when they mean that.
type Transcriber interface {
	Fetch(ctx context.Context, videoID string) (transcript.Transcript, error)
}

var _ Transcriber = (*transcript.Client)(nil)

// errTranscriptsRunning is returned by a RunTranscripts call that overlaps
// another one.
var errTranscriptsRunning = errors.New("sched: RunTranscripts is already running")

var (
	// errNotAVideo is the outcome of a queued entry whose URL names no video.
	errNotAVideo = errors.New("the entry's URL is not a YouTube video's")
	// errFetchPanicked is the outcome of a transcript fetch that panicked.
	errFetchPanicked = errors.New("the transcript fetch panicked")
	// errQueuedTooLong is the outcome of a video still queued
	// config.TranscriptMaxQueued after a poll queued it.
	errQueuedTooLong = errors.New("the video was queued for too long")
)

// maxLangLen bounds a stored language code: the field size RFC 5646
// (section 4.4.1) recommends for language tags. YouTube's are far shorter.
const maxLangLen = 35

// langTag matches the form of a BCP 47 language tag (RFC 5646): a primary
// language subtag, then subtags of up to 8 letters or digits.
var langTag = regexp.MustCompile(`^[A-Za-z]{2,8}(?:-[A-Za-z0-9]{1,8})*$`)

// Transcript outcomes as logged.
const (
	logOK         = string(store.TranscriptOK)
	logNoCaptions = string(store.TranscriptNoCaptions)
	logRecheck    = string(store.TranscriptRecheck)
	logRetry      = string(store.TranscriptRetry)
	logFailed     = string(store.TranscriptFailed)
	logBlocked    = string(store.TranscriptBlocked)
	logNotReady   = "not_ready"
)

// maxTranscriptDue is the furthest ahead the worker ever makes a queued
// video due: a refusal's pause and a retry are capped at config.MaxBackoff,
// a recheck is config.TranscriptRecheckDelay away. A video due further
// ahead was stored while the clock was ahead of the current one, and counts
// as due.
const maxTranscriptDue = max(config.MaxBackoff, config.TranscriptRecheckDelay)

// RunTranscripts fetches the queued transcripts through t, one at a time,
// each once it is due, until ctx is done:
//
//   - a transcript is stored and shown in the entry drawer's Transcription
//     tab;
//   - a video without captions (transcript.ErrNoCaptions, or an empty
//     text) is checked once more config.TranscriptRecheckDelay later,
//     without counting an attempt: YouTube makes the automatic captions of
//     a new upload, or of a stream that has just ended, some time after it
//     is published. A second check without captions is final;
//   - a live stream on air, or a premiere or stream that has not started
//     (transcript.ErrNotReady), is checked again every
//     config.TranscriptRecheckDelay without counting an attempt;
//   - transcript.ErrBlocked leaves the video queued without counting an
//     attempt, and no request is made for config.TranscriptBlockedPause,
//     twice as long after each further refusal in a row (at most
//     config.MaxBackoff), or for the refusal's Retry-After when longer (at
//     most config.MaxBackoff too); any other outcome of a request ends the
//     streak. The pause and the streak are stored, so a restart ends
//     neither, and the video is due again when the pause ends, behind the
//     videos already due;
//   - an answer larger than the client reads (transcript.ErrTooLarge: the
//     caption track of a long video over the fetch size cap) would be so
//     at every attempt: the video is given up on at once;
//   - any other error (the network, an error status, an answer whose format
//     changed) is retried config.TranscriptRetryDelay later, then twice as
//     long after each further failure (at most config.MaxBackoff), until
//     config.TranscriptMaxAttempts requests for the video have failed: an
//     outage that lasts minutes does not use the attempts up. So is
//     transcript.ErrUnavailable (a video YouTube does not play to this
//     client), although it matches ErrNoCaptions: YouTube answers the same
//     way when it stops serving this kind of client;
//   - an entry whose URL names no video is given up on without a request,
//     and so is a video still queued config.TranscriptMaxQueued after the
//     poll that queued it (a stream that does not air, a video refused at
//     each attempt): so whatever YouTube answers, a video is requested
//     only within that time, and the queue holds the videos of that time
//     at most (and of the pause that delays them). A queued time that
//     lies ahead was stored while the clock was ahead: it counts as now,
//     and is stored as now with the outcome that keeps the video queued;
//     an outcome reached without a request leaves the streak of refusals
//     as it is.
//
// The other queued videos are fetched while one waits. Requests are at
// least config.TranscriptDelay apart. While the next video is not due yet
// the worker waits for it, or for a poll that queues a new one; with
// nothing queued it waits for a poll only, so it makes no request unless
// new videos were found (or the user asked for one). While the server's
// switch or the global setting is off it reads no queue and waits until a
// switch changes; the videos of a source switched off are skipped. Each outcome is logged in one line. A request cut
// short by ctx records nothing: the video is fetched after the next start.
// Store errors are logged and retried; an outcome that cannot be stored
// pauses the worker for config.TranscriptBlockedPause, so an unwritable
// database does not turn into a stream of requests for the same video.
//
// RunTranscripts returns an error only when another RunTranscripts call on
// the same scheduler is still active.
func (s *Scheduler) RunTranscripts(ctx context.Context, t Transcriber) error {
	if !s.transcribing.CompareAndSwap(false, true) {
		return errTranscriptsRunning
	}
	defer s.transcribing.Store(false)

	s.log.Info("transcript worker started", "allowed", s.cfg.Transcripts, "delay", s.transcriptDelay, "blocked_pause", s.transcriptBlockedPause,
		"max_attempts", config.TranscriptMaxAttempts, "retry_delay", config.TranscriptRetryDelay,
		"recheck_delay", config.TranscriptRecheckDelay, "max_queued", config.TranscriptMaxQueued)
	// The server's switch thaws the queue when it was off at the last start,
	// as the global setting does when turned back on: the videos frozen
	// meanwhile are not given up on for having waited.
	// No job is picked until that is recorded: the record is retried
	// storeRetryDelay apart until it succeeds or ctx ends.
	for ctx.Err() == nil {
		err := s.st.RecordServerTranscripts(ctx, s.cfg.Transcripts, s.now())
		if err == nil || ctx.Err() != nil {
			break
		}
		s.log.Warn("could not record the server's transcript switch", "err", err, "retry_in", storeRetryDelay)
		s.sleep(ctx, storeRetryDelay)
	}
	for ctx.Err() == nil {
		on, err := s.transcriptsOn(ctx)
		if err == nil && !on {
			s.waitTranscripts(ctx, 0)
			continue
		}
		var (
			job   store.TranscriptJob
			ok    bool
			pause time.Duration
		)
		if err == nil {
			job, ok, err = s.st.NextTranscript(ctx)
		}
		switch {
		case err != nil:
			if ctx.Err() == nil {
				s.log.Warn("could not read the transcript queue", "err", err)
			}
			pause = storeRetryDelay
		case !ok:
			s.waitTranscripts(ctx, 0)
			continue
		default:
			if wait := s.untilDue(job); wait > 0 {
				s.waitTranscripts(ctx, wait)
				continue
			}
			pause = s.transcribe(ctx, t, job)
		}
		if pause > 0 && ctx.Err() == nil {
			s.sleep(ctx, pause)
		}
	}
	s.log.Info("transcript worker stopped")
	return nil
}

// untilDue returns how long to wait before job falls due, 0 when it is due.
// The wait is at most maxSleep, after which the queue is read again, so
// that a wall-clock jump delays a video by at most that much.
func (s *Scheduler) untilDue(job store.TranscriptJob) time.Duration {
	d := job.DueAt.Sub(s.now())
	if d <= 0 || d > maxTranscriptDue {
		return 0
	}
	return min(d, maxSleep)
}

// waitTranscripts blocks until ctx is done, a poll queues videos, or, when
// d is positive, d has passed.
func (s *Scheduler) waitTranscripts(ctx context.Context, d time.Duration) {
	var timeout <-chan time.Time
	if d > 0 {
		c, stop := s.transcriptTimer(d)
		defer stop()
		timeout = c
	}
	select {
	case <-ctx.Done():
	case <-s.transcriptWake:
	case <-timeout:
	}
}

// queuedFor is how long job has been queued at now. A queued time that
// lies ahead was stored while the clock was ahead: it counts as now, as a
// due time too far ahead counts as due (untilDue), and an outcome that
// keeps the video queued stores it as now (store.TranscriptResult.At).
func queuedFor(job store.TranscriptJob, now time.Time) time.Duration {
	return max(now.Sub(job.QueuedAt), 0)
}

// transcribe fetches and records the transcript of job, returning how long
// to wait before the next one.
func (s *Scheduler) transcribe(ctx context.Context, t Transcriber, job store.TranscriptJob) (pause time.Duration) {
	id, isVideo := transcript.VideoID(job.URL)
	var (
		skip   error // why the video is given up on without a request
		reason store.TranscriptReason
	)
	switch {
	case !isVideo:
		skip, reason = errNotAVideo, store.ReasonNotAVideo
	case queuedFor(job, s.now()) >= config.TranscriptMaxQueued:
		skip, reason = errQueuedTooLong, store.ReasonQueuedLong
	}
	if skip != nil {
		// No request was made: only a store error is worth a pause, the
		// streak of refusals goes on, and no attempt is counted.
		r := store.TranscriptResult{Outcome: store.TranscriptFailed, Reason: reason, At: s.now()}
		if !s.recordTranscript(ctx, job, id, logFailed, r, 0, 0, skip) {
			return storeRetryDelay
		}
		return 0
	}
	started := s.now()
	tr, err := s.safeFetch(ctx, t, id)
	if ctx.Err() != nil {
		return 0 // our own shutdown, not the video's fault
	}
	now := s.now()
	took := now.Sub(started)
	pause = s.transcriptDelay

	var (
		r       store.TranscriptResult
		outcome string
		text    store.Transcript
	)
	if err == nil {
		text = cleanTranscript(tr)
	}
	unavailable := errors.Is(err, transcript.ErrUnavailable)
	switch {
	case err == nil && text.Text != "":
		r = store.TranscriptResult{Outcome: store.TranscriptOK, Transcript: text}
		outcome = logOK
	case err == nil, errors.Is(err, transcript.ErrNoCaptions) && !unavailable:
		// No captions, or none with any text. The first time, YouTube
		// may still be making them.
		err = nil
		r.Outcome, outcome = store.TranscriptNoCaptions, logNoCaptions
		if !job.Rechecked {
			r = store.TranscriptResult{Outcome: store.TranscriptRecheck, RetryAt: now.Add(config.TranscriptRecheckDelay)}
			outcome = logRecheck
		}
		r.Reason = store.ReasonNoCaptions
	case errors.Is(err, transcript.ErrNotReady):
		r = store.TranscriptResult{Outcome: store.TranscriptDeferred, RetryAt: now.Add(config.TranscriptRecheckDelay),
			Reason: store.ReasonNotReady}
		outcome, err = logNotReady, nil
	case errors.Is(err, transcript.ErrBlocked):
		pause = s.blockedPause(err, job.Refusals)
		r = store.TranscriptResult{Outcome: store.TranscriptBlocked, RetryAt: now.Add(pause), Reason: store.ReasonRefused}
		outcome = logBlocked
	case errors.Is(err, transcript.ErrTooLarge):
		r = store.TranscriptResult{Outcome: store.TranscriptFailed, Reason: store.ReasonTooLarge}
		outcome = logFailed
	case job.Attempts+1 >= config.TranscriptMaxAttempts:
		r = store.TranscriptResult{Outcome: store.TranscriptFailed, Reason: store.ReasonExhausted}
		if unavailable {
			r.Reason = store.ReasonUnavailable
		}
		outcome = logFailed
	default:
		r = store.TranscriptResult{Outcome: store.TranscriptRetry, RetryAt: now.Add(retryDelay(job.Attempts)),
			Reason: store.ReasonError}
		if unavailable {
			r.Reason = store.ReasonUnavailable
		}
		outcome = logRetry
	}
	r.At, r.Requested = now, true
	if !s.recordTranscript(ctx, job, id, outcome, r, took, pause, err) {
		pause = max(pause, s.transcriptBlockedPause)
	}
	return pause
}

// retryDelay is how long a video waits after a failed attempt when
// attempts earlier ones failed too: config.TranscriptRetryDelay, doubled
// for each earlier failure, at most config.MaxBackoff.
func retryDelay(attempts int) time.Duration {
	return doubled(config.TranscriptRetryDelay, attempts)
}

// doubled returns d doubled n times, at most config.MaxBackoff. It stops
// doubling at the cap, however large n is.
func doubled(d time.Duration, n int) time.Duration {
	for i := 0; i < n && d > 0 && d < config.MaxBackoff; i++ {
		d *= 2
	}
	return min(d, config.MaxBackoff)
}

// safeFetch runs t.Fetch, turning a panic into an error (a failed attempt)
// so that a page that trips up the parser cannot take the process down.
func (s *Scheduler) safeFetch(ctx context.Context, t Transcriber, videoID string) (tr transcript.Transcript, err error) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("transcript fetch panicked", "video", videoID, "panic", r)
			err = errFetchPanicked
		}
	}()
	return t.Fetch(ctx, videoID)
}

// blockedPause is how long to wait after YouTube refused a request (err)
// when the refusals requests before it were refused too:
// config.TranscriptBlockedPause doubled for each of those, at most
// config.MaxBackoff, or the refusal's Retry-After when longer, capped at
// config.MaxBackoff as for a feed. So while YouTube refuses every request
// (it stopped accepting this client, say), the worker soon makes one a
// day.
func (s *Scheduler) blockedPause(err error, refusals int) time.Duration {
	return max(doubled(s.transcriptBlockedPause, refusals), min(retryAfter(err), config.MaxBackoff))
}

// recordTranscript stores the outcome r of job and logs it in one line:
// at info level when there is no cause (a transcript, no captions, a video
// checked again later), as a warning with cause otherwise; with the pause
// that follows a refusal and the refusals in a row so far, or when the
// video is due again. It reports false
// when the outcome could not be stored for a reason other than the entry
// having been deleted meanwhile (or ctx ending).
func (s *Scheduler) recordTranscript(ctx context.Context, job store.TranscriptJob, videoID, outcome string,
	r store.TranscriptResult, took, pause time.Duration, cause error) bool {
	err := s.st.RecordTranscript(ctx, job.EntryID, r)
	switch {
	case err == nil:
	case ctx.Err() != nil:
		return true
	case errors.Is(err, store.ErrNotFound):
		s.log.Debug("entry removed while its transcript was being fetched", "entry", job.EntryID, "video", videoID)
		return true
	default:
		s.log.Error("could not store the transcript outcome", "entry", job.EntryID, "video", videoID,
			"outcome", outcome, "err", err, "pause", s.transcriptBlockedPause)
		return false
	}
	attrs := []any{"entry", job.EntryID, "video", videoID, "outcome", outcome, "took", took}
	if r.Reason != "" && r.Outcome != store.TranscriptOK {
		attrs = append(attrs, "reason", string(r.Reason))
	}
	level := slog.LevelInfo
	if cause != nil {
		level = slog.LevelWarn
		attrs = append(attrs, "err", failureMessage(cause))
	}
	switch {
	case r.Outcome == store.TranscriptOK:
		attrs = append(attrs, "lang", r.Transcript.Lang, "generated", r.Transcript.Generated)
	case r.Outcome == store.TranscriptRetry, r.Outcome == store.TranscriptFailed:
		attrs = append(attrs, "attempts", job.Attempts+1)
	}
	switch {
	case outcome == logBlocked:
		attrs = append(attrs, "pause", pause, "refusals", job.Refusals+1)
	case !r.RetryAt.IsZero():
		attrs = append(attrs, "retry_at", r.RetryAt)
	}
	s.log.Log(ctx, level, "video transcript", attrs...)
	return true
}

// cleanTranscript prepares a fetched transcript for storage, whatever the
// Transcriber returned: valid UTF-8, trimmed, at most
// config.TranscriptMaxRunes runes (a longer text is cut and ends in "…"),
// and a language code only when it has the form of one.
func cleanTranscript(t transcript.Transcript) store.Transcript {
	text := cutRunes(strings.TrimSpace(strings.ToValidUTF8(t.Text, string(utf8.RuneError))), config.TranscriptMaxRunes)
	lang := t.Lang
	if len(lang) > maxLangLen || !langTag.MatchString(lang) {
		lang = ""
	}
	return store.Transcript{Lang: lang, Generated: t.Generated, Text: text}
}

// cutRunes returns s when it has at most n runes, else its first n-1 runes
// and "…". It does not copy s into a rune slice: a hostile text can be large.
func cutRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	kept := 0
	for i := range s {
		if kept == n-1 {
			return s[:i] + "…"
		}
		kept++
	}
	return s
}

// transcriptsOn reports whether the worker may fetch: the server's switch
// (cfg.Transcripts) and the global setting are on.
func (s *Scheduler) transcriptsOn(ctx context.Context) (bool, error) {
	if !s.cfg.Transcripts {
		return false, nil
	}
	return s.st.TranscriptsSetting(ctx)
}

// ErrTranscriptsOff is returned by the transcript actions that the switches
// do not allow: SetTranscripts(on) with the server's switch off, or
// RequestTranscript when any switch is off for the entry.
var ErrTranscriptsOff = errors.New("sched: video transcripts are off")

// ErrNotAVideo is returned by RequestTranscript for an entry that is not a
// YouTube video.
var ErrNotAVideo = errors.New("sched: the entry is not a YouTube video")

// TranscriptSwitches are the three switches an entry's transcript depends
// on.
type TranscriptSwitches struct {
	Server bool // cfg.Transcripts (FR_TRANSCRIPTS)
	Global bool // the global setting
	Source bool // the source's own switch
}

// On reports whether all three are on.
func (w TranscriptSwitches) On() bool { return w.Server && w.Global && w.Source }

// TranscriptSwitchesOf returns the switches of feedID's transcripts (with
// feedID 0, Source is reported on).
func (s *Scheduler) TranscriptSwitchesOf(ctx context.Context, feedID int64) (TranscriptSwitches, error) {
	w := TranscriptSwitches{Server: s.cfg.Transcripts, Source: true}
	var err error
	if w.Global, err = s.st.TranscriptsSetting(ctx); err != nil {
		return w, err
	}
	if feedID != 0 {
		if w.Source, err = s.st.FeedTranscripts(ctx, feedID); err != nil {
			return w, err
		}
	}
	return w, nil
}

// SetTranscripts turns the global transcript setting on or off and wakes
// the worker. Turning it on is refused with ErrTranscriptsOff while the
// server's switch is off; turning it off is always allowed. Turning it on
// queues nothing: only videos found new from then on, and those the user
// asks for, get a transcript; the ones queued before are fetched again,
// their queue age counted from now.
func (s *Scheduler) SetTranscripts(ctx context.Context, on bool) error {
	if on && !s.cfg.Transcripts {
		return ErrTranscriptsOff
	}
	if err := s.st.SetTranscriptsSetting(ctx, on, s.now()); err != nil {
		return err
	}
	s.signalTranscripts()
	return nil
}

// SetFeedTranscripts turns a source's transcript switch on or off and wakes
// the worker; like SetTranscripts it never queues anything.
func (s *Scheduler) SetFeedTranscripts(ctx context.Context, feedID int64, on bool) error {
	if err := s.st.SetFeedTranscripts(ctx, feedID, on, s.now()); err != nil {
		return err
	}
	s.signalTranscripts()
	return nil
}

// RequestTranscript queues the transcript of an entry at the user's request,
// due at once, and wakes the worker. It returns ErrNotAVideo unless the
// entry is a video of a YouTube feed, ErrTranscriptsOff unless all three
// switches are on for it, and the store's errors (ErrNotFound, ErrConflict
// when the transcript was fetched already). The request goes through the
// queue: it waits for config.TranscriptDelay after the previous request and
// for the end of a refusal's pause like any other, so pressing the button
// again and again cannot make requests faster.
func (s *Scheduler) RequestTranscript(ctx context.Context, entryID int64) error {
	e, err := s.st.GetEntry(ctx, entryID)
	if err != nil {
		return err
	}
	if _, ok := transcript.VideoID(e.URL); !ok || e.FeedKind != store.KindYouTube {
		return ErrNotAVideo
	}
	w, err := s.TranscriptSwitchesOf(ctx, e.FeedID)
	if err != nil {
		return err
	}
	if !w.On() {
		return ErrTranscriptsOff
	}
	if err := s.st.RequestTranscript(ctx, entryID, s.now()); err != nil {
		return err
	}
	s.signalTranscripts()
	return nil
}

// signalTranscripts wakes RunTranscripts when it waits for queued videos,
// without blocking; wake-ups that arrive while one is pending coalesce.
func (s *Scheduler) signalTranscripts() {
	select {
	case s.transcriptWake <- struct{}{}:
	default:
	}
}

// newTimer returns a timer's channel and its Stop: the default
// Scheduler.transcriptTimer.
func newTimer(d time.Duration) (<-chan time.Time, func() bool) {
	t := time.NewTimer(d)
	return t.C, t.Stop
}

// sleepCtx waits for d, or until ctx is done.
func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
