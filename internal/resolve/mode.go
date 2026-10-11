package resolve

import (
	"net/url"
	"regexp"
	"strings"
)

// Mode is what a subscription to a YouTube channel follows: everything the
// channel uploads, or only its regular videos, or only its Shorts.
//
// The mode is not stored on its own: it is the feed URL. "Both" is the
// channel's feed (feeds/videos.xml?channel_id=UC<x>); the other two are the
// feeds of the per-type upload playlists YouTube keeps for every channel,
// whose IDs are the channel ID with "UC" replaced by a prefix: "UULF" for
// regular videos (no Shorts and no live streams) and "UUSH" for Shorts
// (feeds/videos.xml?playlist_id=UULF<x> or UUSH<x>). These prefixes are not
// documented by YouTube.
type Mode string

const (
	ModeBoth   Mode = "both"   // regular videos and Shorts: the channel feed
	ModeVideos Mode = "videos" // regular videos only: the UULF playlist feed
	ModeShorts Mode = "shorts" // Shorts only: the UUSH playlist feed
)

// modePrefix is the playlist ID prefix that replaces "UC" for each mode but
// ModeBoth.
var modePrefix = map[Mode]string{
	ModeVideos: "UULF",
	ModeShorts: "UUSH",
}

// modePlaylistPattern matches the playlist ID of a mode feed: a prefix of
// modePrefix and the 22 characters that follow "UC" in the channel ID.
var modePlaylistPattern = regexp.MustCompile(`^(UULF|UUSH)([A-Za-z0-9_-]{22})$`)

// Modes returns every mode, in the order the UI lists them.
func Modes() []Mode { return []Mode{ModeBoth, ModeVideos, ModeShorts} }

// ParseMode returns the mode named s ("both", "videos" or "shorts", exactly).
func ParseMode(s string) (Mode, bool) {
	switch m := Mode(s); m {
	case ModeBoth, ModeVideos, ModeShorts:
		return m, true
	}
	return "", false
}

// ModeFeedURL returns the canonical feed URL that follows the YouTube
// channel id in mode m, or "" when id is not a channel ID or m not a mode.
// The URL is built from the checked id only.
func ModeFeedURL(id string, m Mode) string {
	if !channelIDPattern.MatchString(id) {
		return ""
	}
	if m == ModeBoth {
		return channelFeedURL(id)
	}
	prefix, ok := modePrefix[m]
	if !ok {
		return ""
	}
	return playlistFeedURL(prefix + strings.TrimPrefix(id, "UC"))
}

// FeedMode returns the YouTube channel that feedURL follows, and in which
// mode, when it is a channel feed or the feed of one of the channel's
// per-type upload playlists, in any spelling (m., no www, http, extra
// parameters). Any other URL, other playlists included, has no mode.
func FeedMode(feedURL string) (id string, m Mode, ok bool) {
	u, err := url.Parse(strings.TrimSpace(feedURL))
	if err != nil {
		return "", "", false
	}
	id, m = modeOfFeed(u)
	return id, m, id != ""
}

// IsChannelOrModeFeed reports whether feedURL follows a YouTube channel in
// one of its modes (see FeedMode).
func IsChannelOrModeFeed(feedURL string) bool {
	_, _, ok := FeedMode(feedURL)
	return ok
}

// modeOfFeed is FeedMode for a parsed URL; id is "" for no mode. A channel_id
// parameter decides alone: a feed URL naming a channel is that channel's
// feed, or nothing when the ID is malformed.
func modeOfFeed(u *url.URL) (id string, m Mode) {
	if !isHTTPURL(u) || !isYouTubeHost(u) || u.Path != ytFeedPath {
		return "", ""
	}
	q := u.Query()
	if strings.TrimSpace(q.Get("channel_id")) != "" {
		if id := channelIDOfFeed(u); id != "" {
			return id, ModeBoth
		}
		return "", ""
	}
	sub := modePlaylistPattern.FindStringSubmatch(q.Get("playlist_id"))
	if sub == nil {
		return "", ""
	}
	for mode, prefix := range modePrefix {
		if prefix == sub[1] {
			return "UC" + sub[2], mode
		}
	}
	return "", ""
}
