package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"github.com/giacomomicoli/feedreader/internal/fetch"
)

// The player request. YouTube's Android app asks the same endpoint as its
// web player, but its answers carry caption URLs that need no
// proof-of-origin token. The client version is one the endpoint accepts
// without further headers, an API key or cookies; YouTube may stop
// accepting it at any time, which shows up as ErrBlocked or
// ErrUnavailable for every video.
const (
	playerURL     = "https://www.youtube.com/youtubei/v1/player?prettyPrint=false"
	clientName    = "ANDROID"
	clientVersion = "20.10.38"
	// clientLang is the interface language asked for. Track choice does
	// not depend on it; it keeps YouTube's messages and track names in
	// one language whatever the server's location.
	clientLang = "en"
	// jsonType is the content type sent and expected.
	jsonType = "application/json"
)

// playerRequest returns the POST asking for the player response of video id.
func playerRequest(id string) fetch.Request {
	var body struct {
		Context struct {
			Client struct {
				Name    string `json:"clientName"`
				Version string `json:"clientVersion"`
				Lang    string `json:"hl"`
			} `json:"client"`
		} `json:"context"`
		VideoID string `json:"videoId"`
	}
	body.Context.Client.Name = clientName
	body.Context.Client.Version = clientVersion
	body.Context.Client.Lang = clientLang
	body.VideoID = id
	b, _ := json.Marshal(body) // strings only: cannot fail
	return fetch.Request{
		URL:         playerURL,
		Method:      http.MethodPost,
		Body:        string(b),
		ContentType: jsonType,
		Accept:      jsonType,
	}
}

// playerResponse is the part of the player response this package reads.
// Everything else in the (large) answer is skipped by encoding/json without
// allocating. Lists are fixed-size arrays: encoding/json fills the first
// elements and skips the rest without allocating, where a slice would grow
// to a hundred times the size of a hostile list of tiny elements. Elements
// keep their positions, so indices into the lists stay valid; positions
// past the end of a list hold zero values, which are not usable tracks.
//
// shape is decoded from the same answer in a second pass (decodePlayer).
type playerResponse struct {
	PlayabilityStatus struct {
		Status string `json:"status"`
		Reason string `json:"reason"`
	} `json:"playabilityStatus"`
	VideoDetails struct {
		VideoID string `json:"videoId"`
		// IsLive is set while a live stream is on air, IsUpcoming for a
		// premiere or stream that has not started.
		IsLive     bool `json:"isLive"`
		IsUpcoming bool `json:"isUpcoming"`
	} `json:"videoDetails"`
	Captions struct {
		Renderer captionRenderer `json:"playerCaptionsTracklistRenderer"`
	} `json:"captions"`
	shape playerShape
}

// captionRenderer lists a video's caption tracks and audio tracks; the
// indices point into those lists.
type captionRenderer struct {
	CaptionTracks          [maxCaptionTracks]captionTrack `json:"captionTracks"`
	AudioTracks            [maxAudioTracks]audioTrack     `json:"audioTracks"`
	DefaultAudioTrackIndex index                          `json:"defaultAudioTrackIndex"`
	// DefaultTranslationSourceTrackIndices is a list; only its first
	// element is read.
	DefaultTranslationSourceTrackIndices [1]index `json:"defaultTranslationSourceTrackIndices"`
}

// playerShape is which of the members leading to the caption tracks the
// player response has (a nil pointer: absent or null), and whether the
// track list is empty: YouTube leaves captions out for a video without
// any, or lists no tracks, while an answer with captions but without the
// renderer or the list is one whose format changed. The fixed-size arrays
// of playerResponse cannot tell: a zero value is also what an absent or
// empty list leaves.
//
// It is decoded in a pass of its own, so that nothing larger than a
// pointer is allocated per member: a pointer to captionRenderer would be
// allocated anew, whole, each time a hostile answer repeats the renderer
// after a null.
type playerShape struct {
	Captions *struct {
		Renderer *struct {
			Tracks *listShape `json:"captionTracks"`
		} `json:"playerCaptionsTracklistRenderer"`
	} `json:"captions"`
}

// listShape is a JSON value that was not null: empty when it is [].
type listShape struct{ empty bool }

// UnmarshalJSON looks at b, the whole value, which encoding/json has
// already checked.
func (l *listShape) UnmarshalJSON(b []byte) error {
	l.empty = len(b) >= 2 && b[0] == '[' && len(bytes.TrimSpace(b[1:len(b)-1])) == 0
	return nil
}

// captionTrack is one entry of captionTracks.
type captionTrack struct {
	BaseURL      string `json:"baseUrl"`
	LanguageCode string `json:"languageCode"`
	// Kind is "asr" for automatic captions; VSSID starts with "a." for them
	// (".en" is an authored English track, "a.en" an automatic one).
	Kind  string `json:"kind"`
	VSSID string `json:"vssId"`
}

// generated reports whether the track is automatic speech recognition.
func (t captionTrack) generated() bool {
	return t.Kind == "asr" || strings.HasPrefix(t.VSSID, "a.")
}

// audioTrack is one entry of audioTracks: the original audio or a dub.
type audioTrack struct {
	// AudioTrackID is "<language>.<n>", e.g. "en-US.4"; absent when the
	// video has a single audio track.
	AudioTrackID             string `json:"audioTrackId"`
	DefaultCaptionTrackIndex index  `json:"defaultCaptionTrackIndex"`
}

// isTypeError reports whether err is a JSON value of an unexpected type,
// which encoding/json skips while decoding the rest.
func isTypeError(err error) bool {
	var te *json.UnmarshalTypeError
	return errors.As(err, &te)
}

// index is a non-negative JSON integer of at most maxIndexDigits digits;
// anything else (absent, null, negative, fractional, with an exponent, a
// string, a longer number) is not valid.
type index struct {
	n     int
	valid bool
}

// UnmarshalJSON reads b, one JSON value that encoding/json has already
// checked, by hand: a hostile answer can repeat an index member over and
// over, and json.Unmarshal for each took 5.6 times the answer's size.
func (i *index) UnmarshalJSON(b []byte) error {
	*i = index{}
	if len(b) == 0 || len(b) > maxIndexDigits {
		return nil
	}
	n := 0
	for _, c := range b {
		if c < '0' || c > '9' {
			return nil
		}
		n = n*10 + int(c-'0')
	}
	*i = index{n: n, valid: true}
	return nil
}

// in returns the index when it is valid and below n.
func (i index) in(n int) (int, bool) {
	return i.n, i.valid && i.n < n
}

// track is the caption track chosen for a video.
type track struct {
	url       string // the caption URL to GET, asking for JSON
	lang      string
	generated bool
}

// Playability statuses (playabilityStatus.status).
const (
	statusOK = "OK"
	// statusLoginRequired is a bot check, or an age-restricted or private
	// video, told apart by the reason.
	statusLoginRequired = "LOGIN_REQUIRED"
	// statusUpcoming is a live stream or premiere that has not started.
	statusUpcoming = "LIVE_STREAM_OFFLINE"
)

// unavailableStatuses are statuses of a video this client cannot play:
// "ERROR" (removed or nonexistent, "This video is unavailable"),
// "UNPLAYABLE" (private, members-only, region-blocked) and the age and
// content checks.
var unavailableStatuses = map[string]bool{
	"ERROR": true, "UNPLAYABLE": true, "AGE_CHECK_REQUIRED": true,
	"AGE_VERIFICATION_REQUIRED": true, "CONTENT_CHECK_REQUIRED": true,
}

// loginUnavailableReasons mark a LOGIN_REQUIRED status that concerns the
// video rather than this client (in the English messages clientLang asks
// for): an age-restricted, private or members-only video. Any other sign-in
// demand, such as "Sign in to confirm you're not a bot", is a block, which
// pauses all transcript requests; a per-video refusal taken for one would
// cost that pause again each time the video's turn came round.
var loginUnavailableReasons = []string{
	"confirm your age", "inappropriate for some users", "age-restricted", "private",
	"members-only", "channel's members",
}

// statusPattern is a playability status safe to show in a message.
var statusPattern = regexp.MustCompile(`^[A-Z_]+$`)

// langPattern is a BCP 47-shaped language code, e.g. "en", "zh-Hant",
// "es-419".
var langPattern = regexp.MustCompile(`^[A-Za-z]{2,8}(-[A-Za-z0-9]{1,8})*$`)

// parsePlayer reads the player response for video id, served with
// contentType, and returns the caption track to fetch.
func parsePlayer(body []byte, contentType, id string) (track, error) {
	const step = "player response"
	mt, _, _ := mime.ParseMediaType(contentType)
	switch {
	case mt == "text/html":
		// A consent, captcha or error page instead of the API's answer.
		return track{}, fmt.Errorf("%w: %s is a web page", ErrBlocked, step)
	case mt != jsonType:
		return track{}, formatError(step, "content type is not JSON")
	case len(body) > maxPlayerBytes:
		return track{}, fmt.Errorf("%w: %s", ErrTooLarge, step)
	}
	p, err := decodePlayer(body)
	if err != nil {
		return track{}, formatError(step, "invalid JSON")
	}
	if err := playability(p); err != nil {
		return track{}, err
	}
	if p.VideoDetails.VideoID != id {
		return track{}, formatError(step, "describes another video")
	}
	switch {
	case p.VideoDetails.IsUpcoming:
		return track{}, fmt.Errorf("%w (upcoming)", ErrNotReady)
	case p.VideoDetails.IsLive:
		// Even when it lists a track: its captions would stop where the
		// stream is now.
		return track{}, fmt.Errorf("%w (live)", ErrNotReady)
	}
	// Only an answer without captions, or with an empty track list, means
	// that the video has none. Captions without the renderer or the list,
	// or a list without a usable track, are an answer whose format changed
	// (a renamed field): taken for a video without captions, it would pass
	// for one, silently, for every video.
	switch c := p.shape.Captions; {
	case c == nil:
		return track{}, ErrNoCaptions
	case c.Renderer == nil || c.Renderer.Tracks == nil:
		return track{}, formatError(step, "no caption track list")
	case c.Renderer.Tracks.empty:
		return track{}, ErrNoCaptions
	}
	tracks := &p.Captions.Renderer.CaptionTracks
	var (
		urls    [maxCaptionTracks]string // the usable tracks' caption URLs
		blocked error                    // a URL needs a proof-of-origin token
	)
	for i, t := range tracks {
		if t.BaseURL == "" || !langPattern.MatchString(t.LanguageCode) || len(t.LanguageCode) > maxLangBytes {
			continue
		}
		if urls[i], err = captionURL(t.BaseURL, id); errors.Is(err, ErrBlocked) {
			blocked = err
		}
	}
	i, ok := chooseTrack(&p.Captions.Renderer, func(i int) bool { return urls[i] != "" })
	switch {
	case !ok && blocked != nil:
		return track{}, blocked
	case !ok:
		return track{}, formatError(step, "no usable caption track")
	}
	return track{url: urls[i], lang: tracks[i].LanguageCode, generated: tracks[i].generated()}, nil
}

// decodePlayer decodes the player response, then its shape. Values of
// unexpected types are skipped; only invalid JSON is an error.
func decodePlayer(body []byte) (*playerResponse, error) {
	p := new(playerResponse)
	if err := json.Unmarshal(body, p); err != nil && !isTypeError(err) {
		return nil, err
	}
	if err := json.Unmarshal(body, &p.shape); err != nil && !isTypeError(err) {
		return nil, err
	}
	return p, nil
}

// playability classifies the player response's playabilityStatus. Its
// reason is YouTube's text (and its error screen holds HTML), so neither is
// ever part of the error; the status token is, when it is one.
func playability(p *playerResponse) error {
	status := p.PlayabilityStatus.Status
	switch {
	case status == statusOK:
		return nil
	case status == statusLoginRequired:
		reason := strings.ToLower(p.PlayabilityStatus.Reason)
		for _, marker := range loginUnavailableReasons {
			if strings.Contains(reason, marker) {
				return fmt.Errorf("%w (%s)", ErrUnavailable, status)
			}
		}
		return fmt.Errorf("%w: sign-in required (%s)", ErrBlocked, status)
	case unavailableStatuses[status]:
		return fmt.Errorf("%w (%s)", ErrUnavailable, status)
	case status == statusUpcoming:
		return fmt.Errorf("%w (%s)", ErrNotReady, status)
	case statusPattern.MatchString(status) && len(status) <= maxStatusBytes:
		return formatError("player response", "playability status "+status)
	default:
		return formatError("player response", "no playability status")
	}
}

// chooseTrack picks the caption track YouTube would show for the video's
// own language, among those usable reports true for: the original audio's
// language (the language of audioTracks[defaultAudioTrackIndex]), authored
// captions before automatic ones; else the track YouTube translates from,
// or the default track of the original audio (which follows the interface
// language, so it may be a dub's); else the first authored track, else the
// first track. A chosen automatic track gives way to an authored one in
// the same language. Every index is checked, since the answer is
// untrusted.
func chooseTrack(r *captionRenderer, usable func(int) bool) (int, bool) {
	tracks := r.CaptionTracks[:]
	ok := func(i int) bool { return i >= 0 && i < len(tracks) && usable(i) }
	var original *audioTrack
	if i, valid := r.DefaultAudioTrackIndex.in(maxAudioTracks); valid {
		original = &r.AudioTracks[i]
	}
	if original != nil {
		id, _, _ := strings.Cut(original.AudioTrackID, ".")
		if lang := baseLang(id); lang != "" {
			for _, generated := range []bool{false, true} {
				for i, t := range tracks {
					if ok(i) && t.generated() == generated && baseLang(t.LanguageCode) == lang {
						return i, true
					}
				}
			}
		}
	}
	defaults := []index{r.DefaultTranslationSourceTrackIndices[0]}
	if original != nil {
		defaults = append(defaults, original.DefaultCaptionTrackIndex)
	}
	for _, d := range defaults {
		if i, valid := d.in(len(tracks)); valid && ok(i) {
			return preferAuthored(tracks, i, ok), true
		}
	}
	for _, generated := range []bool{false, true} {
		for i, t := range tracks {
			if ok(i) && t.generated() == generated {
				return i, true
			}
		}
	}
	return 0, false
}

// preferAuthored returns an authored track in the language of track i when
// i is automatic and there is one, else i.
func preferAuthored(tracks []captionTrack, i int, ok func(int) bool) int {
	if !tracks[i].generated() {
		return i
	}
	lang := baseLang(tracks[i].LanguageCode)
	for j, t := range tracks {
		if ok(j) && !t.generated() && baseLang(t.LanguageCode) == lang {
			return j
		}
	}
	return i
}

// baseLang returns the primary language subtag of a language code,
// lower-cased ("en-US" → "en"), or "" when it has none.
func baseLang(code string) string {
	base, _, _ := strings.Cut(code, "-")
	return strings.ToLower(base)
}
