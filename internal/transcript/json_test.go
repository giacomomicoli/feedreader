package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/innertube"
)

// jnode is a JSON value as encoding/json's tokenizer reads it, members in
// order and duplicates kept.
type jnode struct {
	kind    byte   // '{', '[', '"', or 'l' for a number, true, false or null
	text    string // string: decoded; literal: as written
	members []jmember
	elems   []*jnode
}

type jmember struct {
	key string
	val *jnode
}

func readNode(dec *json.Decoder) (*jnode, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch v := tok.(type) {
	case json.Delim:
		n := &jnode{kind: byte(v)}
		for dec.More() {
			var key string
			if v == '{' {
				k, err := dec.Token()
				if err != nil {
					return nil, err
				}
				key = k.(string)
			}
			val, err := readNode(dec)
			if err != nil {
				return nil, err
			}
			if v == '{' {
				n.members = append(n.members, jmember{key, val})
			} else {
				n.elems = append(n.elems, val)
			}
		}
		_, err := dec.Token() // the closing delimiter
		return n, err
	case string:
		return &jnode{kind: '"', text: v}, nil
	case json.Number:
		return &jnode{kind: 'l', text: string(v)}, nil
	case bool:
		if v {
			return &jnode{kind: 'l', text: "true"}, nil
		}
		return &jnode{kind: 'l', text: "false"}, nil
	default:
		return &jnode{kind: 'l', text: "null"}, nil
	}
}

// referenceJSON3 is parseJSON3 written on encoding/json's tokenizer and a
// tree of the whole document: slow and allocating, but plainly right.
func referenceJSON3(body []byte, limit int) (string, error) {
	if !json.Valid(body) {
		return "", errFormat
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	root, err := readNode(dec)
	if err != nil || root.kind != '{' {
		return "", errFormat
	}
	t := newTextBuilder(limit)
	// The document's shape, as parseJSON3 judges it: an events member that
	// is a list, a segment (an element of a cue's segs list), one with a
	// utf8 string.
	events, segments, texts, unknown := false, false, false, false
walk:
	for _, m := range root.members {
		if m.key != "events" || m.val.kind != '[' {
			continue
		}
		events = true
		for _, ev := range m.val.elems {
			t.startCue()
			if ev.kind != '{' {
				unknown = true
			}
			for _, em := range ev.members {
				if em.key != "segs" {
					unknown = unknown || !slices.Contains(knownEventKeys[:], em.key)
					continue
				}
				for _, seg := range em.val.elems {
					segments = true
					text, speaker, isText := "", false, false
					for _, sm := range seg.members {
						switch sm.key {
						case "utf8":
							text, isText = "", sm.val.kind == '"'
							if isText {
								text = sm.val.text
							}
						case "isSpeakerChange":
							speaker = sm.val.kind == 'l' && innertube.Truthy([]byte(sm.val.text))
						}
					}
					texts = texts || isText
					t.writeSegment(text, speaker)
					if t.full() {
						break walk
					}
				}
			}
		}
	}
	if !events || (segments && !texts) || (!segments && unknown) {
		return "", errFormat
	}
	text := t.String()
	if onlyAnnotations(text) {
		return "", ErrNoCaptions
	}
	return text, nil
}

// FuzzParseJSON3_MatchesReference: the walker reads every valid document
// as the tokenizer-based reference does.
func FuzzParseJSON3_MatchesReference(f *testing.F) {
	for _, name := range []string{"json3_manual.json", "json3_asr.json", "json3_annotations_only.json"} {
		f.Add(fixture(f, name), fuzzTextRunes)
	}
	f.Add([]byte(`{"ev\u0065nts":[{"segs":[{"utf8":"a\ud83d\ude00\ud800\u0041","utf8":5}],"segs":[{"isSpeakerChange":1e-400,"utf8":"x"}]}],"events":7}`), fuzzTextRunes)
	f.Add([]byte(` {"events" : [ 5 , [ {"segs":[]} ] , {"segs" : [ "x" , {"utf8" : "a\"b\\\\c\/"} ] } ] } `), 3)
	f.Add([]byte("{\"events\":[{\"segs\":[{\"utf8\":\"\xff\xfe a\"}]}]}"), 1)
	f.Add([]byte(`{"events":[{"tStartMs":0,"sgz":[{"utf8":"x"}]}]}`), fuzzTextRunes)
	f.Add([]byte(`{"events":[{},{"t\u0053tartMs":0,"wWinId":1},5]}`), fuzzTextRunes)
	f.Fuzz(func(t *testing.T, body []byte, limit int) {
		limit = min(max(limit, 1), config.TranscriptMaxRunes)
		got, gotErr := parseJSON3(body, limit)
		want, wantErr := referenceJSON3(body, limit)
		if got != want || errors.Is(gotErr, ErrNoCaptions) != errors.Is(wantErr, ErrNoCaptions) || (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("parseJSON3 = %q, %v; reference %q, %v", got, gotErr, want, wantErr)
		}
	})
}

// FuzzJSONWalker_InvalidInput: on bytes json.Valid did not check, the walk
// must still end within the input.
func FuzzJSONWalker_InvalidInput(f *testing.F) {
	f.Add([]byte(`{"events":[{"segs":[{"utf8":"a\`))
	f.Add([]byte(`{"events":[{"segs":[}],"x":{]`))
	f.Add([]byte(`{"events":[,:,{"segs":[{"utf8":"x","isSpeakerChange":}]}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		w := innertube.NewWalker(body)
		readEvents(w, newTextBuilder(fuzzTextRunes))
		if w.Pos() > len(body) {
			t.Fatalf("walked to %d of %d", w.Pos(), len(body))
		}
	})
}
