package innertube_test

import (
	"bytes"
	"errors"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/giacomomicoli/feedreader/internal/innertube"
	"github.com/giacomomicoli/feedreader/internal/innertube/ittest"
)

// FuzzParseBrowse: whatever the answer, no panic; IDs only without an
// error, valid, distinct, at most max, each written in the answer; a
// refusal or format error never comes with IDs.
func FuzzParseBrowse(f *testing.F) {
	videos, shorts := innertube.TabVideos.Params, innertube.TabShorts.Params
	f.Add(ittest.TabAnswer(testChannel, innertube.TabVideos, ittest.IDs("vid", 20)...), videos)
	f.Add(ittest.TabAnswer(testChannel, innertube.TabShorts, ittest.IDs("sho", 3)...), shorts)
	f.Add(ittest.HomeAnswer(testChannel, ittest.IDs("foreign", 5)...), videos)
	f.Add(ittest.AlertAnswer(), videos)
	f.Add(ittest.ErrorAnswer(), videos)
	f.Add([]byte(`{"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[{"tabRenderer":{"endpoint":{"browseEndpoint":{"params":"`+
		videos+`"}},"selected":true,"content":{"videoId":"aaaaaaaaaaa","videoId":"aaaaaaaaaaa","x":[{"videoId":"bbbbbbbbbbb"}]}}}]}}}`), videos)
	f.Add([]byte(`{"contents":[],"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[null,5,{"tabRenderer":{"selected":1,"content":[]}}]}}}`), videos)
	f.Add([]byte(`{"alerts":5,"error":null,"contents":{"twoColumnBrowseResultsRenderer":{"tabs":{}}}}`), "")
	f.Fuzz(func(t *testing.T, body []byte, params string) {
		ids, err := innertube.ParseBrowse(body, jsonType, params, maxIDs)
		if err != nil && ids != nil {
			t.Fatal("ids with an error")
		}
		if len(ids) > maxIDs {
			t.Fatalf("%d ids", len(ids))
		}
		seen := map[string]bool{}
		for _, id := range ids {
			if !innertube.VideoIDPattern.MatchString(id) || seen[id] || !bytes.Contains(body, []byte(`"`+id+`"`)) {
				t.Fatalf("bad, repeated or invented id %q", id)
			}
			seen[id] = true
		}
		if err != nil && !errors.Is(err, innertube.ErrNoTab) && !errors.Is(err, innertube.ErrAlert) &&
			!errors.Is(err, innertube.ErrFormat) && !errors.Is(err, innertube.ErrTooLarge) {
			t.Fatalf("unexpected error %v", err)
		}
	})
}

// FuzzParseVideo: whatever the answer, no panic; details only for the
// video and channel asked about, with a UTC date, every field within its
// cap and valid UTF-8.
func FuzzParseVideo(f *testing.F) {
	v := testVideo()
	f.Add(ittest.PlayerAnswer(v), v.ID, v.ChannelID)
	f.Add(ittest.AndroidPlayerAnswer(v), v.ID, v.ChannelID)
	f.Add(ittest.MissingPlayerAnswer(), v.ID, v.ChannelID)
	f.Add([]byte(`{"videoDetails":{"videoId":"a","channelId":"c","title":5,"shortDescription":"x","lengthSeconds":"-1"},`+
		`"microformat":{"playerMicroformatRenderer":{"publishDate":"2026-10-10T16:00:41-07:00","isShortsEligible":"yes"}}}`), "a", "c")
	f.Add([]byte(`{"videoDetails":{"videoId":"a","channelId":"c"},"microformat":{"playerMicroformatRenderer":{"publishDate":"2026-13-10T16:00:41Z"}}}`), "a", "c")
	f.Add([]byte(`{"playabilityStatus":{"status":"LOGIN_REQUIRED","reason":"x"},"videoDetails":null}`), "a", "c")
	f.Fuzz(func(t *testing.T, body []byte, id, channelID string) {
		got, err := innertube.ParseVideo(body, jsonType, id, channelID)
		if err != nil {
			if got != (innertube.Video{}) {
				t.Fatal("details with an error")
			}
			return
		}
		if got.ID != id || got.ChannelID != channelID || got.Published.IsZero() || got.Published.Location() != time.UTC {
			t.Fatalf("bad video %+v", got)
		}
		if len(got.Title) > innertube.MaxTitleBytes || len(got.Description) > innertube.MaxDescriptionBytes ||
			len(got.Author) > innertube.MaxAuthorBytes {
			t.Fatalf("a field over its cap: %d %d %d", len(got.Title), len(got.Description), len(got.Author))
		}
		for _, s := range []string{got.Title, got.Description, got.Author} {
			if !utf8.ValidString(s) {
				t.Fatalf("invalid UTF-8 %q", s)
			}
		}
	})
}
