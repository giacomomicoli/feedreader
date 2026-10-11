package transcript

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// syntheticCaptionURL is a caption URL shaped like YouTube's, for video
// testVideoID.
func syntheticCaptionURL(lang, kind string) string {
	u := captionEndpoint + "?v=" + testVideoID + "&ei=synthetic&caps=asr&opi=1&xoaf=5&hl=en&ip=0.0.0.0&ipbits=0" +
		"&expire=9999999999&sparams=ip,ipbits,expire,v,ei,caps,opi,xoaf&signature=AB12.CD34&key=yt8"
	if kind != "" {
		u += "&kind=" + kind
	}
	return u + "&lang=" + lang + "&fmt=srv3"
}

// ct is a caption track of a built player response.
func ct(lang, kind string) map[string]any {
	t := map[string]any{
		"baseUrl":      syntheticCaptionURL(lang, kind),
		"name":         map[string]any{"simpleText": "Synthetic " + lang},
		"vssId":        "." + lang,
		"languageCode": lang,
	}
	if kind != "" {
		t["kind"] = kind
		t["vssId"] = "a." + lang
	}
	return t
}

// playerWith builds an OK player response for testVideoID whose caption
// renderer holds tracks plus the extra keys.
func playerWith(t testing.TB, tracks []map[string]any, extra map[string]any) []byte {
	t.Helper()
	renderer := map[string]any{"captionTracks": tracks}
	for k, v := range extra {
		renderer[k] = v
	}
	b, err := json.Marshal(map[string]any{
		"playabilityStatus": map[string]any{"status": "OK"},
		"videoDetails":      map[string]any{"videoId": testVideoID},
		"captions":          map[string]any{"playerCaptionsTracklistRenderer": renderer},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// statusPlayer builds a player response with only a playability status.
func statusPlayer(t testing.TB, status, reason string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{"playabilityStatus": map[string]any{"status": status, "reason": reason}})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// audio is an audioTracks entry with the original language's track ID.
func audio(id string) map[string]any {
	return map[string]any{"captionTrackIndices": []int{0}, "audioTrackId": id}
}

func TestParsePlayer_ChoosesTheTrackOfTheOriginalLanguage(t *testing.T) {
	tests := []struct {
		name      string
		body      []byte
		wantLang  string
		wantGen   bool
		wantInURL string
	}{
		{
			name:     "dubbed video: authored track of the original audio, not the dub's default",
			body:     fixture(t, "player_ok_dubbed.json"),
			wantLang: "en", wantInURL: "&key=yt8&lang=en&fmt=json3",
		},
		{
			name:     "automatic captions only",
			body:     fixture(t, "player_ok_asr_only.json"),
			wantLang: "en", wantGen: true, wantInURL: "&kind=asr&lang=en&fmt=json3",
		},
		{
			name:     "track names in the web shape do not matter",
			body:     fixture(t, "player_ok_web_names.json"),
			wantLang: "en", wantGen: true,
		},
		{
			name: "automatic captions of the original language beat authored ones in another",
			body: playerWith(t, []map[string]any{ct("en", ""), ct("de", "asr")}, map[string]any{
				"audioTracks": []any{audio("de-DE.4")}, "defaultAudioTrackIndex": 0,
			}),
			wantLang: "de", wantGen: true,
		},
		{
			name: "a regional variant of the original language counts",
			body: playerWith(t, []map[string]any{ct("en", "asr"), ct("en-GB", "")}, map[string]any{
				"audioTracks": []any{audio("en-US.4")}, "defaultAudioTrackIndex": 0,
			}),
			wantLang: "en-GB",
		},
		{
			name: "no original language: the translation source, upgraded to authored captions",
			body: playerWith(t, []map[string]any{ct("fr", ""), ct("fr", "asr"), ct("es", "")}, map[string]any{
				"audioTracks":                          []any{map[string]any{"captionTrackIndices": []int{0}}},
				"defaultAudioTrackIndex":               0,
				"defaultTranslationSourceTrackIndices": []int{1},
			}),
			wantLang: "fr",
		},
		{
			name: "no original language and no translation source: the audio track's default",
			body: playerWith(t, []map[string]any{ct("fr", ""), ct("es", "asr"), ct("pt", "asr")}, map[string]any{
				"audioTracks":            []any{map[string]any{"defaultCaptionTrackIndex": 2}},
				"defaultAudioTrackIndex": 0,
			}),
			wantLang: "pt", wantGen: true,
		},
		{
			name: "hostile indices fall back to the first authored track",
			body: playerWith(t, []map[string]any{ct("fr", "asr"), ct("es", ""), ct("pt", "")}, map[string]any{
				"audioTracks":                          []any{map[string]any{"defaultCaptionTrackIndex": 999, "audioTrackId": 7}},
				"defaultAudioTrackIndex":               -1,
				"defaultTranslationSourceTrackIndices": []any{"0", -1},
			}),
			wantLang: "es",
		},
		{
			name: "string and fractional indices are ignored",
			body: playerWith(t, []map[string]any{ct("fr", "asr"), ct("es", "asr")}, map[string]any{
				"audioTracks":                          []any{map[string]any{"defaultCaptionTrackIndex": 1.5}},
				"defaultAudioTrackIndex":               "0",
				"defaultTranslationSourceTrackIndices": map[string]any{"0": 1},
			}),
			wantLang: "fr", wantGen: true,
		},
		{
			name: "an unusable URL is skipped, never fetched",
			body: playerWith(t, []map[string]any{
				{"baseUrl": "https://evil.example/api/timedtext?lang=en", "languageCode": "en"},
				ct("en", "asr"),
			}, map[string]any{"audioTracks": []any{audio("en.4")}, "defaultAudioTrackIndex": 0}),
			wantLang: "en", wantGen: true,
		},
		{
			name: "an invalid language code is skipped",
			body: playerWith(t, []map[string]any{
				{"baseUrl": syntheticCaptionURL("en", ""), "languageCode": `en"><script>`},
				ct("en", "asr"),
			}, map[string]any{"audioTracks": []any{audio("en.4")}, "defaultAudioTrackIndex": 0}),
			wantLang: "en", wantGen: true,
		},
		{
			name: "vssId marks automatic captions without a kind",
			body: playerWith(t, []map[string]any{
				{"baseUrl": syntheticCaptionURL("en", ""), "languageCode": "en", "vssId": "a.en"},
			}, nil),
			wantLang: "en", wantGen: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr, err := parsePlayer(tt.body, jsonUTF8, testVideoID)
			if err != nil {
				t.Fatalf("parsePlayer: %v", err)
			}
			if tr.lang != tt.wantLang || tr.generated != tt.wantGen {
				t.Errorf("track = %s generated=%v, want %s generated=%v", tr.lang, tr.generated, tt.wantLang, tt.wantGen)
			}
			if !strings.HasPrefix(tr.url, captionEndpoint+"?v="+testVideoID+"&") || !strings.Contains(tr.url, tt.wantInURL) {
				t.Errorf("url = %q, want it to contain %q", tr.url, tt.wantInURL)
			}
		})
	}
}

func TestParsePlayer_TracksPastTheLimitAreIgnored(t *testing.T) {
	tracks := make([]map[string]any, maxCaptionTracks, maxCaptionTracks+1)
	for i := range tracks {
		tracks[i] = ct("fr", "asr")
	}
	tracks = append(tracks, ct("en", ""))
	body := playerWith(t, tracks, map[string]any{
		"audioTracks":                          []any{audio("en.4")},
		"defaultAudioTrackIndex":               0,
		"defaultTranslationSourceTrackIndices": []int{maxCaptionTracks},
	})
	tr, err := parsePlayer(body, jsonUTF8, testVideoID)
	if err != nil || tr.lang != "fr" {
		t.Fatalf("parsePlayer = %+v, %v; want the first track", tr, err)
	}
}

// TestIndex: an index is a JSON integer of ASCII digits only, and of at
// most maxIndexDigits of them; anything else is not valid, and a member
// repeated takes its last value.
func TestIndex(t *testing.T) {
	longest := strings.Repeat("9", maxIndexDigits)
	for _, tt := range []struct {
		lit  string
		want index
	}{
		{"0", index{n: 0, valid: true}},
		{"7", index{n: 7, valid: true}},
		{"199", index{n: 199, valid: true}},
		{longest, index{n: 999_999_999, valid: true}},
		{"1" + strings.Repeat("0", maxIndexDigits), index{}},
		{"-0", index{}},
		{"-1", index{}},
		{"1.5", index{}},
		{"1.0", index{}},
		{"1e2", index{}},
		{"1E2", index{}},
		{"null", index{}},
		{`"3"`, index{}},
		{"true", index{}},
		{"[0]", index{}},
		{"{}", index{}},
	} {
		var r captionRenderer
		if err := json.Unmarshal([]byte(`{"defaultAudioTrackIndex":`+tt.lit+`}`), &r); err != nil {
			t.Fatalf("%s: %v", tt.lit, err)
		}
		if r.DefaultAudioTrackIndex != tt.want {
			t.Errorf("index %s = %+v, want %+v", tt.lit, r.DefaultAudioTrackIndex, tt.want)
		}
	}
	for doc, want := range map[string]index{
		`{"defaultAudioTrackIndex":3,"defaultAudioTrackIndex":null}`: {},
		`{"defaultAudioTrackIndex":null,"defaultAudioTrackIndex":3}`: {n: 3, valid: true},
		`{"defaultAudioTrackIndex":3,"defaultAudioTrackIndex":-3}`:   {},
	} {
		var r captionRenderer
		if err := json.Unmarshal([]byte(doc), &r); err != nil || r.DefaultAudioTrackIndex != want {
			t.Errorf("%s: index %+v, %v; want %+v", doc, r.DefaultAudioTrackIndex, err, want)
		}
	}
}

func TestParsePlayer_Outcomes(t *testing.T) {
	noTracks := func(renderer map[string]any) []byte { return playerWith(t, []map[string]any{}, renderer) }
	// withDetails adds details (JSON members) to the videoDetails of body,
	// built by playerWith.
	withDetails := func(body []byte, details string) []byte {
		old := `"videoDetails":{"videoId":"` + testVideoID + `"}`
		if !strings.Contains(string(body), old) {
			t.Fatalf("no %s in %s", old, body)
		}
		return []byte(strings.Replace(string(body), old, `"videoDetails":{`+details+`,"videoId":"`+testVideoID+`"}`, 1))
	}
	oneTrack := []map[string]any{ct("en", "asr")}
	// withCaptions is an OK player response whose captions member is the
	// JSON value captions.
	withCaptions := func(captions string) []byte {
		return []byte(`{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"` + testVideoID +
			`"},"captions":` + captions + `}`)
	}
	tests := []struct {
		name        string
		body        []byte
		contentType string // "" = JSON
		want        error  // nil: an error that is none of ErrNoCaptions, ErrBlocked and ErrNotReady
	}{
		// YouTube says there are no captions by leaving them out, or with
		// an empty track list. Anything else without a usable track is an
		// answer this package no longer understands, not a video without
		// captions.
		{"no captions key", fixture(t, "player_no_captions.json"), "", ErrNoCaptions},
		{"null captions", withCaptions(`null`), "", ErrNoCaptions},
		{"empty track list", noTracks(nil), "", ErrNoCaptions},
		{"empty track list with spaces", withCaptions(`{"playerCaptionsTracklistRenderer":{"captionTracks":[ ` + "\n\t" + ` ]}}`), "", ErrNoCaptions},
		{"null track list", playerWith(t, nil, nil), "", nil},
		{"track list of another type", withCaptions(`{"playerCaptionsTracklistRenderer":{"captionTracks":{"0":{}}}}`), "", nil},
		{"renderer without tracks", withCaptions(`{"playerCaptionsTracklistRenderer":{}}`), "", nil},
		{"captions without a renderer", withCaptions(`{}`), "", nil},
		{"captions of another type", withCaptions(`"none"`), "", nil},
		{"renamed renderer", renamed(t, "player_ok_dubbed.json", "playerCaptionsTracklistRenderer", "captionsRenderer"), "", nil},
		{"renamed track list", renamed(t, "player_ok_dubbed.json", "captionTracks", "tracks"), "", nil},
		{"renamed track URL", renamed(t, "player_ok_asr_only.json", "baseUrl", "url"), "", nil},
		{"renamed language code", renamed(t, "player_ok_asr_only.json", "languageCode", "lang"), "", nil},
		{"tracks without URLs", playerWith(t, []map[string]any{{"languageCode": "en"}, {}}, nil), "", nil},
		{"removed or nonexistent video", fixture(t, "player_unavailable.json"), "", ErrUnavailable},
		{"region-blocked video", fixture(t, "player_unplayable.json"), "", ErrUnavailable},
		{"age-restricted (web)", statusPlayer(t, "LOGIN_REQUIRED", "Sign in to confirm your age"), "", ErrUnavailable},
		{"age-restricted (Android)", statusPlayer(t, "LOGIN_REQUIRED", "This video may be inappropriate for some users."), "", ErrUnavailable},
		{"private video", statusPlayer(t, "LOGIN_REQUIRED", "This video is private"), "", ErrUnavailable},
		{"members-only video", statusPlayer(t, "LOGIN_REQUIRED", "Join this channel to get access to members-only content like this video."), "", ErrUnavailable},
		{"video for a channel's members", statusPlayer(t, "LOGIN_REQUIRED", "This video is available to this channel's members on level: Synthetic Tier."), "", ErrUnavailable},
		{"age check", statusPlayer(t, "AGE_CHECK_REQUIRED", ""), "", ErrUnavailable},
		{"age verification", statusPlayer(t, "AGE_VERIFICATION_REQUIRED", ""), "", ErrUnavailable},
		{"content check", statusPlayer(t, "CONTENT_CHECK_REQUIRED", ""), "", ErrUnavailable},
		{"bot check", fixture(t, "player_bot_check.json"), "", ErrBlocked},
		{"sign-in for another reason", statusPlayer(t, "LOGIN_REQUIRED", "Something new"), "", ErrBlocked},
		{"upcoming premiere", fixture(t, "player_upcoming.json"), "", ErrNotReady},
		{"upcoming, with an OK status", withDetails(noTracks(nil), `"isUpcoming":true`), "", ErrNotReady},
		{"live stream", fixture(t, "player_live.json"), "", ErrNotReady},
		// The captions of a stream still on air would be cut short.
		{"live stream listing a caption track", withDetails(playerWith(t, oneTrack, nil), `"isLive":true`), "", ErrNotReady},
		{"live stream that has ended, still without captions", withDetails(noTracks(nil), `"isLive":false,"isLiveContent":true`), "", ErrNoCaptions},
		{"live flags of another type", withDetails(noTracks(nil), `"isLive":"true","isUpcoming":1`), "", ErrNoCaptions},
		{"unknown status", statusPlayer(t, "SOMETHING_NEW", ""), "", nil},
		{"missing status", []byte(`{"videoDetails":{"videoId":"` + testVideoID + `"}}`), "", nil},
		{"proof-of-origin token required", fixture(t, "player_pot.json"), "", ErrBlocked},
		{"another video", []byte(strings.ReplaceAll(string(fixture(t, "player_ok_asr_only.json")), `"videoId": "`+testVideoID, `"videoId": "vid00000002`)), "", nil},
		{"no video details", []byte(`{"playabilityStatus":{"status":"OK"},"captions":{"playerCaptionsTracklistRenderer":{"captionTracks":[{"baseUrl":"` +
			syntheticCaptionURL("en", "") + `","languageCode":"en"}]}}}`), "", nil},
		{"captcha page", []byte(`<html><body><div class="g-recaptcha"></div></body></html>`), htmlUTF8, ErrBlocked},
		{"other content type", []byte(`{}`), "text/plain", nil},
		{"invalid JSON", []byte(`{"playabilityStatus":`), "", nil},
		{"not an object", []byte(`[1,2,3]`), "", nil},
		{"only unusable tracks", playerWith(t, []map[string]any{
			{"baseUrl": "http://www.youtube.com/api/timedtext?v=" + testVideoID, "languageCode": "en"},
			{"baseUrl": "javascript:alert(1)", "languageCode": "en"},
			{"baseUrl": "https://www.youtube.com.evil.example/api/timedtext", "languageCode": "en"},
		}, nil), "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctype := tt.contentType
			if ctype == "" {
				ctype = jsonUTF8
			}
			tr, err := parsePlayer(tt.body, ctype, testVideoID)
			if err == nil {
				t.Fatalf("parsePlayer = %+v, want an error", tr)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if tt.want == nil && (errors.Is(err, ErrNoCaptions) || errors.Is(err, ErrBlocked) || !errors.Is(err, errFormat)) {
				t.Errorf("err = %v, want a format error, neither ErrNoCaptions nor ErrBlocked", err)
			}
			if errors.Is(err, ErrUnavailable) && !errors.Is(err, ErrNoCaptions) {
				t.Errorf("ErrUnavailable %v does not match ErrNoCaptions", err)
			}
			checkNotReady(t, err, tt.want == ErrNotReady)
		})
	}
}

// checkNotReady fails unless err matches ErrNotReady exactly when want is
// set, and then none of the other outcomes: the video may still get its
// transcript.
func checkNotReady(t *testing.T, err error, want bool) {
	t.Helper()
	if got := errors.Is(err, ErrNotReady); got != want {
		t.Errorf("err = %v: matches ErrNotReady = %v, want %v", err, got, want)
	}
	if want && (errors.Is(err, ErrNoCaptions) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrBlocked)) {
		t.Errorf("err = %v matches another outcome, want ErrNotReady alone", err)
	}
}

// TestParsePlayer_ErrorsShowNoPageContent: reasons and subreasons are
// YouTube's text (with HTML in it); only the status token is shown.
func TestParsePlayer_ErrorsShowNoPageContent(t *testing.T) {
	for _, body := range [][]byte{
		fixture(t, "player_unavailable.json"),
		fixture(t, "player_bot_check.json"),
		fixture(t, "player_unplayable.json"),
		fixture(t, "player_upcoming.json"),
		statusPlayer(t, "<script>alert(1)</script>", "x"),
		statusPlayer(t, strings.Repeat("A", maxStatusBytes+1), "x"),
		[]byte(`<html><a href="javascript:alert(1)">synthetic</a></html>`),
	} {
		_, err := parsePlayer(body, jsonUTF8, testVideoID)
		if err == nil {
			t.Fatalf("parsePlayer(%.40q): no error", body)
		}
		for _, leak := range []string{"<", "javascript", "unavailable in", "country", "bot", "Error code", "Premieres", strings.Repeat("A", maxStatusBytes+1)} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("error %q shows %q", err, leak)
			}
		}
	}
}

func TestPlayerRequest(t *testing.T) {
	r := playerRequest(testVideoID)
	if r.Method != "POST" || r.URL != playerURL || r.ContentType != jsonType || r.Accept != jsonType || r.ETag != "" || r.LastModified != "" {
		t.Errorf("request = %+v", r)
	}
	var body struct {
		Context struct {
			Client map[string]string `json:"client"`
		} `json:"context"`
		VideoID string `json:"videoId"`
	}
	if err := json.Unmarshal([]byte(r.Body), &body); err != nil {
		t.Fatalf("body %q: %v", r.Body, err)
	}
	want := map[string]string{"clientName": clientName, "clientVersion": clientVersion, "hl": clientLang}
	if fmt.Sprint(body.Context.Client) != fmt.Sprint(want) || body.VideoID != testVideoID {
		t.Errorf("body = %s", r.Body)
	}
	if want := fmt.Sprintf(`{"context":{"client":{"clientName":%q,"clientVersion":%q,"hl":%q}},"videoId":%q}`,
		clientName, clientVersion, clientLang, testVideoID); string(r.Body) != want {
		t.Errorf("body = %s, want %s", r.Body, want)
	}
}
