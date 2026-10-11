package innertube

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/giacomomicoli/feedreader/internal/fetch"
)

// videoIDLen is the length of a video ID.
const videoIDLen = 11

// VideoIDPattern matches a video ID: 11 base64url characters.
var VideoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// ChannelIDPattern matches a channel ID: "UC" and 22 base64url
// characters.
var ChannelIDPattern = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)

// Video is what a player answer tells about a video, each field cut to its
// cap (limits.go) and valid UTF-8. Title, Description and Author are
// YouTube's plain text, not HTML, and not yet cleaned for display.
type Video struct {
	ID          string // the ID asked about
	ChannelID   string // the channel asked about
	Title       string
	Description string
	Author      string    // the channel's name
	Published   time.Time // in UTC, never zero
}

// VideoRequest returns the POST asking for the player answer of video id
// as the web client, whose answers carry the publication date (the
// Android app's carry none).
func VideoRequest(id string) fetch.Request {
	r := PlayerRequest(Web, id)
	r.MaxBytes = MaxVideoBytes
	return r
}

// videoAnswer is the part of a player answer ParseVideo reads. Each string
// is cut to its cap as it is decoded; a member that repeats is read once,
// at its first occurrence (strings and booleans alike).
type videoAnswer struct {
	PlayabilityStatus struct {
		Status scalarString `json:"status"`
		Reason reasonString `json:"reason"`
	} `json:"playabilityStatus"`
	VideoDetails struct {
		VideoID          scalarString      `json:"videoId"`
		ChannelID        scalarString      `json:"channelId"`
		Title            titleString       `json:"title"`
		ShortDescription descriptionString `json:"shortDescription"`
		Author           authorString      `json:"author"`
		IsLive           firstBool         `json:"isLive"`
		IsUpcoming       firstBool         `json:"isUpcoming"`
	} `json:"videoDetails"`
	Microformat struct {
		Renderer struct {
			PublishDate scalarString `json:"publishDate"`
		} `json:"playerMicroformatRenderer"`
	} `json:"microformat"`
}

// boundedString is a JSON string decoded to at most a cap of bytes (cut at
// a rune boundary, invalid UTF-8 replaced). Only the first occurrence of a
// member is decoded, so repeating it costs nothing; a value of another
// type is left empty.
type boundedString struct {
	s   string
	set bool
}

func (b *boundedString) decode(raw []byte, n int) {
	if b.set || len(raw) < 2 || raw[0] != '"' {
		return
	}
	b.s, b.set = Unquote(raw[1:len(raw)-1], n), true
	if len(b.s) > n {
		b.s = string(ClipUTF8Bytes([]byte(b.s), n))
	}
}

type (
	scalarString      struct{ boundedString }
	reasonString      struct{ boundedString }
	titleString       struct{ boundedString }
	descriptionString struct{ boundedString }
	authorString      struct{ boundedString }
)

func (s *scalarString) UnmarshalJSON(raw []byte) error { s.decode(raw, MaxScalarBytes); return nil }
func (s *reasonString) UnmarshalJSON(raw []byte) error { s.decode(raw, MaxReasonBytes); return nil }
func (s *titleString) UnmarshalJSON(raw []byte) error  { s.decode(raw, MaxTitleBytes); return nil }
func (s *descriptionString) UnmarshalJSON(raw []byte) error {
	s.decode(raw, MaxDescriptionBytes)
	return nil
}
func (s *authorString) UnmarshalJSON(raw []byte) error { s.decode(raw, MaxAuthorBytes); return nil }

// firstBool is a JSON boolean read, like boundedString, at its first
// occurrence only, so that every member ParseVideo reads takes its first
// value when an answer repeats it. A value of another type (null
// included) is not one.
type firstBool struct{ v, set bool }

func (b *firstBool) UnmarshalJSON(raw []byte) error {
	if !b.set && (string(raw) == "true" || string(raw) == "false") {
		b.v, b.set = string(raw) == "true", true
	}
	return nil
}

// ParseVideo reads the player answer, served with contentType, for video
// id of channel channelID.
//
// The answer must be about that video (videoDetails.videoId) of that
// channel (videoDetails.channelId), else ErrOtherVideo: a video that does
// not exist answers with status ERROR and no details, and a forged answer
// cannot put another channel's video in a feed. Its publication date
// (microformat.playerMicroformatRenderer.publishDate) must be RFC 3339,
// else ErrNoDate; a live stream or a premiere that has not aired is
// ErrNotAired. A web page, or a sign-in demand that is not about the video
// (a bot check, see Classify), is ErrBlocked. The web client's answer for
// a video it will not play (UNPLAYABLE) still carries the details: only
// the checks above decide.
func ParseVideo(body []byte, contentType, id, channelID string) (Video, error) {
	const step = "player answer"
	switch {
	case IsWebPage(contentType):
		return Video{}, fmt.Errorf("%w: %s is a web page", ErrBlocked, step)
	case !IsJSON(contentType):
		return Video{}, formatError(step, "content type is not JSON")
	case len(body) > MaxVideoBytes:
		return Video{}, fmt.Errorf("%w: %s", ErrTooLarge, step)
	}
	var a videoAnswer
	if err := json.Unmarshal(body, &a); err != nil && !isTypeError(err) {
		return Video{}, formatError(step, "invalid JSON")
	}
	status := a.PlayabilityStatus.Status.s
	if Classify(status, a.PlayabilityStatus.Reason.s) == Refused {
		return Video{}, fmt.Errorf("%w: sign-in required (%s)", ErrBlocked, status)
	}
	d := &a.VideoDetails
	switch {
	case d.VideoID.s != id || d.ChannelID.s != channelID:
		return Video{}, ErrOtherVideo
	case d.IsLive.v || d.IsUpcoming.v:
		return Video{}, ErrNotAired
	}
	published, err := time.Parse(time.RFC3339, a.Microformat.Renderer.PublishDate.s)
	if err != nil || published.IsZero() {
		return Video{}, ErrNoDate
	}
	return Video{
		ID:          id,
		ChannelID:   channelID,
		Title:       d.Title.s,
		Description: d.ShortDescription.s,
		Author:      d.Author.s,
		Published:   published.UTC(),
	}, nil
}

// isTypeError reports whether err is a JSON value of an unexpected type,
// which encoding/json skips while decoding the rest.
func isTypeError(err error) bool {
	var te *json.UnmarshalTypeError
	return errors.As(err, &te)
}
