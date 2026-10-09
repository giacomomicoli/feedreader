package resolve

import (
	"bytes"
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/giacomomicoli/feedreader/internal/fetch"
)

// YouTube feed URLs.
const (
	ytFeedPath = "/feeds/videos.xml"
	ytFeedBase = "https://www.youtube.com" + ytFeedPath
	ytPageBase = "https://www.youtube.com"
)

// ytAvatarPx is the avatar size asked of YouTube's image server. Channel
// pages advertise a 900 px image; the UI shows avatars at 20 CSS px, so 88
// px (the size YouTube's own pages use) is ample even on dense screens.
const ytAvatarPx = 88

var (
	// channelIDPattern: "UC" followed by 22 base64url characters.
	channelIDPattern = regexp.MustCompile(`^UC[A-Za-z0-9_-]{22}$`)
	// playlistIDPattern: playlist IDs vary in prefix (PL, UU, OLAK5uy_, …)
	// and length, but are always base64url characters.
	playlistIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{2,128}$`)
)

// ytFeedParams are the query parameters of a YouTube feed URL.
var ytFeedParams = []string{"channel_id", "playlist_id", "user"}

// ytKind classifies a YouTube URL by the channel, playlist and feed URL forms.
type ytKind int

const (
	ytOther     ytKind = iota // none of the forms below: generic resolution
	ytFeed                    // /feeds/videos.xml?channel_id|playlist_id|user=…
	ytChannel                 // /channel/UC…
	ytPlaylist                // /playlist?list=…
	ytPage                    // /@handle, /c/…, /user/…: the page holds the ID
	ytMalformed               // one of the forms, with a missing or invalid ID
)

// ytRef is a classified YouTube URL.
type ytRef struct {
	kind ytKind
	id   string // channel ID (ytChannel) or playlist ID (ytPlaylist)
	page string // canonical page URL to fetch (ytPage)
}

// isYouTubeHost reports whether u is on youtube.com, www.youtube.com or
// m.youtube.com.
func isYouTubeHost(u *url.URL) bool {
	switch strings.TrimSuffix(strings.ToLower(u.Hostname()), ".") {
	case "youtube.com", "www.youtube.com", "m.youtube.com":
		return true
	}
	return false
}

// classifyYouTube maps a YouTube URL to one of the ytKind forms. Extra
// path segments ("/@handle/videos", "/channel/UC…/featured") and tracking
// parameters are ignored.
func classifyYouTube(u *url.URL) ytRef {
	if u.Path == ytFeedPath {
		q := u.Query()
		for _, p := range ytFeedParams {
			if strings.TrimSpace(q.Get(p)) != "" {
				return ytRef{kind: ytFeed}
			}
		}
		return ytRef{kind: ytMalformed}
	}
	segs := pathSegments(u.Path)
	if len(segs) == 0 {
		return ytRef{kind: ytOther}
	}
	first, second := segs[0], ""
	if len(segs) > 1 {
		second = segs[1]
	}
	switch {
	case strings.EqualFold(first, "channel"):
		if !channelIDPattern.MatchString(second) {
			return ytRef{kind: ytMalformed}
		}
		return ytRef{kind: ytChannel, id: second}
	case strings.EqualFold(first, "playlist") && len(segs) == 1:
		list := u.Query().Get("list")
		if !playlistIDPattern.MatchString(list) {
			return ytRef{kind: ytMalformed}
		}
		return ytRef{kind: ytPlaylist, id: list}
	case strings.HasPrefix(first, "@"):
		if len(first) == 1 {
			return ytRef{kind: ytMalformed}
		}
		return ytRef{kind: ytPage, page: ytPageURL(first)}
	case strings.EqualFold(first, "c"), strings.EqualFold(first, "user"):
		if second == "" {
			return ytRef{kind: ytMalformed}
		}
		return ytRef{kind: ytPage, page: ytPageURL(strings.ToLower(first), second)}
	}
	return ytRef{kind: ytOther}
}

// pathSegments splits a decoded URL path, dropping empty segments.
func pathSegments(p string) []string {
	return strings.FieldsFunc(p, func(r rune) bool { return r == '/' })
}

// ytPageURL builds https://www.youtube.com/<segs…>, escaping each segment.
func ytPageURL(segs ...string) string {
	return ytPageBase + (&url.URL{Path: "/" + strings.Join(segs, "/")}).EscapedPath()
}

// channelFeedURL is the Atom feed of a channel.
func channelFeedURL(id string) string {
	return ytFeedBase + "?channel_id=" + url.QueryEscape(id)
}

// playlistFeedURL is the Atom feed of a playlist.
func playlistFeedURL(id string) string {
	return ytFeedBase + "?playlist_id=" + url.QueryEscape(id)
}

// resolveYouTube runs step 2 for a classified YouTube URL. Only
// ytPage needs a request.
func (r *Resolver) resolveYouTube(ctx context.Context, u *url.URL, ref ytRef) (*Result, error) {
	switch ref.kind {
	case ytFeed:
		return single(Candidate{URL: u.String()}), nil
	case ytChannel:
		return single(Candidate{URL: channelFeedURL(ref.id)}), nil
	case ytPlaylist:
		return single(Candidate{URL: playlistFeedURL(ref.id)}), nil
	case ytPage:
		id, avatar, err := r.channelFromPage(ctx, ref.page)
		if err != nil {
			return nil, err
		}
		return single(Candidate{URL: channelFeedURL(id), IconURL: avatar}), nil
	default:
		return nil, fmt.Errorf("resolve %s: %w", u.Redacted(), ErrYouTubeID)
	}
}

// channelFromPage fetches a YouTube /@handle, /c/… or /user/… page and
// reads the channel ID from it, and the channel's avatar when the page has
// one (best-effort: "" otherwise).
//
// When YouTube answered but the page is unusable (HTTP error, oversized,
// redirect loop) or carries no recognisable ID — e.g. a consent
// interstitial or changed markup — the error matches ErrYouTubeID (and the
// fetch error, if any). Network failures are returned as they are.
func (r *Resolver) channelFromPage(ctx context.Context, page string) (id, avatar string, err error) {
	pageURL, err := url.Parse(page)
	if err != nil {
		return "", "", fmt.Errorf("resolve %s: %w", page, err)
	}
	res, err := r.fetch(ctx, page, fetch.HTMLAccept)
	if err != nil {
		if isURLLevelError(err) {
			return "", "", fmt.Errorf("resolve %s: %w: %w", page, ErrYouTubeID, err)
		}
		return "", "", fmt.Errorf("resolve %s: %w", page, err)
	}
	id, avatar, ok := channelFromHTML(res.Body, finalURL(res, pageURL))
	if !ok {
		return "", "", fmt.Errorf("resolve %s: %w", page, ErrYouTubeID)
	}
	return id, avatar, nil
}

// channelFromHTML extracts the page's own channel ID from, in order of
// preference:
//
//  1. <link rel="canonical" href="https://www.youtube.com/channel/UC…">
//  2. <meta itemprop="identifier" content="UC…">
//  3. <link rel="alternate" type="application/rss+xml"
//     href="https://www.youtube.com/feeds/videos.xml?channel_id=UC…">
//
// Every ID is checked against the channel ID format. Other /channel/UC…
// links on the page (featured channels, scripts) are ignored, and the
// whole document is scanned because YouTube emits these elements after the
// head has implicitly ended.
//
// The same scan reads the channel's avatar from the page's Open Graph image
// (<meta property="og:image">), when the page was served by YouTube; see
// avatarURL. The avatar is best-effort: "" does not make the page unusable.
func channelFromHTML(body []byte, pageURL *url.URL) (id, avatar string, ok bool) {
	var canonical, identifier, alternate string
	ytPage := isYouTubeHost(pageURL)
	_ = scanTags(bytes.NewReader(body), []string{"link", "meta"}, func(t tag) bool {
		switch t.name {
		case "link":
			rel := t.attrs["rel"]
			switch {
			case canonical == "" && hasRel(rel, "canonical"):
				canonical = channelIDFromChannelURL(t.attrs["href"], pageURL)
			case alternate == "" && hasRel(rel, "alternate") &&
				mediaType(t.attrs["type"]) == "application/rss+xml":
				alternate = channelIDFromFeedURL(t.attrs["href"], pageURL)
			}
		case "meta":
			switch {
			case identifier == "" && strings.EqualFold(trimHTMLSpace(t.attrs["itemprop"]), "identifier"):
				if v := trimHTMLSpace(t.attrs["content"]); channelIDPattern.MatchString(v) {
					identifier = v
				}
			case ytPage && avatar == "" && strings.EqualFold(trimHTMLSpace(t.attrs["property"]), "og:image"):
				avatar = avatarURL(t.attrs["content"])
			}
		}
		// The preferred ID source and the avatar end the scan; YouTube
		// emits the avatar after the canonical link.
		return canonical == "" || (ytPage && avatar == "")
	})
	for _, id = range []string{canonical, identifier, alternate} {
		if id != "" {
			return id, avatar, true
		}
	}
	return "", "", false
}

// avatarURL returns the channel avatar named by an og:image content
// attribute, or "" unless it is an absolute http(s) URL (as Open Graph
// requires) of at most MaxIconURLBytes bytes. An image on YouTube's image
// servers is asked for at ytAvatarPx (see smallAvatar).
func avatarURL(content string) string {
	content = trimHTMLSpace(content)
	if len(content) > MaxIconURLBytes {
		return "" // before parsing, which copies it several times
	}
	u, err := url.Parse(content)
	if err != nil || !isHTTPURL(u) {
		return ""
	}
	smallAvatar(u)
	s := u.String()
	if len(s) > MaxIconURLBytes {
		return ""
	}
	return s
}

// smallAvatar rewrites the size option of an image URL on Google's image
// servers, the last "=" options of the path ("…=s900-c-k-c0x00ffffff-no-rj"
// → "…=s88-c-k-c0x00ffffff-no-rj"), to ytAvatarPx. Other URLs, and URLs
// without a size option, are left as they are.
func smallAvatar(u *url.URL) {
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if !strings.HasSuffix(host, ".googleusercontent.com") && !strings.HasSuffix(host, ".ggpht.com") {
		return
	}
	i := strings.LastIndexByte(u.Path, '=')
	if i < 0 {
		return
	}
	opts := strings.Split(u.Path[i+1:], "-")
	for j, o := range opts {
		if isSizeOption(o) {
			opts[j] = "s" + strconv.Itoa(ytAvatarPx)
			u.Path, u.RawPath = u.Path[:i+1]+strings.Join(opts, "-"), ""
			return
		}
	}
}

// isSizeOption reports whether o is an image size option: "s" and digits.
func isSizeOption(o string) bool {
	if len(o) < 2 || o[0] != 's' {
		return false
	}
	for _, c := range o[1:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// channelIDFromChannelURL returns the ID in a YouTube /channel/UC… URL, or
// "".
func channelIDFromChannelURL(href string, base *url.URL) string {
	u, err := base.Parse(trimHTMLSpace(href))
	if err != nil || !isHTTPURL(u) || !isYouTubeHost(u) {
		return ""
	}
	if ref := classifyYouTube(u); ref.kind == ytChannel {
		return ref.id
	}
	return ""
}

// channelIDFromFeedURL returns the channel_id of a YouTube channel feed
// URL, or "".
func channelIDFromFeedURL(href string, base *url.URL) string {
	u, err := base.Parse(trimHTMLSpace(href))
	if err != nil || !isHTTPURL(u) || !isYouTubeHost(u) || u.Path != ytFeedPath {
		return ""
	}
	if id := u.Query().Get("channel_id"); channelIDPattern.MatchString(id) {
		return id
	}
	return ""
}
