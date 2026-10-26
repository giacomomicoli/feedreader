package sched

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/innertube"
	"github.com/giacomomicoli/feedreader/internal/parse"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// The YouTube fallback (FR_YOUTUBE_FALLBACK). YouTube's feed endpoint
// answers 404 or 5xx for working channels for hours at a time, while its
// web API keeps working. When the feed of a channel (in any mode: the
// channel feed, its videos-only or its Shorts-only list; see
// resolve.FeedMode) fails that way, the videos it would list are read from
// that API instead (see innertube.Client), in the same poll and the same
// worker slot, so a feed still has one request in flight at a time:
//
//  1. one browse request per tab the mode follows (fallbackTabs, at most
//     config.YouTubeFallbackMaxBrowse): the newest
//     config.YouTubeFallbackMaxIDs videos of the tab, its continuation
//     never followed;
//  2. one player request for each of those videos the feed has no entry
//     for yet (store.Store.KnownGUIDs), at most
//     config.YouTubeFallbackMaxVideos, newest first, the tabs taken in
//     turn; videos that give no details to store (another channel's, no
//     publication date, live or upcoming, dated more than
//     maxPublishedAhead ahead) are skipped and asked about again at the
//     next fallback.
//
// The requests are made one after another, at least
// config.YouTubeFallbackSpacing apart across all feeds and the add flow
// (fallbackPacer). The entries are built as the feed's would be
// (parse.VideoEntry), so the feed coming back finds them.
//
// No InnerTube request is made while YouTube's refusal of one (by the
// fallback or by the transcript queue) is paused (store.InnerTubePause); a
// refusal met by the fallback pauses both, for as long as a transcript
// refusal would. Any failure (a refusal, a 400, an error alert, an answer
// that is not understood, a network error, no requested tab at all) ends
// the fallback and the poll is recorded as the feed's own failure.

// fallbackTabs are the tabs of a channel's page that hold what each mode
// follows. The channel feed also lists the recordings of past live
// streams, which are on neither the Videos nor the Shorts tab; the
// videos-only list (UULF) leaves them out.
var fallbackTabs = map[resolve.Mode][]innertube.Tab{
	resolve.ModeBoth:   {innertube.TabVideos, innertube.TabShorts, innertube.TabLive},
	resolve.ModeVideos: {innertube.TabVideos},
	resolve.ModeShorts: {innertube.TabShorts},
}

// maxPublishedAhead bounds how far ahead of now a video's publication date
// may lie. The date is the upload's (it equals the feed's <published>), so
// an aired video is never dated ahead but for clock skew; an answer that
// dates a video later would pin it at the top of every grid, so the video
// is skipped (and asked about again at the next fallback).
const maxPublishedAhead = time.Hour

// fallbackSource is the source logged for a poll the fallback stored.
const fallbackSource = "innertube"

var (
	// errFallbackPaused: YouTube refused an InnerTube request recently.
	errFallbackPaused = errors.New("YouTube fallback paused after a refusal")
	// errFallbackNoTab: the channel's page has none of the tabs asked for.
	errFallbackNoTab = errors.New("YouTube fallback: the channel's page has no such tab")
	// errFallbackNothing: the add flow's fallback found no video to store.
	errFallbackNothing = errors.New("YouTube fallback: no video with details to store")
)

// fallbackResult is what a fallback read: the entries of the videos not
// stored yet, and counts for the log.
type fallbackResult struct {
	entries []parse.Entry
	// author is the channel's name, as the first video's details give it.
	author                 string
	listed, known, skipped int
}

// fallbackChannel returns the channel and mode a fallback reads for the
// feed at feedURL of kind kind whose fetch failed with err, when the
// fallback is on and err is a 404 or 5xx answer without Retry-After.
func (s *Scheduler) fallbackChannel(kind store.Kind, feedURL string, err error) (string, resolve.Mode, bool) {
	// A 404 or 5xx carrying Retry-After is a rate limit (rateLimitFailure):
	// the host asked to back off, and the web API is on the same host.
	if !s.cfg.YouTubeFallback || kind != store.KindYouTube || !feedEndpointDown(err) || rateLimitFailure(err) {
		return "", "", false
	}
	id, m, ok := resolve.FeedMode(feedURL)
	if !ok || len(fallbackTabs[m]) == 0 {
		return "", "", false
	}
	return id, m, true
}

// pollFallback runs the fallback for the poll of f, sent at started, whose
// fetch failed with cause, when it applies (fallbackChannel); done reports
// whether it did, and hold is then poll's result.
//
// When the fallback reads the channel, the outcome is stored as a 200
// would be (RecordFetchSuccess: failures cleared, the new entries unread,
// or a pending subscription's initial add, transcripts queued as for a
// poll), keeping the feed's validators and <ttl>, and logged as a poll
// with source=innertube. When it fails, the poll's failure is cause, the
// feed's own, recorded by recordFailure (as a manual one when manual is
// set). Cut short by ctx, it records nothing.
func (s *Scheduler) pollFallback(ctx context.Context, f store.Feed, manual *manualRefresh, started time.Time,
	base time.Duration, cause error) (hold time.Time, done bool) {
	channelID, m, ok := s.fallbackChannel(f.Kind, f.URL, cause)
	if !ok {
		return time.Time{}, false
	}
	res, err := s.readChannel(ctx, s.fetcher, f.ID, channelID, m)
	if ctx.Err() != nil {
		return time.Time{}, true // our own shutdown, not the feed's fault
	}
	fetchedAt := s.now()
	if err != nil {
		s.log.Warn("YouTube fallback failed", "feed", f.ID, "url", redact(f.URL), "mode", m, "err", failureMessage(err))
		return s.recordFailure(ctx, f, manual, started, fetchedAt, base, cause), true
	}
	regular := fetchedAt.Add(withHints(base, storedTTL(f), 0))
	// As for a 200: a pending subscription's first success is an initial
	// add, which never queues transcripts.
	queueTranscripts := s.cfg.Transcripts && !f.InitialPending
	var inserted int
	next, scheduled, err := s.storeOutcome(ctx, f, started, regular, func(scheduled time.Time) (err error) {
		inserted, err = s.st.RecordFetchSuccess(ctx, store.FetchSuccess{
			FeedID:        f.ID,
			FetchedAt:     fetchedAt,
			NextFetchAt:   scheduled,
			ETag:          f.ETag,
			LastModified:  f.LastModified,
			TTLSec:        f.TTLSec,
			Entries:       newEntries(res.entries, fetchedAt),
			InitialUnread: config.InitialUnread,
			// A tab lists older videos than the feed did (it lists 15
			// per tab, the feed 15 in all): those are not new.
			OlderAsRead:      true,
			QueueTranscripts: queueTranscripts,
		})
		return err
	})
	if err != nil {
		if ctx.Err() != nil || errors.Is(err, store.ErrNotFound) {
			return s.checkStored(ctx, f, next, err), true
		}
		s.log.Error("could not store fetched entries", "feed", f.ID, "err", err)
		return s.recordFailure(ctx, f, manual, started, fetchedAt, base, fmt.Errorf("could not save entries: %w", err)), true
	}
	var status int
	if se := (*fetch.StatusError)(nil); errors.As(cause, &se) {
		status = se.StatusCode
	}
	s.log.Info("polled feed", "feed", f.ID, "url", redact(f.URL), "status", status, "new", inserted,
		"took", fetchedAt.Sub(started), "next_fetch", scheduled, "source", fallbackSource, "mode", m,
		"listed", res.listed, "known", res.known, "skipped", res.skipped)
	if queueTranscripts && inserted > 0 {
		s.signalTranscripts()
	}
	return time.Time{}, true
}

// readChannel runs the fallback for channel channelID in mode m through f:
// the entries of the videos feed feedID has not stored (every video for 0,
// the add flow). See the comment at the top of this file.
func (s *Scheduler) readChannel(ctx context.Context, f Fetcher, feedID int64, channelID string, m resolve.Mode) (fallbackResult, error) {
	pause, err := s.st.InnerTubePause(ctx)
	if err != nil {
		return fallbackResult{}, err
	}
	// A pause ending further ahead than any pause lasts was stored while
	// the clock was ahead: it is over, as for the transcript queue.
	if wait := pause.Until.Sub(s.now()); wait > 0 && wait <= maxTranscriptDue {
		return fallbackResult{}, fmt.Errorf("%w until %s", errFallbackPaused, pause.Until.UTC().Format(time.RFC3339))
	}
	c := innertube.NewClient(pacedFetcher{s: s, f: f})
	var (
		res      fallbackResult
		lists    [][]string
		answered bool // a request YouTube did not refuse
		refused  bool // a request YouTube refused
	)
	defer func() {
		if answered && !refused && pause.Refusals > 0 && ctx.Err() == nil {
			if err := s.st.RecordInnerTubeAnswer(ctx); err != nil {
				s.log.Warn("could not end the InnerTube refusal streak", "err", err)
			}
		}
	}()
	tabs := fallbackTabs[m]
	for _, tab := range tabs[:min(len(tabs), config.YouTubeFallbackMaxBrowse)] {
		ids, err := c.Browse(ctx, channelID, tab, config.YouTubeFallbackMaxIDs)
		if errors.Is(err, innertube.ErrNoTab) {
			answered = true
			continue
		}
		if err != nil {
			refused = errors.Is(err, innertube.ErrBlocked)
			return fallbackResult{}, s.fallbackError(ctx, err, pause.Refusals)
		}
		answered = true
		lists = append(lists, ids)
	}
	if len(lists) == 0 {
		return fallbackResult{}, errFallbackNoTab
	}
	ids := interleave(lists)
	res.listed = len(ids)
	unknown, err := s.unknownVideos(ctx, feedID, ids)
	if err != nil {
		return fallbackResult{}, err
	}
	res.known = len(ids) - len(unknown)
	limit := s.now().Add(maxPublishedAhead)
	for _, id := range unknown[:min(len(unknown), config.YouTubeFallbackMaxVideos)] {
		v, err := c.Video(ctx, id, channelID)
		if err != nil && !errors.Is(err, innertube.ErrSkipVideo) {
			refused = errors.Is(err, innertube.ErrBlocked)
			return fallbackResult{}, s.fallbackError(ctx, err, pause.Refusals)
		}
		answered = true
		e, ok := parse.Entry{}, false
		if err == nil && !v.Published.After(limit) {
			e, ok = parse.VideoEntry(v.ID, v.Title, v.Description, v.Author, v.Published)
		}
		if !ok {
			res.skipped++
			continue
		}
		if res.author == "" {
			res.author = e.Author
		}
		res.entries = append(res.entries, e)
	}
	return res, nil
}

// fallbackError returns err, a failed fallback request, after recording a
// refusal: no InnerTube request is made for the pause a transcript refusal
// would get (blockedPause) after the refusals before it.
func (s *Scheduler) fallbackError(ctx context.Context, err error, refusals int) error {
	if ctx.Err() != nil || !errors.Is(err, innertube.ErrBlocked) {
		return err
	}
	pause := s.blockedPause(err, refusals)
	until := s.now().Add(pause)
	if rerr := s.st.RecordInnerTubeRefusal(ctx, until); rerr != nil {
		s.log.Error("could not store the InnerTube pause", "err", rerr)
	}
	s.log.Warn("YouTube refused a fallback request", "err", failureMessage(err), "pause", pause, "refusals", refusals+1)
	return err
}

// unknownVideos returns the IDs among ids, in order, whose entries feed
// feedID does not have (all of them for feed 0).
func (s *Scheduler) unknownVideos(ctx context.Context, feedID int64, ids []string) ([]string, error) {
	if feedID == 0 || len(ids) == 0 {
		return ids, nil
	}
	guids := make([]string, len(ids))
	for i, id := range ids {
		guids[i] = parse.VideoGUID(id)
	}
	known, err := s.st.KnownGUIDs(ctx, feedID, guids)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ids))
	for i, id := range ids {
		if !known[guids[i]] {
			out = append(out, id)
		}
	}
	return out, nil
}

// interleave merges lists, each newest first, taking them in turn, without
// repeats: with a limit on the videos asked about, each tab gets its share
// of the newest.
func interleave(lists [][]string) []string {
	var out []string
	seen := map[string]bool{}
	for i := 0; ; i++ {
		more := false
		for _, l := range lists {
			if i >= len(l) {
				continue
			}
			more = true
			if !seen[l[i]] {
				seen[l[i]] = true
				out = append(out, l[i])
			}
		}
		if !more {
			return out
		}
	}
}

// pacedFetcher makes each fallback request wait its turn (fallbackPacer).
type pacedFetcher struct {
	s *Scheduler
	f Fetcher
}

func (p pacedFetcher) Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
	if err := p.s.fallbackTurn(ctx); err != nil {
		return nil, err
	}
	return p.f.Fetch(ctx, r)
}

// fallbackTurn waits until the next fallback request may be sent: at least
// fallbackSpacing after the one before, whichever feed or add flow sent it.
// It returns ctx's error when ctx ends first.
func (s *Scheduler) fallbackTurn(ctx context.Context) error {
	s.fallbackMu.Lock()
	now := time.Now()
	at := max(s.fallbackNext.Sub(now), 0)
	s.fallbackNext = now.Add(at + s.fallbackSpacing)
	s.fallbackMu.Unlock()
	if at > 0 {
		s.fallbackSleep(ctx, at)
	}
	return ctx.Err()
}
