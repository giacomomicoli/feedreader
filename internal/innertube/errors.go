package innertube

import (
	"errors"
	"fmt"
)

// Outcomes of the fallback's requests (Client). None is a bug; the callers
// treat each as the failure of that request, or skip one video.
var (
	// ErrBlocked: YouTube refused this client (a rate limit, a sign-in
	// demand or bot check, a consent or captcha page, a redirect). Callers
	// pause every InnerTube request.
	ErrBlocked = errors.New("innertube: YouTube refused the request")
	// ErrRejected: YouTube answered 400: it does not accept the request as
	// sent (a client version it no longer takes, say). Asking again in
	// the same way gets the same answer.
	ErrRejected = errors.New("innertube: YouTube rejected the request")
	// ErrAlert: the answer is an error alert or an error object instead of
	// a channel's page: the channel does not exist, say.
	ErrAlert = errors.New("innertube: YouTube answered with an error")
	// ErrNoTab: the channel's page has no such tab (YouTube answered with
	// another one).
	ErrNoTab = errors.New("innertube: the channel has no such tab")
	// ErrTooLarge: an answer is larger than its cap (limits.go).
	ErrTooLarge = errors.New("innertube: YouTube's answer is too large")
	// ErrFormat: an answer is not what this package understands: the
	// undocumented API changed, or the answer is hostile.
	ErrFormat = errors.New("innertube: unexpected answer from YouTube")

	// ErrSkipVideo: a player answer gives no details to store for the
	// video asked about. ErrOtherVideo, ErrNoDate and ErrNotAired match
	// it.
	ErrSkipVideo = errors.New("innertube: no details for the video")
	// ErrOtherVideo: the answer is about another video or another
	// channel, or about none (a video that does not exist).
	ErrOtherVideo error = &subError{msg: "innertube: the answer is about another video or channel", parent: ErrSkipVideo}
	// ErrNoDate: the answer has no publication date in RFC 3339 form.
	ErrNoDate error = &subError{msg: "innertube: the video has no publication date", parent: ErrSkipVideo}
	// ErrNotAired: a live stream on air, or a premiere or stream that has
	// not started: it has no final publication date yet.
	ErrNotAired error = &subError{msg: "innertube: the video is live or has not aired yet", parent: ErrSkipVideo}

	// errInvalidID is a channel or video ID of the wrong form; no request
	// is made for it.
	errInvalidID = errors.New("innertube: invalid ID")
)

// subError is a sentinel that also matches a broader one.
type subError struct {
	msg    string
	parent error
}

func (e *subError) Error() string { return e.msg }
func (e *subError) Unwrap() error { return e.parent }

// formatError returns ErrFormat with detail, for the answer named step.
func formatError(step, detail string) error {
	return fmt.Errorf("%w: %s: %s", ErrFormat, step, detail)
}
