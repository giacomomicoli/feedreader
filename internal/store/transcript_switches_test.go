package store

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
)

const otherChannelFeed = "https://www.youtube.com/feeds/videos.xml?channel_id=UCsynthetic111111111111"

// TestRecordFetchSuccess_QueuesOnlyWhenBothSwitchesAreOn: a poll queues the
// transcripts of its new entries only while the global setting and the
// feed's own switch are on, as the poll's transaction reads them.
func TestRecordFetchSuccess_QueuesOnlyWhenBothSwitchesAreOn(t *testing.T) {
	for _, tc := range []struct {
		global, feed bool
		want         int
	}{
		{false, false, 0}, {false, true, 0}, {true, false, 0}, {true, true, 2},
	} {
		t.Run(fmt.Sprintf("global=%v,feed=%v", tc.global, tc.feed), func(t *testing.T) {
			s := openTest(t)
			ctx := t.Context()
			f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
			mustNoErr(t, s.SetTranscriptsSetting(ctx, tc.global, t0))
			mustNoErr(t, s.SetFeedTranscripts(ctx, f.ID, tc.feed, t0))
			n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: t0, QueueTranscripts: true,
				Entries: []NewEntry{entryAt("a", t0), entryAt("b", t0)}})
			if err != nil || n != 2 {
				t.Fatalf("RecordFetchSuccess = %d, %v", n, err)
			}
			if got := len(queued(t, s)); got != tc.want {
				t.Errorf("queued %d transcripts, want %d", got, tc.want)
			}
		})
	}
}

// TestNextTranscript_SkipsSourcesSwitchedOff: the queued transcripts of a
// source whose switch is off stay queued, frozen, and are skipped; the
// other sources' come next.
func TestNextTranscript_SkipsSourcesSwitchedOff(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	a := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	b := mustCreateFeed(t, s, newFeed(otherChannelFeed, KindYouTube), nil, config.InitialUnread)
	pollNew(t, s, a.ID, true, entryAt("a1", t0))
	setClock(s, t0.Add(time.Minute))
	pollNew(t, s, b.ID, true, entryAt("b1", t0))
	aID, bID := entryID(t, s, a.ID, "a1"), entryID(t, s, b.ID, "b1")

	if j := next(t, s); j.EntryID != aID {
		t.Fatalf("NextTranscript = entry %d, want %d (due first)", j.EntryID, aID)
	}
	mustNoErr(t, s.SetFeedTranscripts(ctx, a.ID, false, s.now()))
	if j := next(t, s); j.EntryID != bID {
		t.Fatalf("NextTranscript with source a off = entry %d, want %d", j.EntryID, bID)
	}
	mustNoErr(t, s.SetFeedTranscripts(ctx, b.ID, false, s.now()))
	if j, ok, err := s.NextTranscript(ctx); ok || err != nil {
		t.Fatalf("NextTranscript with both off = %+v, %v, %v; want none", j, ok, err)
	}
	if got := queued(t, s); !slices.Equal(got, []int64{aID, bID}) {
		t.Errorf("queued = %v, want both still queued %v", got, []int64{aID, bID})
	}
	if _, err := s.FeedTranscripts(ctx, b.ID+1); !errors.Is(err, ErrNotFound) {
		t.Errorf("FeedTranscripts(unknown) err = %v, want ErrNotFound", err)
	}
	if err := s.SetFeedTranscripts(ctx, b.ID+1, true, t0); !errors.Is(err, ErrNotFound) {
		t.Errorf("SetFeedTranscripts(unknown) err = %v, want ErrNotFound", err)
	}
}

// TestSetTranscriptsSetting_ThawsOnlyWhenTurnedOn: turning the global switch
// on from off restarts the queue age of the pending transcripts (they were
// frozen) and keeps their due time and attempts; saving "on" again does not
// restart it, so the age bound cannot be dodged; turning it off changes no
// row. Nothing is ever queued by a switch.
func TestSetTranscriptsSetting_ThawsOnlyWhenTurnedOn(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), entriesNewestFirst("old", 2, t0), config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("v", t0))
	id := entryID(t, s, f.ID, "v")
	retryAt := t0.Add(time.Hour)
	mustNoErr(t, s.RecordTranscript(ctx, id, TranscriptResult{Outcome: TranscriptRetry, RetryAt: retryAt,
		Reason: ReasonError, Requested: true, At: t0}))

	t1, t2, t3 := t0.Add(time.Hour), t0.Add(2*time.Hour), t0.Add(3*time.Hour)
	mustNoErr(t, s.SetTranscriptsSetting(ctx, false, t1))
	if r := rowOf(t, s, id); r.queuedAt != t0.Unix() {
		t.Errorf("queued_at after turning off = %d, want %d", r.queuedAt, t0.Unix())
	}
	if on, err := s.TranscriptsSetting(ctx); err != nil || on {
		t.Errorf("TranscriptsSetting = %v, %v; want off", on, err)
	}
	mustNoErr(t, s.SetTranscriptsSetting(ctx, true, t2))
	if r := rowOf(t, s, id); r.queuedAt != t2.Unix() || r.dueAt != retryAt.Unix() || r.attempts != 1 {
		t.Errorf("row after turning on = %+v, want queued at %d, due at %d, 1 attempt", r, t2.Unix(), retryAt.Unix())
	}
	mustNoErr(t, s.SetTranscriptsSetting(ctx, true, t3))
	if r := rowOf(t, s, id); r.queuedAt != t2.Unix() {
		t.Errorf("queued_at after saving on again = %d, want %d (unchanged)", r.queuedAt, t2.Unix())
	}
	if n := countRows(t, s, "entry_transcripts", "1"); n != 1 {
		t.Errorf("%d transcript rows, want 1: turning on queues nothing", n)
	}
}

// TestRecordServerTranscripts_ThawsWhenTurnedBackOn: the server's switch
// (FR_TRANSCRIPTS) as last started is stored, on until recorded off;
// recording it on after off restarts the queue age of the pending
// transcripts, as the global setting does; on after on changes nothing.
func TestRecordServerTranscripts_ThawsWhenTurnedBackOn(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("v", t0))
	id := entryID(t, s, f.ID, "v")
	if on, err := s.ServerTranscripts(ctx); err != nil || !on {
		t.Fatalf("ServerTranscripts before any record = %v, %v; want on", on, err)
	}
	t1, t2, t3 := t0.Add(time.Hour), t0.Add(2*time.Hour), t0.Add(3*time.Hour)
	mustNoErr(t, s.RecordServerTranscripts(ctx, true, t1))
	if r := rowOf(t, s, id); r.queuedAt != t0.Unix() {
		t.Errorf("queued_at after on → on = %d, want %d", r.queuedAt, t0.Unix())
	}
	mustNoErr(t, s.RecordServerTranscripts(ctx, false, t2))
	if on, err := s.ServerTranscripts(ctx); err != nil || on {
		t.Errorf("ServerTranscripts = %v, %v; want off", on, err)
	}
	if r := rowOf(t, s, id); r.queuedAt != t0.Unix() {
		t.Errorf("queued_at after turning off = %d, want %d", r.queuedAt, t0.Unix())
	}
	mustNoErr(t, s.RecordServerTranscripts(ctx, true, t3))
	if r := rowOf(t, s, id); r.queuedAt != t3.Unix() || r.dueAt != t0.Unix() {
		t.Errorf("row after off → on = %+v, want queued at %d, due at %d", r, t3.Unix(), t0.Unix())
	}
	if on, err := s.TranscriptsSetting(ctx); err != nil || !on {
		t.Errorf("the global setting changed: %v, %v", on, err)
	}
}

// TestSetFeedTranscripts_ThawsThatSourceOnly: turning a source's switch on
// from off restarts the queue age of its own pending transcripts only.
func TestSetFeedTranscripts_ThawsThatSourceOnly(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	a := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	b := mustCreateFeed(t, s, newFeed(otherChannelFeed, KindYouTube), nil, config.InitialUnread)
	pollNew(t, s, a.ID, true, entryAt("a1", t0))
	pollNew(t, s, b.ID, true, entryAt("b1", t0))
	aID, bID := entryID(t, s, a.ID, "a1"), entryID(t, s, b.ID, "b1")
	t1, t2 := t0.Add(time.Hour), t0.Add(2*time.Hour)
	mustNoErr(t, s.SetFeedTranscripts(ctx, a.ID, false, t1))
	mustNoErr(t, s.SetFeedTranscripts(ctx, b.ID, true, t1)) // already on: no thaw
	mustNoErr(t, s.SetFeedTranscripts(ctx, a.ID, true, t2))
	if r := rowOf(t, s, aID); r.queuedAt != t2.Unix() || r.dueAt != t0.Unix() {
		t.Errorf("source a row = %+v, want queued at %d, due at %d", r, t2.Unix(), t0.Unix())
	}
	if r := rowOf(t, s, bID); r.queuedAt != t0.Unix() {
		t.Errorf("source b row = %+v, want queued at %d (untouched)", r, t0.Unix())
	}
	if on, err := s.FeedTranscripts(ctx, a.ID); err != nil || !on {
		t.Errorf("FeedTranscripts(a) = %v, %v; want on", on, err)
	}
}

// TestRequestTranscript: the user's request queues the transcript at once:
// a new row; a done one (no captions, given up on) from scratch; a pending
// one made due now with its attempts kept. A fetched transcript is refused
// (ErrConflict), an unknown entry is ErrNotFound. The pause after a refusal
// still delays it.
func TestRequestTranscript(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), entriesNewestFirst("old", 1, t0), config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("none", t0), entryAt("failed", t0), entryAt("pending", t0), entryAt("ok", t0))
	id := func(guid string) int64 { return entryID(t, s, f.ID, guid) }
	retryAt := t0.Add(2 * time.Hour)
	mustNoErr(t, s.RecordTranscript(ctx, id("none"), TranscriptResult{Outcome: TranscriptNoCaptions,
		Reason: ReasonNoCaptions, Requested: true, At: t0}))
	mustNoErr(t, s.RecordTranscript(ctx, id("failed"), TranscriptResult{Outcome: TranscriptFailed,
		Reason: ReasonExhausted, Requested: true, At: t0}))
	mustNoErr(t, s.RecordTranscript(ctx, id("pending"), TranscriptResult{Outcome: TranscriptRetry, RetryAt: retryAt,
		Reason: ReasonError, Requested: true, At: t0}))
	mustNoErr(t, s.RecordTranscript(ctx, id("ok"), TranscriptResult{Outcome: TranscriptOK,
		Transcript: Transcript{Text: "x"}, Requested: true, At: t0}))

	at := t0.Add(time.Hour)
	for _, guid := range []string{"old0", "none", "failed", "pending"} {
		mustNoErr(t, s.RequestTranscript(ctx, id(guid), at))
	}
	for guid, want := range map[string]transcriptRow{
		"old0":    {status: "pending", queuedAt: at.Unix(), dueAt: at.Unix(), updatedAt: at.Unix()},
		"none":    {status: "pending", queuedAt: at.Unix(), dueAt: at.Unix(), updatedAt: at.Unix()},
		"failed":  {status: "pending", queuedAt: at.Unix(), dueAt: at.Unix(), updatedAt: at.Unix()},
		"pending": {status: "pending", attempts: 1, queuedAt: at.Unix(), dueAt: at.Unix(), updatedAt: at.Unix()},
	} {
		if got := rowOf(t, s, id(guid)); got != want {
			t.Errorf("%s: row = %+v, want %+v", guid, got, want)
		}
	}
	if st, err := s.TranscriptStateOf(ctx, id("none")); err != nil || st.Reason != "" || st.Rechecked {
		t.Errorf("state of a re-queued row = %+v, %v; want the reason cleared", st, err)
	}
	if st, err := s.TranscriptStateOf(ctx, id("pending")); err != nil || st.Reason != ReasonError {
		t.Errorf("state of a pending row = %+v, %v; want its reason kept", st, err)
	}
	if err := s.RequestTranscript(ctx, id("ok"), at); !errors.Is(err, ErrConflict) {
		t.Errorf("RequestTranscript(ok) err = %v, want ErrConflict", err)
	}
	if got := rowOf(t, s, id("ok")); got.status != "ok" || got.text != "x" {
		t.Errorf("fetched row changed: %+v", got)
	}
	if err := s.RequestTranscript(ctx, id("ok")+100, at); !errors.Is(err, ErrNotFound) {
		t.Errorf("RequestTranscript(unknown) err = %v, want ErrNotFound", err)
	}
	if n := countRows(t, s, "entry_transcripts", "entry_id = ?", id("ok")+100); n != 0 {
		t.Errorf("a row was written for an unknown entry")
	}

	// A refusal's pause delays a requested transcript like any other.
	pausedUntil := at.Add(time.Hour)
	mustNoErr(t, s.RecordTranscript(ctx, id("pending"), TranscriptResult{Outcome: TranscriptBlocked, RetryAt: pausedUntil,
		Reason: ReasonRefused, Requested: true, At: at}))
	mustNoErr(t, s.RequestTranscript(ctx, id("pending"), at))
	if j := next(t, s); !j.DueAt.Equal(pausedUntil) {
		t.Errorf("next job due at %v, want the end of the pause %v", j.DueAt, pausedUntil)
	}
}

// TestTranscriptStateOf: what the UI shows of an entry's transcript.
func TestTranscriptStateOf(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), entriesNewestFirst("old", 1, t0), config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("p", t0), entryAt("ok", t0))
	id := func(guid string) int64 { return entryID(t, s, f.ID, guid) }

	if st, err := s.TranscriptStateOf(ctx, id("old0")); err != nil || st != (TranscriptState{}) {
		t.Errorf("state never queued = %+v, %v; want zero", st, err)
	}
	if st, err := s.TranscriptStateOf(ctx, id("old0")+100); err != nil || st.Status != StatusNone {
		t.Errorf("state of an unknown entry = %+v, %v; want none", st, err)
	}
	pausedUntil := t0.Add(time.Hour)
	mustNoErr(t, s.RecordTranscript(ctx, id("p"), TranscriptResult{Outcome: TranscriptBlocked, RetryAt: pausedUntil,
		Reason: ReasonRefused, Requested: true, At: t0}))
	st, err := s.TranscriptStateOf(ctx, id("p"))
	want := TranscriptState{Status: StatusPending, Reason: ReasonRefused, QueuedAt: t0, DueAt: pausedUntil, PausedUntil: pausedUntil}
	if err != nil || st != want {
		t.Errorf("pending state = %+v, %v; want %+v", st, err, want)
	}
	mustNoErr(t, s.RecordTranscript(ctx, id("ok"), TranscriptResult{Outcome: TranscriptOK,
		Transcript: Transcript{Lang: "en", Generated: true, Text: "x"}, Requested: true, At: t0}))
	if st, err := s.TranscriptStateOf(ctx, id("ok")); err != nil || st.Status != StatusOK || st.Lang != "en" ||
		!st.Generated || st.Reason != "" {
		t.Errorf("ok state = %+v, %v", st, err)
	}
}

// TestRecordTranscript_StoresTheReason: each outcome stores its reason, a
// transcript clears it; an unknown reason is refused; a final failure
// reached without a request counts no attempt.
func TestRecordTranscript_StoresTheReason(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("v", t0), entryAt("w", t0))
	v, w := entryID(t, s, f.ID, "v"), entryID(t, s, f.ID, "w")
	reasonOf := func(id int64) TranscriptReason {
		st, err := s.TranscriptStateOf(ctx, id)
		mustNoErr(t, err)
		return st.Reason
	}
	retryAt := t0.Add(time.Hour)
	for _, step := range []struct {
		r    TranscriptResult
		want TranscriptReason
	}{
		{TranscriptResult{Outcome: TranscriptDeferred, RetryAt: retryAt, Reason: ReasonNotReady, Requested: true}, ReasonNotReady},
		{TranscriptResult{Outcome: TranscriptRecheck, RetryAt: retryAt, Reason: ReasonNoCaptions, Requested: true}, ReasonNoCaptions},
		{TranscriptResult{Outcome: TranscriptRetry, RetryAt: retryAt, Reason: ReasonUnavailable, Requested: true}, ReasonUnavailable},
		{TranscriptResult{Outcome: TranscriptOK, Transcript: Transcript{Text: "x"}, Reason: ReasonError, Requested: true}, ""},
	} {
		mustNoErr(t, s.RecordTranscript(ctx, v, step.r))
		if got := reasonOf(v); got != step.want {
			t.Errorf("after %s: reason = %q, want %q", step.r.Outcome, got, step.want)
		}
	}
	err := s.RecordTranscript(ctx, w, TranscriptResult{Outcome: TranscriptRetry, RetryAt: retryAt, Reason: "bogus"})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown reason: err = %v, want ErrInvalid", err)
	}
	mustNoErr(t, s.RecordTranscript(ctx, w, TranscriptResult{Outcome: TranscriptFailed, Reason: ReasonQueuedLong}))
	if r := rowOf(t, s, w); r.status != "failed" || r.attempts != 0 {
		t.Errorf("failed without a request: row = %+v, want failed with 0 attempts", r)
	}
	if got := reasonOf(w); got != ReasonQueuedLong {
		t.Errorf("reason = %q, want %q", got, ReasonQueuedLong)
	}
}

// TestRecordFetchSuccess_PendingFeedQueuesNothing: the first success of a
// pending subscription follows the initial-add rule, in the same
// transaction as queueing would: it queues no transcript even with both
// stored switches on. The next success queues as usual.
func TestRecordFetchSuccess_PendingFeedQueuesNothing(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	nf := newFeed(channelFeed, KindYouTube)
	nf.InitialPending, nf.LastError = true, "HTTP 404"
	f := mustCreateFeed(t, s, nf, nil, config.InitialUnread)
	transcriptsOn(t, s)
	n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: t0, QueueTranscripts: true,
		InitialUnread: config.InitialUnread, Entries: entriesNewestFirst("v", 2*config.InitialUnread, t0)})
	if err != nil || n != 2*config.InitialUnread {
		t.Fatalf("RecordFetchSuccess = %d, %v", n, err)
	}
	if got := queued(t, s); len(got) != 0 {
		t.Errorf("the first success of a pending feed queued %v", got)
	}
	setClock(s, t0.Add(time.Hour))
	if n := pollNew(t, s, f.ID, true, entryAt("new", t0.Add(time.Hour))); n != 1 {
		t.Fatalf("next poll inserted %d", n)
	}
	if got := queued(t, s); !slices.Equal(got, []int64{entryID(t, s, f.ID, "new")}) {
		t.Errorf("queued after the next poll = %v, want the new entry", got)
	}
}
