package transcript

import "testing"

func TestVideoID(t *testing.T) {
	const id = "abcdefghi_-"
	for raw, want := range map[string]string{
		// The form entries of YouTube feeds use.
		"https://www.youtube.com/watch?v=" + id:            id,
		"https://www.youtube.com/watch?v=" + id + "&t=1s":  id,
		"https://www.youtube.com/watch?t=1s&v=" + id:       id,
		"http://www.youtube.com/watch?v=" + id:             id,
		"https://youtube.com/watch?v=" + id:                id,
		"https://m.youtube.com/watch?v=" + id:              id,
		"HTTPS://WWW.YouTube.com/watch?v=" + id:            id,
		"https://www.youtube.com/watch?v=" + id + "#t=10s": id,

		// Not an entry URL of a video.
		"https://www.youtube.com/watch?v=abcdefghij":              "", // 10 characters
		"https://www.youtube.com/watch?v=abcdefghijkl":            "", // 12 characters
		"https://www.youtube.com/watch?v=abcdefghij.":             "",
		"https://www.youtube.com/watch?v=abcdefghij%2F":           "",
		"https://www.youtube.com/watch?v=" + id + "&v=" + id:      "",
		"https://www.youtube.com/watch?v=":                        "",
		"https://www.youtube.com/watch":                           "",
		"https://www.youtube.com.evil.example/watch?v=" + id:      "",
		"https://evil.example/watch?v=" + id:                      "",
		"https://youtu.be/" + id:                                  "",
		"https://www.youtube.com/shorts/" + id:                    "",
		"https://www.youtube.com/embed/" + id:                     "",
		"https://www.youtube.com/watch/?v=" + id:                  "",
		"https://www.youtube.com:8443/watch?v=" + id:              "",
		"https://user@www.youtube.com/watch?v=" + id:              "",
		"ftp://www.youtube.com/watch?v=" + id:                     "",
		"javascript:alert(1)//www.youtube.com/watch?v=" + id:      "",
		"//www.youtube.com/watch?v=" + id:                         "",
		"https://www.youtube.com/watch?v=" + id + "&v=":           "",
		"https://www.youtube.com/watch?v=abc%zzdefgh":             "",
		"https://www.youtube.com/feeds/videos.xml?channel_id=UC1": "",
		"": "",
	} {
		got, ok := VideoID(raw)
		if got != want || ok != (want != "") {
			t.Errorf("VideoID(%q) = %q, %v; want %q", raw, got, ok, want)
		}
	}
}
