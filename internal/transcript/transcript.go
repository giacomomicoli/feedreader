// Package transcript fetches the transcript of a YouTube video without an
// API key or cookies, the way YouTube's own Android app does. There is no
// official API for this, so it takes two requests of an undocumented one:
//
//  1. A POST of the video ID to the player endpoint
//     (www.youtube.com/youtubei/v1/player), which answers with the player
//     response: whether the video plays, and its caption tracks, each with
//     a signed URL on www.youtube.com/api/timedtext.
//  2. A GET of the chosen track's URL, asking for YouTube's JSON caption
//     format.
//
// The watch page is not used: the caption URLs it lists need a
// proof-of-origin token that only a browser can compute.
//
// Every outcome other than a transcript is normal: a video without
// captions, a live stream or premiere that has not aired yet, a request
// YouTube blocks (it rate-limits and blocks clients that ask too often or
// from data-centre addresses), a caption track too long to read, or an
// answer whose format changed. Callers
// fetch transcripts one at a time, slowly, and treat failures as a missing
// transcript.
package transcript

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/innertube"
)

// Fetcher is the subset of *fetch.Client the package needs.
type Fetcher interface {
	Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error)
}

var _ Fetcher = (*fetch.Client)(nil)

// Transcript is the text of one caption track.
type Transcript struct {
	Lang      string // BCP 47 language code of the track, e.g. "en"
	Generated bool   // YouTube's automatic speech recognition, not authored captions
	Text      string // plain text, never HTML; see Client.Fetch for its bounds
}

// Outcomes other than a transcript. Callers record them; none is a bug.
var (
	// ErrNoCaptions: the video has no caption track (or none usable).
	ErrNoCaptions = errors.New("transcript: the video has no captions")
	// ErrBlocked: YouTube refused to serve the page or the captions to
	// this client (rate limit, bot check, consent wall). Callers should
	// pause before the next request.
	ErrBlocked = errors.New("transcript: YouTube blocked the request")
	// ErrUnavailable: YouTube does not play the video to this client:
	// removed, private, members-only, age-restricted or region-blocked.
	// It matches ErrNoCaptions too. YouTube answers the same way ("This
	// video is unavailable") when it stops serving this kind of client
	// altogether, so it is not proof that the video is gone.
	ErrUnavailable error = &subError{msg: "transcript: the video is unavailable", parent: ErrNoCaptions}
	// ErrNotReady: the video is a live stream on air, or a premiere or
	// stream that has not started. It matches none of the other outcomes:
	// once the video has aired it may well have captions, so callers ask
	// again later.
	ErrNotReady = errors.New("transcript: the video is live or has not aired yet")
	// ErrTooLarge: an answer is larger than the client reads: the caption
	// track of a long video, or the player response, over the Fetcher's
	// size cap (fetch.ErrTooLarge) or this package's own bound. Asking
	// again gets the same answer, so callers need not.
	ErrTooLarge = errors.New("transcript: YouTube's answer is too large")
)

// subError is a sentinel that also matches a broader one.
type subError struct {
	msg    string
	parent error
}

func (e *subError) Error() string { return e.msg }
func (e *subError) Unwrap() error { return e.parent }

// errFormat is an answer from YouTube that is not what this package
// understands: the undocumented API changed, or the answer is hostile.
var errFormat = errors.New("unexpected answer from YouTube")

// errInvalidID is a video ID that is not 11 base64url characters.
var errInvalidID = errors.New("transcript: invalid video ID")

// videoIDPattern: YouTube video IDs are 11 base64url characters.
var videoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// watchPath is the path of a video's page, the form of every YouTube entry
// URL (https://www.youtube.com/watch?v=<id>).
const watchPath = "/watch"

// VideoID returns the ID of the video an entry URL points to
// (https://www.youtube.com/watch?v=<id>), and whether it is one. The host
// may also be youtube.com or m.youtube.com and the scheme http; other forms
// (youtu.be, /shorts/, /embed/) are not entry URLs and are refused, as is
// a URL with credentials, a port or more than one v parameter.
func VideoID(entryURL string) (string, bool) {
	u, err := url.Parse(entryURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.Opaque != "" || u.Path != watchPath {
		return "", false
	}
	switch strings.ToLower(u.Host) {
	case "www.youtube.com", "youtube.com", "m.youtube.com":
	default:
		return "", false
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["v"]) != 1 || !videoIDPattern.MatchString(q["v"][0]) {
		return "", false
	}
	return q["v"][0], true
}

// Client fetches transcripts through a Fetcher. It holds no mutable state
// and is safe for concurrent use when its Fetcher is.
type Client struct {
	f Fetcher
}

// New returns a Client that performs HTTP through f.
func New(f Fetcher) *Client { return &Client{f: f} }

// Fetch returns the transcript of video id. Errors match ErrNoCaptions,
// ErrNotReady, ErrBlocked or ErrTooLarge via errors.Is when they mean that;
// anything else (network, an answer whose format changed) is returned
// wrapped. A cancelled ctx
// returns ctx's error.
//
// Fetch makes at most two requests through the Fetcher, the player POST and
// the caption GET, and none for an invalid id; neither follows a redirect,
// which is ErrBlocked. It picks the track YouTube
// shows for the video's original language, authored captions before
// automatic ones; Text is that track's text with cues joined into
// paragraphs, at most config.TranscriptMaxRunes runes ("…" ends a cut one).
// An error from the Fetcher stays reachable with errors.As (a
// *fetch.StatusError keeps its Retry-After); messages hold no page content
// and name the caption endpoint, not the signed caption URL.
func (c *Client) Fetch(ctx context.Context, id string) (Transcript, error) {
	if !videoIDPattern.MatchString(id) {
		return Transcript{}, errInvalidID
	}
	if err := ctx.Err(); err != nil {
		return Transcript{}, err
	}
	res, err := c.f.Fetch(ctx, playerRequest(id))
	if err != nil {
		return Transcript{}, requestError(ctx, "player request", playerURL, playerURL, err)
	}
	tr, err := parsePlayer(res.Body, res.ContentType, id)
	if err != nil {
		return Transcript{}, err
	}
	if err := ctx.Err(); err != nil {
		return Transcript{}, err
	}
	res, err = c.f.Fetch(ctx, captionRequest(tr.url))
	if err != nil {
		return Transcript{}, requestError(ctx, "caption track", tr.url, captionEndpoint, err)
	}
	if err := checkCaptionResponse(res.Body, res.ContentType, res.FinalURL); err != nil {
		return Transcript{}, err
	}
	text, err := parseJSON3(res.Body, config.TranscriptMaxRunes)
	if err != nil {
		return Transcript{}, err
	}
	return Transcript{Lang: tr.lang, Generated: tr.generated, Text: text}, nil
}

// requestError classifies an error of the Fetcher for request step (a
// label) of rawURL. Statuses that mean YouTube refuses this client become
// ErrBlocked, an answer over the size cap ErrTooLarge. The message shows a status as "HTTP <code>" (the URL it came
// from may be a captcha page's, carrying the caption URL), and other
// errors with endpoint in place of rawURL.
func requestError(ctx context.Context, step, rawURL, endpoint string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	e := &fetchError{step: step, msg: strings.ReplaceAll(err.Error(), rawURL, endpoint), err: err}
	if errors.Is(err, fetch.ErrTooLarge) {
		e.kind = ErrTooLarge
	}
	var se *fetch.StatusError
	if errors.As(err, &se) {
		e.msg = fmt.Sprintf("HTTP %d", se.StatusCode)
		if innertube.BlockedStatus(se.StatusCode) {
			e.kind = ErrBlocked
		}
	}
	return e
}

// fetchError is a failed request. It matches its kind (ErrBlocked,
// ErrTooLarge, or nil) and the Fetcher's error.
type fetchError struct {
	kind error
	step string
	msg  string
	err  error
}

func (e *fetchError) Error() string {
	prefix := "transcript"
	if e.kind != nil {
		prefix = e.kind.Error()
	}
	return prefix + ": " + e.step + ": " + e.msg
}

func (e *fetchError) Unwrap() []error {
	if e.kind != nil {
		return []error{e.kind, e.err}
	}
	return []error{e.err}
}

// formatError returns errFormat with detail, for the request named step.
func formatError(step, detail string) error {
	return fmt.Errorf("transcript: %s: %w: %s", step, errFormat, detail)
}
