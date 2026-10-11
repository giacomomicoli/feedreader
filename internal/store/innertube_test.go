package store

import (
	"errors"
	"fmt"
	"maps"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
)

// TestKnownGUIDs: of the GUIDs asked about, those the feed already has,
// matched exactly; another feed's entries do not count; no GUID is no
// query; more than MaxKnownGUIDs is refused.
func TestKnownGUIDs(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube),
		[]NewEntry{entryAt("yt:video:aaaaaaaaaaa", t0), entryAt("yt:video:bbbbbbbbbbb", t0)}, config.InitialUnread)
	other := mustCreateFeed(t, s, newFeed(channelFeed+"x", KindYouTube), []NewEntry{entryAt("yt:video:ccccccccccc", t0)}, config.InitialUnread)

	got, err := s.KnownGUIDs(ctx, f.ID, []string{"yt:video:aaaaaaaaaaa", "yt:video:ccccccccccc", "yt:video:AAAAAAAAAAA",
		"yt:video:aaaaaaaaaaa", "x') OR 1=1 --", "%", "yt:video:bbbbbbbbbbb"})
	want := map[string]bool{"yt:video:aaaaaaaaaaa": true, "yt:video:bbbbbbbbbbb": true}
	if err != nil || !maps.Equal(got, want) {
		t.Fatalf("KnownGUIDs = %v, %v; want %v", got, err, want)
	}
	if got, err := s.KnownGUIDs(ctx, other.ID, []string{"yt:video:aaaaaaaaaaa"}); err != nil || len(got) != 0 {
		t.Errorf("other feed: KnownGUIDs = %v, %v", got, err)
	}
	if got, err := s.KnownGUIDs(ctx, f.ID, nil); err != nil || len(got) != 0 {
		t.Errorf("no GUIDs: KnownGUIDs = %v, %v", got, err)
	}
	many := make([]string, MaxKnownGUIDs+1)
	for i := range many {
		many[i] = fmt.Sprint(i)
	}
	if _, err := s.KnownGUIDs(ctx, f.ID, many); !errors.Is(err, ErrInvalid) {
		t.Errorf("%d GUIDs: err = %v, want ErrInvalid", len(many), err)
	}
	if got, err := s.KnownGUIDs(ctx, f.ID, many[:MaxKnownGUIDs]); err != nil || len(got) != 0 {
		t.Errorf("%d GUIDs: KnownGUIDs = %v, %v", MaxKnownGUIDs, got, err)
	}
}

// TestRecordFetchSuccess_OlderAsRead: with OlderAsRead, the entries a
// fetch inserts that are published no later than the newest entry the feed
// already had (among those not dated after the fetch) are stored read (and never queued for transcripts); newer
// ones are unread as usual. Existing entries keep their state, and a feed
// without entries stores everything unread. A pending subscription follows
// the initial-add rule instead.
func TestRecordFetchSuccess_OlderAsRead(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	transcriptsOn(t, s)
	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), []NewEntry{entryAt("kept", t0)}, config.InitialUnread)
	e := entryID(t, s, f.ID, "kept")
	mustNoErr(t, s.SetRead(ctx, e, false, t0))
	n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: f.ID, FetchedAt: t0.Add(time.Hour), OlderAsRead: true,
		QueueTranscripts: true, Entries: []NewEntry{
			entryAt("new", t0.Add(time.Minute)), entryAt("same-time", t0), entryAt("old", t0.Add(-time.Hour)), entryAt("kept", t0)}})
	if err != nil || n != 3 {
		t.Fatalf("RecordFetchSuccess = %d, %v", n, err)
	}
	read := map[string]bool{}
	for _, g := range []string{"new", "same-time", "old", "kept"} {
		var isRead bool
		mustNoErr(t, s.db.QueryRowContext(ctx, `SELECT is_read FROM entries WHERE feed_id = ? AND guid = ?`, f.ID, g).Scan(&isRead))
		read[g] = isRead
	}
	if want := map[string]bool{"new": false, "same-time": true, "old": true, "kept": false}; !maps.Equal(read, want) {
		t.Errorf("read = %v, want %v", read, want)
	}
	if ids := queued(t, s); len(ids) != 1 || ids[0] != entryID(t, s, f.ID, "new") {
		t.Errorf("queued transcripts %v, want only the new entry's", ids)
	}

	// A stored entry dated after the fetch is no reference: it would make
	// every new entry look old.
	future := mustCreateFeed(t, s, newFeed(channelFeed+"f", KindYouTube),
		[]NewEntry{entryAt("future", t0.Add(30*24*time.Hour)), entryAt("past", t0.Add(-2*time.Hour))}, config.InitialUnread)
	if n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: future.ID, FetchedAt: t0, OlderAsRead: true,
		Entries: []NewEntry{entryAt("fnew", t0.Add(-time.Hour)), entryAt("fold", t0.Add(-3*time.Hour))}}); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	for g, want := range map[string]bool{"fnew": false, "fold": true} {
		var isRead bool
		mustNoErr(t, s.db.QueryRowContext(ctx, `SELECT is_read FROM entries WHERE feed_id = ? AND guid = ?`, future.ID, g).Scan(&isRead))
		if isRead != want {
			t.Errorf("future-dated stored entry: %s read = %v, want %v", g, isRead, want)
		}
	}

	empty := mustCreateFeed(t, s, newFeed(channelFeed+"e", KindYouTube), nil, config.InitialUnread)
	if n, err := s.RecordFetchSuccess(ctx, FetchSuccess{FeedID: empty.ID, FetchedAt: t0, OlderAsRead: true,
		Entries: []NewEntry{entryAt("a", t0.Add(-time.Hour))}}); err != nil || n != 1 {
		t.Fatal(n, err)
	}
	var isRead bool
	mustNoErr(t, s.db.QueryRowContext(ctx, `SELECT is_read FROM entries WHERE feed_id = ?`, empty.ID).Scan(&isRead))
	if isRead {
		t.Error("a feed without entries stored its first one read")
	}
}

// TestInnerTubePause_SharedWithTranscripts: the YouTube fallback and the
// transcript queue share one refusal pause: a refusal the fallback records
// delays every queued transcript and lengthens the streak the transcript
// worker sees, and a transcript refusal is the pause the fallback reads.
// An answer that was not a refusal ends the streak and keeps the pause; a
// restart keeps both.
func TestInnerTubePause_SharedWithTranscripts(t *testing.T) {
	s := openTest(t)
	ctx := t.Context()
	pause := func(wantUntil time.Time, wantRefusals int, when string) {
		t.Helper()
		p, err := s.InnerTubePause(ctx)
		if err != nil || !p.Until.Equal(wantUntil) || p.Refusals != wantRefusals {
			t.Errorf("%s: InnerTubePause = %+v, %v; want until %v, %d refusals", when, p, err, wantUntil, wantRefusals)
		}
	}
	pause(time.Time{}, 0, "never refused")
	mustNoErr(t, s.RecordInnerTubeAnswer(ctx))
	pause(time.Time{}, 0, "an answer without a pause")

	f := mustCreateFeed(t, s, newFeed(channelFeed, KindYouTube), nil, config.InitialUnread)
	pollNew(t, s, f.ID, true, entryAt("v1", t0))

	until := t0.Add(time.Hour)
	mustNoErr(t, s.RecordInnerTubeRefusal(ctx, until))
	pause(until, 1, "after a fallback refusal")
	if job := next(t, s); !job.DueAt.Equal(until) || job.Refusals != 1 {
		t.Errorf("transcript job after a fallback refusal = %+v; want due at the pause's end, 1 refusal", job)
	}

	later := t0.Add(3 * time.Hour)
	mustNoErr(t, s.RecordTranscript(ctx, entryID(t, s, f.ID, "v1"), TranscriptResult{Outcome: TranscriptBlocked,
		RetryAt: later, Requested: true}))
	pause(later, 2, "after a transcript refusal")

	mustNoErr(t, s.RecordInnerTubeAnswer(ctx))
	pause(later, 0, "after an answer")
}
