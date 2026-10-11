package transcript

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/giacomomicoli/feedreader/internal/config"
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
							speaker = sm.val.kind == 'l' && truthy([]byte(sm.val.text))
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

// FuzzUnquote: unquote decodes like encoding/json, and its cut prefix
// clips to the same text as the whole string.
func FuzzUnquote(f *testing.F) {
	for _, s := range []string{`plain`, `a\nb\t\"\\\/`, `\u00e9\ud83d\ude00`, `\ud800x`, `\udc00\ud83d`, "\xff\xfe", `\u0000`, `\ud83d\u0041`} {
		f.Add([]byte(`"`+s+`"`), 3)
	}
	f.Fuzz(func(t *testing.T, lit []byte, max int) {
		var want string
		lit = bytes.TrimSpace(lit)
		if len(lit) < 2 || !json.Valid(lit) || json.Unmarshal(lit, &want) != nil {
			return
		}
		contents := lit[1 : len(lit)-1]
		if got := unquote(contents, len(lit)*utf8.UTFMax); got != want {
			t.Fatalf("unquote(%q) = %q, want %q", contents, got, want)
		}
		max = min(len(lit), max&0xff)
		got := unquote(contents, max)
		if !strings.HasPrefix(want, got) || clipUTF8(got, max) != clipUTF8(want, max) {
			t.Fatalf("unquote(%q, %d) = %q; whole %q", contents, max, got, want)
		}
	})
}

func TestTruthy(t *testing.T) {
	for lit, want := range map[string]bool{
		"true": true, "1": true, "-2": true, "0.001": true, "1e-400": true, "3E2": true,
		"false": false, "null": false, "0": false, "-0": false, "0.0": false, "0e5": false, "": false, "x1": false,
	} {
		if got := truthy([]byte(lit)); got != want {
			t.Errorf("truthy(%q) = %v", lit, got)
		}
	}
}

func TestKeyIs(t *testing.T) {
	for raw, want := range map[string]bool{
		`utf8`: true, `utf\u0038`: true, `\u0075tf8`: true, `UTF8`: false, `utf8 `: false,
		`utf\u00388`: false, `ut`: false, `utf\ud800`: false,
	} {
		if got := keyIs([]byte(raw), "utf8"); got != want {
			t.Errorf("keyIs(%q) = %v", raw, got)
		}
	}
}

// TestJSONWalker_TerminatesOnInvalidInput: the walker only ever sees
// documents json.Valid accepted, but must not loop or read out of bounds
// on anything else.
func TestJSONWalker_TerminatesOnInvalidInput(t *testing.T) {
	for _, doc := range []string{``, `{`, `[`, `"`, `{"a`, `{"a":`, `{"a":}`, `{:}`, `[,,]`, `{]`, `[}`, `{"a" 1}`, `}}}`, `{"a":"\`, `{,"a":[1,]}`, `x`} {
		w := &jsonWalker{b: []byte(doc)}
		w.object(func([]byte) { w.array(func() { w.skip() }) })
		w.skip()
		if w.i > len(doc) {
			t.Errorf("%q: read to %d", doc, w.i)
		}
	}
}

// FuzzJSONWalker_InvalidInput: on bytes json.Valid did not check, the walk
// must still end within the input.
func FuzzJSONWalker_InvalidInput(f *testing.F) {
	f.Add([]byte(`{"events":[{"segs":[{"utf8":"a\`))
	f.Add([]byte(`{"events":[{"segs":[}],"x":{]`))
	f.Add([]byte(`{"events":[,:,{"segs":[{"utf8":"x","isSpeakerChange":}]}]}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		w := &jsonWalker{b: body}
		readEvents(w, newTextBuilder(fuzzTextRunes))
		if w.i > len(body) {
			t.Fatalf("walked to %d of %d", w.i, len(body))
		}
	})
}
