package resolve

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/parse"
	"github.com/giacomomicoli/feedreader/internal/safeurl"
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
		// Every spelling of a channel's feed (m., no www, http, extra
		// parameters) becomes the canonical URL, so one channel is not
		// subscribed twice under two URLs.
		if id := channelIDOfFeed(u); id != "" {
			return single(Candidate{URL: channelFeedURL(id)}), nil
		}
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
		return nil, fmt.Errorf("resolve %s: %w", safeurl.Redacted(u), ErrYouTubeID)
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

// channelPage is what a YouTube channel page tells about its channel.
type channelPage struct {
	id     string // the page's own channel ID
	avatar string // its avatar (see avatarURL), or ""
	title  string // its name (og:title, see parse.PlainTitle), or ""
}

// channelFromHTML extracts the page's own channel ID and avatar; see
// channelPageFromHTML.
func channelFromHTML(body []byte, pageURL *url.URL) (id, avatar string, ok bool) {
	cp, ok := channelPageFromHTML(body, pageURL)
	return cp.id, cp.avatar, ok
}

// channelPageFromHTML extracts the page's own channel ID from, in order of
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
// The same scan reads, when the page was served by YouTube, the channel's
// avatar from the page's Open Graph image (<meta property="og:image">; see
// avatarURL) and its name from the Open Graph title (<meta
// property="og:title">, reduced to one bounded line of plain text by
// parse.PlainTitle). Both are best-effort: "" does not make the page
// unusable, and neither is returned without an ID.
func channelPageFromHTML(body []byte, pageURL *url.URL) (channelPage, bool) {
	var canonical, identifier, alternate, avatar, title string
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
			property := strings.ToLower(trimHTMLSpace(t.attrs["property"]))
			switch {
			case identifier == "" && strings.EqualFold(trimHTMLSpace(t.attrs["itemprop"]), "identifier"):
				if v := trimHTMLSpace(t.attrs["content"]); channelIDPattern.MatchString(v) {
					identifier = v
				}
			case ytPage && avatar == "" && property == "og:image":
				avatar = avatarURL(t.attrs["content"])
			case ytPage && title == "" && property == "og:title":
				title = parse.PlainTitle(t.attrs["content"])
			}
		}
		// The preferred ID source, the avatar and the title end the scan;
		// YouTube emits the Open Graph tags after the canonical link.
		return canonical == "" || (ytPage && (avatar == "" || title == ""))
	})
	for _, id := range []string{canonical, identifier, alternate} {
		if id != "" {
			return channelPage{id: id, avatar: avatar, title: title}, true
		}
	}
	return channelPage{}, false
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
	if err != nil {
		return ""
	}
	return channelIDOfFeed(u)
}

// channelIDOfFeed returns the channel_id of u when it is a YouTube channel
// feed URL, or "".
func channelIDOfFeed(u *url.URL) string {
	if !isHTTPURL(u) || !isYouTubeHost(u) || u.Path != ytFeedPath {
		return ""
	}
	if id := u.Query().Get("channel_id"); channelIDPattern.MatchString(id) {
		return id
	}
	return ""
}

// IsChannelFeed reports whether feedURL is the video feed of a YouTube
// channel (youtube.com/feeds/videos.xml?channel_id=UC…), as opposed to a
// playlist's or any other feed.
func IsChannelFeed(feedURL string) bool {
	_, ok := ChannelFeedID(feedURL)
	return ok
}

// ChannelFeedID returns the channel ID of feedURL when it is the video feed
// of a YouTube channel (see IsChannelFeed).
func ChannelFeedID(feedURL string) (string, bool) {
	u, err := url.Parse(strings.TrimSpace(feedURL))
	if err != nil {
		return "", false
	}
	id := channelIDOfFeed(u)
	return id, id != ""
}

// ChannelFeedURL returns the canonical video feed URL of the YouTube channel
// id (https://www.youtube.com/feeds/videos.xml?channel_id=UC…), or "" when id
// is not a channel ID.
func ChannelFeedURL(id string) string {
	if !channelIDPattern.MatchString(id) {
		return ""
	}
	return channelFeedURL(id)
}

// ErrChannelNotConfirmed is returned by ConfirmChannel when YouTube's page of
// the channel could not be fetched or does not name that channel.
var ErrChannelNotConfirmed = errors.New("the channel's page on YouTube does not confirm the channel")

// Channel is a YouTube channel as its page describes it.
type Channel struct {
	ID      string
	Title   string // og:title as one bounded line of plain text, or ""
	IconURL string // avatar (see ChannelAvatar), or ""
}

// isChannelPage reports whether u, the URL a page was served from, is a
// channel's own page on YouTube (/@handle, /c/…, /user/… or /channel/UC…).
// A redirect elsewhere on YouTube (a watch page names its uploader's feed,
// with the video's title and thumbnail) confirms nothing.
func isChannelPage(u *url.URL) bool {
	if !isHTTPURL(u) || !isYouTubeHost(u) {
		return false
	}
	k := classifyYouTube(u).kind
	return k == ytPage || k == ytChannel
}

// ChannelFromPage returns the channel that res, the response to a request
// for requested, confirms as ConfirmChannel would: requested is a channel's
// own page on YouTube (/@handle, /c/…, /user/… or /channel/UC… of that same
// channel), it was served from such a page on YouTube (see isChannelPage),
// and it names its channel. It makes no request, so the add flow can reuse a
// page it fetched anyway. Any other page (a video page names its channel's
// feed too) confirms nothing.
func ChannelFromPage(requested string, res *fetch.Result) (Channel, bool) {
	if res == nil || res.NotModified || len(res.Body) == 0 {
		return Channel{}, false
	}
	u, err := url.Parse(strings.TrimSpace(requested))
	if err != nil || !isHTTPURL(u) || !isYouTubeHost(u) {
		return Channel{}, false
	}
	ref := classifyYouTube(u)
	if ref.kind != ytPage && ref.kind != ytChannel {
		return Channel{}, false
	}
	served := finalURL(res, u)
	if !isChannelPage(served) {
		return Channel{}, false
	}
	cp, ok := channelPageFromHTML(res.Body, served)
	if !ok || (ref.kind == ytChannel && cp.id != ref.id) {
		return Channel{}, false
	}
	return Channel{ID: cp.id, Title: cp.title, IconURL: cp.avatar}, true
}

// ConfirmChannel fetches YouTube's page of the channel id
// (https://www.youtube.com/channel/UC…, one request) and checks that the page
// was served by YouTube and names that channel as its own (see
// channelPageFromHTML), returning the channel's name and avatar from the
// same page. Anything else, the fetch failing included, is an error
// matching ErrChannelNotConfirmed (and the fetch error, if any); so is a
// page served from anywhere but a channel's own page on YouTube (see
// isChannelPage), such as a watch page after a redirect. An id that
// is not a channel ID is refused without a request. The page URL is built
// from the checked id only, so nothing else can choose what is fetched.
func (r *Resolver) ConfirmChannel(ctx context.Context, id string) (Channel, error) {
	if !channelIDPattern.MatchString(id) {
		return Channel{}, fmt.Errorf("confirm channel: invalid channel id: %w", ErrChannelNotConfirmed)
	}
	page := ytPageURL("channel", id)
	pageURL, err := url.Parse(page)
	if err != nil {
		return Channel{}, fmt.Errorf("confirm channel %s: %w: %w", page, ErrChannelNotConfirmed, err)
	}
	res, err := r.fetch(ctx, page, fetch.HTMLAccept)
	if err != nil {
		return Channel{}, fmt.Errorf("confirm channel %s: %w: %w", page, ErrChannelNotConfirmed, err)
	}
	served := finalURL(res, pageURL)
	if !isChannelPage(served) {
		return Channel{}, fmt.Errorf("confirm channel %s: page served from %s: %w", page,
			safeurl.Redacted(served), ErrChannelNotConfirmed)
	}
	cp, ok := channelPageFromHTML(res.Body, served)
	if !ok || cp.id != id {
		return Channel{}, fmt.Errorf("confirm channel %s: %w", page, ErrChannelNotConfirmed)
	}
	return Channel{ID: id, Title: cp.title, IconURL: cp.avatar}, nil
}

// ChannelAvatar returns the avatar of the YouTube channel whose video feed
// is feedURL, read from the channel's page (youtube.com/channel/UC…) with
// one request; see channelFromHTML. It is best-effort: "" when the page has
// no usable avatar or is not that channel's page. For a feed that is not a
// channel feed (IsChannelFeed) it returns "" without a request. Errors are
// those of the fetch.
func (r *Resolver) ChannelAvatar(ctx context.Context, feedURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(feedURL))
	if err != nil {
		return "", nil
	}
	id := channelIDOfFeed(u)
	if id == "" {
		return "", nil
	}
	page := ytPageURL("channel", id)
	pageURL, err := url.Parse(page)
	if err != nil {
		return "", fmt.Errorf("channel avatar %s: %w", page, err)
	}
	res, err := r.fetch(ctx, page, fetch.HTMLAccept)
	if err != nil {
		return "", fmt.Errorf("channel avatar %s: %w", page, err)
	}
	if got, avatar, ok := channelFromHTML(res.Body, finalURL(res, pageURL)); ok && got == id {
		return avatar, nil
	}
	return "", nil
}
