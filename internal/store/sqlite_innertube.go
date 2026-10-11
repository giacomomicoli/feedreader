package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// MaxKnownGUIDs bounds the GUIDs one KnownGUIDs call asks about: far more
// than a YouTube fallback lists (two tabs of at most 15 videos), and few
// enough to stay one small parameterized query.
const MaxKnownGUIDs = 64

// KnownGUIDs returns which of guids the feed feedID already has entries
// for, matched exactly, with one parameterized query. It returns ErrInvalid
// for more than MaxKnownGUIDs GUIDs; none is an empty result without a
// query.
func (s *SQLite) KnownGUIDs(ctx context.Context, feedID int64, guids []string) (map[string]bool, error) {
	known := make(map[string]bool, len(guids))
	switch {
	case len(guids) == 0:
		return known, nil
	case len(guids) > MaxKnownGUIDs:
		return nil, fmt.Errorf("store: known GUIDs of feed %d: %d GUIDs, at most %d: %w", feedID, len(guids), MaxKnownGUIDs, ErrInvalid)
	}
	args := make([]any, 0, len(guids)+1)
	args = append(args, feedID)
	for _, g := range guids {
		args = append(args, g)
	}
	query := `SELECT guid FROM entries WHERE feed_id = ? AND guid IN (?` + strings.Repeat(",?", len(guids)-1) + `)`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: known GUIDs of feed %d: %w", feedID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var g string
		if err := rows.Scan(&g); err != nil {
			return nil, fmt.Errorf("store: known GUIDs of feed %d: %w", feedID, err)
		}
		known[g] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: known GUIDs of feed %d: %w", feedID, err)
	}
	return known, nil
}

// InnerTubePause returns the pause after YouTube's last refusal of an
// InnerTube request, from the transcript queue or the YouTube fallback
// (the transcript_pause row they share): when it ends (zero when never
// paused) and how many requests were refused in a row.
func (s *SQLite) InnerTubePause(ctx context.Context) (InnerTubePause, error) {
	var (
		until    sql.NullInt64
		refusals sql.NullInt64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT paused_until FROM transcript_pause WHERE id = 1), (SELECT refusals FROM transcript_pause WHERE id = 1)`).
		Scan(&until, &refusals)
	if err != nil {
		return InnerTubePause{}, fmt.Errorf("store: InnerTube pause: %w", err)
	}
	var p InnerTubePause
	if until.Valid {
		p.Until = timeFromUnix(until.Int64)
	}
	p.Refusals = int(refusals.Int64)
	return p, nil
}

// RecordInnerTubeRefusal stores a refusal of an InnerTube request by the
// YouTube fallback: no InnerTube request, transcripts included, is made
// before until, and the streak of refusals grows by one, as a transcript
// refusal does (TranscriptBlocked).
func (s *SQLite) RecordInnerTubeRefusal(ctx context.Context, until time.Time) error {
	if _, err := s.db.ExecContext(ctx, pauseTranscriptsSQL, until.Unix()); err != nil {
		return fmt.Errorf("store: record InnerTube refusal: %w", classify(err))
	}
	return nil
}

// RecordInnerTubeAnswer ends the streak of refusals after an InnerTube
// request of the YouTube fallback that YouTube answered without refusing
// it. The end of the last pause is kept.
func (s *SQLite) RecordInnerTubeAnswer(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, endRefusalsSQL); err != nil {
		return fmt.Errorf("store: record InnerTube answer: %w", classify(err))
	}
	return nil
}
