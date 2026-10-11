package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/store"
	"github.com/giacomomicoli/feedreader/internal/transcript"
)

const (
	channelURL = "https://www.youtube.com/feeds/videos.xml?channel_id=UCtestchannel0000000000"
	watchURL   = "https://www.youtube.com/watch?v="
)

// fakeTranscriber answers Fetch with respond and records the video ids it
// was asked for, when (by clock, if set), and how many fetches ran at once.
type fakeTranscriber struct {
	mu        sync.Mutex
	respond   func(ctx context.Context, id string) (transcript.Transcript, error)
	clock     *testClock
	calls     []string
	times     []time.Time
	active    int
	maxActive int
}

func newTranscriber(respond func(ctx context.Context, id string) (transcript.Transcript, error)) *fakeTranscriber {
	return &fakeTranscriber{respond: respond}
}

// transcriptOf is the fake transcript of video id.
func transcriptOf(id string) transcript.Transcript {
	return transcript.Transcript{Lang: "en", Generated: true, Text: "Transcript of " + id + "\nsecond line"}
}

// answering returns a transcriber that answers each video id from results,
// and a transcript for the others.
func answering(results map[string]error) *fakeTranscriber {
	return newTranscriber(func(_ context.Context, id string) (transcript.Transcript, error) {
		if err := results[id]; err != nil {
			return transcript.Transcript{}, err
		}
		return transcriptOf(id), nil
	})
}

func (f *fakeTranscriber) Fetch(ctx context.Context, id string) (transcript.Transcript, error) {
	f.mu.Lock()
	f.calls = append(f.calls, id)
	if f.clock != nil {
		f.times = append(f.times, f.clock.Now())
	}
	f.active++
	f.maxActive = max(f.maxActive, f.active)
	respond := f.respond
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.active--
		f.mu.Unlock()
	}()
	return respond(ctx, id)
}

func (f *fakeTranscriber) Calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

// Times returns when each request was made, by the env's clock.
func (f *fakeTranscriber) Times() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.times)
}

func (f *fakeTranscriber) MaxActive() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxActive
}

// sleeper replaces Scheduler.sleep: it records every pause and returns at
// once, except for pauses of length hold, which last until release (or the
// end of ctx). When a pause ends, the clock has moved on by its length.
type sleeper struct {
	mu      sync.Mutex
	clock   *testClock
	pauses  []time.Duration
	hold    time.Duration
	holding bool
	gate    chan struct{}
}

func newSleeper(clock *testClock, hold time.Duration) *sleeper {
	return &sleeper{clock: clock, hold: hold, gate: make(chan struct{})}
}

func (s *sleeper) sleep(ctx context.Context, d time.Duration) {
	s.mu.Lock()
	s.pauses = append(s.pauses, d)
	held := s.hold > 0 && d == s.hold
	s.holding = held
	s.mu.Unlock()
	if held {
		select {
		case <-s.gate:
		case <-ctx.Done():
		}
	}
	s.mu.Lock()
	s.holding = false
	s.clock.Advance(d)
	s.mu.Unlock()
}

func (s *sleeper) Pauses() []time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.pauses)
}

func (s *sleeper) Holding() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.holding
}

func (s *sleeper) release() { close(s.gate) }

// timers replaces Scheduler.transcriptTimer, which the worker waits on
// until a queued video falls due. It records every wait. An automatic
// timer fires at once, the clock moved on by its length; a manual one
// fires only when the test calls fire, unless the worker stops it first
// (it stopped waiting for another reason).
type timers struct {
	mu       sync.Mutex
	clock    *testClock
	manual   bool
	waits    []time.Duration
	pending  chan time.Time // the manual timer that has not fired yet
	pendingD time.Duration
}

func newTimers(clock *testClock) *timers { return &timers{clock: clock} }

func (tm *timers) newTimer(d time.Duration) (<-chan time.Time, func() bool) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.waits = append(tm.waits, d)
	c := make(chan time.Time, 1)
	if !tm.manual {
		tm.clock.Advance(d)
		c <- tm.clock.Now()
		return c, func() bool { return false }
	}
	tm.pending, tm.pendingD = c, d
	return c, func() bool {
		tm.mu.Lock()
		defer tm.mu.Unlock()
		if tm.pending != c {
			return false
		}
		tm.pending = nil
		return true
	}
}

// setManual makes the timers from now on fire only when fire is called.
func (tm *timers) setManual() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.manual = true
}

// Waiting reports whether the worker waits on a manual timer.
func (tm *timers) Waiting() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.pending != nil
}

// fire fires the manual timer the worker waits on, moving the clock on by
// its length.
func (tm *timers) fire() {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.clock.Advance(tm.pendingD)
	tm.pending <- tm.clock.Now()
	tm.pending = nil
}

func (tm *timers) Waits() []time.Duration {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return slices.Clone(tm.waits)
}

// recorded is one RecordTranscript call.
type recorded struct {
	entryID int64
	result  store.TranscriptResult
}

// transcriptStore wraps a store: it records the outcomes the worker stores,
// tells when the worker found the queue empty, and can fail calls.
type transcriptStore struct {
	store.Store
	mu        sync.Mutex
	records   []recorded
	idle      int   // NextTranscript calls that found nothing queued
	nextErr   error // returned by the next NextTranscript call, once
	recordErr error // returned by the next RecordTranscript call, once
}

func (s *transcriptStore) NextTranscript(ctx context.Context) (store.TranscriptJob, bool, error) {
	s.mu.Lock()
	err := s.nextErr
	s.nextErr = nil
	s.mu.Unlock()
	if err != nil {
		return store.TranscriptJob{}, false, err
	}
	job, ok, err := s.Store.NextTranscript(ctx)
	if err == nil && !ok {
		s.mu.Lock()
		s.idle++
		s.mu.Unlock()
	}
	return job, ok, err
}

func (s *transcriptStore) RecordTranscript(ctx context.Context, entryID int64, r store.TranscriptResult) error {
	s.mu.Lock()
	err := s.recordErr
	s.recordErr = nil
	s.mu.Unlock()
	if err != nil {
		return err
	}
	if err := s.Store.RecordTranscript(ctx, entryID, r); err != nil {
		return err
	}
	s.mu.Lock()
	s.records = append(s.records, recorded{entryID, r})
	s.mu.Unlock()
	return nil
}

func (s *transcriptStore) Records() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.records)
}

func (s *transcriptStore) Idle() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.idle
}

// outcomes returns the outcome recorded for each entry, in order.
func outcomes(recs []recorded, entryID int64) []store.TranscriptOutcome {
	var out []store.TranscriptOutcome
	for _, r := range recs {
		if r.entryID == entryID {
			out = append(out, r.result.Outcome)
		}
	}
	return out
}

// transcriptEnv is a scheduler with transcripts on over a real store, with
// a test clock that only moves when the worker pauses (sleep) or waits for
// a video to fall due (timers).
type transcriptEnv struct {
	st     *transcriptStore
	s      *Scheduler
	clock  *testClock
	sleep  *sleeper
	timers *timers
	log    *logCapture
	feed   store.Feed
}

func newTranscriptEnv(t *testing.T, hold time.Duration) *transcriptEnv {
	t.Helper()
	st := &transcriptStore{Store: openStore(t)}
	cfg := config.Default()
	cfg.Transcripts = true
	s := New(st, staticFetcher(nil, errors.New("no feed fetch expected")), cfg, nil)
	clock := newClock(t0)
	s.now = clock.Now
	sl, tm := newSleeper(clock, hold), newTimers(clock)
	s.sleep, s.transcriptTimer = sl.sleep, tm.newTimer
	e := &transcriptEnv{st: st, s: s, clock: clock, sleep: sl, timers: tm, log: captureLog(t, s)}
	e.feed = addFeedOfKind(t, st, store.KindYouTube, channelURL, t0.Add(time.Hour))
	if err := st.SetTranscriptsSetting(t.Context(), true, t0); err != nil {
		t.Fatal(err)
	}
	return e
}

// restart is the env after the process stopped and started again at at: a
// new scheduler, clock, pauses and log over the same database.
func (e *transcriptEnv) restart(t *testing.T, at time.Time) *transcriptEnv {
	t.Helper()
	cfg := config.Default()
	cfg.Transcripts = true
	s := New(e.st, staticFetcher(nil, errors.New("no feed fetch expected")), cfg, nil)
	clock := newClock(at)
	s.now = clock.Now
	sl, tm := newSleeper(clock, 0), newTimers(clock)
	s.sleep, s.transcriptTimer = sl.sleep, tm.newTimer
	return &transcriptEnv{st: e.st, s: s, clock: clock, sleep: sl, timers: tm, log: captureLog(t, s), feed: e.feed}
}

// transcriber is newTranscriber with request times read off the env's clock.
func (e *transcriptEnv) transcriber(respond func(ctx context.Context, id string) (transcript.Transcript, error)) *fakeTranscriber {
	tr := newTranscriber(respond)
	tr.clock = e.clock
	return tr
}

// queue stores videos as new entries of a poll of the env's feed, their
// transcripts queued in the order given, and returns their entry ids.
func (e *transcriptEnv) queue(t *testing.T, videos ...string) []int64 {
	t.Helper()
	now := e.clock.Now()
	entries := make([]store.NewEntry, len(videos))
	for i, v := range videos {
		// The store inserts a fetch's entries from last to first, and the
		// lowest id goes first in the queue.
		entries[len(videos)-1-i] = store.NewEntry{GUID: "yt:video:" + v, URL: watchURL + v, Title: "Video " + v,
			PublishedAt: now}
	}
	n, err := e.st.RecordFetchSuccess(t.Context(), store.FetchSuccess{FeedID: e.feed.ID, FetchedAt: now,
		Entries: entries, QueueTranscripts: true})
	if err != nil || n != len(videos) {
		t.Fatalf("RecordFetchSuccess = %d, %v; want %d inserted", n, err, len(videos))
	}
	ids := make([]int64, len(videos))
	for i, v := range videos {
		ids[i] = feedEntries(t, e.st, e.feed.ID)["yt:video:"+v].ID
	}
	return ids
}

// run starts RunTranscripts in the background; stop cancels it and waits
// for it to return (also at cleanup).
func (e *transcriptEnv) run(t *testing.T, tr Transcriber) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.s.RunTranscripts(ctx, tr) }()
	waitFor(t, "RunTranscripts to start", e.s.transcribing.Load)
	stop = sync.OnceFunc(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("RunTranscripts: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("RunTranscripts did not return after cancel")
		}
	})
	t.Cleanup(stop)
	return stop
}

// waitIdle waits until the worker has found the queue empty n times.
func (e *transcriptEnv) waitIdle(t *testing.T, n int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("the worker to find the queue empty %d times", n), func() bool { return e.st.Idle() >= n })
}

// transcriptLine is one "video transcript" log line.
type transcriptLine struct {
	Level    slog.Level    `json:"level"`
	Msg      string        `json:"msg"`
	Entry    int64         `json:"entry"`
	Video    string        `json:"video"`
	Outcome  string        `json:"outcome"`
	Took     time.Duration `json:"took"`
	Err      string        `json:"err"`
	Attempts int           `json:"attempts"`
	Pause    time.Duration `json:"pause"`
	Refusals int           `json:"refusals"`
	RetryAt  time.Time     `json:"retry_at"`
}

// transcriptLines decodes the log lines with message msg.
func transcriptLines(t *testing.T, c *logCapture, msg string) []transcriptLine {
	t.Helper()
	var out []transcriptLine
	dec := json.NewDecoder(strings.NewReader(c.String()))
	for {
		var l transcriptLine
		err := dec.Decode(&l)
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("decoding the log: %v", err)
		}
		if l.Msg == msg {
			out = append(out, l)
		}
	}
}

// TestRunTranscripts_RecordsEachOutcomeOneRequestAtATime: a transcript is
// stored; a video without captions (or with an empty text) is checked once
// more TranscriptRecheckDelay later, since YouTube may still be making its
// automatic captions, and is then done without one.
func TestRunTranscripts_RecordsEachOutcomeOneRequestAtATime(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	videos := []string{"okokokokok1", "nocaptions1", "wrappednone", "emptytext01"}
	ids := e.queue(t, videos...)
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) {
		switch id {
		case "nocaptions1":
			return transcript.Transcript{}, transcript.ErrNoCaptions
		case "wrappednone":
			return transcript.Transcript{}, fmt.Errorf("caption tracks: %w", transcript.ErrNoCaptions)
		case "emptytext01":
			return transcript.Transcript{Lang: "en", Text: " \n "}, nil
		}
		return transcriptOf(id), nil
	})
	e.run(t, tr)
	e.waitIdle(t, 1)

	// Oldest queued first; the ones without captions again, in the order
	// they fell due, each TranscriptRecheckDelay after its first check.
	if got, want := tr.Calls(), append(slices.Clone(videos), videos[1:]...); !slices.Equal(got, want) {
		t.Fatalf("requests = %q, want %q", got, want)
	}
	times := tr.Times()
	for i := range len(videos) - 1 {
		if d := times[len(videos)+i].Sub(times[1+i]); d != config.TranscriptRecheckDelay {
			t.Errorf("%s checked again %s after its first check, want %s", videos[1+i], d, config.TranscriptRecheckDelay)
		}
	}
	if n := tr.MaxActive(); n != 1 {
		t.Errorf("%d transcript requests at once, want 1", n)
	}
	recs := e.st.Records()
	if got := outcomes(recs, ids[0]); !slices.Equal(got, []store.TranscriptOutcome{store.TranscriptOK}) {
		t.Errorf("entry %d: outcomes %v, want ok", ids[0], got)
	}
	for _, id := range ids[1:] {
		if got, want := outcomes(recs, id), []store.TranscriptOutcome{store.TranscriptRecheck, store.TranscriptNoCaptions}; !slices.Equal(got, want) {
			t.Errorf("entry %d: outcomes %v, want %v", id, got, want)
		}
	}
	got, err := e.st.GetTranscript(t.Context(), ids[0])
	if want := (store.Transcript{Lang: "en", Generated: true, Text: transcriptOf("okokokokok1").Text}); err != nil || got != want {
		t.Errorf("stored transcript = %+v, %v; want %+v", got, err, want)
	}
	// A pause of TranscriptDelay after every request, and nothing else.
	if got, want := e.sleep.Pauses(), slices.Repeat([]time.Duration{config.TranscriptDelay}, len(tr.Calls())); !slices.Equal(got, want) {
		t.Errorf("pauses = %v, want %v", got, want)
	}

	// One log line per outcome, at info level: none of them is a failure.
	lines := transcriptLines(t, e.log, "video transcript")
	wantOutcomes := []string{"ok", "recheck", "recheck", "recheck", "no_captions", "no_captions", "no_captions"}
	if len(lines) != len(wantOutcomes) {
		t.Fatalf("%d transcript log lines, want %d:\n%s", len(lines), len(wantOutcomes), e.log.String())
	}
	for i, l := range lines {
		if l.Level != slog.LevelInfo || l.Video != tr.Calls()[i] || l.Outcome != wantOutcomes[i] || l.Took < 0 {
			t.Errorf("log line %d = %+v, want video %s, outcome %s at info", i, l, tr.Calls()[i], wantOutcomes[i])
		}
		if wantRecheck := times[i].Add(config.TranscriptRecheckDelay); l.Outcome == "recheck" && !l.RetryAt.Equal(wantRecheck) {
			t.Errorf("log line %d: retry_at %v, want %v", i, l.RetryAt, wantRecheck)
		}
	}
}

// retrySchedule returns when the n requests for a video that keeps failing
// are made, the first at start: TranscriptRetryDelay after the first
// failure, twice as long after each further one, at most MaxBackoff.
func retrySchedule(start time.Time, n int) []time.Time {
	out := []time.Time{start}
	delay := config.TranscriptRetryDelay
	for len(out) < n {
		out = append(out, out[len(out)-1].Add(min(delay, config.MaxBackoff)))
		delay *= 2
	}
	return out
}

// ytReply is one answer of youTubeAnswers: a body served as JSON, or an
// error.
type ytReply struct {
	body string
	err  error
}

// youTubeAnswers stands in for YouTube behind the real transcript client:
// the n-th player request gets the n-th of players (the last one over and
// over), a caption request the same synthetic captions for every video. It
// records when, by clock, each player request was made.
type youTubeAnswers struct {
	mu      sync.Mutex
	clock   *testClock
	players []ytReply
	// captions, when set, answers every caption request instead.
	captions *ytReply
	times    []time.Time
}

// The URLs the transcript client requests, and what YouTube answers with.
const (
	ytPlayerURL  = "https://www.youtube.com/youtubei/v1/player?prettyPrint=false"
	ytCaptionURL = "https://www.youtube.com/api/timedtext?v="
	ytJSON       = "application/json; charset=UTF-8"
	ytCaptions   = `{"events":[{"tStartMs":0,"segs":[{"utf8":"Synthetic words."}]}]}`
)

// playerAnswer is a synthetic player response for video with the given
// playability status and videoDetails members (details, may be empty),
// listing one caption track when captions is set.
func playerAnswer(video, status, details string, captions bool) string {
	body := `{"playabilityStatus":{"status":"` + status + `"},"videoDetails":{` + details
	if details != "" {
		body += ","
	}
	body += `"videoId":"` + video + `"}`
	if captions {
		body += `,"captions":{"playerCaptionsTracklistRenderer":{"captionTracks":[{"baseUrl":"` + ytCaptionURL + video +
			`&lang=en","languageCode":"en","kind":"asr"}]}}`
	}
	return body + "}"
}

func (y *youTubeAnswers) Fetch(_ context.Context, r fetch.Request) (*fetch.Result, error) {
	y.mu.Lock()
	defer y.mu.Unlock()
	var a ytReply
	switch {
	case r.URL == ytPlayerURL:
		y.times = append(y.times, y.clock.Now())
		a = y.players[min(len(y.times), len(y.players))-1]
	case strings.HasPrefix(r.URL, ytCaptionURL) && y.captions != nil:
		a = *y.captions
	case strings.HasPrefix(r.URL, ytCaptionURL):
		a = ytReply{body: ytCaptions}
	default:
		return nil, &fetch.StatusError{URL: r.URL, StatusCode: http.StatusNotFound}
	}
	if a.err != nil {
		return nil, a.err
	}
	return &fetch.Result{StatusCode: http.StatusOK, Body: []byte(a.body), ContentType: ytJSON, FinalURL: r.URL}, nil
}

// Times returns when each player request was made.
func (y *youTubeAnswers) Times() []time.Time {
	y.mu.Lock()
	defer y.mu.Unlock()
	return slices.Clone(y.times)
}

// TestRunTranscripts_RetriesHoursApartThenGivesUp: through the real
// transcript client, a request that fails for a reason that may pass (an
// error from YouTube or the network, an answer this package does not
// understand, a video YouTube does not play to this client) is retried
// TranscriptRetryDelay later, then twice as long after each further
// failure, until TranscriptMaxAttempts requests have failed. So an outage
// of the connection or of YouTube that lasts minutes does not use the
// attempts up, and the video is never requested before it is due.
func TestRunTranscripts_RetriesHoursApartThenGivesUp(t *testing.T) {
	const video = "brokenpage1"
	for _, tt := range []struct {
		name     string
		answer   ytReply
		captions *ytReply // nil: synthetic captions
		wantErr  string   // in the log line
	}{
		{"YouTube error", ytReply{err: &fetch.StatusError{URL: ytPlayerURL, StatusCode: http.StatusServiceUnavailable}}, nil, "HTTP 503"},
		{"network down", ytReply{err: errors.New("dial tcp: lookup www.youtube.com: no such host")}, nil, "no such host"},
		// transcript.ErrUnavailable: YouTube answers the same way when it
		// stops serving this kind of client, so it is not taken as final
		// at once although it matches transcript.ErrNoCaptions.
		{"unavailable video", ytReply{body: playerAnswer(video, "ERROR", "", false)}, nil, "unavailable"},
		{"changed answer", ytReply{body: `{"playabilityStatus":{"status":"SOMETHING_NEW"}}`}, nil, "SOMETHING_NEW"},
		// A renamed field is not a video without captions, which would
		// be final after one more check, silently, for every video.
		{"renamed track URL", ytReply{body: strings.Replace(playerAnswer(video, "OK", "", true), `"baseUrl":`, `"url":`, 1)},
			nil, "no usable caption track"},
		{"renamed caption text", ytReply{body: playerAnswer(video, "OK", "", true)},
			&ytReply{body: strings.ReplaceAll(ytCaptions, `"utf8":`, `"text":`)}, "no segment has a text"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTranscriptEnv(t, 0)
			ids := e.queue(t, video)
			yt := &youTubeAnswers{clock: e.clock, players: []ytReply{tt.answer}, captions: tt.captions}
			e.run(t, transcript.New(yt))
			e.waitIdle(t, 1)

			if got, want := yt.Times(), retrySchedule(t0, config.TranscriptMaxAttempts); !slices.EqualFunc(got, want, time.Time.Equal) {
				t.Errorf("requests at %v, want %v", got, want)
			}
			want := append(slices.Repeat([]store.TranscriptOutcome{store.TranscriptRetry}, config.TranscriptMaxAttempts-1),
				store.TranscriptFailed)
			if got := outcomes(e.st.Records(), ids[0]); !slices.Equal(got, want) {
				t.Errorf("outcomes = %v, want %v", got, want)
			}
			if _, ok, err := e.st.NextTranscript(t.Context()); ok || err != nil {
				t.Errorf("queue after giving up: job %v, %v; want empty", ok, err)
			}
			lines := transcriptLines(t, e.log, "video transcript")
			if len(lines) != config.TranscriptMaxAttempts {
				t.Fatalf("%d log lines, want %d", len(lines), config.TranscriptMaxAttempts)
			}
			if first := lines[0]; first.Outcome != "retry" || first.Level != slog.LevelWarn || first.Attempts != 1 ||
				!first.RetryAt.Equal(t0.Add(config.TranscriptRetryDelay)) || !strings.Contains(first.Err, tt.wantErr) {
				t.Errorf("first log line = %+v, want a retry warning after 1 attempt, due again at %v",
					first, t0.Add(config.TranscriptRetryDelay))
			}
			last := lines[len(lines)-1]
			if last.Outcome != "failed" || last.Level != slog.LevelWarn || last.Attempts != config.TranscriptMaxAttempts ||
				!strings.Contains(last.Err, tt.wantErr) {
				t.Errorf("last log line = %+v, want a warning that gives up after %d attempts with the error", last, config.TranscriptMaxAttempts)
			}
		})
	}
}

// TestRunTranscripts_WaitsForARetryWithoutRequests: while the only queued
// video waits for its retry, the worker makes no request: it waits until
// the video falls due (at most maxSleep at a time, reading the queue again
// in between) or a poll queues another video, which goes first.
func TestRunTranscripts_WaitsForARetryWithoutRequests(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	e.timers.setManual()
	ids := e.queue(t, "brokenpage1")
	var failed bool
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) {
		if id == "brokenpage1" && !failed { // only the worker calls Fetch, one at a time
			failed = true
			return transcript.Transcript{}, errors.New("player request: HTTP 500")
		}
		return transcriptOf(id), nil
	})
	stop := e.run(t, tr)
	retryAt := t0.Add(config.TranscriptRetryDelay)

	waitFor(t, "the wait for the retry", e.timers.Waiting)
	if got := tr.Calls(); !slices.Equal(got, []string{"brokenpage1"}) {
		t.Fatalf("requests = %q, want the failed one only", got)
	}
	if got, want := e.timers.Waits(), []time.Duration{min(retryAt.Sub(e.clock.Now()), maxSleep)}; !slices.Equal(got, want) {
		t.Errorf("waits = %v, want %v", got, want)
	}

	// A wake-up with nothing new due: the worker reads the queue again and
	// goes back to waiting.
	e.s.signalTranscripts()
	waitFor(t, "the worker to wait again", func() bool { return len(e.timers.Waits()) == 2 && e.timers.Waiting() })
	if n := len(tr.Calls()); n != 1 {
		t.Fatalf("%d requests after a wake-up with nothing due, want 1", n)
	}
	// A poll queues a new video: it is fetched at once, before the retry.
	e.clock.Advance(time.Minute)
	newID := e.queue(t, "newvideo001")[0]
	e.s.signalTranscripts()
	waitFor(t, "the new video", func() bool { return len(e.st.Records()) == 2 })
	if got := tr.Calls(); !slices.Equal(got, []string{"brokenpage1", "newvideo001"}) {
		t.Fatalf("requests = %q, want the new video right after the failed one", got)
	}
	if got := tr.Times()[1]; !got.Before(retryAt) {
		t.Errorf("the new video was requested at %v, not before the retry at %v", got, retryAt)
	}

	// Then nothing until the retry falls due.
	for e.clock.Now().Before(retryAt) {
		waitFor(t, "a wait", e.timers.Waiting)
		if n := len(tr.Calls()); n != 2 {
			t.Fatalf("%d requests at %v, before the retry at %v", n, e.clock.Now(), retryAt)
		}
		e.timers.fire()
	}
	waitFor(t, "the retry", func() bool { return len(e.st.Records()) == 3 })
	if got := tr.Times(); len(got) != 3 || !got[2].Equal(retryAt) {
		t.Errorf("requests at %v, want the retry at %v", got, retryAt)
	}
	for _, d := range e.timers.Waits() {
		if d <= 0 || d > maxSleep {
			t.Errorf("waited %s at once, want at most %s", d, maxSleep)
		}
	}
	if got := outcomes(e.st.Records(), ids[0]); !slices.Equal(got, []store.TranscriptOutcome{store.TranscriptRetry, store.TranscriptOK}) {
		t.Errorf("outcomes of the retried video = %v", got)
	}
	if got := outcomes(e.st.Records(), newID); !slices.Equal(got, []store.TranscriptOutcome{store.TranscriptOK}) {
		t.Errorf("outcomes of the new video = %v", got)
	}

	// Nothing queued: no timer, only a wait for a poll, which stopping ends.
	e.waitIdle(t, 1)
	waits := len(e.timers.Waits())
	stop()
	if len(e.timers.Waits()) != waits {
		t.Error("the worker set a timer with nothing queued")
	}
}

// TestRunTranscripts_NotReadyIsCheckedAgainLater: through the real
// transcript client, a premiere or live stream that has not aired is
// checked again every TranscriptRecheckDelay without counting an attempt.
// Once it has aired, a first answer without captions is checked once more
// (YouTube makes them after the stream), then the transcript is stored.
func TestRunTranscripts_NotReadyIsCheckedAgainLater(t *testing.T) {
	const video = "premiere001"
	e := newTranscriptEnv(t, 0)
	ids := e.queue(t, video)
	yt := &youTubeAnswers{clock: e.clock, players: []ytReply{
		{body: playerAnswer(video, "LIVE_STREAM_OFFLINE", `"isUpcoming":true`, false)},
		{body: playerAnswer(video, "OK", `"isLive":true`, false)},
		{body: playerAnswer(video, "OK", `"isLiveContent":true`, false)},
		{body: playerAnswer(video, "OK", `"isLiveContent":true`, true)},
	}}
	e.run(t, transcript.New(yt))
	e.waitIdle(t, 1)

	want := make([]time.Time, len(yt.players))
	for i := range want {
		want[i] = t0.Add(time.Duration(i) * config.TranscriptRecheckDelay)
	}
	if got := yt.Times(); !slices.EqualFunc(got, want, time.Time.Equal) {
		t.Errorf("player requests at %v, want %v", got, want)
	}
	wantOutcomes := []store.TranscriptOutcome{store.TranscriptDeferred, store.TranscriptDeferred, store.TranscriptRecheck, store.TranscriptOK}
	if got := outcomes(e.st.Records(), ids[0]); !slices.Equal(got, wantOutcomes) {
		t.Errorf("outcomes = %v, want %v", got, wantOutcomes)
	}
	if tr, err := e.st.GetTranscript(t.Context(), ids[0]); err != nil || tr.Text != "Synthetic words." {
		t.Errorf("stored transcript = %+v, %v", tr, err)
	}
	// Checking again is normal: info lines, and no attempt counted.
	lines := transcriptLines(t, e.log, "video transcript")
	if len(lines) != len(wantOutcomes) {
		t.Fatalf("%d log lines, want %d", len(lines), len(wantOutcomes))
	}
	for i, l := range lines[:2] {
		if l.Outcome != "not_ready" || l.Level != slog.LevelInfo || l.Attempts != 0 ||
			!l.RetryAt.Equal(want[i].Add(config.TranscriptRecheckDelay)) {
			t.Errorf("log line %d = %+v, want not_ready at info, due again %s later", i, l, config.TranscriptRecheckDelay)
		}
	}
}

// notAiredChecks is how many times a stream that never airs is checked:
// every TranscriptRecheckDelay while it has been queued for less than
// TranscriptMaxQueued.
const notAiredChecks = int((config.TranscriptMaxQueued + config.TranscriptRecheckDelay - 1) / config.TranscriptRecheckDelay)

// TestRunTranscripts_NotReadyGivesUpAfterTheMaxWait: a stream that never
// airs (or never ends) is checked every TranscriptRecheckDelay until it has
// been queued for TranscriptMaxQueued, then given up on without a request.
func TestRunTranscripts_NotReadyGivesUpAfterTheMaxWait(t *testing.T) {
	const video = "waitingroom"
	e := newTranscriptEnv(t, 0)
	ids := e.queue(t, video)
	yt := &youTubeAnswers{clock: e.clock, players: []ytReply{{body: playerAnswer(video, "LIVE_STREAM_OFFLINE", "", false)}}}
	e.run(t, transcript.New(yt))
	e.waitIdle(t, 1)

	times := yt.Times()
	if len(times) != notAiredChecks || !times[len(times)-1].Equal(t0.Add(time.Duration(notAiredChecks-1)*config.TranscriptRecheckDelay)) {
		t.Errorf("player requests at %v, want %d, %s apart", times, notAiredChecks, config.TranscriptRecheckDelay)
	}
	want := append(slices.Repeat([]store.TranscriptOutcome{store.TranscriptDeferred}, notAiredChecks), store.TranscriptFailed)
	if got := outcomes(e.st.Records(), ids[0]); !slices.Equal(got, want) {
		t.Errorf("outcomes = %v, want %v", got, want)
	}
	lines := transcriptLines(t, e.log, "video transcript")
	if last := lines[len(lines)-1]; last.Outcome != "failed" || last.Level != slog.LevelWarn || last.Attempts != 1 ||
		last.Video != video || !strings.Contains(last.Err, "queued for too long") {
		t.Errorf("last log line = %+v, want a warning that gives up on the stream", last)
	}
}

// TestRunTranscripts_DueTimeFromBeforeTheClockWasSetBack: a video due
// further ahead than the worker ever sets was stored while the clock was
// ahead; it counts as due, so a clock set back cannot stall it.
func TestRunTranscripts_DueTimeFromBeforeTheClockWasSetBack(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	ids := e.queue(t, "clockahead1")
	ahead := e.clock.Now().Add(2 * max(config.MaxBackoff, config.TranscriptRecheckDelay))
	mustNoErr(t, e.st.RecordTranscript(t.Context(), ids[0], store.TranscriptResult{Outcome: store.TranscriptDeferred, RetryAt: ahead}))
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) { return transcriptOf(id), nil })
	e.run(t, tr)
	e.waitIdle(t, 1)
	if got := tr.Times(); len(got) != 1 || !got[0].Equal(t0) {
		t.Errorf("requests at %v, want one at once (%v)", got, t0)
	}
	if waits := e.timers.Waits(); len(waits) != 0 {
		t.Errorf("waits = %v, want none", waits)
	}
}

// TestRunTranscripts_BlockedPausesWithoutCountingAnAttempt: when YouTube
// refuses a request, the worker makes none for TranscriptBlockedPause, and
// the video stays queued with its attempts untouched, due again when the
// pause ends: behind the videos that were already due.
func TestRunTranscripts_BlockedPausesWithoutCountingAnAttempt(t *testing.T) {
	e := newTranscriptEnv(t, config.TranscriptBlockedPause)
	ids := e.queue(t, "blockedonce", "secondvideo")
	var blocks int
	tr := newTranscriber(func(_ context.Context, id string) (transcript.Transcript, error) {
		if id == "blockedonce" && blocks == 0 { // only the worker calls Fetch, one at a time
			blocks++
			return transcript.Transcript{}, fmt.Errorf("player request: %w", transcript.ErrBlocked)
		}
		return transcriptOf(id), nil
	})
	e.run(t, tr)
	waitFor(t, "the pause after the refusal", e.sleep.Holding)

	if got := tr.Calls(); !slices.Equal(got, []string{"blockedonce"}) {
		t.Errorf("requests before the pause = %q, want only the refused one", got)
	}
	// Still queued, its attempts untouched, due when the pause ends.
	if recs := e.st.Records(); len(recs) != 1 || recs[0].entryID != ids[0] ||
		recs[0].result.Outcome != store.TranscriptBlocked || !recs[0].result.RetryAt.Equal(t0.Add(config.TranscriptBlockedPause)) {
		t.Errorf("records during the pause = %+v, want the refused video blocked until %v", recs, t0.Add(config.TranscriptBlockedPause))
	}
	if job, ok, err := e.st.NextTranscript(t.Context()); err != nil || !ok || job.EntryID != ids[1] ||
		!job.DueAt.Equal(t0.Add(config.TranscriptBlockedPause)) {
		t.Errorf("queue during the pause: %+v, %v, %v; want the other video first, due when the pause ends", job, ok, err)
	}
	lines := transcriptLines(t, e.log, "video transcript")
	if len(lines) != 1 || lines[0].Outcome != "blocked" || lines[0].Level != slog.LevelWarn {
		t.Errorf("log lines = %+v, want one blocked warning", lines)
	}

	e.sleep.release()
	e.waitIdle(t, 1)
	if got, want := tr.Calls(), []string{"blockedonce", "secondvideo", "blockedonce"}; !slices.Equal(got, want) {
		t.Errorf("requests = %q, want %q", got, want)
	}
	recs := e.st.Records()
	if got, want := outcomes(recs, ids[0]), []store.TranscriptOutcome{store.TranscriptBlocked, store.TranscriptOK}; !slices.Equal(got, want) {
		t.Errorf("outcomes of the refused video = %v, want %v", got, want)
	}
	if got := outcomes(recs, ids[1]); !slices.Equal(got, []store.TranscriptOutcome{store.TranscriptOK}) {
		t.Errorf("outcomes of the other video = %v, want ok", got)
	}
	if got := e.sleep.Pauses(); len(got) != 3 || got[0] != config.TranscriptBlockedPause ||
		got[1] != config.TranscriptDelay || got[2] != config.TranscriptDelay {
		t.Errorf("pauses = %v, want the blocked pause, then the delay after each request", got)
	}
}

// TestRunTranscripts_BlockedPauseSurvivesARestart: the pause after a
// refusal is stored, so a worker started again during it, by a restart,
// makes no request before it ends either: neither for the videos that were
// queued, nor for one a poll queued meanwhile.
func TestRunTranscripts_BlockedPauseSurvivesARestart(t *testing.T) {
	e := newTranscriptEnv(t, config.TranscriptBlockedPause)
	e.queue(t, "blockedonce", "secondvideo")
	stop := e.run(t, newTranscriber(func(context.Context, string) (transcript.Transcript, error) {
		return transcript.Transcript{}, fmt.Errorf("player request: %w", transcript.ErrBlocked)
	}))
	waitFor(t, "the pause after the refusal", e.sleep.Holding)
	stop()

	restartAt := t0.Add(time.Minute)
	after := e.restart(t, restartAt)
	after.queue(t, "queuedafter")
	tr := after.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) { return transcriptOf(id), nil })
	after.run(t, tr)
	after.waitIdle(t, 1)

	pausedUntil := t0.Add(config.TranscriptBlockedPause)
	times := tr.Times()
	if len(times) != 3 || !times[0].Equal(pausedUntil) {
		t.Fatalf("requests after the restart at %v, want 3, the first when the pause ends (%v)", times, pausedUntil)
	}
	var waited time.Duration
	for _, d := range after.timers.Waits() {
		waited += d
	}
	if want := pausedUntil.Sub(restartAt); waited != want {
		t.Errorf("waited %s after the restart, want the rest of the pause, %s", waited, want)
	}
}

// refusalPauses returns the pauses after each refusal of a video that
// YouTube refuses every time: TranscriptBlockedPause, twice as long after
// each further refusal in a row, at most MaxBackoff, until the video has
// been queued for TranscriptMaxQueued.
func refusalPauses() []time.Duration {
	var (
		out    []time.Duration
		queued time.Duration
	)
	for d := config.TranscriptBlockedPause; queued < config.TranscriptMaxQueued; d = min(2*d, config.MaxBackoff) {
		out = append(out, d)
		queued += d
	}
	return out
}

// refused is the error of a request YouTube refused, with its Retry-After
// when ra is positive.
func refused(ra time.Duration) error {
	if ra > 0 {
		return fmt.Errorf("player request: %w: %w", transcript.ErrBlocked,
			&fetch.StatusError{URL: ytPlayerURL, StatusCode: http.StatusTooManyRequests, RetryAfter: ra})
	}
	return fmt.Errorf("player request: %w", transcript.ErrBlocked)
}

// TestRunTranscripts_RefusalsInARowPauseLonger: while YouTube refuses every
// request (it stopped accepting this client, say), each pause is twice as
// long as the one before, at most MaxBackoff, so a lasting refusal costs a
// request a day; and a video still queued TranscriptMaxQueued after the poll
// that found it is given up on without a request, so the queue cannot grow
// without end.
func TestRunTranscripts_RefusalsInARowPauseLonger(t *testing.T) {
	const video = "refusedever"
	want := refusalPauses()
	if n := len(want) - slices.Index(want, config.MaxBackoff); n < 2 {
		t.Fatalf("pauses %v: the video is given up on before the pause reaches its cap twice", want)
	}
	e := newTranscriptEnv(t, 0)
	ids := e.queue(t, video)
	tr := e.transcriber(func(context.Context, string) (transcript.Transcript, error) {
		return transcript.Transcript{}, refused(0)
	})
	e.run(t, tr)
	e.waitIdle(t, 1)

	if got := e.sleep.Pauses(); !slices.Equal(got, want) {
		t.Errorf("pauses = %v, want %v", got, want)
	}
	at := t0
	for i, got := range tr.Times() {
		if i >= len(want) || !got.Equal(at) {
			t.Fatalf("requests at %v, want one when each pause of %v ends", tr.Times(), want)
		}
		at = at.Add(want[i])
	}
	if n := len(tr.Calls()); n != len(want) {
		t.Errorf("%d requests, want %d", n, len(want))
	}
	wantOutcomes := append(slices.Repeat([]store.TranscriptOutcome{store.TranscriptBlocked}, len(want)), store.TranscriptFailed)
	if got := outcomes(e.st.Records(), ids[0]); !slices.Equal(got, wantOutcomes) {
		t.Errorf("outcomes = %v, want %v", got, wantOutcomes)
	}
	if job, ok, err := e.st.NextTranscript(t.Context()); ok || err != nil {
		t.Errorf("queue after giving up: %+v, %v, %v; want empty", job, ok, err)
	}
	lines := transcriptLines(t, e.log, "video transcript")
	if len(lines) != len(want)+1 {
		t.Fatalf("%d log lines, want %d", len(lines), len(want)+1)
	}
	for i, l := range lines[:len(want)] {
		if l.Outcome != "blocked" || l.Level != slog.LevelWarn || l.Refusals != i+1 || l.Pause != want[i] {
			t.Errorf("log line %d = %+v, want refusal %d, pause %s", i, l, i+1, want[i])
		}
	}
	if last := lines[len(want)]; last.Outcome != "failed" || last.Level != slog.LevelWarn || last.Video != video ||
		!strings.Contains(last.Err, "queued for too long") {
		t.Errorf("last log line = %+v, want a warning that gives up on the video", last)
	}
}

// TestRunTranscripts_RetryAfterLongerThanTheRefusalPause: a refusal's
// Retry-After longer than the pause its place in the streak gives wins, up
// to MaxBackoff; the streak goes on counting from the refusals.
func TestRunTranscripts_RetryAfterLongerThanTheRefusalPause(t *testing.T) {
	retryAfters := []time.Duration{3 * config.TranscriptBlockedPause, 0, config.TranscriptBlockedPause, 2 * config.MaxBackoff}
	want := []time.Duration{3 * config.TranscriptBlockedPause, 2 * config.TranscriptBlockedPause,
		4 * config.TranscriptBlockedPause, config.MaxBackoff, config.TranscriptDelay}
	e := newTranscriptEnv(t, 0)
	e.queue(t, "ratelimited")
	var calls int
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) {
		calls++ // only the worker calls Fetch, one at a time
		if calls <= len(retryAfters) {
			return transcript.Transcript{}, refused(retryAfters[calls-1])
		}
		return transcriptOf(id), nil
	})
	e.run(t, tr)
	e.waitIdle(t, 1)
	if got := e.sleep.Pauses(); !slices.Equal(got, want) {
		t.Errorf("pauses = %v, want %v", got, want)
	}
}

// TestRunTranscripts_AnAnswerEndsTheRefusalStreak: any answer but a refusal
// ends the streak, so the next refusal pauses for TranscriptBlockedPause
// again.
func TestRunTranscripts_AnAnswerEndsTheRefusalStreak(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	e.queue(t, "firstvideo1", "secondvide1")
	var calls int
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) {
		calls++ // only the worker calls Fetch, one at a time
		if calls == 1 || calls == 2 || calls == 4 {
			return transcript.Transcript{}, refused(0)
		}
		return transcriptOf(id), nil
	})
	e.run(t, tr)
	e.waitIdle(t, 1)

	// The second video was due before the first one's refusal.
	if got, want := tr.Calls(), []string{"firstvideo1", "secondvide1", "firstvideo1", "secondvide1", "secondvide1"}; !slices.Equal(got, want) {
		t.Fatalf("requests = %q, want %q", got, want)
	}
	want := []time.Duration{config.TranscriptBlockedPause, 2 * config.TranscriptBlockedPause, config.TranscriptDelay,
		config.TranscriptBlockedPause, config.TranscriptDelay}
	if got := e.sleep.Pauses(); !slices.Equal(got, want) {
		t.Errorf("pauses = %v, want %v", got, want)
	}
	var streak []int
	for _, l := range transcriptLines(t, e.log, "video transcript") {
		if l.Outcome == "blocked" {
			streak = append(streak, l.Refusals)
		}
	}
	if !slices.Equal(streak, []int{1, 2, 1}) {
		t.Errorf("refusals logged = %v, want [1 2 1]", streak)
	}
}

// TestRunTranscripts_NoRequestLeavesTheRefusalStreak: an outcome reached
// without a request (an entry URL that names no video) tells nothing about
// YouTube, so the streak of refusals goes on.
func TestRunTranscripts_NoRequestLeavesTheRefusalStreak(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	e.queue(t, "refusedvid1")
	n, err := e.st.RecordFetchSuccess(t.Context(), store.FetchSuccess{FeedID: e.feed.ID, FetchedAt: e.clock.Now(),
		QueueTranscripts: true, Entries: []store.NewEntry{{GUID: "not-a-video", URL: "https://example.com/post",
			Title: "Post", PublishedAt: e.clock.Now()}}})
	if err != nil || n != 1 {
		t.Fatalf("RecordFetchSuccess = %d, %v", n, err)
	}
	var calls int
	tr := e.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) {
		calls++ // only the worker calls Fetch, one at a time
		if calls <= 2 {
			return transcript.Transcript{}, refused(0)
		}
		return transcriptOf(id), nil
	})
	e.run(t, tr)
	e.waitIdle(t, 1)

	if got := len(e.st.Records()); got != 4 {
		t.Fatalf("%d outcomes, want 4: two refusals, the entry that is no video, the transcript", got)
	}
	want := []time.Duration{config.TranscriptBlockedPause, 2 * config.TranscriptBlockedPause, config.TranscriptDelay}
	if got := e.sleep.Pauses(); !slices.Equal(got, want) {
		t.Errorf("pauses = %v, want %v", got, want)
	}
}

// TestRunTranscripts_RefusalStreakSurvivesARestart: the streak is stored
// with the pause, so a worker started again during a refusal's pause waits
// for the rest of it, and a refusal after the restart pauses as the next
// in the streak. The time a video has been queued is stored too: it is
// given up on as if the process had not stopped.
func TestRunTranscripts_RefusalStreakSurvivesARestart(t *testing.T) {
	const video = "refusedever"
	want := refusalPauses()
	const before = 3 // refusals before the restart
	e := newTranscriptEnv(t, want[before-1])
	ids := e.queue(t, video)
	refuse := func(context.Context, string) (transcript.Transcript, error) {
		return transcript.Transcript{}, refused(0)
	}
	stop := e.run(t, e.transcriber(refuse))
	waitFor(t, "the third pause", e.sleep.Holding)
	stop()
	if got := e.sleep.Pauses(); !slices.Equal(got, want[:before]) {
		t.Fatalf("pauses before the restart = %v, want %v", got, want[:before])
	}

	pausedUntil := t0
	for _, d := range want[:before] {
		pausedUntil = pausedUntil.Add(d)
	}
	restartAt := pausedUntil.Add(-want[before-1] / 2)
	after := e.restart(t, restartAt)
	tr := after.transcriber(refuse)
	after.run(t, tr)
	after.waitIdle(t, 1)

	var waited time.Duration
	for _, d := range after.timers.Waits() {
		waited += d
	}
	if want := pausedUntil.Sub(restartAt); waited != want {
		t.Errorf("waited %s after the restart, want the rest of the pause, %s", waited, want)
	}
	if got := after.sleep.Pauses(); !slices.Equal(got, want[before:]) {
		t.Errorf("pauses after the restart = %v, want %v", got, want[before:])
	}
	if times := tr.Times(); len(times) == 0 || !times[0].Equal(pausedUntil) {
		t.Errorf("requests after the restart at %v, want the first when the pause ends (%v)", times, pausedUntil)
	}
	wantOutcomes := append(slices.Repeat([]store.TranscriptOutcome{store.TranscriptBlocked}, len(want)-before), store.TranscriptFailed)
	if got := outcomes(e.st.Records(), ids[0])[before:]; !slices.Equal(got, wantOutcomes) {
		t.Errorf("outcomes after the restart = %v, want %v", got, wantOutcomes)
	}
}

// TestRunTranscripts_QueuedWhileTheClockWasAhead: a video queued while the
// clock was ahead has a queued time that lies ahead once the clock is set
// back. It counts as queued now, and an outcome that keeps it queued
// stores that time, so it is given up on after TranscriptMaxQueued all the
// same: a clock set back cannot make the requests for it go on forever.
func TestRunTranscripts_QueuedWhileTheClockWasAhead(t *testing.T) {
	for _, tt := range []struct {
		name     string
		err      error
		requests int
	}{
		{"not aired", fmt.Errorf("player response: %w", transcript.ErrNotReady), notAiredChecks},
		{"refused", refused(0), len(refusalPauses())},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const video = "clockahead1"
			e := newTranscriptEnv(t, 0)
			e.clock.Set(t0.Add(10 * 365 * 24 * time.Hour))
			ids := e.queue(t, video)
			after := e.restart(t, t0)
			var calls int
			tr := after.transcriber(func(_ context.Context, id string) (transcript.Transcript, error) {
				calls++ // only the worker calls Fetch, one at a time
				if calls > tt.requests {
					return transcriptOf(id), nil // still queued: end the test
				}
				return transcript.Transcript{}, tt.err
			})
			after.run(t, tr)
			after.waitIdle(t, 1)

			if n := len(tr.Calls()); n != tt.requests {
				t.Errorf("%d requests, want %d", n, tt.requests)
			}
			if got := outcomes(after.st.Records(), ids[0]); len(got) == 0 || got[len(got)-1] != store.TranscriptFailed {
				t.Errorf("outcomes = %v, want the video given up on", got)
			}
		})
	}
}

// TestRunTranscripts_TooLargeFailsAtOnce: through the real transcript
// client, a caption track over the fetch size cap would be so at every
// attempt: the video is given up on after the first.
func TestRunTranscripts_TooLargeFailsAtOnce(t *testing.T) {
	const video = "longvideo01"
	e := newTranscriptEnv(t, 0)
	ids := e.queue(t, video)
	yt := &youTubeAnswers{clock: e.clock, players: []ytReply{{body: playerAnswer(video, "OK", "", true)}},
		captions: &ytReply{err: fmt.Errorf("fetch %s: %w", ytCaptionURL, fetch.ErrTooLarge)}}
	e.run(t, transcript.New(yt))
	e.waitIdle(t, 1)

	if got := yt.Times(); len(got) != 1 {
		t.Errorf("player requests at %v, want 1", got)
	}
	if got := outcomes(e.st.Records(), ids[0]); !slices.Equal(got, []store.TranscriptOutcome{store.TranscriptFailed}) {
		t.Errorf("outcomes = %v, want failed at once", got)
	}
	lines := transcriptLines(t, e.log, "video transcript")
	if len(lines) != 1 || lines[0].Outcome != "failed" || lines[0].Level != slog.LevelWarn || lines[0].Attempts != 1 ||
		!strings.Contains(lines[0].Err, "too large") {
		t.Errorf("log lines = %+v, want one warning that gives up after 1 attempt", lines)
	}
}

// TestRunTranscripts_RealClientStoresTheTranscript: the worker and the real
// transcript.Client fit together: a queued video's player request and
// caption track (synthetic answers) end in a stored transcript, with the
// two requests the client makes and nothing else.
func TestRunTranscripts_RealClientStoresTheTranscript(t *testing.T) {
	const (
		video      = "realclient1"
		playerURL  = "https://www.youtube.com/youtubei/v1/player?prettyPrint=false"
		captionURL = "https://www.youtube.com/api/timedtext?v=" + video + "&lang=en"
		jsonType   = "application/json; charset=UTF-8"
	)
	player := `{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"` + video + `"},
		"captions":{"playerCaptionsTracklistRenderer":{"captionTracks":[
		{"baseUrl":"` + captionURL + `","languageCode":"en","kind":"asr","vssId":"a.en"}]}}}`
	captions := `{"events":[{"tStartMs":0,"segs":[{"utf8":"Synthetic words"},{"utf8":" for a test,"}]},
		{"tStartMs":1000,"segs":[{"utf8":"then a second cue."}]}]}`
	f := routeFetcher(map[string]*fetch.Result{
		playerURL: {StatusCode: http.StatusOK, Body: []byte(player), ContentType: jsonType, FinalURL: playerURL},
		captionURL + "&fmt=json3": {StatusCode: http.StatusOK, Body: []byte(captions), ContentType: jsonType,
			FinalURL: captionURL + "&fmt=json3"},
	})
	e := newTranscriptEnv(t, 0)
	ids := e.queue(t, video)
	e.run(t, transcript.New(f))
	e.waitIdle(t, 1)

	got, err := e.st.GetTranscript(t.Context(), ids[0])
	want := store.Transcript{Lang: "en", Generated: true, Text: "Synthetic words for a test, then a second cue."}
	if err != nil || got != want {
		t.Errorf("stored transcript = %+v, %v; want %+v", got, err, want)
	}
	calls := f.Calls()
	if len(calls) != 2 || calls[0].URL != playerURL || calls[0].Method != http.MethodPost ||
		calls[1].URL != captionURL+"&fmt=json3" || calls[1].Method != "" {
		t.Errorf("requests = %+v, want the player POST, then the caption GET", calls)
	}
}

// TestRunTranscripts_BlockedHonoursRetryAfter: through the real
// transcript.Client, a 429 from YouTube pauses the worker for
// TranscriptBlockedPause, or for the answer's Retry-After when that is
// longer, capped at config.MaxBackoff as for a feed.
func TestRunTranscripts_BlockedHonoursRetryAfter(t *testing.T) {
	for _, tt := range []struct {
		name       string
		retryAfter time.Duration
		want       time.Duration
	}{
		{"no Retry-After", 0, config.TranscriptBlockedPause},
		{"shorter Retry-After", config.TranscriptBlockedPause / 2, config.TranscriptBlockedPause},
		{"longer Retry-After", 3 * config.TranscriptBlockedPause, 3 * config.TranscriptBlockedPause},
		{"Retry-After past the cap", 2 * config.MaxBackoff, config.MaxBackoff},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newTranscriptEnv(t, tt.want)
			ids := e.queue(t, "ratelimited")
			f := staticFetcher(nil, &fetch.StatusError{URL: "https://www.youtube.com/youtubei/v1/player",
				StatusCode: http.StatusTooManyRequests, RetryAfter: tt.retryAfter})
			stop := e.run(t, transcript.New(f))
			waitFor(t, "the pause after the refusal", e.sleep.Holding)
			stop()

			if got := e.sleep.Pauses(); !slices.Equal(got, []time.Duration{tt.want}) {
				t.Errorf("pauses = %v, want %v", got, tt.want)
			}
			if got := len(f.Calls()); got != 1 {
				t.Errorf("%d requests, want 1", got)
			}
			if got := outcomes(e.st.Records(), ids[0]); !slices.Equal(got, []store.TranscriptOutcome{store.TranscriptBlocked}) {
				t.Errorf("outcomes = %v, want blocked", got)
			}
			if job, ok, err := e.st.NextTranscript(t.Context()); err != nil || !ok || !job.DueAt.Equal(t0.Add(tt.want)) {
				t.Errorf("queue after the refusal: %+v, %v, %v; want the video due in %s", job, ok, err, tt.want)
			}
			lines := transcriptLines(t, e.log, "video transcript")
			if len(lines) != 1 || lines[0].Outcome != "blocked" || lines[0].Pause != tt.want {
				t.Errorf("log lines = %+v, want one blocked warning with pause %s", lines, tt.want)
			}
		})
	}
}

// TestRunTranscripts_MakesNoRequestWithoutNewVideos: the worker only ever
// requests queued videos, and with none it waits for a poll to queue one.
func TestRunTranscripts_MakesNoRequestWithoutNewVideos(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	tr := answering(nil)
	e.run(t, tr)
	e.waitIdle(t, 1)
	e.s.signalTranscripts() // a stray wake-up finds nothing either
	e.waitIdle(t, 2)
	if got := tr.Calls(); len(got) != 0 {
		t.Fatalf("requests with nothing queued = %q, want none", got)
	}

	ids := e.queue(t, "newvideo001")
	e.s.signalTranscripts() // as the poll that queued it does
	e.waitIdle(t, 3)
	if got := tr.Calls(); !slices.Equal(got, []string{"newvideo001"}) {
		t.Errorf("requests after a poll queued a video = %q, want it", got)
	}
	if _, err := e.st.GetTranscript(t.Context(), ids[0]); err != nil {
		t.Errorf("GetTranscript: %v", err)
	}
}

// TestPoll_QueuesTheTranscriptsOfNewYouTubeVideos: with all three switches
// on (the server's, the global setting, the source's), a poll of a YouTube
// feed queues its new videos and wakes the worker; the initial add, polls
// without new videos, other feeds, and every poll with any switch off queue
// nothing.
func TestPoll_QueuesTheTranscriptsOfNewYouTubeVideos(t *testing.T) {
	body, err := os.ReadFile("testdata/youtube.xml")
	mustNoErr(t, err)
	queued := func(t *testing.T, st store.Store) []string {
		t.Helper()
		var urls []string
		for {
			job, ok, err := st.NextTranscript(t.Context())
			mustNoErr(t, err)
			if !ok {
				return urls
			}
			urls = append(urls, job.URL)
			mustNoErr(t, st.RecordTranscript(t.Context(), job.EntryID, store.TranscriptResult{Outcome: store.TranscriptNoCaptions}))
		}
	}
	woken := func(s *Scheduler) bool {
		select {
		case <-s.transcriptWake:
			return true
		default:
			return false
		}
	}
	newVideo := []string{watchURL + "vid00000001"}
	type sw struct{ server, global, source bool }
	for _, tt := range []struct {
		name  string
		sw    sw
		kind  store.Kind
		queue []string
	}{
		{"youtube with every switch on", sw{true, true, true}, store.KindYouTube, newVideo},
		{"youtube with the server's switch off", sw{false, true, true}, store.KindYouTube, nil},
		{"youtube with the global setting off", sw{true, false, true}, store.KindYouTube, nil},
		{"youtube with the source's switch off", sw{true, true, false}, store.KindYouTube, nil},
		{"youtube with every switch off", sw{false, false, false}, store.KindYouTube, nil},
		{"other feed with every switch on", sw{true, true, true}, store.KindRSS, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			// vid00000002 is already stored; vid00000001 is new.
			f := addFeedOfKind(t, st, tt.kind, channelURL, t0, item{id: "yt:video:vid00000002", title: "Older short", published: t0})
			mustNoErr(t, st.SetTranscriptsSetting(t.Context(), tt.sw.global, t0))
			mustNoErr(t, st.SetFeedTranscripts(t.Context(), f.ID, tt.sw.source, t0))
			cfg := config.Default()
			cfg.Transcripts = tt.sw.server
			s := New(st, staticFetcher(ok(channelURL, body), nil), cfg, testLogger(t))
			s.now = newClock(t0.Add(time.Hour)).Now
			if woken(s) || len(queued(t, st)) != 0 {
				t.Fatal("the initial add queued a transcript")
			}

			pollOnce(t, s, st, f.ID)
			if !tt.sw.source {
				// Skipped by the queue while off: turn it on to see it.
				mustNoErr(t, st.SetFeedTranscripts(t.Context(), f.ID, true, t0))
			}
			if got := queued(t, st); !slices.Equal(got, tt.queue) {
				t.Errorf("queued after a poll with a new video = %q, want %q", got, tt.queue)
			}
			if got := woken(s); len(tt.queue) > 0 && !got {
				t.Error("the worker was not woken for a queued video")
			}
			pollOnce(t, s, st, f.ID) // nothing new this time
			if got := queued(t, st); len(got) != 0 || woken(s) {
				t.Errorf("a poll without new videos queued %q or woke the worker", got)
			}
		})
	}
}

// TestSubscribe_NeverQueuesTranscripts: the initial add stores every video
// of the feed at once, exactly the burst transcripts must not cause.
func TestSubscribe_NeverQueuesTranscripts(t *testing.T) {
	body, err := os.ReadFile("testdata/youtube.xml")
	mustNoErr(t, err)
	st := openStore(t)
	cfg := config.Default()
	cfg.Transcripts = true
	s := New(st, routeFetcher(map[string]*fetch.Result{channelURL: ok(channelURL, body)}), cfg, testLogger(t))
	// Every switch on: only the initial add itself keeps the queue empty.
	mustNoErr(t, st.SetTranscriptsSetting(t.Context(), true, t0))
	if _, err := s.Subscribe(t.Context(), Subscription{FeedURL: channelURL}); err != nil {
		t.Fatal(err)
	}
	if job, ok, err := st.NextTranscript(t.Context()); ok || err != nil {
		t.Errorf("after subscribing: queued %+v, %v; want nothing", job, err)
	}
	if len(s.transcriptWake) != 0 {
		t.Error("subscribing woke the transcript worker")
	}
}

// TestRunTranscripts_CancelRecordsNothing: stopping the worker during a
// request records nothing, so the video is requested again after the next
// start as if never tried; a pause ends at once too.
func TestRunTranscripts_CancelRecordsNothing(t *testing.T) {
	t.Run("during a request", func(t *testing.T) {
		e := newTranscriptEnv(t, 0)
		ids := e.queue(t, "slowvideo01")
		started := make(chan struct{})
		tr := newTranscriber(func(ctx context.Context, _ string) (transcript.Transcript, error) {
			close(started)
			<-ctx.Done()
			return transcript.Transcript{}, fmt.Errorf("player request: %w", ctx.Err())
		})
		stop := e.run(t, tr)
		<-started
		stop()
		if recs := e.st.Records(); len(recs) != 0 {
			t.Errorf("records after cancelling = %+v, want none", recs)
		}
		job, ok, err := e.st.NextTranscript(t.Context())
		if err != nil || !ok || job.EntryID != ids[0] || job.Attempts != 0 {
			t.Errorf("queue after cancelling = %+v, %v, %v; want the video, no attempt", job, ok, err)
		}
		if lines := transcriptLines(t, e.log, "video transcript"); len(lines) != 0 {
			t.Errorf("log lines after cancelling = %+v, want none", lines)
		}
		if len(e.sleep.Pauses()) != 0 {
			t.Errorf("pauses = %v after cancelling, want none", e.sleep.Pauses())
		}
	})
	t.Run("during the delay", func(t *testing.T) {
		e := newTranscriptEnv(t, 0)
		e.s.sleep = sleepCtx // the real pause, TranscriptDelay long
		e.queue(t, "firstvideo1", "secondvide1")
		tr := answering(nil)
		stop := e.run(t, tr)
		waitFor(t, "the first request", func() bool { return len(e.st.Records()) == 1 })
		start := time.Now()
		stop()
		if took := time.Since(start); took >= config.TranscriptDelay {
			t.Errorf("stopping took %s, the whole delay", took)
		}
		if got := tr.Calls(); len(got) != 1 {
			t.Errorf("requests = %q, want 1 (the second one waits for the delay)", got)
		}
	})
}

// TestRunTranscripts_NotAVideoFailsWithoutARequest: an entry URL that names
// no video (transcript.VideoID) is given up on at once, without a request
// or a pause, unless that cannot be stored.
func TestRunTranscripts_NotAVideoFailsWithoutARequest(t *testing.T) {
	for _, storeErr := range []error{nil, errors.New("database is locked")} {
		t.Run(fmt.Sprint("store error ", storeErr), func(t *testing.T) {
			e := newTranscriptEnv(t, 0)
			e.st.recordErr = storeErr
			n, err := e.st.RecordFetchSuccess(t.Context(), store.FetchSuccess{FeedID: e.feed.ID, FetchedAt: time.Now(),
				QueueTranscripts: true, Entries: []store.NewEntry{{GUID: "not-a-video", URL: "https://example.com/post",
					Title: "Post", PublishedAt: time.Now()}}})
			if err != nil || n != 1 {
				t.Fatalf("RecordFetchSuccess = %d, %v", n, err)
			}
			tr := answering(nil)
			e.run(t, tr)
			e.waitIdle(t, 1)
			if got := tr.Calls(); len(got) != 0 {
				t.Errorf("requests = %q, want none", got)
			}
			if recs := e.st.Records(); len(recs) != 1 || recs[0].result.Outcome != store.TranscriptFailed {
				t.Errorf("records = %+v, want one failed", recs)
			}
			// Without a request there is nothing to wait for, except a
			// store that refused the write: no busy loop on it.
			var want []time.Duration
			if storeErr != nil {
				want = []time.Duration{storeRetryDelay}
			}
			if got := e.sleep.Pauses(); !slices.Equal(got, want) {
				t.Errorf("pauses = %v, want %v", got, want)
			}
		})
	}
}

// TestRunTranscripts_PanicIsAFailedAttempt: a fetch that panics (a page
// that trips up the parser) counts as a failed attempt; the worker and the
// process carry on.
func TestRunTranscripts_PanicIsAFailedAttempt(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	ids := e.queue(t, "panicvideo1", "goodvideo01")
	tr := newTranscriber(func(_ context.Context, id string) (transcript.Transcript, error) {
		if id == "panicvideo1" {
			var tracks []string
			_ = tracks[len(id)] // index out of range
		}
		return transcriptOf(id), nil
	})
	e.run(t, tr)
	e.waitIdle(t, 1)
	if got := outcomes(e.st.Records(), ids[0]); len(got) != config.TranscriptMaxAttempts || got[len(got)-1] != store.TranscriptFailed {
		t.Errorf("outcomes of the panicking video = %v, want %d attempts, the last one failed", got, config.TranscriptMaxAttempts)
	}
	if got := outcomes(e.st.Records(), ids[1]); !slices.Equal(got, []store.TranscriptOutcome{store.TranscriptOK}) {
		t.Errorf("outcomes of the other video = %v, want ok", got)
	}
	if errs := e.log.withMsg("transcript fetch panicked"); len(errs) != config.TranscriptMaxAttempts {
		t.Errorf("%d panic log lines, want %d", len(errs), config.TranscriptMaxAttempts)
	}
}

// TestRunTranscripts_EntryDeletedMeanwhile: a video unsubscribed while its
// transcript was being fetched is skipped, and the worker goes on.
func TestRunTranscripts_EntryDeletedMeanwhile(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	e.queue(t, "unsubscribe")
	unsubscribed := e.feed.ID
	tr := newTranscriber(func(ctx context.Context, id string) (transcript.Transcript, error) {
		if id == "unsubscribe" {
			if err := e.st.DeleteFeed(ctx, unsubscribed); err != nil {
				return transcript.Transcript{}, err
			}
		}
		return transcriptOf(id), nil
	})
	e.feed = addFeedOfKind(t, e.st, store.KindYouTube, channelURL+"x", time.Now().Add(time.Hour))
	ids := e.queue(t, "keptvideo01")
	e.run(t, tr)
	e.waitIdle(t, 1)
	if got := tr.Calls(); !slices.Equal(got, []string{"unsubscribe", "keptvideo01"}) {
		t.Errorf("requests = %q", got)
	}
	if recs := e.st.Records(); len(recs) != 1 || recs[0].entryID != ids[0] {
		t.Errorf("records = %+v, want only the kept video's", recs)
	}
	if errs := e.log.records(slog.LevelWarn); len(errs) != 0 {
		t.Errorf("warnings or errors for an unsubscribed video: %+v", errs)
	}
}

// TestRunTranscripts_StoreErrorsDoNotTurnIntoRequests: when the queue cannot
// be read the worker retries later without a request; when an outcome
// cannot be stored it waits TranscriptBlockedPause before requesting the
// same video again.
func TestRunTranscripts_StoreErrorsDoNotTurnIntoRequests(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	ids := e.queue(t, "unsavedvid1")
	e.st.nextErr = errors.New("disk I/O error")
	e.st.recordErr = errors.New("database is locked")
	tr := answering(nil)
	e.run(t, tr)
	e.waitIdle(t, 1)

	if got := tr.Calls(); !slices.Equal(got, []string{"unsavedvid1", "unsavedvid1"}) {
		t.Errorf("requests = %q, want the video twice (its first outcome was lost)", got)
	}
	if got, want := e.sleep.Pauses(), []time.Duration{storeRetryDelay, config.TranscriptBlockedPause, config.TranscriptDelay}; !slices.Equal(got, want) {
		t.Errorf("pauses = %v, want %v", got, want)
	}
	if got := outcomes(e.st.Records(), ids[0]); !slices.Equal(got, []store.TranscriptOutcome{store.TranscriptOK}) {
		t.Errorf("outcomes = %v, want ok", got)
	}
	errs := e.log.records(slog.LevelError)
	if len(errs) != 1 || errs[0].Msg != "could not store the transcript outcome" {
		t.Errorf("errors logged = %+v, want the failed store write", errs)
	}
}

func TestRunTranscripts_RefusesASecondRun(t *testing.T) {
	e := newTranscriptEnv(t, 0)
	e.run(t, answering(nil))
	if err := e.s.RunTranscripts(t.Context(), answering(nil)); err == nil {
		t.Fatal("a second RunTranscripts was accepted")
	}
}

func TestCleanTranscript(t *testing.T) {
	long := strings.Repeat("é", config.TranscriptMaxRunes+1)
	for _, tt := range []struct {
		name string
		in   transcript.Transcript
		want store.Transcript
	}{
		{"kept", transcript.Transcript{Lang: "pt-BR", Generated: true, Text: "Olá\nmundo"},
			store.Transcript{Lang: "pt-BR", Generated: true, Text: "Olá\nmundo"}},
		{"trimmed", transcript.Transcript{Lang: "en", Text: "\n  text \n"}, store.Transcript{Lang: "en", Text: "text"}},
		{"invalid UTF-8", transcript.Transcript{Lang: "en", Text: "a\xffb"}, store.Transcript{Lang: "en", Text: "a\uFFFDb"}},
		{"cut", transcript.Transcript{Lang: "fr", Text: long},
			store.Transcript{Lang: "fr", Text: strings.Repeat("é", config.TranscriptMaxRunes-1) + "…"}},
		{"bogus language", transcript.Transcript{Lang: `en"><script>`, Text: "t"}, store.Transcript{Text: "t"}},
		{"overlong language", transcript.Transcript{Lang: "en" + strings.Repeat("-abcdefgh", maxLangLen), Text: "t"},
			store.Transcript{Text: "t"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := cleanTranscript(tt.in); got != tt.want {
				t.Errorf("cleanTranscript = {%q %v, %d runes ending %q}, want {%q %v, %d runes ending %q}",
					got.Lang, got.Generated, utf8.RuneCountInString(got.Text), tail(got.Text),
					tt.want.Lang, tt.want.Generated, utf8.RuneCountInString(tt.want.Text), tail(tt.want.Text))
			}
		})
	}
}

// tail returns the last few characters of s, for messages.
func tail(s string) string {
	const n = 8
	r := []rune(s)
	return string(r[max(len(r)-n, 0):])
}
