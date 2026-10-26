// Package ittest builds synthetic InnerTube answers for tests: browse and
// player answers with the structure YouTube's web client receives (as
// measured), but made-up IDs and text. Only tests import it.
package ittest

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/giacomomicoli/feedreader/internal/innertube"
)

// HeaderVideoID is the video a channel page's header links to (its
// trailer, or a live stream): it is not one of the tab's videos.
const HeaderVideoID = "HeaderVid00"

// ContinuationToken is the token of the next page of a tab's list.
const ContinuationToken = "4qmFsgSyntheticContinuationToken"

// homeParams are the params of a channel's Home tab.
const homeParams = "EghmZWF0dXJlZPIGBAoCMgA%3D"

// q returns s as a JSON string.
func q(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// header is a channel page's header, with an avatar that links to
// HeaderVideoID.
func header(channelID string) string {
	return `"header":{"pageHeaderRenderer":{"pageTitle":"Synthetic channel","content":{"pageHeaderViewModel":{` +
		`"image":{"decoratedAvatarViewModel":{"rendererContext":{"commandContext":{"onTap":{"innertubeCommand":` +
		`{"watchEndpoint":{"videoId":` + q(HeaderVideoID) + `}}}}}}},` +
		`"metadata":{"contentMetadataViewModel":{"metadataRows":[{"metadataParts":[{"text":{"content":"@synthetic"}}]}]}}}}}}`
}

// videoItem is a grid item of the Videos tab: a lockupViewModel that names
// its video five times, as YouTube's does.
func videoItem(id string) string {
	return `{"richItemRenderer":{"content":{"lockupViewModel":{"contentImage":{"thumbnailViewModel":{"image":{"sources":[` +
		`{"url":"https://i.ytimg.com/vi/` + id + `/hqdefault.jpg","width":480,"height":270}]},"overlays":[` +
		`{"thumbnailBottomOverlayViewModel":{"badges":[{"thumbnailBadgeViewModel":{"text":"12:34","badgeStyle":"THUMBNAIL_OVERLAY_BADGE_STYLE_DEFAULT"}}]}},` +
		`{"thumbnailHoverOverlayToggleActionsViewModel":{"buttons":[{"toggleButtonViewModel":{"defaultButtonViewModel":{"buttonViewModel":{"onTap":{"innertubeCommand":{"playlistEditEndpoint":{"actions":[{"addedVideoId":` + q(id) + `,"action":"ACTION_ADD_VIDEO"}]}}}}}}}]}}]}},` +
		`"metadata":{"lockupMetadataViewModel":{"title":{"content":"Synthetic video ` + id + `"},"metadata":{"contentMetadataViewModel":{"metadataRows":[{"metadataParts":[{"text":{"content":"1.2K views"}},{"text":{"content":"3 days ago"}}]}]}}}},` +
		`"contentId":` + q(id) + `,"contentType":"LOCKUP_CONTENT_TYPE_VIDEO",` +
		`"rendererContext":{"loggingContext":{"loggingDirectives":{"trackingParams":"x"}},"commandContext":{"onTap":{"innertubeCommand":{` +
		`"commandMetadata":{"webCommandMetadata":{"url":"/watch?v=` + id + `"}},"watchEndpoint":{"videoId":` + q(id) + `,"watchEndpointSupportedOnesieConfig":{"html5PlaybackOnesieConfig":{"commonConfig":{"url":"https://example.invalid/initplayback?id=` + id + `"}}}}}}}},` +
		`"menu":{"menuRenderer":{"items":[{"menuServiceItemRenderer":{"serviceEndpoint":{"signalServiceEndpoint":{"actions":[{"addToPlaylistCommand":{"videoId":` + q(id) + `}}]}}}},` +
		`{"menuServiceItemRenderer":{"serviceEndpoint":{"shareEntityServiceEndpoint":{"videoId":` + q(id) + `}}}}]}}}}}}`
}

// shortItem is a grid item of the Shorts tab.
func shortItem(id string) string {
	return `{"richItemRenderer":{"content":{"shortsLockupViewModel":{"entityId":"shorts-shelf-item-` + id + `",` +
		`"thumbnail":{"sources":[{"url":"https://i.ytimg.com/vi/` + id + `/oardefault.jpg","width":1080,"height":1920}]},` +
		`"onTap":{"innertubeCommand":{"commandMetadata":{"webCommandMetadata":{"url":"/shorts/` + id + `"}},` +
		`"reelWatchEndpoint":{"videoId":` + q(id) + `,"playerParams":"8AEBoAMEyAMkuAQF","sequenceProvider":"REEL_WATCH_SEQUENCE_PROVIDER_RPC"}}},` +
		`"overlayMetadata":{"primaryText":{"content":"Synthetic short ` + id + `"},"secondaryText":{"content":"1.2K views"}}}}}}`
}

// continuation is the last grid item: the token of the next page.
func continuation() string {
	return `{"continuationItemRenderer":{"trigger":"CONTINUATION_TRIGGER_ON_ITEM_SHOWN","continuationEndpoint":{` +
		`"continuationCommand":{"token":` + q(ContinuationToken) + `,"request":"CONTINUATION_REQUEST_TYPE_BROWSE"}}}}`
}

// tabRenderer is one tab; content is "" for a tab that is not selected.
func tabRenderer(channelID, title, params string, selected bool, content string) string {
	s := `{"tabRenderer":{"endpoint":{"clickTrackingParams":"x","commandMetadata":{"webCommandMetadata":{"url":"/channel/` +
		channelID + `"}},"browseEndpoint":{"browseId":` + q(channelID) + `,"params":` + q(params) + `}},` +
		`"title":` + q(title) + `,"selected":` + fmt.Sprint(selected)
	if content != "" {
		s += `,"content":` + content
	}
	return s + `,"trackingParams":"x"}}`
}

// page wraps tabs (and extra top-level members) in a browse answer.
func page(channelID string, tabs []string, extra ...string) []byte {
	parts := append([]string{
		`"responseContext":{"serviceTrackingParams":[{"service":"GFEEDBACK","params":[{"key":"logged_in","value":"0"}]}]}`,
		header(channelID),
		`"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[` + strings.Join(tabs, ",") + `]}}`,
	}, extra...)
	parts = append(parts, `"metadata":{"channelMetadataRenderer":{"title":"Synthetic channel","externalId":`+q(channelID)+`}}`)
	return []byte("{" + strings.Join(parts, ",") + "}")
}

// TabAnswer is the browse answer for tab of channel channelID, listing ids
// newest first in a grid that continues on another page. The page's
// header links to HeaderVideoID, and the other tabs are listed without
// content.
func TabAnswer(channelID string, tab innertube.Tab, ids ...string) []byte {
	items := make([]string, 0, len(ids)+1)
	for _, id := range ids {
		if tab == innertube.TabShorts {
			items = append(items, shortItem(id))
		} else {
			items = append(items, videoItem(id))
		}
	}
	items = append(items, continuation())
	grid := `{"richGridRenderer":{"contents":[` + strings.Join(items, ",") + `],"header":{"feedFilterChipBarRenderer":{"contents":[]}}}}`
	tabs := []string{tabRenderer(channelID, "Home", homeParams, false, "")}
	for _, t := range []innertube.Tab{innertube.TabVideos, innertube.TabShorts, innertube.TabLive} {
		content := ""
		if t == tab {
			content = grid
		}
		tabs = append(tabs, tabRenderer(channelID, t.Name, t.Params, t == tab, content))
	}
	return page(channelID, tabs)
}

// HomeAnswer is what YouTube answers when a channel has no tab like the
// one asked for: its Home tab, selected, with shelves of videos (ids,
// which may be other channels').
func HomeAnswer(channelID string, ids ...string) []byte {
	items := make([]string, 0, len(ids))
	for _, id := range ids {
		items = append(items, `{"gridVideoRenderer":{"videoId":`+q(id)+`,"navigationEndpoint":{"watchEndpoint":{"videoId":`+q(id)+`}}}}`)
	}
	shelf := `{"sectionListRenderer":{"contents":[{"itemSectionRenderer":{"contents":[{"shelfRenderer":{"content":` +
		`{"horizontalListRenderer":{"items":[` + strings.Join(items, ",") + `]}}}}]}}]}}`
	return page(channelID, []string{
		tabRenderer(channelID, "Home", homeParams, true, shelf),
		tabRenderer(channelID, "Videos", innertube.TabVideos.Params, false, ""),
	})
}

// AlertAnswer is the answer for a channel that does not exist: an error
// alert and no tabs.
func AlertAnswer() []byte {
	return []byte(`{"responseContext":{"serviceTrackingParams":[]},"alerts":[{"alertRenderer":{"type":"ERROR",` +
		`"text":{"runs":[{"text":"This channel does not exist."}]}}}]}`)
}

// ErrorAnswer is the error object of a request YouTube rejects (served
// with status 400).
func ErrorAnswer() []byte {
	return []byte(`{"error":{"code":400,"message":"Request contains an invalid argument.","errors":[` +
		`{"message":"Request contains an invalid argument.","domain":"global","reason":"badRequest"}],"status":"INVALID_ARGUMENT"}}`)
}

// Video is a video a player answer describes.
type Video struct {
	ID, ChannelID, Title, Description, Author string
	Published                                 time.Time
	// Date overrides the publishDate written (Published in RFC 3339 with
	// a -07:00 offset, as YouTube writes it); "-" leaves it out.
	Date string
	// Status is the playability status; "" is UNPLAYABLE, the web
	// client's usual answer, which still carries the details.
	Status, Reason     string
	IsLive, IsUpcoming bool
}

// PlayerAnswer is the web client's player answer for v.
func PlayerAnswer(v Video) []byte {
	status := v.Status
	if status == "" {
		status = "UNPLAYABLE"
	}
	date := v.Date
	if date == "" {
		date = v.Published.In(time.FixedZone("", -7*60*60)).Format(time.RFC3339)
	}
	micro := ""
	if date != "-" {
		micro = `,"microformat":{"playerMicroformatRenderer":{"thumbnail":{"thumbnails":[{"url":"https://i.ytimg.com/vi/` + v.ID +
			`/maxresdefault.jpg"}]},"title":{"simpleText":` + q(v.Title) + `},"description":{"simpleText":` + q(v.Description) + `},` +
			`"lengthSeconds":"754","ownerProfileUrl":"http://www.youtube.com/@synthetic","externalChannelId":` + q(v.ChannelID) + `,` +
			`"isFamilySafe":true,"isUnlisted":false,"hasYpcMetadata":false,"viewCount":"1234","category":"Education",` +
			`"publishDate":` + q(date) + `,"ownerChannelName":` + q(v.Author) + `,"uploadDate":` + q(date) + `,"isShortsEligible":false}}`
	}
	return []byte(`{"responseContext":{"serviceTrackingParams":[]},"playabilityStatus":{"status":` + q(status) +
		`,"reason":` + q(v.Reason) + `,"errorScreen":{"playerErrorMessageRenderer":{"reason":{"simpleText":"<b>x</b>"}}}},` +
		`"videoDetails":{"videoId":` + q(v.ID) + `,"title":` + q(v.Title) + `,"lengthSeconds":"754","keywords":["a","b"],` +
		`"channelId":` + q(v.ChannelID) + `,"isOwnerViewing":false,"shortDescription":` + q(v.Description) + `,` +
		`"isCrawlable":true,"thumbnail":{"thumbnails":[{"url":"https://i.ytimg.com/vi/` + v.ID + `/default.jpg"}]},` +
		`"allowRatings":true,"viewCount":"1234","author":` + q(v.Author) + `,"isPrivate":false,"isUnpluggedCorpus":false,` +
		`"isLiveContent":false,"isLive":` + fmt.Sprint(v.IsLive) + `,"isUpcoming":` + fmt.Sprint(v.IsUpcoming) + `}` +
		micro + `,"trackingParams":"x"}`)
}

// AndroidPlayerAnswer is the Android app's player answer for v: details
// and streams, but no microformat, so no publication date.
func AndroidPlayerAnswer(v Video) []byte {
	return []byte(`{"responseContext":{},"playabilityStatus":{"status":"OK","playableInEmbed":true},` +
		`"streamingData":{"expiresInSeconds":"21540","formats":[{"itag":18,"url":"https://example.invalid/videoplayback?id=` + v.ID + `"}]},` +
		`"videoDetails":{"videoId":` + q(v.ID) + `,"title":` + q(v.Title) + `,"channelId":` + q(v.ChannelID) + `,` +
		`"shortDescription":` + q(v.Description) + `,"author":` + q(v.Author) + `,"isLive":false},"trackingParams":"x"}`)
}

// MissingPlayerAnswer is the player answer for a video that does not
// exist: status ERROR and no details.
func MissingPlayerAnswer() []byte {
	return []byte(`{"responseContext":{},"playabilityStatus":{"status":"ERROR","reason":"This video is unavailable",` +
		`"errorScreen":{"playerErrorMessageRenderer":{"reason":{"simpleText":"This video is unavailable"}}}},"trackingParams":"x"}`)
}

// IDs returns n distinct synthetic video IDs starting with prefix (at most
// 8 characters).
func IDs(prefix string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("%s%0*d", prefix, 11-len(prefix), i)
	}
	return out
}
