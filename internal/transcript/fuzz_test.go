package transcript

import (
	"net/url"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

// YouTube's answers are untrusted: none of these may panic, a chosen track
// must be fetchable only on YouTube's caption endpoint, and the text must
// stay within its bounds.

// fuzzTextRunes keeps fuzzed transcripts short so truncation is exercised.
const fuzzTextRunes = 64

func FuzzParsePlayer(f *testing.F) {
	for _, name := range []string{
		"player_ok_dubbed.json", "player_ok_asr_only.json", "player_ok_web_names.json",
		"player_no_captions.json", "player_unavailable.json", "player_bot_check.json",
		"player_unplayable.json", "player_upcoming.json", "player_live.json", "player_pot.json",
	} {
		f.Add(fixture(f, name), jsonUTF8)
	}
	f.Add([]byte(`{"captions":{"playerCaptionsTracklistRenderer":{"captionTracks":[{},{"baseUrl":5}],"audioTracks":[[]],"defaultAudioTrackIndex":-1}}}`), jsonUTF8)
	f.Add([]byte(`{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"`+testVideoID+`"},"captions":{"playerCaptionsTracklistRenderer":null,"playerCaptionsTracklistRenderer":{"captionTracks":[ ]}}}`), jsonUTF8)
	f.Add(renamed(f, "player_ok_asr_only.json", "baseUrl", "url"), jsonUTF8)
	f.Add([]byte(`<html>g-recaptcha</html>`), htmlUTF8)
	f.Fuzz(func(t *testing.T, body []byte, contentType string) {
		tr, err := parsePlayer(body, contentType, testVideoID)
		if err != nil {
			if tr != (track{}) {
				t.Fatalf("error %v with a track %+v", err, tr)
			}
			return
		}
		assertCaptionURL(t, tr.url)
		if !langPattern.MatchString(tr.lang) || len(tr.lang) > maxLangBytes {
			t.Fatalf("language %q", tr.lang)
		}
	})
}

func FuzzCaptionURL(f *testing.F) {
	f.Add(syntheticCaptionURL("en", "asr"))
	f.Add(captionEndpoint + "?v=" + testVideoID + "&exp=xpe&lang=en")
	f.Add("https://www.youtube.com/api/timedtext?fmt=srv3&fmt=vtt&lang=en#x")
	f.Add("https://www.youtube.com.evil.example/api/timedtext?lang=en")
	f.Fuzz(func(t *testing.T, raw string) {
		got, err := captionURL(raw, testVideoID)
		if err != nil {
			return
		}
		assertCaptionURL(t, got)
	})
}

// assertCaptionURL fails unless raw is on YouTube's caption endpoint, with
// a query that survives a round trip and asks for JSON once.
func assertCaptionURL(t *testing.T, raw string) {
	t.Helper()
	if !strings.HasPrefix(raw, captionEndpoint+"?") || len(raw) > maxCaptionURLBytes+len("&fmt="+captionFormat) {
		t.Fatalf("caption URL %q", raw)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != captionHost || u.Path != captionPath || u.User != nil || u.Fragment != "" || u.String() != raw {
		t.Fatalf("caption URL %q parses as %+v, %v", raw, u, err)
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(q["fmt"]) != 1 || q.Get("fmt") != captionFormat || q.Get("exp") == potMarker {
		t.Fatalf("caption URL query %q", u.RawQuery)
	}
}

func FuzzParseJSON3(f *testing.F) {
	for _, name := range []string{"json3_manual.json", "json3_asr.json", "json3_annotations_only.json"} {
		f.Add(fixture(f, name))
	}
	f.Add(json3(cue("a\x00b\u202ec&amp;<i>d</i>"), `{"segs":[{"utf8":"\ud800","isSpeakerChange":1}]}`, `[]`, `5`))
	f.Add(json3(cue(strings.Repeat("ab. ", fuzzTextRunes))))
	f.Add([]byte(`{"events":[{"segs":[{"text":"a"}]}],"events":null}`))
	f.Add([]byte(`{"events":[{"tStartMs":0,"sgz":[{"utf8":"x"}]}]}`))
	f.Add([]byte(`{"events":[{},{"t\u0053tartMs":0,"wWinId":1},{"a<\n\"":1}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		text, err := parseJSON3(body, fuzzTextRunes)
		if err != nil {
			if text != "" {
				t.Fatalf("error %v with text %q", err, text)
			}
			// An unknown key is named safe for a log line: bounded, and
			// only letters, digits, _ and ? (with … when cut).
			if _, key, ok := strings.Cut(err.Error(), "unknown event key "); ok {
				name := strings.TrimSuffix(strings.Trim(key, `"`), "…")
				if len(name) > maxLoggedKeyBytes || strings.TrimFunc(name, func(r rune) bool {
					return r == '_' || r == '?' || r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r))
				}) != "" {
					t.Fatalf("unsafe key in %q", err)
				}
			}
			return
		}
		if text == "" || !utf8.ValidString(text) || utf8.RuneCountInString(text) > fuzzTextRunes {
			t.Fatalf("text %q", text)
		}
		if strings.TrimSpace(text) != text || strings.Contains(text, "  ") || strings.Contains(text, "\n\n\n") {
			t.Fatalf("whitespace not normalized: %q", text)
		}
		for _, r := range text {
			if r != '\n' && (unicode.IsControl(r) || dropped(r) || unicode.IsSpace(r) && r != ' ') {
				t.Fatalf("text %q holds %U", text, r)
			}
		}
	})
}

func FuzzVideoID(f *testing.F) {
	f.Add("https://www.youtube.com/watch?v=" + testVideoID)
	f.Add("https://m.youtube.com/watch?v=" + testVideoID + "&v=x")
	f.Add("https://www.youtube.com/watch?v=%41bcdefghijk")
	f.Fuzz(func(t *testing.T, raw string) {
		id, ok := VideoID(raw)
		if ok != (id != "") || ok && !videoIDPattern.MatchString(id) {
			t.Fatalf("VideoID(%q) = %q, %v", raw, id, ok)
		}
	})
}
