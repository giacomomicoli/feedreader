package sched

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/store"
	"github.com/giacomomicoli/feedreader/internal/transcript"
)

// settingReads wraps a transcriptStore to count the worker's reads of the
// global setting: the first thing it does each time round its loop.
type settingReads struct {
	*transcriptStore
	n atomic.Int64
}

func (s *settingReads) TranscriptsSetting(ctx context.Context) (bool, error) {
	s.n.Add(1)
	return s.transcriptStore.TranscriptsSetting(ctx)
}

// withSettingReads makes e's scheduler count its setting reads.
func withSettingReads(e *transcriptEnv) *settingReads {
	sr := &settingReads{transcriptStore: e.st}
	e.s.st = sr
	return sr
}

// TestRunTranscripts_GlobalSettingOffMakesNoRequest: with the global setting
// off the worker does not even read the queue; turning it on (through the
// scheduler) wakes it, and the video queued before is fetched.
func TestRunTranscripts_GlobalSettingOffMakesNoRequest(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	e.queue(t, "frozenvideo")
	mustNoErr(t, e.s.SetTranscripts(t.Context(), false))
	sr := withSettingReads(e)
	drainTranscriptWake(e.s)
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) { return transcriptOf(id), nil })
	e.run(t, tr)
	waitFor(t, "the worker to read the setting", func() bool { return sr.n.Load() >= 1 })
	time.Sleep(50 * time.Millisecond) // give a wrong worker time to go on
	if n := len(tr.Calls()); n != 0 || e.st.Idle() != 0 || sr.n.Load() != 1 {
		t.Fatalf("with the setting off: %d requests, %d queue reads, %d setting reads; want 0, 0, 1",
			n, e.st.Idle(), sr.n.Load())
	}
	mustNoErr(t, e.s.SetTranscripts(t.Context(), true))
	e.waitIdle(t, 1)
	if got := tr.Calls(); !slices.Equal(got, []string{"frozenvideo"}) {
		t.Errorf("requests after turning on = %q, want the frozen video", got)
	}
}

// TestRunTranscripts_ServerSwitchOffMakesNoRequest: with FR_TRANSCRIPTS off
// the worker never reads the setting nor the queue, and the global setting
// cannot be turned on; it can be turned off.
func TestRunTranscripts_ServerSwitchOffMakesNoRequest(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	e.queue(t, "neverasked1")
	e.s.cfg.Transcripts = false
	sr := withSettingReads(e)
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) { return transcriptOf(id), nil })
	e.run(t, tr)
	if err := e.s.SetTranscripts(t.Context(), true); !errors.Is(err, ErrTranscriptsOff) {
		t.Errorf("SetTranscripts(true) with the server's switch off: err = %v, want ErrTranscriptsOff", err)
	}
	mustNoErr(t, e.s.SetTranscripts(t.Context(), false))
	e.s.signalTranscripts()
	time.Sleep(50 * time.Millisecond)
	if n := len(tr.Calls()); n != 0 || e.st.Idle() != 0 || sr.n.Load() != 0 {
		t.Errorf("with the server's switch off: %d requests, %d queue reads, %d setting reads; want none", n, e.st.Idle(), sr.n.Load())
	}
	if err := e.s.RequestTranscript(t.Context(), feedEntries(t, e.st, e.feed.ID)["yt:video:neverasked1"].ID); !errors.Is(err, ErrTranscriptsOff) {
		t.Errorf("RequestTranscript with the server's switch off: err = %v, want ErrTranscriptsOff", err)
	}
}

// TestRunTranscripts_SourceSwitchedOffIsSkipped: the worker skips the queued
// videos of a source switched off, fetching the others', and fetches them
// once the source is switched back on (which wakes it).
func TestRunTranscripts_SourceSwitchedOffIsSkipped(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	e.queue(t, "sourceAvid1")
	other := addFeedOfKind(t, e.st, store.KindYouTube, channelURL+"1", t0.Add(time.Hour))
	if _, err := e.st.RecordFetchSuccess(t.Context(), store.FetchSuccess{FeedID: other.ID, FetchedAt: e.clock.Now(),
		QueueTranscripts: true, Entries: []store.NewEntry{{GUID: "yt:video:sourceBvid1", URL: watchURL + "sourceBvid1",
			Title: "B", PublishedAt: t0}}}); err != nil {
		t.Fatal(err)
	}
	mustNoErr(t, e.s.SetFeedTranscripts(t.Context(), e.feed.ID, false))
	drainTranscriptWake(e.s)
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) { return transcriptOf(id), nil })
	e.run(t, tr)
	e.waitIdle(t, 1)
	if got := tr.Calls(); !slices.Equal(got, []string{"sourceBvid1"}) {
		t.Fatalf("requests with source A off = %q, want only B's video", got)
	}
	mustNoErr(t, e.s.SetFeedTranscripts(t.Context(), e.feed.ID, true))
	waitFor(t, "source A's video", func() bool { return len(tr.Calls()) == 2 })
	if got := tr.Calls(); !slices.Equal(got, []string{"sourceBvid1", "sourceAvid1"}) {
		t.Errorf("requests after switching A back on = %q, want B's then A's", got)
	}
	if err := e.s.SetFeedTranscripts(t.Context(), other.ID+100, true); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("SetFeedTranscripts(unknown) err = %v, want ErrNotFound", err)
	}
}

// TestRequestTranscript_Rules: the "Fetch transcript" button queues a
// YouTube video's transcript only while every switch is on for it, never a
// fetched one again, and wakes the worker.
func TestRequestTranscript_Rules(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	ctx := t.Context()
	ids := e.queue(t, "requested01", "fetched0001")
	mustNoErr(t, e.st.RecordTranscript(ctx, ids[0], store.TranscriptResult{Outcome: store.TranscriptNoCaptions,
		Reason: store.ReasonNoCaptions, Requested: true}))
	mustNoErr(t, e.st.RecordTranscript(ctx, ids[1], store.TranscriptResult{Outcome: store.TranscriptOK,
		Transcript: store.Transcript{Text: "x"}, Requested: true}))
	blog := addFeedOfKind(t, e.st, store.KindRSS, "https://blog.example/feed", t0.Add(time.Hour))
	_, err := e.st.RecordFetchSuccess(ctx, store.FetchSuccess{FeedID: blog.ID, FetchedAt: t0,
		Entries: []store.NewEntry{{GUID: "post1", URL: watchURL + "lookslikeit", Title: "Post", PublishedAt: t0}}})
	mustNoErr(t, err)
	post := feedEntries(t, e.st, blog.ID)["post1"].ID
	_, err = e.st.RecordFetchSuccess(ctx, store.FetchSuccess{FeedID: e.feed.ID, FetchedAt: t0,
		Entries: []store.NewEntry{{GUID: "not-a-video", URL: "https://example.com/x", Title: "X", PublishedAt: t0}}})
	mustNoErr(t, err)
	notVideo := feedEntries(t, e.st, e.feed.ID)["not-a-video"].ID

	for _, tc := range []struct {
		name string
		id   int64
		want error
	}{
		{"an entry of a blog", post, ErrNotAVideo},
		{"a YouTube entry without a video URL", notVideo, ErrNotAVideo},
		{"a fetched transcript", ids[1], store.ErrConflict},
		{"an unknown entry", ids[1] + 100, store.ErrNotFound},
	} {
		if err := e.s.RequestTranscript(ctx, tc.id); !errors.Is(err, tc.want) {
			t.Errorf("RequestTranscript(%s) err = %v, want %v", tc.name, err, tc.want)
		}
	}
	for _, off := range []func() error{
		func() error { return e.s.SetTranscripts(ctx, false) },
		func() error { return e.s.SetFeedTranscripts(ctx, e.feed.ID, false) },
	} {
		mustNoErr(t, e.s.SetTranscripts(ctx, true))
		mustNoErr(t, e.s.SetFeedTranscripts(ctx, e.feed.ID, true))
		mustNoErr(t, off())
		if err := e.s.RequestTranscript(ctx, ids[0]); !errors.Is(err, ErrTranscriptsOff) {
			t.Errorf("RequestTranscript with a switch off: err = %v, want ErrTranscriptsOff", err)
		}
	}
	mustNoErr(t, e.s.SetTranscripts(ctx, true))
	mustNoErr(t, e.s.SetFeedTranscripts(ctx, e.feed.ID, true))
	drainTranscriptWake(e.s)
	mustNoErr(t, e.s.RequestTranscript(ctx, ids[0]))
	if len(e.s.transcriptWake) != 1 {
		t.Error("RequestTranscript did not wake the worker")
	}
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) { return transcriptOf(id), nil })
	e.run(t, tr)
	e.waitIdle(t, 1)
	if got := tr.Calls(); !slices.Equal(got, []string{"requested01"}) {
		t.Errorf("requests = %q, want the requested video", got)
	}
}

// TestRequestTranscript_CannotHurryTheWorker: pressing the button again and
// again during a refusal's pause, or right after a request, makes no request
// sooner: the pause and TranscriptDelay still hold.
func TestRequestTranscript_CannotHurryTheWorker(t *testing.T) {
	e := newTranscriptEnv(t, config.TranscriptBlockedPause)
	ctx := t.Context()
	ids := e.queue(t, "spammedvid1")
	var calls atomic.Int64
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) {
		if calls.Add(1) == 1 {
			return transcript.Transcript{}, refused(0)
		}
		return transcriptOf(id), nil
	})
	e.run(t, tr)
	waitFor(t, "the pause after the refusal", e.sleep.Holding)
	for range 5 {
		mustNoErr(t, e.s.RequestTranscript(ctx, ids[0]))
	}
	e.sleep.release()
	e.waitIdle(t, 1)
	times := tr.Times()
	if len(times) != 2 || times[1].Sub(times[0]) < config.TranscriptBlockedPause {
		t.Errorf("requests at %v, want 2, the second after the pause of %s", times, config.TranscriptBlockedPause)
	}
}

// TestRunTranscripts_ReasonOfEachOutcome is the retry table as stored: for
// each answer of the transcript client, the state the UI reads after one
// check, and after the checks that end it.
func TestRunTranscripts_ReasonOfEachOutcome(t *testing.T) {
	type state struct {
		status   store.TranscriptStatus
		reason   store.TranscriptReason
		attempts int
	}
	other := errors.New("connection reset")
	unavailable := fmt.Errorf("player: %w", transcript.ErrUnavailable)
	for _, tc := range []struct {
		name        string
		answer      error // every answer
		first, last state
	}{
		{"transcript", nil, state{store.StatusOK, "", 0}, state{store.StatusOK, "", 0}},
		{"no captions", transcript.ErrNoCaptions,
			state{store.StatusPending, store.ReasonNoCaptions, 0}, state{store.StatusNoCaptions, store.ReasonNoCaptions, 0}},
		{"not aired", transcript.ErrNotReady,
			state{store.StatusPending, store.ReasonNotReady, 0}, state{store.StatusFailed, store.ReasonQueuedLong, 0}},
		{"too large", transcript.ErrTooLarge,
			state{store.StatusFailed, store.ReasonTooLarge, 1}, state{store.StatusFailed, store.ReasonTooLarge, 1}},
		{"unavailable", unavailable,
			state{store.StatusPending, store.ReasonUnavailable, 1},
			state{store.StatusFailed, store.ReasonUnavailable, config.TranscriptMaxAttempts}},
		{"other error", other,
			state{store.StatusPending, store.ReasonError, 1},
			state{store.StatusFailed, store.ReasonExhausted, config.TranscriptMaxAttempts}},
		{"refused", refused(0),
			state{store.StatusPending, store.ReasonRefused, 0}, state{store.StatusFailed, store.ReasonQueuedLong, 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A refusal's pause is held (the others are timers) so that
			// the state after it can be read.
			e := newTranscriptEnv(t, config.TranscriptBlockedPause)
			ids := e.queue(t, "reasonvideo")
			e.timers.setManual()
			stateOf := func() state {
				st, err := e.st.TranscriptStateOf(t.Context(), ids[0])
				mustNoErr(t, err)
				return state{st.Status, st.Reason, st.Attempts}
			}
			tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) {
				if tc.answer == nil {
					return transcriptOf(id), nil
				}
				return transcript.Transcript{}, tc.answer
			})
			e.run(t, tr)
			waitFor(t, "the first outcome", func() bool { return len(e.st.Records()) >= 1 })
			waitFor(t, "the worker to wait", func() bool { return e.timers.Waiting() || e.st.Idle() > 0 || e.sleep.Holding() })
			if got := stateOf(); got != tc.first {
				t.Errorf("after the first check: %+v, want %+v", got, tc.first)
			}
			if e.sleep.Holding() {
				e.sleep.release()
			}
			for e.st.Idle() == 0 {
				if e.timers.Waiting() {
					e.timers.fire()
				}
				time.Sleep(time.Millisecond)
			}
			if got := stateOf(); got != tc.last {
				t.Errorf("at the end: %+v, want %+v", got, tc.last)
			}
		})
	}
}

// TestRunTranscripts_QueuedTooLongLeavesTheRefusalStreak: giving a video up
// after TranscriptMaxQueued makes no request, so it tells nothing about
// YouTube: the streak of refusals goes on.
func TestRunTranscripts_QueuedTooLongLeavesTheRefusalStreak(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	e.queue(t, "staleevideo")
	e.clock.Set(t0.Add(config.TranscriptMaxQueued))
	e.queue(t, "freshvideo1")
	var calls int
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) {
		calls++ // only the worker calls Fetch, one at a time
		if calls == 1 {
			return transcript.Transcript{}, refused(0)
		}
		return transcriptOf(id), nil
	})
	// The stale video, queued first, is given up on without a request;
	// the fresh one is refused, and fetched after the refusal's pause.
	e.run(t, tr)
	e.waitIdle(t, 1)
	if got := tr.Calls(); !slices.Equal(got, []string{"freshvideo1", "freshvideo1"}) {
		t.Fatalf("requests = %q, want the fresh video twice (refused, then fetched)", got)
	}
	lines := transcriptLines(t, e.log, "video transcript")
	var streak []int
	for _, l := range lines {
		if l.Outcome == logBlocked {
			streak = append(streak, l.Refusals)
		}
	}
	if !slices.Equal(streak, []int{1}) {
		t.Errorf("refusals logged = %v, want [1]", streak)
	}
	if got := e.sleep.Pauses(); len(got) == 0 || got[0] != config.TranscriptBlockedPause {
		t.Errorf("pauses = %v, want the refusal's pause first", got)
	}
}

// TestRunTranscripts_ServerSwitchTurnedBackOnThaws: a video queued, then
// frozen for longer than TranscriptMaxQueued because the server ran with
// FR_TRANSCRIPTS=false, is fetched once the server runs with it on again:
// the worker records the server's switch at start and, when it turned on,
// counts the queued videos' age from then, as turning the global setting
// back on does.
func TestRunTranscripts_ServerSwitchTurnedBackOnThaws(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	e.queue(t, "frozenlong1")
	e.s.cfg.Transcripts = false
	stop := e.run(t, e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) {
		t.Errorf("request for %s with the server's switch off", id)
		return transcriptOf(id), nil
	}))
	waitFor(t, "the worker to record the server's switch", func() bool {
		on, err := e.st.ServerTranscripts(t.Context())
		return err == nil && !on
	})
	stop()

	after := e.restart(t, t0.Add(config.TranscriptMaxQueued+time.Hour))
	tr := after.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) { return transcriptOf(id), nil })
	after.run(t, tr)
	after.waitIdle(t, 1)
	if got := tr.Calls(); !slices.Equal(got, []string{"frozenlong1"}) {
		t.Errorf("requests after the server's switch turned back on = %q, want the frozen video", got)
	}
}

// failingServerRecord fails the first fails RecordServerTranscripts calls
// and counts the queue reads made meanwhile.
type failingServerRecord struct {
	*transcriptStore
	fails          int
	calls          atomic.Int64
	readsBeforeOK  atomic.Int64
	recordedServer atomic.Bool
}

func (s *failingServerRecord) RecordServerTranscripts(ctx context.Context, on bool, at time.Time) error {
	if s.calls.Add(1) <= int64(s.fails) {
		return errors.New("database is locked")
	}
	err := s.transcriptStore.RecordServerTranscripts(ctx, on, at)
	if err == nil {
		s.recordedServer.Store(true)
	}
	return err
}

func (s *failingServerRecord) NextTranscript(ctx context.Context) (store.TranscriptJob, bool, error) {
	if !s.recordedServer.Load() {
		s.readsBeforeOK.Add(1)
	}
	return s.transcriptStore.NextTranscript(ctx)
}

// TestRunTranscripts_ServerSwitchRecordIsRetried: when the server's switch
// cannot be recorded at start (the store fails), the worker retries it
// storeRetryDelay apart and reads no queue meanwhile, so a video frozen
// while the server's switch was off is not given up on before the thaw.
func TestRunTranscripts_ServerSwitchRecordIsRetried(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	e.queue(t, "frozenlong2")
	mustNoErr(t, e.st.RecordServerTranscripts(t.Context(), false, t0))

	after := e.restart(t, t0.Add(config.TranscriptMaxQueued+time.Hour))
	const fails = 3
	fs := &failingServerRecord{transcriptStore: after.st, fails: fails}
	after.s.st = fs
	tr := after.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) { return transcriptOf(id), nil })
	after.run(t, tr)
	after.waitIdle(t, 1)
	if got := tr.Calls(); !slices.Equal(got, []string{"frozenlong2"}) {
		t.Errorf("requests = %q, want the frozen video", got)
	}
	if n := fs.calls.Load(); n != fails+1 {
		t.Errorf("%d attempts to record the server's switch, want %d", n, fails+1)
	}
	if n := fs.readsBeforeOK.Load(); n != 0 {
		t.Errorf("%d queue reads before the server's switch was recorded, want 0", n)
	}
	if got := after.sleep.Pauses(); len(got) < fails || !slices.Equal(got[:fails], slices.Repeat([]time.Duration{storeRetryDelay}, fails)) {
		t.Errorf("pauses = %v, want %d of %s first", got, fails, storeRetryDelay)
	}
}

// TestRunTranscripts_ServerSwitchRecordStopsOnCancel: retrying the record
// ends with the worker.
func TestRunTranscripts_ServerSwitchRecordStopsOnCancel(t *testing.T) {
	e := newTranscriptEnv(t, storeRetryDelay)
	fs := &failingServerRecord{transcriptStore: e.st, fails: math.MaxInt}
	e.s.st = fs
	stop := e.run(t, e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) { return transcriptOf(id), nil }))
	waitFor(t, "the retry pause", e.sleep.Holding)
	stop() // returns: RunTranscripts ended
	if n := fs.readsBeforeOK.Load(); n != 0 {
		t.Errorf("%d queue reads without the server's switch recorded", n)
	}
}

// drainTranscriptWake empties the worker's wake-up channel, so that a test
// sees only the wake-ups that come after.
func drainTranscriptWake(s *Scheduler) {
	for len(s.transcriptWake) > 0 {
		<-s.transcriptWake
	}
}

// TestPoll_PendingFirstSuccessQueuesNoTranscripts: the first successful
// fetch of a pending subscription is stored as an initial add, which never
// queues transcripts, even with every switch on; the polls after it queue
// their new videos as usual.
func TestPoll_PendingFirstSuccessQueuesNoTranscripts(t *testing.T) {
	st := openStore(t)
	clock := newClock(t0)
	s, fetcher, feed := pendingSubscription(t, st, clock)
	if !s.cfg.Transcripts {
		t.Fatal("the server's switch is off")
	}
	mustNoErr(t, st.SetTranscriptsSetting(t.Context(), true, t0))
	if on, err := st.FeedTranscripts(t.Context(), feed.ID); err != nil || !on {
		t.Fatalf("FeedTranscripts = %v, %v", on, err)
	}
	drainTranscriptWake(s)
	items := itemsNewestFirst("v", 3*config.InitialUnread, t0)
	fetcher.setRespond(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		return ok(r.URL, atomDoc(chanTitle, items...)), nil
	})
	clock.Set(getFeed(t, st, feed.ID).NextFetchAt)
	pollOnce(t, s, st, feed.ID)
	if got := getFeed(t, st, feed.ID); got.InitialPending || len(feedEntries(t, st, feed.ID)) != len(items) {
		t.Fatalf("the first success was not stored: %+v", got)
	}
	if job, ok, err := st.NextTranscript(t.Context()); ok || err != nil {
		t.Errorf("the first success of a pending feed queued %+v, %v", job, err)
	}
	if len(s.transcriptWake) != 0 {
		t.Error("the first success of a pending feed woke the transcript worker")
	}

	more := append(itemsNewestFirst("w", 1, t0.Add(time.Hour)), items...)
	fetcher.setRespond(func(_ context.Context, r fetch.Request) (*fetch.Result, error) {
		return ok(r.URL, atomDoc(chanTitle, more...)), nil
	})
	clock.Set(getFeed(t, st, feed.ID).NextFetchAt)
	pollOnce(t, s, st, feed.ID)
	if job, ok, err := st.NextTranscript(t.Context()); !ok || err != nil || job.EntryID != feedEntries(t, st, feed.ID)["w0"].ID {
		t.Errorf("the next poll queued %+v, %v, %v; want the new video", job, ok, err)
	}
}
