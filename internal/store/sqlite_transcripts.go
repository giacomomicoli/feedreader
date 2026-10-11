package store

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// nextTranscriptSQL selects the queued job due first. It reads the partial
// index entry_transcripts_pending in its order (due_at, attempts, entry_id)
// and stops at the first row; CROSS JOIN keeps entry_transcripts the outer
// loop. The job is due at its due_at, or when the pause after a refusal
// ends if that is later: the pause delays every job alike, so it does not
// change their order. It comes with the streak of refusals in a row. The
// rows of a feed whose transcript switch is off are skipped: they stay
// queued, frozen, until it is turned back on.
const nextTranscriptSQL = `SELECT t.entry_id, e.url, t.attempts, t.rechecked, t.queued_at,
		max(t.due_at, coalesce((SELECT paused_until FROM transcript_pause WHERE id = 1), 0)),
		coalesce((SELECT refusals FROM transcript_pause WHERE id = 1), 0)
	FROM entry_transcripts t CROSS JOIN entries e ON e.id = t.entry_id CROSS JOIN feeds f ON f.id = e.feed_id
	WHERE t.status = 'pending' AND f.transcripts_enabled = 1
	ORDER BY t.due_at, t.attempts, t.entry_id
	LIMIT 1`

// pauseTranscriptsSQL stores the end of the pause after a refusal, and
// adds the refusal to the streak.
const pauseTranscriptsSQL = `INSERT INTO transcript_pause (id, paused_until, refusals) VALUES (1, ?, 1)
	ON CONFLICT (id) DO UPDATE SET paused_until = excluded.paused_until, refusals = refusals + 1`

// endRefusalsSQL ends the streak of refusals: a request was not refused.
const endRefusalsSQL = `UPDATE transcript_pause SET refusals = 0 WHERE id = 1 AND refusals <> 0`

// transcriptSwitchesSQL reads, for one feed, whether the global transcript
// setting and the feed's own switch are both on.
const transcriptSwitchesSQL = `SELECT
	EXISTS (SELECT 1 FROM app_settings WHERE key = '` + settingTranscripts + `' AND value = '1')
	AND EXISTS (SELECT 1 FROM feeds WHERE id = ? AND transcripts_enabled = 1)`

// queueTranscripts queues the transcripts of the entries ids of feedID as of
// at, the time of the poll that found them, due at once: when the global
// transcript setting and the feed's switch are on as tx reads them, so a
// switch turned off before the poll's write is obeyed.
func queueTranscripts(ctx context.Context, tx *sql.Tx, feedID int64, ids []int64, at time.Time) error {
	if len(ids) == 0 {
		return nil
	}
	var on bool
	if err := tx.QueryRowContext(ctx, transcriptSwitchesSQL, feedID).Scan(&on); err != nil {
		return fmt.Errorf("read the transcript switches: %w", err)
	}
	if !on {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO entry_transcripts (entry_id, status, queued_at, due_at, updated_at)
		VALUES (?, 'pending', ?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("prepare transcript queueing: %w", err)
	}
	defer stmt.Close()
	for _, id := range ids {
		if _, err := stmt.ExecContext(ctx, id, at.Unix(), at.Unix(), at.Unix()); err != nil {
			return fmt.Errorf("queue the transcript of entry %d: %w", id, classify(err))
		}
	}
	return nil
}

// NextTranscript returns the queued entry whose transcript is due first:
// the one with the earliest due time, then the one with fewer attempts,
// then the lowest id, whether or not that time has come. Its DueAt is not
// before the end of the pause the last TranscriptBlocked stored, and its
// Refusals is the streak of refusals so far. ok is false when none is
// queued.
func (s *SQLite) NextTranscript(ctx context.Context) (TranscriptJob, bool, error) {
	var (
		job         TranscriptJob
		url         sql.NullString
		queued, due int64
	)
	err := s.db.QueryRowContext(ctx, nextTranscriptSQL).Scan(&job.EntryID, &url, &job.Attempts, &job.Rechecked,
		&queued, &due, &job.Refusals)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TranscriptJob{}, false, nil
	case err != nil:
		return TranscriptJob{}, false, fmt.Errorf("store: next transcript: %w", err)
	}
	job.URL = url.String
	job.QueuedAt, job.DueAt = timeFromUnix(queued), timeFromUnix(due)
	return job, true, nil
}

// recordTranscriptSQL is the update each outcome makes to a queued row. The
// first arguments of each are updated_at and reason; an outcome that keeps
// the row queued takes its new due_at (TranscriptResult.RetryAt) after
// them, TranscriptFailed the attempts to add (1 after a request, else 0),
// and TranscriptOK the transcript's fields. A final outcome leaves due_at as
// it was: the row leaves the queue. RecordTranscript adds queuedAgainSQL to
// the outcomes that keep the row queued.
var recordTranscriptSQL = map[TranscriptOutcome]string{
	TranscriptOK:         `status = 'ok', updated_at = ?, reason = ?, lang = ?, is_generated = ?, text = ?`,
	TranscriptNoCaptions: `status = 'no_captions', updated_at = ?, reason = ?`,
	TranscriptRetry:      `attempts = attempts + 1, updated_at = ?, reason = ?, due_at = ?`,
	TranscriptDeferred:   `updated_at = ?, reason = ?, due_at = ?`,
	TranscriptBlocked:    `updated_at = ?, reason = ?, due_at = ?`,
	TranscriptRecheck:    `rechecked = 1, updated_at = ?, reason = ?, due_at = ?`,
	TranscriptFailed:     `status = 'failed', updated_at = ?, reason = ?, attempts = attempts + ?`,
}

// queuedAgainSQL brings a queued_at that lies ahead of the update's time
// (its argument), stored while the clock was ahead, back to that time, as
// RecordTranscript does for a row that stays queued: how long a row has
// been queued must count from a time that has passed.
const queuedAgainSQL = `, queued_at = min(queued_at, ?)`

// RecordTranscript stores the outcome of an attempt at a queued entry's
// transcript, as of r.At (now when zero); see TranscriptOutcome. Only
// queued (pending) rows are written: it returns ErrNotFound when the entry
// was deleted meanwhile (its row went with it) or its transcript is done.
// An outcome that keeps the entry queued also brings a queued time that
// lies ahead of r.At (stored while the clock was ahead) back to r.At.
//
// TranscriptBlocked also stores its pause and adds to the streak of
// refusals in a row; any other outcome with r.Requested ends the streak.
// Either is written in the same transaction, and even when the entry is
// gone: the answer concerned the client, not the video. r.Reason is stored
// with every outcome but TranscriptOK, which clears it. It returns
// ErrInvalid for an unknown outcome or reason, a TranscriptOK whose text is
// blank, or an outcome that keeps the entry queued without a RetryAt.
func (s *SQLite) RecordTranscript(ctx context.Context, entryID int64, r TranscriptResult) error {
	set, ok := recordTranscriptSQL[r.Outcome]
	if !ok {
		return fmt.Errorf("store: record transcript of entry %d: unknown outcome %q: %w", entryID, r.Outcome, ErrInvalid)
	}
	at := r.At
	if at.IsZero() {
		at = s.now()
	}
	if !r.Reason.valid() {
		return fmt.Errorf("store: record transcript of entry %d: unknown reason %q: %w", entryID, r.Reason, ErrInvalid)
	}
	reason := nullStr(string(r.Reason))
	if r.Outcome == TranscriptOK {
		reason = nil
	}
	args := []any{at.Unix(), reason}
	switch {
	case r.Outcome.staysQueued():
		if r.RetryAt.IsZero() {
			return fmt.Errorf("store: record transcript of entry %d: %s without a time to retry: %w", entryID, r.Outcome, ErrInvalid)
		}
		set += queuedAgainSQL
		args = append(args, r.RetryAt.Unix(), at.Unix())
	case r.Outcome == TranscriptFailed:
		args = append(args, boolInt(r.Requested))
	case r.Outcome == TranscriptOK:
		if strings.TrimSpace(r.Transcript.Text) == "" {
			return fmt.Errorf("store: record transcript of entry %d: empty text: %w", entryID, ErrInvalid)
		}
		args = append(args, nullStr(r.Transcript.Lang), boolInt(r.Transcript.Generated), r.Transcript.Text)
	}
	args = append(args, entryID)
	update := `UPDATE entry_transcripts SET ` + set + ` WHERE entry_id = ? AND status = 'pending'`
	var (
		streak     string // the change to the streak of refusals, if any
		streakArgs []any
	)
	switch {
	case r.Outcome == TranscriptBlocked:
		streak, streakArgs = pauseTranscriptsSQL, []any{r.RetryAt.Unix()}
	case r.Requested:
		streak = endRefusalsSQL
	}
	var err error
	if streak != "" {
		var notQueued error
		err = s.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, streak, streakArgs...); err != nil {
				return classify(err)
			}
			notQueued = execOne(ctx, tx, "queued transcript of entry", entryID, update, args...)
			if errors.Is(notQueued, ErrNotFound) {
				return nil // the pause or the end of the streak is kept
			}
			return notQueued
		})
		err = cmp.Or(err, notQueued)
	} else {
		err = execOne(ctx, s.db, "queued transcript of entry", entryID, update, args...)
	}
	if err != nil {
		return fmt.Errorf("store: record transcript (%s): %w", r.Outcome, err)
	}
	return nil
}

// GetTranscript returns the entry's transcript, or ErrNotFound unless one
// was fetched.
func (s *SQLite) GetTranscript(ctx context.Context, entryID int64) (Transcript, error) {
	var (
		t    Transcript
		lang sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT lang, is_generated, text FROM entry_transcripts WHERE entry_id = ? AND status = 'ok'`,
		entryID).Scan(&lang, &t.Generated, &t.Text)
	if err != nil {
		return Transcript{}, fmt.Errorf("store: transcript of entry %d: %w", entryID, classify(err))
	}
	t.Lang = lang.String
	return t, nil
}

// TranscriptStateOf returns the stored state of the entry's transcript, with
// the end of the stored pause; StatusNone when it was never queued.
func (s *SQLite) TranscriptStateOf(ctx context.Context, entryID int64) (TranscriptState, error) {
	var (
		st                   TranscriptState
		status, reason, lang sql.NullString
		queued, due, paused  sql.NullInt64
		attempts             sql.NullInt64
		generated, rechecked sql.NullBool
	)
	err := s.db.QueryRowContext(ctx, `SELECT t.status, t.reason, t.lang, t.is_generated, t.attempts, t.rechecked,
			t.queued_at, t.due_at, (SELECT paused_until FROM transcript_pause WHERE id = 1)
		FROM (SELECT 1) LEFT JOIN entry_transcripts t ON t.entry_id = ?`, entryID).Scan(
		&status, &reason, &lang, &generated, &attempts, &rechecked, &queued, &due, &paused)
	if err != nil {
		return TranscriptState{}, fmt.Errorf("store: transcript state of entry %d: %w", entryID, classify(err))
	}
	st.Status, st.Reason, st.Lang = TranscriptStatus(status.String), TranscriptReason(reason.String), lang.String
	st.Generated, st.Attempts, st.Rechecked = generated.Bool, int(attempts.Int64), rechecked.Bool
	if queued.Valid {
		st.QueuedAt, st.DueAt = timeFromUnix(queued.Int64), timeFromUnix(due.Int64)
	}
	if paused.Valid {
		st.PausedUntil = timeFromUnix(paused.Int64)
	}
	return st, nil
}

// requestTranscriptSQL queues an entry's transcript at the user's request
// (?1 is the request's time, ?2 the entry): a new row, a done one queued
// again from scratch, or a pending one made due now with its attempts kept.
// A fetched transcript ('ok') is left alone: the upsert then changes no row,
// and neither does an entry that does not exist.
const requestTranscriptSQL = `INSERT INTO entry_transcripts (entry_id, status, queued_at, due_at, updated_at)
	SELECT id, 'pending', ?1, ?1, ?1 FROM entries WHERE id = ?2
	ON CONFLICT (entry_id) DO UPDATE SET
		attempts  = CASE WHEN status = 'pending' THEN attempts ELSE 0 END,
		rechecked = CASE WHEN status = 'pending' THEN rechecked ELSE 0 END,
		reason    = CASE WHEN status = 'pending' THEN reason ELSE NULL END,
		status = 'pending', queued_at = ?1, due_at = ?1, updated_at = ?1
	WHERE status <> 'ok'`

// RequestTranscript queues the entry's transcript as of at, due at once; see
// Store.RequestTranscript.
func (s *SQLite) RequestTranscript(ctx context.Context, entryID int64, at time.Time) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, requestTranscriptSQL, at.Unix(), entryID)
		if err != nil {
			return classify(err)
		}
		if n, err := res.RowsAffected(); err != nil || n > 0 {
			return err
		}
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM entries WHERE id = ?)`, entryID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("entry %d: %w", entryID, ErrNotFound)
		}
		return fmt.Errorf("entry %d: transcript already fetched: %w", entryID, ErrConflict)
	})
	if err != nil {
		return fmt.Errorf("store: request transcript: %w", err)
	}
	return nil
}

// settingTranscripts is the app_settings key of the global transcript
// switch: '1' = on; any other value, or none, = off.
const settingTranscripts = "transcripts"

// transcriptsOnSQL reads the global transcript switch.
const transcriptsOnSQL = `SELECT EXISTS (SELECT 1 FROM app_settings WHERE key = '` + settingTranscripts + `' AND value = '1')`

// TranscriptsSetting returns the global transcript switch (off unless set).
func (s *SQLite) TranscriptsSetting(ctx context.Context) (bool, error) {
	var on bool
	if err := s.db.QueryRowContext(ctx, transcriptsOnSQL).Scan(&on); err != nil {
		return false, fmt.Errorf("store: transcripts setting: %w", err)
	}
	return on, nil
}

// thawSQL restarts the queue age of the pending transcripts a switch turned
// back on lets through, frozen while it was off: queued_at = ?1; due_at,
// attempts and rechecked are kept. Callers may narrow the rows with an AND.
const thawSQL = `UPDATE entry_transcripts SET queued_at = ?1, updated_at = ?1 WHERE status = 'pending'`

// SetTranscriptsSetting stores the global transcript switch, thawing every
// pending transcript when it turns on from off.
func (s *SQLite) SetTranscriptsSetting(ctx context.Context, on bool, at time.Time) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var was bool
		if err := tx.QueryRowContext(ctx, transcriptsOnSQL).Scan(&was); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, settingTranscripts, strconv.Itoa(boolInt(on))); err != nil {
			return err
		}
		if on && !was {
			if _, err := tx.ExecContext(ctx, thawSQL, at.Unix()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: set transcripts setting: %w", err)
	}
	return nil
}

// settingServerTranscripts is the app_settings key of the server's
// transcript switch (FR_TRANSCRIPTS) as the last start recorded it: '0' =
// off; any other value, or none, = on.
const settingServerTranscripts = "transcripts_server"

// serverOffSQL reads whether the server's switch was recorded off.
const serverOffSQL = `SELECT EXISTS (SELECT 1 FROM app_settings WHERE key = '` + settingServerTranscripts + `' AND value = '0')`

// ServerTranscripts returns the server's transcript switch as last recorded
// (on unless recorded off).
func (s *SQLite) ServerTranscripts(ctx context.Context) (bool, error) {
	var off bool
	if err := s.db.QueryRowContext(ctx, serverOffSQL).Scan(&off); err != nil {
		return false, fmt.Errorf("store: server transcripts switch: %w", err)
	}
	return !off, nil
}

// RecordServerTranscripts records the server's transcript switch, thawing
// every pending transcript when it turns on from off (they were frozen
// while the server ran with it off).
func (s *SQLite) RecordServerTranscripts(ctx context.Context, on bool, at time.Time) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var wasOff bool
		if err := tx.QueryRowContext(ctx, serverOffSQL).Scan(&wasOff); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO app_settings (key, value) VALUES (?, ?)
			ON CONFLICT (key) DO UPDATE SET value = excluded.value`, settingServerTranscripts, strconv.Itoa(boolInt(on))); err != nil {
			return err
		}
		if on && wasOff {
			if _, err := tx.ExecContext(ctx, thawSQL, at.Unix()); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: record server transcripts switch: %w", err)
	}
	return nil
}

// FeedTranscripts returns the feed's transcript switch, or ErrNotFound.
func (s *SQLite) FeedTranscripts(ctx context.Context, feedID int64) (bool, error) {
	var on bool
	err := s.db.QueryRowContext(ctx, `SELECT transcripts_enabled FROM feeds WHERE id = ?`, feedID).Scan(&on)
	if err != nil {
		return false, fmt.Errorf("store: transcripts of feed %d: %w", feedID, classify(err))
	}
	return on, nil
}

// SetFeedTranscripts stores the feed's transcript switch, thawing the feed's
// pending transcripts when it turns on from off.
func (s *SQLite) SetFeedTranscripts(ctx context.Context, feedID int64, on bool, at time.Time) error {
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var was bool
		if err := tx.QueryRowContext(ctx, `SELECT transcripts_enabled FROM feeds WHERE id = ?`, feedID).Scan(&was); err != nil {
			return fmt.Errorf("feed %d: %w", feedID, classify(err))
		}
		if _, err := tx.ExecContext(ctx, `UPDATE feeds SET transcripts_enabled = ? WHERE id = ?`, boolInt(on), feedID); err != nil {
			return err
		}
		if on && !was {
			if _, err := tx.ExecContext(ctx, thawSQL+` AND entry_id IN (SELECT id FROM entries WHERE feed_id = ?2)`,
				at.Unix(), feedID); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("store: set transcripts of feed: %w", err)
	}
	return nil
}
