package innertube_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/innertube"
	"github.com/giacomomicoli/feedreader/internal/innertube/ittest"
)

var published = time.Date(2026, 10, 10, 23, 0, 41, 0, time.UTC)

func testVideo() ittest.Video {
	return ittest.Video{ID: "vid00000001", ChannelID: testChannel, Title: "A <b>title</b> & more",
		Description: "Line one\nhttps://example.com/x\n\nLine two", Author: "Synthetic channel", Published: published}
}

// TestParseVideo: the web client's answer (status UNPLAYABLE) gives the
// details as written and the publication date in UTC.
func TestParseVideo(t *testing.T) {
	v := testVideo()
	got, err := innertube.ParseVideo(ittest.PlayerAnswer(v), jsonType, v.ID, v.ChannelID)
	want := innertube.Video{ID: v.ID, ChannelID: v.ChannelID, Title: v.Title, Description: v.Description,
		Author: v.Author, Published: published}
	if err != nil || got != want {
		t.Fatalf("ParseVideo = %+v, %v; want %+v", got, err, want)
	}
	if got.Published.Location() != time.UTC {
		t.Errorf("Published in %v, want UTC", got.Published.Location())
	}
}

func TestParseVideo_Refused(t *testing.T) {
	v := testVideo()
	tests := []struct {
		name        string
		contentType string
		body        []byte
		want        error
	}{
		{"bot check", jsonType, func() []byte {
			v := v
			v.Status, v.Reason = "LOGIN_REQUIRED", "Sign in to confirm you're not a bot"
			return ittest.PlayerAnswer(v)
		}(), innertube.ErrBlocked},
		{"web page", "text/html", []byte("<!doctype html><title>Before you continue</title>"), innertube.ErrBlocked},
		{"other content type", "text/plain", ittest.PlayerAnswer(v), innertube.ErrFormat},
		{"invalid JSON", jsonType, []byte(`{"videoDetails":`), innertube.ErrFormat},
		{"over the cap", jsonType, []byte(`"` + strings.Repeat("a", innertube.MaxVideoBytes) + `"`), innertube.ErrTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := innertube.ParseVideo(tt.body, tt.contentType, v.ID, v.ChannelID)
			if !errors.Is(err, tt.want) || got != (innertube.Video{}) {
				t.Fatalf("ParseVideo = %+v, %v; want %v", got, err, tt.want)
			}
			if errors.Is(err, innertube.ErrSkipVideo) {
				t.Error("a refusal must not pass for a skipped video")
			}
			if strings.Contains(err.Error(), "bot") || strings.Contains(err.Error(), "continue") {
				t.Errorf("message %q quotes the answer", err)
			}
		})
	}
}

// TestParseVideo_Skipped: answers that give nothing to store for the video
// asked about are ErrSkipVideo: another video or channel, a missing
// video (status ERROR), no date or one that is not RFC 3339 (the Android
// app's answer has none), a live stream or an upcoming premiere.
func TestParseVideo_Skipped(t *testing.T) {
	v := testVideo()
	with := func(f func(*ittest.Video)) []byte {
		c := v
		f(&c)
		return ittest.PlayerAnswer(c)
	}
	tests := []struct {
		name string
		body []byte
		want error
	}{
		{"other video", with(func(c *ittest.Video) { c.ID = "vid00000002" }), innertube.ErrOtherVideo},
		{"other channel", with(func(c *ittest.Video) { c.ChannelID = "UCsyntheticChannel000002" }), innertube.ErrOtherVideo},
		{"missing video", ittest.MissingPlayerAnswer(), innertube.ErrOtherVideo},
		{"android answer", ittest.AndroidPlayerAnswer(v), innertube.ErrNoDate},
		{"no date", with(func(c *ittest.Video) { c.Date = "-" }), innertube.ErrNoDate},
		{"relative date", with(func(c *ittest.Video) { c.Date = "3 days ago" }), innertube.ErrNoDate},
		{"date only", with(func(c *ittest.Video) { c.Date = "2026-10-10" }), innertube.ErrNoDate},
		{"no offset", with(func(c *ittest.Video) { c.Date = "2026-10-10T16:00:41" }), innertube.ErrNoDate},
		{"month 13", with(func(c *ittest.Video) { c.Date = "2026-13-10T16:00:41Z" }), innertube.ErrNoDate},
		{"zero date", with(func(c *ittest.Video) { c.Date = "0001-01-01T00:00:00Z" }), innertube.ErrNoDate},
		{"live", with(func(c *ittest.Video) { c.IsLive = true }), innertube.ErrNotAired},
		{"upcoming", with(func(c *ittest.Video) { c.IsUpcoming = true }), innertube.ErrNotAired},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := innertube.ParseVideo(tt.body, jsonType, v.ID, v.ChannelID)
			if !errors.Is(err, tt.want) || !errors.Is(err, innertube.ErrSkipVideo) || got != (innertube.Video{}) {
				t.Fatalf("ParseVideo = %+v, %v; want %v", got, err, tt.want)
			}
		})
	}
}

// TestParseVideo_AgeRestricted: a sign-in demand about the video itself is
// not a refusal of this client; its details are read when it has them.
func TestParseVideo_AgeRestricted(t *testing.T) {
	v := testVideo()
	v.Status, v.Reason = "LOGIN_REQUIRED", "Sign in to confirm your age"
	if _, err := innertube.ParseVideo(ittest.PlayerAnswer(v), jsonType, v.ID, v.ChannelID); err != nil {
		t.Fatalf("ParseVideo: %v", err)
	}
}

// TestParseVideo_FieldsAreCapped: each field is cut to its cap at a rune
// boundary; a repeated member and values of another type are harmless.
func TestParseVideo_FieldsAreCapped(t *testing.T) {
	v := testVideo()
	v.Title = strings.Repeat("é", innertube.MaxTitleBytes)
	v.Description = strings.Repeat("d", 2*innertube.MaxDescriptionBytes)
	v.Author = strings.Repeat("a", 2*innertube.MaxAuthorBytes)
	got, err := innertube.ParseVideo(ittest.PlayerAnswer(v), jsonType, v.ID, v.ChannelID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Title) > innertube.MaxTitleBytes || len(got.Title) < innertube.MaxTitleBytes-1 || !strings.HasPrefix(v.Title, got.Title) ||
		len(got.Description) != innertube.MaxDescriptionBytes || len(got.Author) != innertube.MaxAuthorBytes {
		t.Errorf("lengths: title %d, description %d, author %d", len(got.Title), len(got.Description), len(got.Author))
	}
	body := `{"videoDetails":{"videoId":"` + v.ID + `","videoId":"vid00000002","channelId":"` + testChannel +
		`","title":5,"author":{"x":1},"isLive":"yes"},"microformat":{"playerMicroformatRenderer":{"publishDate":"2026-10-10T16:00:41-07:00"}}}`
	got, err = innertube.ParseVideo([]byte(body), jsonType, v.ID, testChannel)
	if err != nil || got.Title != "" || got.Author != "" || !got.Published.Equal(published) {
		t.Fatalf("ParseVideo = %+v, %v", got, err)
	}
}

// TestVideoRequest: a POST of the video as the web client, with the
// player answer's cap.
func TestVideoRequest(t *testing.T) {
	r := innertube.VideoRequest("vid00000001")
	if r.Method != "POST" || r.URL != innertube.PlayerURL || r.MaxBytes != innertube.MaxVideoBytes ||
		r.Body != `{"context":{"client":{"clientName":"WEB","clientVersion":"`+innertube.WebVersion+`","hl":"en"}},"videoId":"vid00000001"}` {
		t.Fatalf("request = %+v", r)
	}
}

// TestParseVideo_DuplicateMembersFirstWins: a member that repeats is read
// once, its first occurrence, for every member read (strings, booleans
// and the status alike).
func TestParseVideo_DuplicateMembersFirstWins(t *testing.T) {
	const id = "aaaaaaaaaaa"
	body := func(status, live string) []byte {
		return []byte(`{"playabilityStatus":{` + status + `,"reason":"Sign in to confirm you're not a bot"},` +
			`"videoDetails":{"videoId":"` + id + `","channelId":"` + testChannel + `","title":"first","title":"second",` + live + `},` +
			`"microformat":{"playerMicroformatRenderer":{"publishDate":"2026-10-10T16:00:41-07:00"}}}`)
	}
	ok := `"status":"OK"`
	for _, tt := range []struct {
		name, status, live string
		want               error
	}{
		{"live false then true", ok, `"isLive":false,"isLive":true`, nil},
		{"live true then false", ok, `"isLive":true,"isLive":false`, innertube.ErrNotAired},
		{"upcoming false then true", ok, `"isUpcoming":false,"isUpcoming":true`, nil},
		{"live null then true", ok, `"isLive":null,"isLive":true`, innertube.ErrNotAired},
		{"live string then true", ok, `"isLive":"yes","isLive":true`, innertube.ErrNotAired},
		{"status OK then LOGIN_REQUIRED", `"status":"OK","status":"LOGIN_REQUIRED"`, `"isLive":false`, nil},
		{"status LOGIN_REQUIRED then OK", `"status":"LOGIN_REQUIRED","status":"OK"`, `"isLive":false`, innertube.ErrBlocked},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v, err := innertube.ParseVideo(body(tt.status, tt.live), jsonType, id, testChannel)
			if !errors.Is(err, tt.want) || (tt.want == nil && (err != nil || v.Title != "first")) {
				t.Fatalf("ParseVideo = %+v, %v; want %v", v, err, tt.want)
			}
		})
	}
}
