package transcript

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/config"
)

// allocated returns the bytes allocated while f runs: an upper bound on the
// memory f holds at once.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// filled returns prefix, unit repeated and suffix, at most size bytes.
func filled(size int, prefix, unit, suffix string) []byte {
	n := max(0, (size-len(prefix)-len(suffix))/len(unit))
	return []byte(prefix + strings.Repeat(unit, n) + suffix)
}

// hostileDocBytes is the size of the hostile documents below. Their cost
// is per element, so the ratio measured here is the ratio at the size caps
// (it was measured there too); the documents are larger than the text
// limit at its longest, so cutting the text is exercised.
const hostileDocBytes = 2 << 20

// allocSlack is what decoding may allocate besides a copy of the
// document: the text at its longest (config.TranscriptMaxRunes runes of up
// to utf8.UTFMax bytes) grown by doubling, twice (decoded segment, text).
const allocSlack = 4 << 20

// TestHostileAnswers_BoundedMemory: lists of millions of tiny elements, huge
// strings and deep nesting, within the size caps, must not cost more than
// about their size. Decoding them into slices or with a Decoder per list
// took 10 to 90 times their size.
func TestHostileAnswers_BoundedMemory(t *testing.T) {
	const player = `{"playabilityStatus":{"status":"OK"},"videoDetails":{"videoId":"` + testVideoID + `"},"captions":{"playerCaptionsTracklistRenderer":{`
	longURL := syntheticCaptionURL("en", "") + "&x=" + strings.Repeat("a", maxCaptionURLBytes)
	players := map[string][]byte{
		"empty caption tracks":    filled(hostileDocBytes, player+`"captionTracks":[`, `{},`, `{}]}}}`),
		"tiny caption tracks":     filled(hostileDocBytes, player+`"captionTracks":[`, `{"baseUrl":"a"},`, `{}]}}}`),
		"too long caption URLs":   filled(hostileDocBytes, player+`"captionTracks":[`, `{"baseUrl":"`+longURL+`","languageCode":"en"},`, `{}]}}}`),
		"non-object tracks":       filled(hostileDocBytes, player+`"captionTracks":[`, `5,`, `5]}}}`),
		"audio caption indices":   filled(hostileDocBytes, player+`"captionTracks":[{}],"audioTracks":[{"captionTrackIndices":[`, `0,`, `0]}]}}}`),
		"translation sources":     filled(hostileDocBytes, player+`"captionTracks":[{}],"defaultTranslationSourceTrackIndices":[`, `0,`, `0]}}}`),
		"unknown array":           filled(hostileDocBytes, `{"x":[`, `{},`, `{}]}`),
		"nesting":                 filled(hostileDocBytes, `{"x":`, `[`, ``),
		"long playability reason": filled(hostileDocBytes, `{"playabilityStatus":{"status":"ERROR","reason":"`, `a`, `"}}`),
		// Members repeated, or nulled and repeated: the second pass that
		// reads which of them are there must not allocate a renderer each
		// time (a pointer to one took 270 times the document's size).
		"repeated track lists": filled(hostileDocBytes, player+`"captionTracks":[{}]`, `,"captionTracks":[{}]`, `}}}`),
		"nulled track lists":   filled(hostileDocBytes, player+`"x":0`, `,"captionTracks":null,"captionTracks":[{}]`, `}}}`),
		"nulled renderers":     filled(hostileDocBytes, `{"captions":{"x":0`, `,"playerCaptionsTracklistRenderer":null,"playerCaptionsTracklistRenderer":{}`, `}}`),
		"nulled captions":      filled(hostileDocBytes, `{"x":0`, `,"captions":null,"captions":{}`, `}`),
		// Indices repeated: each is decoded again, so it must not allocate
		// (json.Unmarshal per index took 5.6 times the document's size).
		"repeated audio track index": filled(hostileDocBytes, player+`"captionTracks":[{}]`,
			`,"defaultAudioTrackIndex":0`, `}}}`),
		"repeated caption track index": filled(hostileDocBytes, player+`"captionTracks":[{}],"audioTracks":[{"x":0`,
			`,"defaultCaptionTrackIndex":0`, `}]}}}`),
		"repeated translation sources": filled(hostileDocBytes, player+`"captionTracks":[{}]`,
			`,"defaultTranslationSourceTrackIndices":[0]`, `}}}`),
	}
	for name, doc := range players {
		t.Run("player/"+name, func(t *testing.T) {
			if n := allocated(func() { _, _ = parsePlayer(doc, jsonUTF8, testVideoID) }); n > uint64(len(doc))+allocSlack {
				t.Errorf("allocated %d KB for a %d KB document", n>>10, len(doc)>>10)
			}
		})
	}
	captions := map[string][]byte{
		"empty events":          filled(hostileDocBytes, `{"events":[`, `{},`, `{}]}`),
		"non-object events":     filled(hostileDocBytes, `{"events":[`, `5,`, `5]}`),
		"blank events":          filled(hostileDocBytes, `{"events":[`, `{"segs":[{"utf8":" "}]},`, `{}]}`),
		"one cue, empty segs":   filled(hostileDocBytes, `{"events":[{"segs":[`, `{},`, `{}]}]}`),
		"one cue, blank segs":   filled(hostileDocBytes, `{"events":[{"segs":[`, `{"utf8":"  "},`, `{}]}]}`),
		"one cue, text segs":    filled(hostileDocBytes, `{"events":[{"segs":[`, `{"utf8":"ab"},`, `{}]}]}`),
		"one cue, escaped segs": filled(hostileDocBytes, `{"events":[{"segs":[`, `{"utf8":"\n"},`, `{}]}]}`),
		"speaker flags":         filled(hostileDocBytes, `{"events":[{"segs":[`, `{"isSpeakerChange":1},`, `{}]}]}`),
		"one long segment":      filled(hostileDocBytes, `{"events":[{"segs":[{"utf8":"`, `a`, `"}]}]}`),
		"one long blank seg":    filled(hostileDocBytes, `{"events":[{"segs":[{"utf8":"`, ` `, `"}]}]}`),
		"one long escaped seg":  filled(hostileDocBytes, `{"events":[{"segs":[{"utf8":"`, `\u00e9`, `"}]}]}`),
		"entities":              filled(hostileDocBytes, `{"events":[{"segs":[{"utf8":"`, `&amp;`, `"}]}]}`),
		"tags":                  filled(hostileDocBytes, `{"events":[{"segs":[{"utf8":"`, `<i>`, `"}]}]}`),
		"unknown array":         filled(hostileDocBytes, `{"events":[],"x":[`, `{},`, `{}]}`),
		// Event keys are checked against knownEventKeys: escaped ones are
		// decoded, a few bytes each at most.
		"escaped known event keys": filled(hostileDocBytes, `{"events":[{"x":0`, `,"t\u0053tartMs":0`, `}]}`),
		"escaped unknown keys":     filled(hostileDocBytes, `{"events":[`, `{"\u0061":0},`, `{}]}`),
		"long unknown key":         filled(hostileDocBytes, `{"events":[{"`, `\u0061`, `":0}]}`),
		"nesting":                  filled(hostileDocBytes, `{"events":[`, `[`, ``),
	}
	for name, doc := range captions {
		t.Run("captions/"+name, func(t *testing.T) {
			if n := allocated(func() { _, _ = parseJSON3(doc, config.TranscriptMaxRunes) }); n > uint64(len(doc))+allocSlack {
				t.Errorf("allocated %d KB for a %d KB document", n>>10, len(doc)>>10)
			}
		})
	}
}

// TestHostileAnswers_TrackListIsCapped: tracks past maxCaptionTracks are
// never decoded, however many there are.
func TestHostileAnswers_TrackListIsCapped(t *testing.T) {
	doc := filled(hostileDocBytes, `{"captions":{"playerCaptionsTracklistRenderer":{"captionTracks":[`,
		`{"baseUrl":"`+syntheticCaptionURL("en", "")+`","languageCode":"en"},`, `{}]}}}`)
	if n := bytes.Count(doc, []byte("baseUrl")); n <= maxCaptionTracks {
		t.Fatalf("only %d tracks", n)
	}
	p, err := decodePlayer(doc)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(p.Captions.Renderer.CaptionTracks); got != maxCaptionTracks {
		t.Errorf("%d tracks decoded", got)
	}
}
