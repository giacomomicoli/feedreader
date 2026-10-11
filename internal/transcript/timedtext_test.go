package transcript

import (
	"errors"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/giacomomicoli/feedreader/internal/config"
)

func TestCaptionURL_RewritesOnlyTheFormat(t *testing.T) {
	const base = captionEndpoint + "?v=" + testVideoID + "&ei=x&caps=asr&expire=9999999999&sparams=ip,ipbits,expire,v&signature=AB.CD&key=yt8&lang=en"
	for raw, want := range map[string]string{
		base + "&fmt=srv3":                            base + "&fmt=json3",
		base:                                          base + "&fmt=json3",
		base + "&fmt=vtt&kind=asr":                    base + "&fmt=json3&kind=asr",
		base + "&fmt=srv3&fmt=srv1":                   base + "&fmt=json3",
		base + "&fmt=json3":                           base + "&fmt=json3",
		base + "&name=Synthetic%20name":               base + "&name=Synthetic%20name&fmt=json3",
		base + "&tlang=pt-BR&fmt=srv3&x=1":            base + "&tlang=pt-BR&fmt=json3&x=1",
		captionEndpoint + "?lang=en&v=" + testVideoID: captionEndpoint + "?lang=en&v=" + testVideoID + "&fmt=json3",
	} {
		got, err := captionURL(raw, testVideoID)
		if err != nil || got != want {
			t.Errorf("captionURL(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestCaptionURL_RefusesAnythingElse(t *testing.T) {
	const query = "?v=" + testVideoID + "&lang=en&fmt=srv3"
	for _, raw := range []string{
		"http://www.youtube.com/api/timedtext" + query,
		"https://www.youtube.com.evil.example/api/timedtext" + query,
		"https://evil.example/api/timedtext" + query,
		"https://youtube.com/api/timedtext" + query,
		"https://WWW.YOUTUBE.COM/api/timedtext" + query,
		"https://www.youtube.com./api/timedtext" + query,
		"https://www.youtube.com:443/api/timedtext" + query,
		"https://user@www.youtube.com/api/timedtext" + query,
		"https://www.youtube.com/api/timedtext/x" + query,
		"https://www.youtube.com/API/timedtext" + query,
		"https://www.youtube.com/api/%74imedtext" + query,
		"https://www.youtube.com/watch" + query,
		"https://www.youtube.com/api/timedtext" + query + "#frag",
		"https://www.youtube.com/api/timedtext" + query + "&x=<script>",
		"https://www.youtube.com/api/timedtext" + query + "&x=a b",
		"https://www.youtube.com/api/timedtext" + query + "&x=%zz",
		"https://www.youtube.com/api/timedtext" + query + "&x=1;y=2",
		"https://www.youtube.com/api/timedtext?v=vid00000002&lang=en",
		"https://www.youtube.com/api/timedtext?v=" + testVideoID + "&v=" + testVideoID,
		"https://www.youtube.com/api/timedtext?lang=en",                           // no v: which video?
		"https://www.youtube.com/api/timedtext?lang=en&fmt=srv3&V=" + testVideoID, // keys are case-sensitive
		"https://www.youtube.com/api/timedtext" + query + "&x=" + strings.Repeat("a", maxCaptionURLBytes),
		"/api/timedtext" + query,
		"//www.youtube.com/api/timedtext" + query,
		"javascript:alert(1)",
		"https:www.youtube.com/api/timedtext" + query,
		"",
	} {
		if got, err := captionURL(raw, testVideoID); err == nil || errors.Is(err, ErrBlocked) {
			t.Errorf("captionURL(%.80q) = %q, %v; want it refused", raw, got, err)
		}
	}
}

func TestCaptionURL_ProofOfOriginIsBlocked(t *testing.T) {
	raw := captionEndpoint + "?v=" + testVideoID + "&caps=asr&exp=xpe&xoaf=5&sparams=ip,ipbits,expire,v,ei,caps,opi,exp,xoaf&lang=en"
	if _, err := captionURL(raw, testVideoID); !errors.Is(err, ErrBlocked) {
		t.Fatalf("err = %v, want ErrBlocked", err)
	}
}

func TestParseJSON3_Fixtures(t *testing.T) {
	for name, want := range map[string]string{
		"json3_manual.json": "Synthetic line one wrapped [Narrator] second cue.\n\n>> A new speaker & styled words",
		"json3_asr.json":    "alpha beta gamma delta.\n\n>> epsilon zeta",
	} {
		got, err := parseJSON3(fixture(t, name), config.TranscriptMaxRunes)
		if err != nil || got != want {
			t.Errorf("%s: parseJSON3 = %q, %v; want %q", name, got, err, want)
		}
	}
}

// TestParseJSON3_AnnotationsOnlyIsNoCaptions: the automatic captions of a
// music video are only sound tags such as [Music]; that is no transcript.
func TestParseJSON3_AnnotationsOnlyIsNoCaptions(t *testing.T) {
	if got, err := parseJSON3(fixture(t, "json3_annotations_only.json"), config.TranscriptMaxRunes); !errors.Is(err, ErrNoCaptions) {
		t.Fatalf("parseJSON3 = %q, %v; want ErrNoCaptions", got, err)
	}
}

// json3 builds a caption document from events given as JSON fragments.
func json3(events ...string) []byte {
	return []byte(`{"wireMagic":"pb3","pens":[{}],"events":[` + strings.Join(events, ",") + `]}`)
}

// cue is an event of one segment.
func cue(text string) string { return `{"tStartMs":0,"segs":[{"utf8":` + quoteJSON(text) + `}]}` }

func quoteJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r < ' ':
			b.WriteString(`\u00`)
			b.WriteByte("0123456789abcdef"[r>>4])
			b.WriteByte("0123456789abcdef"[r&0xf])
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func TestParseJSON3_Text(t *testing.T) {
	tests := []struct {
		name string
		doc  []byte
		want string
	}{
		{"cues are joined by spaces", json3(cue("one"), cue("two"), cue(" three ")), "one two three"},
		{"segments are concatenated", json3(`{"segs":[{"utf8":"wo"},{"utf8":"rd"},{"utf8":" next"}]}`), "word next"},
		{"line wraps and runs of spaces collapse", json3(cue("a\n\nb\t c"), `{"aAppend":1,"segs":[{"utf8":"\n"}]}`, cue("d")), "a b c d"},
		{"events without text are skipped", json3(`{}`, `{"segs":null}`, `{"segs":[]}`, `{"segs":{}}`, `{"segs":[{}]}`, `5`, `"x"`, `null`, `[]`, cue("kept")), "kept"},
		{"non-string text is skipped", json3(`{"segs":[{"utf8":5},{"utf8":{"a":1}},{"utf8":"ok"}]}`), "ok"},
		{"entities are decoded", json3(cue("it&#39;s &amp; &lt;b&gt; fine")), "it's & <b> fine"},
		{"formatting tags are dropped", json3(cue("<i>soft</i> <b>bold</b> <font color=\"#fff\">c</font> <u>u</u>")), "soft bold c u"},
		{"other markup stays literal text", json3(cue("<script>alert(1)</script> Vec<T>")), "<script>alert(1)</script> Vec<T>"},
		{"control characters become spaces", json3(cue("a\x00b\x07c\x7fd\u0085e\u009bf")), "a b c d e f"},
		{"bidirectional overrides are dropped", json3(cue("left\u202eright\u2066x\u2069\u202a\u202c")), "leftrightx"},
		{"invisible format characters are dropped", json3(cue("zero\u200bwidth\u00adsoft\u2060join\ufeffbom\u200emark\u2061f")), "zerowidthsoftjoinbommarkf"},
		{"zero-width joiners stay", json3(cue("a\u200db\u200cc")), "a\u200db\u200cc"},
		{"number marks and flag tags stay", json3(cue("\u0600١ \U0001F3F4\U000E0067\U000E0062\U000E007F")), "\u0600١ \U0001F3F4\U000E0067\U000E0062\U000E007F"},
		{"marks, annotations and joiners are dropped", json3(cue("a\u061cb\u200fc\ufff9d\ufffbe\u2060f")), "abcdef"},
		{"a lone surrogate becomes U+FFFD", json3(`{"segs":[{"utf8":"a\ud800b"}]}`), "a\ufffdb"},
		{"duplicate keys", []byte(`{"events":[` + cue("first") + `],"events":[` + cue("second") + `]}`), "first second"},
		{"speaker change flag", json3(cue("a."), `{"segs":[{"utf8":"b","isSpeakerChange":true}]}`, `{"segs":[{"utf8":"c","isSpeakerChange":0}]}`), "a.\n\nb c"},
		{"speaker marker", json3(cue("a"), cue(" >> b"), cue(">c")), "a\n\n>> b >c"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseJSON3(tt.doc, config.TranscriptMaxRunes)
			if err != nil || got != tt.want {
				t.Errorf("parseJSON3 = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

// TestParseJSON3_NoText: a track without cues, whose cues are all
// recognisably caption-free (an empty object, or only the known timing and
// window keys: knownEventKeys) with no segment anywhere, or whose cues hold
// no text but spaces and control characters, is a video without captions.
func TestParseJSON3_NoText(t *testing.T) {
	for name, doc := range map[string][]byte{
		"no events":           json3(),
		"one empty event":     []byte(`{"events":[{}]}`),
		"window-only events":  json3(`{}`, `{"tStartMs":0,"dDurationMs":1000,"id":1,"wpWinPosId":1,"wsWinStyleId":1}`, `{"tStartMs":5,"wWinId":1,"aAppend":1}`),
		"escaped known keys":  []byte(`{"events":[{"t\u0053tartMs":0}]}`),
		"empty segment lists": json3(`{"segs":[]}`, `{"segs":null}`, `{"tStartMs":5}`),
		"whitespace only":     json3(cue(" \n\t "), cue("\u00a0\u2028")),
		"controls only":       json3(cue("\x00\x01"), cue("\u202e")),
		"empty texts":         json3(cue(""), `{"tStartMs":0,"dDurationMs":1}`),
	} {
		if got, err := parseJSON3(doc, config.TranscriptMaxRunes); !errors.Is(err, ErrNoCaptions) {
			t.Errorf("%s: parseJSON3 = %q, %v; want ErrNoCaptions", name, got, err)
		}
	}
}

// deeperThanEncodingJSON is a nesting depth encoding/json refuses (its
// limit is 10000).
const deeperThanEncodingJSON = 1 << 14

func TestParseJSON3_Malformed(t *testing.T) {
	for name, doc := range map[string][]byte{
		"empty":         nil,
		"truncated":     []byte(`{"events":[{"segs":[{"utf8":"a"`),
		"not an object": []byte(`[{"segs":[{"utf8":"a"}]}]`),
		"no events key": []byte(`{"wireMagic":"pb3"}`),
		"HTML":          []byte(`<html><body>consent</body></html>`),
		"too deep":      []byte(`{"events":[` + strings.Repeat("[", deeperThanEncodingJSON) + `]}`),
		// Segments without a text where YouTube puts it: the format
		// changed (a renamed field), which must not pass for a video
		// without captions.
		"events not a list": []byte(`{"events":{"segs":[{"utf8":"x"}]}}`),
		"events null":       []byte(`{"events":null}`),
		"renamed events":    renamed(t, "json3_manual.json", "events", "cues"),
		"renamed text":      renamed(t, "json3_manual.json", "utf8", "text"),
		"cues without text": json3(`{"tStartMs":0,"dDurationMs":1000}`, `{"segs":[{}]}`, `{"segs":null}`),
		// No segment anywhere, but an event with a key that is not one of
		// the known timing and window keys: a renamed segs list looks
		// exactly like that.
		"renamed segments":         renamed(t, "json3_asr.json", "segs", "segments"),
		"renamed segs, one event":  []byte(`{"events":[{"tStartMs":0,"sgz":[{"utf8":"x"}]}]}`),
		"mixed window and unknown": json3(`{}`, `{"tStartMs":0,"dDurationMs":1000,"id":1}`, `{"tStartMs":5,"sgz":[]}`),
		"event that is no object":  json3(`{"tStartMs":0}`, `5`),
		"texts of another type":    json3(`{"segs":[{"utf8":5},{"utf8":null}]}`),
	} {
		got, err := parseJSON3(doc, config.TranscriptMaxRunes)
		if !errors.Is(err, errFormat) || errors.Is(err, ErrNoCaptions) || errors.Is(err, ErrBlocked) {
			t.Errorf("%s: parseJSON3 = %q, %v; want a format error", name, got, err)
		}
	}
}

// keptFormat is every format character (Unicode Cf) a transcript keeps,
// because it changes what is shown: ZWNJ and ZWJ, the prepended number
// marks of Arabic, Syriac and Kaithi, and the emoji tag characters of
// subdivision flags.
var keptFormat = []rune{
	0x200c, 0x200d,
	0x0600, 0x0601, 0x0602, 0x0603, 0x0604, 0x0605, 0x06dd, 0x070f, 0x0890, 0x0891, 0x08e2, 0x110bd, 0x110cd,
}

// TestDropped_FormatCharacters checks dropped against Go's own table of
// Unicode category Cf: every format character is dropped but keptFormat
// and the tag characters U+E0020–U+E007F, which all are format characters.
func TestDropped_FormatCharacters(t *testing.T) {
	const tagFirst, tagLast = 0xe0020, 0xe007f
	kept := map[rune]bool{}
	for _, r := range keptFormat {
		kept[r] = true
	}
	for r := rune(tagFirst); r <= tagLast; r++ {
		kept[r] = true
	}
	for r := range kept {
		if !unicode.Is(unicode.Cf, r) {
			t.Errorf("%U is kept but is not a format character", r)
		}
	}
	n := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if !unicode.Is(unicode.Cf, r) {
			if dropped(r) && !bidiControl(r) {
				t.Errorf("%U is dropped but is not a format character", r)
			}
			continue
		}
		n++
		if dropped(r) == kept[r] {
			t.Errorf("%U: dropped = %v, want %v", r, dropped(r), !kept[r])
		}
	}
	if n == 0 {
		t.Fatal("no format characters in the table")
	}
}

// TestParseJSON3_UnknownKeyIsNamed: the format error names the first
// unknown event key, so the log says what YouTube renamed, but bounded and
// with anything but letters, digits and _ replaced: never the body.
func TestParseJSON3_UnknownKeyIsNamed(t *testing.T) {
	for doc, want := range map[string]string{
		`{"events":[{"tStartMs":0,"sgz":[{"utf8":"x"}]}]}`: `unknown event key "sgz"`,
		`{"events":[{"a<b>\n\"c":1}]}`:                     `unknown event key "a?b???c"`,
		`{"events":[{"` + strings.Repeat("k", 4*maxLoggedKeyBytes) + `":1}]}`: `unknown event key "` +
			strings.Repeat("k", maxLoggedKeyBytes) + `…"`,
		`{"events":[7]}`: `an event is not an object`,
	} {
		_, err := parseJSON3([]byte(doc), config.TranscriptMaxRunes)
		if !errors.Is(err, errFormat) || !strings.Contains(err.Error(), want) {
			t.Errorf("parseJSON3(%.60q) err = %v; want a format error with %q", doc, err, want)
		}
	}
}

func TestParseJSON3_TruncatesAtTheLimit(t *testing.T) {
	const limit = config.TranscriptMaxRunes
	for _, word := range []string{"word", "été", "日本語", "😀"} {
		t.Run(word, func(t *testing.T) {
			var events []string
			for n := 0; n <= limit; n += utf8.RuneCountInString(word) + 1 {
				events = append(events, cue(word))
			}
			got, err := parseJSON3(json3(events...), limit)
			if err != nil {
				t.Fatal(err)
			}
			if n := utf8.RuneCountInString(got); n != limit && n != limit-1 {
				t.Errorf("%d runes, want at most %d and close to it", n, limit)
			}
			if !utf8.ValidString(got) || !strings.HasSuffix(got, ellipsis) {
				t.Errorf("cut text ends %q", got[max(0, len(got)-16):])
			}
			if body := strings.TrimSuffix(got, ellipsis); strings.TrimRightFunc(body, unicode.IsSpace) != body {
				t.Errorf("space before the ellipsis")
			}
		})
	}
}

func TestParseJSON3_ExactlyAtTheLimitIsNotCut(t *testing.T) {
	const limit = 12
	got, err := parseJSON3(json3(cue("abcde"), cue("fghijk")), limit)
	if err != nil || got != "abcde fghijk" {
		t.Errorf("parseJSON3 = %q, %v", got, err)
	}
	got, err = parseJSON3(json3(cue("abcde"), cue("fghijkl")), limit)
	if err != nil || got != "abcde fghij"+ellipsis {
		t.Errorf("parseJSON3 = %q, %v", got, err)
	}
}

func TestParseJSON3_Paragraphs(t *testing.T) {
	sentence := strings.Repeat("x", paragraphSoftRunes/2) + "."
	soft, err := parseJSON3(json3(cue(sentence), cue(sentence), cue("next")), config.TranscriptMaxRunes)
	if err != nil {
		t.Fatal(err)
	}
	if want := sentence + " " + sentence + "\n\nnext"; soft != want {
		t.Errorf("long paragraph after a sentence end: %q", soft)
	}

	noStop := strings.Repeat("y", paragraphSoftRunes)
	open, err := parseJSON3(json3(cue(noStop), cue("next")), config.TranscriptMaxRunes)
	if err != nil || strings.Contains(open, "\n") {
		t.Errorf("paragraph split without a sentence end: %q, %v", open, err)
	}

	long := strings.Repeat("z", paragraphHardRunes)
	hard, err := parseJSON3(json3(cue(long), cue("next")), config.TranscriptMaxRunes)
	if err != nil || hard != long+"\n\nnext" {
		t.Errorf("very long paragraph is not split: %.40q…, %v", hard, err)
	}
}
