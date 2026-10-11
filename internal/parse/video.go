package parse

import (
	"net/url"
	"regexp"
	"strings"
	"time"
)

// YouTube videos read from somewhere other than a feed document (the
// fallback when a channel's feed does not answer) are stored exactly as the
// channel's feed would list them, so that the feed coming back finds the
// same entries.
const (
	// youTubeGUIDPrefix is the Atom id of a video in YouTube's feeds:
	// "yt:video:<id>".
	youTubeGUIDPrefix = "yt:video:"
	// youTubeThumbnailURL and youTubeThumbnailFile make a video's
	// thumbnail URL, the media:thumbnail of its feed entry. YouTube's
	// feeds spread these over the hosts i1 to i4.ytimg.com, which serve
	// the same image; one fixed host is used.
	youTubeThumbnailURL  = "https://i1.ytimg.com/vi/"
	youTubeThumbnailFile = "/hqdefault.jpg"
)

// youTubeVideoIDPattern matches a video ID: 11 base64url characters.
var youTubeVideoIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)

// VideoEntry returns the entry of YouTube video id, given its title,
// description and channel name as YouTube's API returns them (untrusted
// plain text) and its publication date, as the entry of the video in its
// channel's feed would be: GUID "yt:video:<id>", link
// https://www.youtube.com/watch?v=<id> and thumbnail built from the ID
// alone, title and author as plain text, the description as escaped text
// (as media:description is), and published and updated both the
// publication date, in UTC. It reports false, and returns nothing, for an
// ID that is not a video ID or a zero date.
func VideoEntry(id, title, description, author string, published time.Time) (Entry, bool) {
	if !youTubeVideoIDPattern.MatchString(id) || published.IsZero() {
		return Entry{}, false
	}
	published = published.UTC()
	e := Entry{
		GUID:         VideoGUID(id),
		URL:          youTubeWatchURL + url.Values{"v": {id}}.Encode(),
		Author:       plainText(author),
		ThumbnailURL: youTubeThumbnailURL + id + youTubeThumbnailFile,
		PublishedAt:  published,
		UpdatedAt:    published,
	}
	if desc := strings.TrimSpace(description); desc != "" {
		e.SummaryHTML = textHTML(desc)
	}
	// As buildEntry titles a video.
	e.Title = titleText(title, kindText)
	if e.Title == "" {
		e.Title = excerpt(e.SummaryHTML, titleExcerptRunes)
	}
	if e.Title == "" {
		e.Title = untitled
	}
	return e, true
}

// VideoGUID returns the GUID of the entry of YouTube video id in its
// channel's feed (and from VideoEntry).
func VideoGUID(id string) string { return youTubeGUIDPrefix + id }
