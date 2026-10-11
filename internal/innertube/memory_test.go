package innertube_test

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/innertube"
	"github.com/giacomomicoli/feedreader/internal/innertube/ittest"
)

// The memory tests decode hostile answers at their caps and check that
// the work allocates about nothing but what is returned: a hostile answer
// costs its own size (held by the fetch) and no more.

// allocated returns the bytes f allocates.
func allocated(f func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// filled returns prefix, unit repeated, and suffix, about size bytes.
func filled(size int, prefix, unit, suffix string) []byte {
	n := max(0, (size-len(prefix)-len(suffix))/len(unit))
	return []byte(prefix + strings.Repeat(unit, n) + suffix)
}

// validatorSlack is what encoding/json's syntax check (json.Valid, and the
// one json.Unmarshal runs first) may allocate: a stack of one word per
// nesting level, at most 10 000 levels (deeper input is refused), so about
// 400 KB whatever the answer's size (TestJSONValidDepthBound).
const validatorSlack = 512 << 10

// gridHead opens a selected Videos tab whose content is a list.
var gridHead = `{"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[{"tabRenderer":{"endpoint":{"browseEndpoint":{"browseId":"` +
	testChannel + `","params":"` + innertube.TabVideos.Params + `"}},"title":"Videos","selected":true,"content":[`

const gridTail = `{}]}}]}}}`

// tabWith is a selected Videos tab whose content is content.
func tabWith(content string) string {
	return strings.TrimSuffix(gridHead, "[") + content + `}}]}}}`
}

// distinctIDs is a grid of as many distinct videoId members as fit in
// size.
func distinctIDs(size int) []byte {
	const digits = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	var b strings.Builder
	b.WriteString(gridHead)
	for i := 0; b.Len() < size-40; i++ {
		id := []byte("aaaaaaaaaaa")
		for j, x := 0, i; j < len(id) && x > 0; j, x = j+1, x/len(digits) {
			id[j] = digits[x%len(digits)]
		}
		b.WriteString(`{"videoId":"` + string(id) + `"},`)
	}
	b.WriteString(gridTail)
	return []byte(b.String())
}

// TestHostileBrowse_BoundedMemory: at the cap, every hostile shape
// allocates no more than the validator's depth stack, in bounded time.
func TestHostileBrowse_BoundedMemory(t *testing.T) {
	const size = innertube.MaxBrowseBytes
	esc := string(rune(backslash)) + "u0049"
	docs := map[string][]byte{
		"deep nesting (refused by json.Valid)":  filled(size, `{"contents":`, `[`, ``),
		"deep nesting 9 000 levels in the tab":  []byte(tabWith(strings.Repeat("[", 9000) + strings.Repeat("]", 9000))),
		"huge string in the tab":                []byte(tabWith(`{"a":"` + strings.Repeat("a", size-400) + `"}`)),
		"millions of tiny objects":              filled(size, gridHead, `{},`, gridTail),
		"millions of videoId members, one id":   filled(size, gridHead, `{"videoId":"aaaaaaaaaaa"},`, gridTail),
		"millions of distinct videoIds":         distinctIDs(size),
		"millions of escaped videoId keys":      filled(size, gridHead, `{"video`+esc+`d":"aaaaaaaaaaa"},`, gridTail),
		"millions of long escaped keys":         filled(size, gridHead, `{"`+strings.Repeat(esc, 8)+`":1},`, gridTail),
		"tiny objects before the tab":           filled(size, `{"x":[`, `{},`, `{}],"contents":`+strings.TrimPrefix(tabWith(`{"videoId":"aaaaaaaaaaa"}`), `{"contents":`)),
		"many tabs, none selected":              filled(size, `{"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[`, `{"tabRenderer":{"selected":false,"content":{"videoId":"aaaaaaaaaaa"}}},`, `{}]}}}`),
		"millions of alerts":                    filled(size, `{"alerts":[`, `{"alertRenderer":{"type":"INFO"}},`, `{}]}`),
		"millions of alerts, escaped keys":      filled(size, `{"alerts":[`, `{"alertRenderer":{"`+string(rune(backslash))+`u0074ype":"INFO"}},`, `{}]}`),
		"many selected tabs, other params":      filled(size, `{"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[`, `{"tabRenderer":{"endpoint":{"browseEndpoint":{"params":"EgZvdGhlcnPyBgQKAjoA"}},"selected":true,"content":{}}},`, `{}]}}}`),
		"home tab with many foreign videos":     ittest.HomeAnswer(testChannel, ittest.IDs("foreign", size/200)...),
		"twice the cap":                         make([]byte, 2*size),
		"not JSON":                              filled(size, `{`, `"a":1,`, ``),
		"Videos tab of 30 videos (normal size)": ittest.TabAnswer(testChannel, innertube.TabVideos, ittest.IDs("vid", 30)...),
	}
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			if len(doc) > 2*size {
				t.Fatalf("fixture is %d bytes", len(doc))
			}
			start := time.Now()
			var ids []string
			var err error
			n := allocated(func() { ids, err = innertube.ParseBrowse(doc, jsonType, innertube.TabVideos.Params, maxIDs) })
			took := time.Since(start)
			t.Logf("%-40s %5d KB  alloc %7d B  ids %2d  %v  err=%v", name, len(doc)>>10, n, len(ids), took.Round(time.Millisecond), err)
			if n > validatorSlack {
				t.Errorf("allocated %d B", n)
			}
			if took > 2*time.Second {
				t.Errorf("took %v", took)
			}
			if len(ids) > maxIDs {
				t.Errorf("%d ids", len(ids))
			}
		})
	}
}

// TestJSONValidDepthBound: what json.Valid allocates depends on the
// nesting depth (which it caps at 10 000), not on the size.
func TestJSONValidDepthBound(t *testing.T) {
	for _, size := range []int{1 << 20, 8 << 20, 32 << 20} {
		doc := filled(size, `{"x":`, `[`, ``)
		if n := allocated(func() { json.Valid(doc) }); n > validatorSlack {
			t.Errorf("json.Valid on %d MB of '[': allocated %d B", size>>20, n)
		}
	}
}

// TestHostileVideo_BoundedMemory: a hostile player answer at its cap
// allocates at most the capped fields and the validator's stack.
func TestHostileVideo_BoundedMemory(t *testing.T) {
	const size = innertube.MaxVideoBytes
	id := "aaaaaaaaaaa"
	head := `{"playabilityStatus":{"status":"UNPLAYABLE"},"videoDetails":{"videoId":"` + id + `","channelId":"` + testChannel + `",`
	date := `"microformat":{"playerMicroformatRenderer":{"publishDate":"2026-10-10T16:00:41-07:00"}}}`
	esc := string(rune(backslash)) + "u00e9"
	docs := map[string][]byte{
		"huge title":                  filled(size, head+`"title":"`, `a`, `"},`+date),
		"huge escaped description":    filled(size, head+`"shortDescription":"`, esc, `"},`+date),
		"repeated titles":             filled(size, head+`"x":0`, `,"title":"`+strings.Repeat("t", 3000)+`"`, `},`+date),
		"repeated huge reasons":       filled(size, `{"playabilityStatus":{"x":0`, `,"reason":"`+strings.Repeat("r", 20000)+`"`, `}}`),
		"deep nesting":                filled(size, `{"x":`, `[`, ``),
		"millions of tiny members":    filled(size, `{"videoDetails":{`, `"a":1,`, `"b":2}}`),
		"millions of tiny elements":   filled(size, `{"x":[`, `0,`, `0]}`),
		"huge publishDate":            filled(size, head+`"title":"t"},"microformat":{"playerMicroformatRenderer":{"publishDate":"`, `1`, `"}}}`),
		"twice the cap":               make([]byte, 2*size),
		"a web page served as JSON":   filled(size, `<html>`, `<p>`, ``),
		"normal answer (about 10 KB)": ittest.PlayerAnswer(testVideo()),
	}
	limit := uint64(innertube.MaxTitleBytes+innertube.MaxDescriptionBytes+innertube.MaxAuthorBytes+innertube.MaxReasonBytes) + validatorSlack
	for name, doc := range docs {
		t.Run(name, func(t *testing.T) {
			start := time.Now()
			var v innertube.Video
			var err error
			n := allocated(func() { v, err = innertube.ParseVideo(doc, jsonType, id, testChannel) })
			took := time.Since(start)
			t.Logf("%-30s %4d KB  alloc %7d B  %v  err=%v", name, len(doc)>>10, n, took.Round(time.Millisecond), err)
			if n > limit {
				t.Errorf("allocated %d B", n)
			}
			if len(v.Title) > innertube.MaxTitleBytes || len(v.Description) > innertube.MaxDescriptionBytes {
				t.Error("a field over its cap")
			}
			if took > 2*time.Second {
				t.Errorf("took %v", took)
			}
		})
	}
}

func BenchmarkParseBrowse(b *testing.B) {
	body := ittest.TabAnswer(testChannel, innertube.TabVideos, ittest.IDs("vid", 30)...)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = innertube.ParseBrowse(body, jsonType, innertube.TabVideos.Params, maxIDs)
	}
}

func BenchmarkParseBrowse_HostileTiny(b *testing.B) {
	body := filled(innertube.MaxBrowseBytes, gridHead, `{},`, gridTail)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = innertube.ParseBrowse(body, jsonType, innertube.TabVideos.Params, maxIDs)
	}
}

func BenchmarkParseVideo(b *testing.B) {
	body := ittest.PlayerAnswer(testVideo())
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for b.Loop() {
		_, _ = innertube.ParseVideo(body, jsonType, "vid00000001", testChannel)
	}
}
