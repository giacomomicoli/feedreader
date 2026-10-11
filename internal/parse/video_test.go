package parse

import (
	"fmt"
	"html"
	"strings"
	"testing"
	"time"
)

// youTubeAtom renders the Atom entry YouTube's channel feed has for a
// video: the same fields VideoEntry takes.
func youTubeAtom(id, title, description, author string, published time.Time) []byte {
	e := html.EscapeString
	return fmt.Appendf(nil, `<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns:yt="http://www.youtube.com/xml/schemas/2015" xmlns:media="http://search.yahoo.com/mrss/" xmlns="http://www.w3.org/2005/Atom">
 <id>yt:channel:syntheticChannel000001</id><title>%[4]s</title>
 <entry>
  <id>yt:video:%[1]s</id><yt:videoId>%[1]s</yt:videoId><yt:channelId>UCsyntheticChannel000001</yt:channelId>
  <title>%[2]s</title>
  <link rel="alternate" href="https://www.youtube.com/shorts/%[1]s"/>
  <author><name>%[4]s</name><uri>https://www.youtube.com/channel/UCsyntheticChannel000001</uri></author>
  <published>%[5]s</published><updated>%[5]s</updated>
  <media:group><media:title>%[2]s</media:title>
   <media:thumbnail url="https://i1.ytimg.com/vi/%[1]s/hqdefault.jpg" width="480" height="360"/>
   <media:description>%[3]s</media:description></media:group>
 </entry>
</feed>`, id, e(title), e(description), e(author), published.Format(time.RFC3339))
}

// TestVideoEntry_EqualsTheFeedEntry: a video read from YouTube's web API is
// stored exactly as its entry in the channel's feed would be (GUID, link,
// title, summary, author, thumbnail, dates), so the feed coming back
// neither duplicates nor changes it.
func TestVideoEntry_EqualsTheFeedEntry(t *testing.T) {
	published := time.Date(2026, 10, 10, 23, 0, 41, 0, time.UTC)
	for _, tt := range []struct{ name, title, description, author string }{
		{"plain", "A video", "Line one\nhttps://example.com/x\n\n\nLine two", "Synthetic channel"},
		{"markup is text", "Vec<T> & <b>bold</b>", "<script>alert(1)</script> & <a href=x>y</a>", "A & B <i>"},
		{"empty description", "Title", "", "Channel"},
		{"spaces", "  spaced   title  ", "  \n description \n ", " Name "},
		{"long", strings.Repeat("t", 300), strings.Repeat("d", 6000), strings.Repeat("a", 300)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const id = "vid00000001"
			doc, err := Parse(youTubeAtom(id, tt.title, tt.description, tt.author, published), "application/atom+xml",
				"https://www.youtube.com/feeds/videos.xml?channel_id=UCsyntheticChannel000001")
			if err != nil || len(doc.Entries) != 1 {
				t.Fatalf("Parse: %v", err)
			}
			got, ok := VideoEntry(id, tt.title, tt.description, tt.author, published)
			if !ok {
				t.Fatal("VideoEntry refused a valid video")
			}
			if want := doc.Entries[0]; got != want {
				t.Errorf("VideoEntry =\n %+v\nfeed entry =\n %+v", got, want)
			}
		})
	}
}

// FuzzVideoEntry: whatever the fields, the entry honours the Entry
// contract and stays a feed entry of the video: the GUID, link and
// thumbnail are made from the ID alone, and the summary is escaped text
// (no markup but line breaks).
func FuzzVideoEntry(f *testing.F) {
	f.Add("aaaaaaaaaaa", "Title", "Line\n\nline", "Author", int64(1760137241))
	f.Add("a-_Z9aaaaaa", "<b>x</b>", "<script>alert(1)</script>&amp;", "&lt;i&gt;", int64(0))
	f.Add("aaaaaaaaaaa", "", "\u202e\x00", " ", int64(-1))
	f.Fuzz(func(t *testing.T, id, title, description, author string, unix int64) {
		e, ok := VideoEntry(id, title, description, author, time.Unix(unix, 0))
		if !ok {
			return
		}
		if e.GUID != "yt:video:"+id || e.URL != "https://www.youtube.com/watch?v="+id ||
			e.ThumbnailURL != "https://i1.ytimg.com/vi/"+id+"/hqdefault.jpg" {
			t.Fatalf("entry %+v", e)
		}
		checkHTTPURL(t, "URL", e.URL)
		checkHTTPURL(t, "ThumbnailURL", e.ThumbnailURL)
		checkRunes(t, "Title", e.Title)
		checkRunes(t, "Author", e.Author)
		checkUTC(t, e.PublishedAt)
		if e.Title == "" || e.UpdatedAt != e.PublishedAt {
			t.Fatalf("entry %+v", e)
		}
		if s := strings.ReplaceAll(e.SummaryHTML, "<br>", ""); strings.ContainsAny(s, "<>") {
			t.Fatalf("summary holds markup: %q", e.SummaryHTML)
		}
	})
}

// TestVideoEntry_Checks: the GUID, link and thumbnail are
// built from a checked ID on fixed hosts; an invalid ID or a zero date is
// refused.
func TestVideoEntry_Checks(t *testing.T) {
	published := time.Date(2026, 10, 10, 23, 0, 41, 0, time.FixedZone("", -7*60*60))
	e, ok := VideoEntry("a-_Z9aaaaaa", "t", "", "", published)
	if !ok || e.GUID != "yt:video:a-_Z9aaaaaa" || e.URL != "https://www.youtube.com/watch?v=a-_Z9aaaaaa" ||
		e.ThumbnailURL != "https://i1.ytimg.com/vi/a-_Z9aaaaaa/hqdefault.jpg" ||
		!e.PublishedAt.Equal(published) || e.PublishedAt.Location() != time.UTC || e.UpdatedAt != e.PublishedAt {
		t.Fatalf("VideoEntry = %+v, %v", e, ok)
	}
	if e.Title != "t" || e.SummaryHTML != "" || e.Author != "" {
		t.Errorf("fields = %+v", e)
	}
	for _, id := range []string{"", "short", "aaaaaaaaaaaa", "aaaaaaaaa/a", "aaaaaaaaa?a", "aaaaaaaaa&a", "aaaaaaaa..a"} {
		if _, ok := VideoEntry(id, "t", "", "", published); ok {
			t.Errorf("VideoEntry(%q) accepted", id)
		}
	}
	if _, ok := VideoEntry("aaaaaaaaaaa", "t", "", "", time.Time{}); ok {
		t.Error("VideoEntry accepted a zero date")
	}
	if e, _ := VideoEntry("aaaaaaaaaaa", "", "", "", published); e.Title != untitled {
		t.Errorf("empty title = %q", e.Title)
	}
}
