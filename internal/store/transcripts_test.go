package store

import (
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
)

const channelFeed = "https://www.youtube.com/feeds/videos.xml?channel_id=UCsynthetic000000000000"

// setClock makes s.now return at.
func setClock(s *SQLite, at time.Time) { s.now = func() time.Time { return at } }

// transcriptsOn turns the global transcript setting on.
func transcriptsOn(t *testing.T, s *SQLite) {
	t.Helper()
	mustNoErr(t, s.SetTranscriptsSetting(t.Context(), true, s.now()))
}

// pollNew records a poll of feedID that delivers entries, queueing the
// transcripts of those that are new when queue is set (with the global
// transcript setting turned on), and returns how many were inserted.
func pollNew(t *testing.T, s *SQLite, feedID int64, queue bool, entries ...NewEntry) int {
	t.Helper()
	if queue {
		transcriptsOn(t, s)
	}
	n, err := s.RecordFetchSuccess(t.Context(), FetchSuccess{FeedID: feedID, FetchedAt: s.now(),
		Entries: entries, QueueTranscripts: queue})
	if err != nil {
		t.Fatalf("RecordFetchSuccess: %v", err)
	}
	return n
}

// queued returns the ids of the entries whose transcript is pending, in the
// order NextTranscript hands them out.
func queued(t *testing.T, s *SQLite) []int64 {
	t.Helper()
	var ids []int64
	rows, err := s.db.QueryContext(t.Context(),
		`SELECT entry_id FROM entry_transcripts WHERE status = 'pending' ORDER BY due_at, attempts, entry_id`)
	mustNoErr(t, err)
	defer rows.Close()
	for rows.Next() {
		var id int64
		mustNoErr(t, rows.Scan(&id))
		ids = append(ids, id)
	}
	mustNoErr(t, rows.Err())
	return ids
}

// next calls NextTranscript and fails the test unless a job is queued.
func next(t *testing.T, s *SQLite) TranscriptJob {
	t.Helper()
	job, ok, err := s.NextTranscript(t.Context())
	if err != nil || !ok {
		t.Fatalf("NextTranscript = %+v, %v, %v; want a job", job, ok, err)
	}
	return job
}

// transcriptRow is an entry_transcripts row as stored.
type transcriptRow struct {
	status, lang, text         string
	generated, rechecked       bool
	attempts                   int
	queuedAt, dueAt, updatedAt int64
}

func rowOf(t *testing.T, s *SQLite, entryID int64) transcriptRow {
	t.Helper()
	var r transcriptRow
	var lang, text *string
	err := s.db.QueryRowContext(t.Context(),
		`SELECT status, lang, is_generated, text, attempts, rechecked, queued_at, due_at, updated_at
		FROM entry_transcripts WHERE entry_id = ?`,
		entryID).Scan(&r.status, &lang, &r.generated, &text, &r.attempts, &r.rechecked, &r.queuedAt, &r.dueAt, &r.updatedAt)
	mustNoErr(t, err)
	if lang != nil {
		r.lang = *lang
	}
	if text != nil {
		r.text = *text
	}
	return r
}

func TestRecordFetchSuccess_QueuesTheTranscriptsOfInsertedEntriesOnly(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), entriesNewestFirst("v", 3, t0), config.InitialUnread)
	if n := countRows(t, s, "entry_transcripts", ""); n != 0 {
		t.Fatalf("the initial add queued %d transcripts, want 0", n)
	}

	// Known entries are refreshed, not queued; new ones are.
	later := t0.Add(time.Hour)
	setClock(s, later)
	if n := pollNew(t, s, f.ID, true, append([]NewEntry{entryAt("new1", later), entryAt("new2", later)},
		entriesNewestFirst("v", 3, t0)...)...); n != 2 {
		t.Fatalf("inserted = %d, want 2", n)
	}
	new1, new2 := entryID(t, s, f.ID, "new1"), entryID(t, s, f.ID, "new2")
	got := queued(t, s)
	slices.Sort(got)
	if want := []int64{min(new1, new2), max(new1, new2)}; !slices.Equal(got, want) {
		t.Fatalf("queued = %v, want the new entries %v", got, want)
	}
	if r := rowOf(t, s, new1); r != (transcriptRow{status: "pending", queuedAt: later.Unix(), dueAt: later.Unix(),
		updatedAt: later.Unix()}) {
		t.Errorf("queued row = %+v, want pending, queued and due at the poll", r)
	}

	// Without QueueTranscripts (transcripts off, or not a YouTube feed)
	// nothing is queued.
	if n := pollNew(t, s, f.ID, false, entryAt("new3", later)); n != 1 {
		t.Fatalf("inserted = %d, want 1", n)
	}
	if n := countRows(t, s, "entry_transcripts", ""); n != 2 {
		t.Errorf("transcripts = %d after a poll without QueueTranscripts, want 2", n)
	}
	// A poll that finds nothing new queues nothing either.
	pollNew(t, s, f.ID, true, entriesNewestFirst("v", 3, t0)...)
	if n := countRows(t, s, "entry_transcripts", ""); n != 2 {
		t.Errorf("transcripts = %d after a poll without new entries, want 2", n)
	}
}

// TestRecordFetchSuccess_QueueingIsPartOfTheSameTransaction: when queueing
// fails, the poll stores nothing, so no new entry is left without its
// queued transcript.
func TestRecordFetchSuccess_QueueingIsPartOfTheSameTransaction(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), entriesNewestFirst("v", 1, t0), config.InitialUnread)
	transcriptsOn(t, s)
	_, err := s.db.ExecContext(t.Context(), `CREATE TRIGGER refuse BEFORE INSERT ON entry_transcripts
		BEGIN SELECT RAISE(ABORT, 'refused'); END`)
	mustNoErr(t, err)
	_, err = s.RecordFetchSuccess(t.Context(), FetchSuccess{FeedID: f.ID, FetchedAt: t0.Add(time.Hour),
		ETag: `"v2"`, Entries: []NewEntry{entryAt("new", t0)}, QueueTranscripts: true})
	if err == nil {
		t.Fatal("RecordFetchSuccess succeeded although queueing failed")
	}
	if n := countRows(t, s, "entries", "feed_id = ?", f.ID); n != 1 {
		t.Errorf("entries = %d, want 1 (the poll rolled back)", n)
	}
	if got, _ := s.GetFeed(t.Context(), f.ID); got.ETag != `"v1"` {
		t.Errorf("ETag = %q, want the old one (the poll rolled back)", got.ETag)
	}
}

func TestNextTranscript_NothingQueued(t *testing.T) {
	s := openTest(t)
	job, ok, err := s.NextTranscript(t.Context())
	if err != nil || ok {
		t.Fatalf("NextTranscript on an empty queue = %+v, %v, %v; want none", job, ok, err)
	}
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), entriesNewestFirst("v", 1, t0), config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("done", t0))
	mustNoErr(t, s.RecordTranscript(t.Context(), entryID(t, s, f.ID, "done"), TranscriptResult{Outcome: TranscriptNoCaptions}))
	if job, ok, err := s.NextTranscript(t.Context()); err != nil || ok {
		t.Fatalf("NextTranscript with only finished rows = %+v, %v, %v; want none", job, ok, err)
	}
}

// TestNextTranscript_DueFirst: jobs are taken in the order they fall due:
// a new one when it is queued, one that stays queued at the time its last
// check set. Among jobs due at the same time, the one with fewer attempts
// goes first, then the lowest id. The job due first is returned with its
// due time even when that is still ahead: the caller waits for it.
func TestNextTranscript_DueFirst(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	other := mustCreateFeed(t, s, newFeed(channelFeed+"x", KindYouTube), nil, config.InitialUnread)

	setClock(s, t0.Add(2*time.Hour))
	pollNew(t, s, f.ID, true, entryAt("b", t0), entryAt("a", t0))
	setClock(s, t0.Add(time.Hour)) // a feed polled earlier, stored later
	pollNew(t, s, other.ID, true, entryAt("c", t0))
	a, b, c := entryID(t, s, f.ID, "a"), entryID(t, s, f.ID, "b"), entryID(t, s, other.ID, "c")
	if got, want := queued(t, s), []int64{c, a, b}; !slices.Equal(got, want) {
		t.Fatalf("queue order = %v, want %v (queued first, then lowest id)", got, want)
	}
	job := next(t, s)
	want := TranscriptJob{EntryID: c, URL: "https://example.com/c", QueuedAt: t0.Add(time.Hour), DueAt: t0.Add(time.Hour)}
	if job != want {
		t.Fatalf("NextTranscript = %+v, want %+v", job, want)
	}

	// A failed attempt and a deferral are due again at their RetryAt,
	// behind jobs due earlier, whatever the order they were queued in.
	setClock(s, t0.Add(2*time.Hour))
	mustNoErr(t, s.RecordTranscript(ctx, a, TranscriptResult{Outcome: TranscriptRetry, RetryAt: t0.Add(5 * time.Hour)}))
	mustNoErr(t, s.RecordTranscript(ctx, c, TranscriptResult{Outcome: TranscriptDeferred, RetryAt: t0.Add(3 * time.Hour)}))
	if got, want := queued(t, s), []int64{b, c, a}; !slices.Equal(got, want) {
		t.Fatalf("after a retry and a deferral: queue order = %v, want %v", got, want)
	}
	if j := next(t, s); j.EntryID != b || j.Attempts != 0 || !j.DueAt.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("NextTranscript = %+v, want entry %d due when queued", j, b)
	}
	mustNoErr(t, s.RecordTranscript(ctx, b, TranscriptResult{Outcome: TranscriptNoCaptions}))
	// Still ahead: returned with its due time, queued time unchanged.
	if j := next(t, s); j.EntryID != c || !j.DueAt.Equal(t0.Add(3*time.Hour)) || !j.QueuedAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("NextTranscript = %+v, want entry %d due at %v", j, c, t0.Add(3*time.Hour))
	}

	// Due at the same time: fewer attempts first.
	mustNoErr(t, s.RecordTranscript(ctx, c, TranscriptResult{Outcome: TranscriptRecheck, RetryAt: t0.Add(5 * time.Hour)}))
	if got, want := queued(t, s), []int64{c, a}; !slices.Equal(got, want) {
		t.Fatalf("queue order = %v, want %v (no attempts before one)", got, want)
	}
	if j := next(t, s); j.EntryID != c || j.Attempts != 0 || !j.Rechecked {
		t.Fatalf("NextTranscript = %+v, want entry %d, rechecked, with no attempts", j, c)
	}
	mustNoErr(t, s.RecordTranscript(ctx, c, TranscriptResult{Outcome: TranscriptNoCaptions}))
	if j := next(t, s); j.EntryID != a || j.Attempts != 1 || j.Rechecked || !j.DueAt.Equal(t0.Add(5*time.Hour)) {
		t.Fatalf("NextTranscript = %+v, want entry %d with 1 attempt", j, a)
	}
}

func TestRecordTranscript_Outcomes(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("ok", t0), entryAt("none", t0), entryAt("retry", t0),
		entryAt("deferred", t0), entryAt("recheck", t0), entryAt("failed", t0))
	id := func(guid string) int64 { return entryID(t, s, f.ID, guid) }
	text := "First line\nSecond line <b>not markup</b> & more"

	at := t0.Add(time.Minute)
	setClock(s, at)
	retryAt, laterAt := at.Add(time.Hour), at.Add(2*time.Hour)
	mustNoErr(t, s.RecordTranscript(ctx, id("ok"), TranscriptResult{Outcome: TranscriptOK,
		Transcript: Transcript{Lang: "en-GB", Generated: true, Text: text}}))
	mustNoErr(t, s.RecordTranscript(ctx, id("none"), TranscriptResult{Outcome: TranscriptNoCaptions}))
	mustNoErr(t, s.RecordTranscript(ctx, id("retry"), TranscriptResult{Outcome: TranscriptRetry, RetryAt: retryAt}))
	mustNoErr(t, s.RecordTranscript(ctx, id("retry"), TranscriptResult{Outcome: TranscriptRetry, RetryAt: laterAt}))
	mustNoErr(t, s.RecordTranscript(ctx, id("deferred"), TranscriptResult{Outcome: TranscriptDeferred, RetryAt: retryAt}))
	mustNoErr(t, s.RecordTranscript(ctx, id("recheck"), TranscriptResult{Outcome: TranscriptRecheck, RetryAt: laterAt}))
	mustNoErr(t, s.RecordTranscript(ctx, id("failed"), TranscriptResult{Outcome: TranscriptRetry, RetryAt: retryAt}))
	mustNoErr(t, s.RecordTranscript(ctx, id("failed"), TranscriptResult{Outcome: TranscriptFailed, RetryAt: laterAt, Requested: true}))

	queuedAt, rechecked := t0.Unix(), true
	for guid, want := range map[string]transcriptRow{
		"ok":       {status: "ok", lang: "en-GB", generated: true, text: text, queuedAt: queuedAt, dueAt: queuedAt, updatedAt: at.Unix()},
		"none":     {status: "no_captions", queuedAt: queuedAt, dueAt: queuedAt, updatedAt: at.Unix()},
		"retry":    {status: "pending", attempts: 2, queuedAt: queuedAt, dueAt: laterAt.Unix(), updatedAt: at.Unix()},
		"deferred": {status: "pending", queuedAt: queuedAt, dueAt: retryAt.Unix(), updatedAt: at.Unix()},
		"recheck":  {status: "pending", rechecked: rechecked, queuedAt: queuedAt, dueAt: laterAt.Unix(), updatedAt: at.Unix()},
		// A final outcome leaves the due time alone: the row is done.
		"failed": {status: "failed", attempts: 2, queuedAt: queuedAt, dueAt: retryAt.Unix(), updatedAt: at.Unix()},
	} {
		if got := rowOf(t, s, id(guid)); got != want {
			t.Errorf("%s: row = %+v, want %+v", guid, got, want)
		}
	}

	// Only a fetched transcript is shown.
	got, err := s.GetTranscript(ctx, id("ok"))
	if err != nil || got != (Transcript{Lang: "en-GB", Generated: true, Text: text}) {
		t.Errorf("GetTranscript(ok) = %+v, %v", got, err)
	}
	for _, guid := range []string{"none", "retry", "deferred", "recheck", "failed"} {
		if got, err := s.GetTranscript(ctx, id(guid)); !errors.Is(err, ErrNotFound) {
			t.Errorf("GetTranscript(%s) = %+v, %v; want ErrNotFound", guid, got, err)
		}
	}
	notQueued := mustCreateFeed(t, s, newFeed("https://a.example/feed", KindRSS), entriesNewestFirst("e", 1, t0), 1)
	if _, err := s.GetTranscript(ctx, entryID(t, s, notQueued.ID, "e0")); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetTranscript of an entry never queued: err = %v, want ErrNotFound", err)
	}

	// Finished rows are not written again: only queued ones are.
	for _, guid := range []string{"ok", "none", "failed"} {
		err := s.RecordTranscript(ctx, id(guid), TranscriptResult{Outcome: TranscriptRetry, RetryAt: laterAt})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("RecordTranscript on finished %s: err = %v, want ErrNotFound", guid, err)
		}
	}
	if got := rowOf(t, s, id("ok")); got.status != "ok" || got.text != text {
		t.Errorf("finished row changed: %+v", got)
	}
}

// TestRecordTranscript_BlockedPausesTheQueue: after YouTube refused a
// request no queued entry is due before the pause ends, whether it was
// queued before or during the pause, and the pause outlives the process: a
// store opened again on the same database keeps it. An entry due later
// keeps its own time.
func TestRecordTranscript_BlockedPausesTheQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedreader.db")
	open := func() *SQLite {
		s, err := OpenSQLite(path)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		setClock(s, t0)
		return s
	}
	s := open()
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("refused", t0), entryAt("waiting", t0), entryAt("later", t0))
	id := func(guid string) int64 { return entryID(t, s, f.ID, guid) }
	laterAt, pausedUntil := t0.Add(3*time.Hour), t0.Add(time.Hour)
	mustNoErr(t, s.RecordTranscript(ctx, id("later"), TranscriptResult{Outcome: TranscriptRetry, RetryAt: laterAt}))
	mustNoErr(t, s.RecordTranscript(ctx, id("refused"), TranscriptResult{Outcome: TranscriptBlocked, RetryAt: pausedUntil}))

	// The refused entry stays queued as a deferred one does.
	if got, want := rowOf(t, s, id("refused")), (transcriptRow{status: "pending", queuedAt: t0.Unix(),
		dueAt: pausedUntil.Unix(), updatedAt: t0.Unix()}); got != want {
		t.Errorf("refused row = %+v, want %+v", got, want)
	}
	// The others wait for the pause too, in their order.
	if j := next(t, s); j.EntryID != id("waiting") || !j.QueuedAt.Equal(t0) || !j.DueAt.Equal(pausedUntil) {
		t.Errorf("NextTranscript during the pause = %+v, want entry %d due when the pause ends (%v)", j, id("waiting"), pausedUntil)
	}
	setClock(s, t0.Add(time.Minute))
	pollNew(t, s, f.ID, true, entryAt("new", t0.Add(time.Minute)))
	mustNoErr(t, s.RecordTranscript(ctx, id("waiting"), TranscriptResult{Outcome: TranscriptNoCaptions}))
	if j := next(t, s); j.EntryID != id("new") || !j.DueAt.Equal(pausedUntil) {
		t.Errorf("NextTranscript of an entry queued during the pause = %+v, want it due at %v", j, pausedUntil)
	}

	// A restart keeps the pause.
	newID := id("new")
	mustNoErr(t, s.Close())
	s = open()
	t.Cleanup(func() { mustNoErr(t, s.Close()) })
	if j := next(t, s); j.EntryID != newID || !j.DueAt.Equal(pausedUntil) {
		t.Errorf("NextTranscript after a restart = %+v, want entry %d due at %v", j, newID, pausedUntil)
	}

	// Past the pause each entry is due at its own time again.
	mustNoErr(t, s.RecordTranscript(ctx, newID, TranscriptResult{Outcome: TranscriptNoCaptions}))
	mustNoErr(t, s.RecordTranscript(ctx, id("refused"), TranscriptResult{Outcome: TranscriptNoCaptions}))
	if j := next(t, s); j.EntryID != id("later") || !j.DueAt.Equal(laterAt) {
		t.Errorf("NextTranscript = %+v, want entry %d due at %v", j, id("later"), laterAt)
	}
}

// TestRecordTranscript_BlockedPauseOutlivesItsEntry: a refusal concerns
// the client, not the video, so its pause is stored even when the entry
// was deleted while its transcript was being fetched.
func TestRecordTranscript_BlockedPauseOutlivesItsEntry(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	other := mustCreateFeed(t, s, newFeed(channelFeed+"x", KindYouTube), nil, config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("gone", t0))
	pollNew(t, s, other.ID, true, entryAt("kept", t0))
	gone := entryID(t, s, f.ID, "gone")
	mustNoErr(t, s.DeleteFeed(ctx, f.ID))

	pausedUntil := t0.Add(time.Hour)
	err := s.RecordTranscript(ctx, gone, TranscriptResult{Outcome: TranscriptBlocked, RetryAt: pausedUntil})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("RecordTranscript of a deleted entry: err = %v, want ErrNotFound", err)
	}
	if j := next(t, s); j.EntryID != entryID(t, s, other.ID, "kept") || !j.DueAt.Equal(pausedUntil) {
		t.Errorf("NextTranscript = %+v, want the kept entry due at %v", j, pausedUntil)
	}
}

// TestRecordTranscript_EntryDeletedMeanwhile: unsubscribing deletes the
// queued rows with the entries, and a worker that was fetching one of them
// finds nothing to write into, also once newer entries exist (entry ids are
// never reused).
func TestRecordTranscript_EntryDeletedMeanwhile(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("v", t0))
	job := next(t, s)

	mustNoErr(t, s.DeleteFeed(ctx, f.ID))
	if n := countRows(t, s, "entry_transcripts", ""); n != 0 {
		t.Fatalf("transcripts after unsubscribing = %d, want 0", n)
	}
	again := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	pollNew(t, s, again.ID, true, entryAt("v", t0))
	for _, o := range []TranscriptOutcome{TranscriptOK, TranscriptNoCaptions, TranscriptRetry, TranscriptDeferred,
		TranscriptBlocked, TranscriptRecheck, TranscriptFailed} {
		err := s.RecordTranscript(ctx, job.EntryID, TranscriptResult{Outcome: o, Transcript: Transcript{Text: "t"}, RetryAt: t0})
		if !errors.Is(err, ErrNotFound) {
			t.Errorf("RecordTranscript(%s) of a deleted entry: err = %v, want ErrNotFound", o, err)
		}
	}
	if j := next(t, s); j.EntryID == job.EntryID || j.Attempts != 0 {
		t.Errorf("the new subscription's job = %+v, reuses the deleted entry %d or its attempts", j, job.EntryID)
	}
}

func TestRecordTranscript_RejectsInvalidResults(t *testing.T) {
	s := openTest(t)
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("v", t0))
	id := entryID(t, s, f.ID, "v")
	for name, r := range map[string]TranscriptResult{
		"unknown outcome":    {Outcome: "pending"},
		"empty outcome":      {},
		"transcript no text": {Outcome: TranscriptOK, Transcript: Transcript{Lang: "en"}},
		"transcript blank":   {Outcome: TranscriptOK, Transcript: Transcript{Text: " \n\t"}},
		// An entry that stays queued needs a time to be due again.
		"retry without a time":    {Outcome: TranscriptRetry},
		"deferred without a time": {Outcome: TranscriptDeferred},
		"blocked without a time":  {Outcome: TranscriptBlocked},
		"recheck without a time":  {Outcome: TranscriptRecheck},
	} {
		if err := s.RecordTranscript(t.Context(), id, r); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if got := rowOf(t, s, id); got.status != "pending" || got.attempts != 0 || got.rechecked || got.dueAt != got.queuedAt {
		t.Errorf("row after invalid results = %+v, want untouched", got)
	}
	if j := next(t, s); !j.DueAt.Equal(t0) {
		t.Errorf("NextTranscript after invalid results = %+v, want it due at %v (no pause)", j, t0)
	}
}

// TestNextTranscript_ReadsThePendingIndexInOrder guards the queue's index:
// the next job is read off the partial index, without a sort and without
// visiting finished rows.
func TestNextTranscript_ReadsThePendingIndexInOrder(t *testing.T) {
	s := openTest(t)
	plan := explain(t, s, nextTranscriptSQL)
	if strings.Contains(plan, "TEMP B-TREE") || !strings.Contains(plan, "entry_transcripts_pending") {
		t.Errorf("NextTranscript plan does not walk the pending index in order:\n%s", plan)
	}
}

// TestEntryView_HasTranscript: an entry view tells whether a transcript was
// fetched for the entry, and only then: not while it is queued, nor when
// the video had no captions or was given up on.
func TestEntryView_HasTranscript(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), entriesNewestFirst("old", 1, t0), config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("ok", t0), entryAt("queued", t0), entryAt("none", t0), entryAt("failed", t0))
	id := func(guid string) int64 { return entryID(t, s, f.ID, guid) }
	mustNoErr(t, s.RecordTranscript(ctx, id("ok"), TranscriptResult{Outcome: TranscriptOK, Transcript: Transcript{Text: "t"}}))
	mustNoErr(t, s.RecordTranscript(ctx, id("none"), TranscriptResult{Outcome: TranscriptNoCaptions}))
	mustNoErr(t, s.RecordTranscript(ctx, id("failed"), TranscriptResult{Outcome: TranscriptFailed}))

	page, err := s.ListEntries(ctx, ListQuery{Scope: Scope{Kind: ScopeAll}, Limit: config.ScopePageSize})
	mustNoErr(t, err)
	listed := map[string]bool{}
	for _, e := range page.Entries {
		listed[e.GUID] = e.HasTranscript
		got, err := s.GetEntry(ctx, e.ID)
		mustNoErr(t, err)
		if got.HasTranscript != e.HasTranscript {
			t.Errorf("%s: GetEntry HasTranscript = %v, ListEntries %v", e.GUID, got.HasTranscript, e.HasTranscript)
		}
	}
	want := map[string]bool{"old0": false, "ok": true, "queued": false, "none": false, "failed": false}
	if !maps.Equal(listed, want) {
		t.Errorf("HasTranscript = %v, want %v", listed, want)
	}
}

// TestRecordTranscript_RefusalStreak: each refusal lengthens the streak of
// refusals in a row that the jobs report, and a restart keeps it. Any other
// outcome of a request ends it, also when its entry was deleted meanwhile
// (the answer concerned the client, not the video), while an outcome
// reached without a request leaves it as it is.
func TestRecordTranscript_RefusalStreak(t *testing.T) {
	path := filepath.Join(t.TempDir(), "feedreader.db")
	open := func() *SQLite {
		s, err := OpenSQLite(path)
		if err != nil {
			t.Fatalf("OpenSQLite: %v", err)
		}
		setClock(s, t0)
		return s
	}
	s := open()
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	other := mustCreateFeed(t, s, newFeed(channelFeed+"x", KindYouTube), nil, config.InitialUnread)
	outcomes := []TranscriptOutcome{TranscriptOK, TranscriptNoCaptions, TranscriptRetry, TranscriptDeferred,
		TranscriptRecheck, TranscriptFailed}
	entries := []NewEntry{entryAt("refused", t0), entryAt("skipped", t0)}
	for _, o := range outcomes {
		entries = append(entries, entryAt(string(o), t0))
	}
	pollNew(t, s, f.ID, true, entries...)
	pollNew(t, s, other.ID, true, entryAt("gone", t0))
	id := func(guid string) int64 { return entryID(t, s, f.ID, guid) }
	gone := entryID(t, s, other.ID, "gone")
	streak := func(want int, when string) {
		t.Helper()
		if got := next(t, s).Refusals; got != want {
			t.Errorf("%s: Refusals = %d, want %d", when, got, want)
		}
	}
	refuse := func(entryID int64) {
		t.Helper()
		mustNoErr(t, s.RecordTranscript(ctx, entryID, TranscriptResult{Outcome: TranscriptBlocked,
			RetryAt: t0.Add(time.Hour), Requested: true}))
	}

	streak(0, "before any refusal")
	for want := 1; want <= 3; want++ {
		refuse(id("refused"))
		streak(want, fmt.Sprintf("after %d refusals", want))
	}
	mustNoErr(t, s.Close())
	s = open()
	t.Cleanup(func() { mustNoErr(t, s.Close()) })
	streak(3, "after a restart")

	// No request: a URL that names no video, a video queued too long.
	mustNoErr(t, s.RecordTranscript(ctx, id("skipped"), TranscriptResult{Outcome: TranscriptFailed}))
	streak(3, "after an outcome without a request")

	mustNoErr(t, s.DeleteFeed(ctx, other.ID))
	err := s.RecordTranscript(ctx, gone, TranscriptResult{Outcome: TranscriptNoCaptions, Requested: true})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("RecordTranscript of a deleted entry: err = %v, want ErrNotFound", err)
	}
	streak(0, "after an answer for an entry deleted meanwhile")

	for _, o := range outcomes {
		refuse(id("refused"))
		streak(1, "after a refusal that starts a new streak")
		r := TranscriptResult{Outcome: o, RetryAt: t0.Add(time.Hour), Requested: true}
		if o == TranscriptOK {
			r.Transcript = Transcript{Text: "t"}
		}
		mustNoErr(t, s.RecordTranscript(ctx, id(string(o)), r))
		streak(0, "after "+string(o))
	}
}

// TestRecordTranscript_QueuedAtAheadIsBroughtBack: a queued time that lies
// ahead (stored while the clock was ahead) is brought back to the time of
// an update that keeps the entry queued, the caller's At or else now, so
// that how long the entry has been queued counts from a time that has
// passed. A queued time in the past stays, and a final outcome leaves it.
func TestRecordTranscript_QueuedAtAheadIsBroughtBack(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	ahead := t0.Add(365 * 24 * time.Hour)
	setClock(s, ahead)
	queuedAgain := []TranscriptOutcome{TranscriptRetry, TranscriptDeferred, TranscriptBlocked, TranscriptRecheck}
	var entries []NewEntry
	for _, o := range queuedAgain {
		entries = append(entries, entryAt(string(o), ahead))
	}
	pollNew(t, s, f.ID, true, append(entries, entryAt("at", ahead), entryAt("done", ahead))...)
	id := func(guid string) int64 { return entryID(t, s, f.ID, guid) }

	setClock(s, t0) // the clock set back
	retryAt := t0.Add(time.Hour)
	for _, o := range queuedAgain {
		mustNoErr(t, s.RecordTranscript(ctx, id(string(o)), TranscriptResult{Outcome: o, RetryAt: retryAt, Requested: true}))
		if r := rowOf(t, s, id(string(o))); r.queuedAt != t0.Unix() || r.updatedAt != t0.Unix() || r.dueAt != retryAt.Unix() {
			t.Errorf("%s: row = %+v, want queued and updated at %v, due at %v", o, r, t0, retryAt)
		}
	}

	at := t0.Add(time.Minute)
	mustNoErr(t, s.RecordTranscript(ctx, id("at"), TranscriptResult{Outcome: TranscriptDeferred, RetryAt: retryAt, At: at}))
	if r := rowOf(t, s, id("at")); r.queuedAt != at.Unix() || r.updatedAt != at.Unix() {
		t.Errorf("deferred at %v: row = %+v, want queued and updated then", at, r)
	}
	later := at.Add(time.Hour)
	mustNoErr(t, s.RecordTranscript(ctx, id("at"), TranscriptResult{Outcome: TranscriptDeferred, RetryAt: retryAt, At: later}))
	if r := rowOf(t, s, id("at")); r.queuedAt != at.Unix() || r.updatedAt != later.Unix() {
		t.Errorf("deferred again at %v: row = %+v, want queued at %v still, updated then", later, r, at)
	}

	mustNoErr(t, s.RecordTranscript(ctx, id("done"), TranscriptResult{Outcome: TranscriptNoCaptions, Requested: true}))
	if r := rowOf(t, s, id("done")); r.queuedAt != ahead.Unix() {
		t.Errorf("final outcome: row = %+v, want its queued time left as it was", r)
	}
}
