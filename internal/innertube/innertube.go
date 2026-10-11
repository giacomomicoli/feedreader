// Package innertube holds what this program shares about YouTube's
// InnerTube API, the undocumented API behind YouTube's own apps
// (www.youtube.com/youtubei/v1/…): the requests and the client profiles
// they are sent as, how an answer tells that YouTube refuses this client,
// and a JSON walker that reads its large answers without allocating for
// what it skips.
//
// There is no API key, no cookie and no account: a request names a client
// profile (the app it presents itself as) and its version, built into the
// binary. YouTube may stop accepting a profile or a version at any time,
// which shows up as refusals or answers in another format; callers treat
// both as a failure of that request.
//
// Every answer is untrusted. Callers bound its size before decoding it,
// read only the members they need, and never take a URL to request from it.
package innertube

import (
	"encoding/json"
	"mime"
	"net/http"
	"regexp"
	"strings"

	"github.com/giacomomicoli/feedreader/internal/fetch"
)

// Endpoints and content types.
const (
	// PlayerURL answers with a video's player response: whether and how
	// it plays, and what it is.
	PlayerURL = "https://www.youtube.com/youtubei/v1/player?prettyPrint=false"
	// JSONType is the content type of requests and answers.
	JSONType = "application/json"
	// Lang is the interface language asked for, so that YouTube's
	// messages come in one language whatever the server's location.
	Lang = "en"
)

// Client profiles: the app a request presents itself as.
const (
	// AndroidName and AndroidVersion are YouTube's Android app. Its player
	// responses carry caption URLs that need no proof-of-origin token; the
	// version is one the endpoint accepts without further headers, an API
	// key or cookies.
	AndroidName    = "ANDROID"
	AndroidVersion = "20.10.38"
)

// Profile is a client profile: clientName and clientVersion as a request's
// context sends them.
type Profile struct {
	Name    string
	Version string
}

// Android is the profile of YouTube's Android app (AndroidName).
var Android = Profile{Name: AndroidName, Version: AndroidVersion}

// requestContext is the context member every request carries.
type requestContext struct {
	Client struct {
		Name    string `json:"clientName"`
		Version string `json:"clientVersion"`
		Lang    string `json:"hl"`
	} `json:"client"`
}

func newContext(p Profile) requestContext {
	var c requestContext
	c.Client.Name, c.Client.Version, c.Client.Lang = p.Name, p.Version, Lang
	return c
}

// PlayerRequest returns the POST asking for the player response of video
// id as profile p.
func PlayerRequest(p Profile, id string) fetch.Request {
	return post(PlayerURL, struct {
		Context requestContext `json:"context"`
		VideoID string         `json:"videoId"`
	}{newContext(p), id})
}

// post returns the POST of body, as JSON, to endpoint, asking for JSON.
// fetch never follows a POST's redirects.
func post(endpoint string, body any) fetch.Request {
	b, _ := json.Marshal(body) // strings only: cannot fail
	return fetch.Request{
		URL:         endpoint,
		Method:      http.MethodPost,
		Body:        string(b),
		ContentType: JSONType,
		Accept:      JSONType,
	}
}

// BlockedStatus reports whether an HTTP status means YouTube refuses this
// client: a rate limit, a refusal, a demand to sign in, or a redirect (to
// a consent or captcha page; no InnerTube request follows redirects).
func BlockedStatus(code int) bool {
	switch {
	case code == http.StatusTooManyRequests, code == http.StatusForbidden, code == http.StatusUnauthorized:
		return true
	default:
		return code >= http.StatusMultipleChoices && code < http.StatusBadRequest
	}
}

// IsWebPage reports whether contentType is that of a web page: a consent,
// captcha or error page served instead of the API's answer, which means
// YouTube refuses this client.
func IsWebPage(contentType string) bool {
	mt, _, _ := mime.ParseMediaType(contentType)
	return mt == "text/html"
}

// IsJSON reports whether contentType is JSONType.
func IsJSON(contentType string) bool {
	mt, _, _ := mime.ParseMediaType(contentType)
	return mt == JSONType
}

// Playability is what a player response's playabilityStatus says about
// the video, for this client.
type Playability int

const (
	// Playable: the status is OK.
	Playable Playability = iota
	// Refused: YouTube refuses this client (a sign-in demand that is not
	// about the video, such as a bot check).
	Refused
	// Unavailable: the video does not play to this client: removed,
	// private, members-only, age-restricted or region-blocked.
	Unavailable
	// NotReady: a live stream or premiere that has not started.
	NotReady
	// Unknown: a status this package does not know, or none.
	Unknown
)

// Playability statuses (playabilityStatus.status).
const (
	StatusOK = "OK"
	// StatusLoginRequired is a bot check, or an age-restricted or private
	// video, told apart by the reason.
	StatusLoginRequired = "LOGIN_REQUIRED"
	// StatusUpcoming is a live stream or premiere that has not started.
	StatusUpcoming = "LIVE_STREAM_OFFLINE"
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
// video rather than this client (in the English messages Lang asks for):
// an age-restricted, private or members-only video. Any other sign-in
// demand, such as "Sign in to confirm you're not a bot", is a refusal of
// this client.
var loginUnavailableReasons = []string{
	"confirm your age", "inappropriate for some users", "age-restricted", "private",
	"members-only", "channel's members",
}

// Classify reads a playabilityStatus: its status token and YouTube's
// reason text. The reason only tells a LOGIN_REQUIRED about the video from
// one about this client; it is never shown.
func Classify(status, reason string) Playability {
	switch {
	case status == StatusOK:
		return Playable
	case status == StatusLoginRequired:
		reason = strings.ToLower(reason)
		for _, marker := range loginUnavailableReasons {
			if strings.Contains(reason, marker) {
				return Unavailable
			}
		}
		return Refused
	case unavailableStatuses[status]:
		return Unavailable
	case status == StatusUpcoming:
		return NotReady
	}
	return Unknown
}

// MaxStatusBytes bounds a playability status shown in an error message,
// such as "LIVE_STREAM_OFFLINE".
const MaxStatusBytes = 40

// statusPattern is a playability status safe to show in a message.
var statusPattern = regexp.MustCompile(`^[A-Z_]+$`)

// StatusToken reports whether status is a token safe to show in a message:
// upper-case letters and underscores, at most MaxStatusBytes.
func StatusToken(status string) bool {
	return len(status) <= MaxStatusBytes && statusPattern.MatchString(status)
}
