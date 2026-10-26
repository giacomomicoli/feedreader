package innertube_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/innertube"
	"github.com/giacomomicoli/feedreader/internal/innertube/ittest"
)

const (
	testChannel = "UCsyntheticChannel000001"
	jsonType    = "application/json; charset=UTF-8"
	maxIDs      = 15
	backslash   = 0x5c
)

// TestParseBrowse_VideosTab: the IDs of the Videos tab's grid, in its
// order, each once although every item names its video five times; at
// most max; the header's video and the continuation are not taken.
func TestParseBrowse_VideosTab(t *testing.T) {
	ids := ittest.IDs("vid", 30)
	body := ittest.TabAnswer(testChannel, innertube.TabVideos, ids...)
	if n := strings.Count(string(body), ids[0]); n < 5 {
		t.Fatalf("fixture names its first video %d times, want at least 5", n)
	}
	got, err := innertube.ParseBrowse(body, jsonType, innertube.TabVideos.Params, maxIDs)
	if err != nil || !slices.Equal(got, ids[:maxIDs]) {
		t.Fatalf("ParseBrowse = %v, %v; want the first %d ids in order", got, err, maxIDs)
	}
	all, err := innertube.ParseBrowse(body, jsonType, innertube.TabVideos.Params, 100)
	if err != nil || !slices.Equal(all, ids) {
		t.Fatalf("ParseBrowse(max 100) = %v, %v; want all 30 (no header id, no continuation)", all, err)
	}
	if slices.Contains(all, ittest.HeaderVideoID) {
		t.Fatal("the header's video was taken")
	}
}

// TestParseBrowse_ShortsTab: the Shorts tab's items name their video in
// reelWatchEndpoint.
func TestParseBrowse_ShortsTab(t *testing.T) {
	ids := ittest.IDs("sho", 3)
	body := ittest.TabAnswer(testChannel, innertube.TabShorts, ids...)
	got, err := innertube.ParseBrowse(body, jsonType, innertube.TabShorts.Params, maxIDs)
	if err != nil || !slices.Equal(got, ids) {
		t.Fatalf("ParseBrowse = %v, %v; want %v", got, err, ids)
	}
	// The Shorts answer is not read as the Videos tab.
	if got, err := innertube.ParseBrowse(body, jsonType, innertube.TabVideos.Params, maxIDs); !errors.Is(err, innertube.ErrNoTab) || got != nil {
		t.Fatalf("Videos tab of a Shorts answer = %v, %v; want ErrNoTab", got, err)
	}
}

// TestParseBrowse_LiveTab: the Live tab's grid lists its streams as the
// Videos tab lists its videos, and is read only when asked for.
func TestParseBrowse_LiveTab(t *testing.T) {
	ids := ittest.IDs("liv", 3)
	body := ittest.TabAnswer(testChannel, innertube.TabLive, ids...)
	got, err := innertube.ParseBrowse(body, jsonType, innertube.TabLive.Params, maxIDs)
	if err != nil || !slices.Equal(got, ids) {
		t.Fatalf("ParseBrowse = %v, %v; want %v", got, err, ids)
	}
	if got, err := innertube.ParseBrowse(body, jsonType, innertube.TabVideos.Params, maxIDs); !errors.Is(err, innertube.ErrNoTab) || got != nil {
		t.Fatalf("Videos tab of a Live answer = %v, %v; want ErrNoTab", got, err)
	}
}

// TestParseBrowse_HomeTab: a channel without the tab asked for gets its
// Home tab, whose shelves may list other channels' videos: no IDs at all.
func TestParseBrowse_HomeTab(t *testing.T) {
	body := ittest.HomeAnswer(testChannel, ittest.IDs("foreign", 20)...)
	for _, tab := range []innertube.Tab{innertube.TabVideos, innertube.TabShorts, innertube.TabLive} {
		got, err := innertube.ParseBrowse(body, jsonType, tab.Params, maxIDs)
		if !errors.Is(err, innertube.ErrNoTab) || got != nil {
			t.Errorf("%s: ParseBrowse = %v, %v; want ErrNoTab", tab.Name, got, err)
		}
	}
}

// TestParseBrowse_Failures: an alert, an error object, a web page, a body
// over the cap and anything that is not a JSON browse answer are errors
// with no IDs.
func TestParseBrowse_Failures(t *testing.T) {
	videos := innertube.TabVideos.Params
	selectedNoParams := `{"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[{"tabRenderer":{"selected":true,"content":{"videoId":"aaaaaaaaaaa"}}}]}}}`
	notSelected := strings.Replace(string(ittest.TabAnswer(testChannel, innertube.TabVideos, "aaaaaaaaaaa")), `"selected":true`, `"selected":false`, 1)
	alertWithTab := strings.Replace(string(ittest.TabAnswer(testChannel, innertube.TabVideos, "aaaaaaaaaaa")), `{"responseContext"`,
		`{"alerts":[{"alertWithButtonRenderer":{"type":"ERROR"}}],"responseContext"`, 1)
	tests := []struct {
		name, contentType string
		body              []byte
		want              error
	}{
		{"alert", jsonType, ittest.AlertAnswer(), innertube.ErrAlert},
		{"alert next to a tab", jsonType, []byte(alertWithTab), innertube.ErrAlert},
		{"error object", jsonType, ittest.ErrorAnswer(), innertube.ErrAlert},
		{"web page", "text/html; charset=utf-8", []byte("<html>consent</html>"), innertube.ErrBlocked},
		{"other type", "text/plain", []byte("{}"), innertube.ErrFormat},
		{"invalid JSON", jsonType, []byte(`{"contents":`), innertube.ErrFormat},
		{"over the cap", jsonType, []byte(`"` + strings.Repeat("a", innertube.MaxBrowseBytes) + `"`), innertube.ErrTooLarge},
		{"no params", jsonType, []byte(selectedNoParams), innertube.ErrNoTab},
		{"tab not selected", jsonType, []byte(notSelected), innertube.ErrNoTab},
		{"empty object", jsonType, []byte(`{}`), innertube.ErrNoTab},
		{"not an object", jsonType, []byte(`[1,2]`), innertube.ErrNoTab},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := innertube.ParseBrowse(tt.body, tt.contentType, videos, maxIDs)
			if !errors.Is(err, tt.want) || got != nil {
				t.Fatalf("ParseBrowse = %v, %v; want %v", got, err, tt.want)
			}
			if strings.Contains(err.Error(), "exist") || strings.Contains(err.Error(), "argument") {
				t.Errorf("message %q quotes the answer", err)
			}
		})
	}
}

// TestParseBrowse_IDsAreChecked: only 11-character base64url IDs written
// plainly are taken, each once; an escaped key still counts.
func TestParseBrowse_IDsAreChecked(t *testing.T) {
	// %U stands for a JSON \u escape: an escaped ID is not taken, an
	// escaped key is.
	content := strings.ReplaceAll(`{"a":[{"videoId":"short"},{"videoId":"aaaaaaaaaaaa"},{"videoId":"aaaa aaaaaa"},{"videoId":"aaaaaaaaa%U0061a"},`+
		`{"videoId":5},{"video%U0049d":"bbbbbbbbbbb"},{"videoId":"ccccccccccc"},{"videoId":"bbbbbbbbbbb"},{"x":"videoId"},`+
		`{"videoIds":"ddddddddddd"},{"videoId":"<script>xx"}]}`, "%U", string(rune(backslash))+"u")
	body := `{"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[{"tabRenderer":{"endpoint":{"browseEndpoint":{"params":` +
		`"` + innertube.TabVideos.Params + `"}},"selected":true,"content":` + content + `}}]}}}`
	got, err := innertube.ParseBrowse([]byte(body), jsonType, innertube.TabVideos.Params, maxIDs)
	if want := []string{"bbbbbbbbbbb", "ccccccccccc"}; err != nil || !slices.Equal(got, want) {
		t.Fatalf("ParseBrowse = %v, %v; want %v", got, err, want)
	}
}

// TestBrowseRequest: a POST of the channel and the tab's params as the web
// client, to the fixed endpoint, with the browse cap.
func TestBrowseRequest(t *testing.T) {
	r := innertube.BrowseRequest(innertube.Web, testChannel, innertube.TabShorts)
	want := `{"context":{"client":{"clientName":"WEB","clientVersion":"` + innertube.WebVersion + `","hl":"en"}},` +
		`"browseId":"` + testChannel + `","params":"` + innertube.TabShorts.Params + `"}`
	if r.Method != "POST" || r.URL != innertube.BrowseURL || r.Body != want || r.MaxBytes != innertube.MaxBrowseBytes ||
		r.ContentType != innertube.JSONType || r.Accept != innertube.JSONType {
		t.Fatalf("request = %+v", r)
	}
	if !strings.HasPrefix(innertube.BrowseURL, "https://www.youtube.com/youtubei/v1/") ||
		!strings.HasPrefix(innertube.PlayerURL, "https://www.youtube.com/youtubei/v1/") {
		t.Fatal("endpoints left www.youtube.com")
	}
}

// tabDoc is a browse answer of one tab of params, selected as written,
// with content.
func tabDoc(selected, params, content string) string {
	return `{"contents":{"twoColumnBrowseResultsRenderer":{"tabs":[{"tabRenderer":{"endpoint":{"browseEndpoint":{"params":"` +
		params + `"}},"title":"Videos","selected":` + selected + `,"content":` + content + `}}]}}}`
}

// TestParseBrowse_HostileShapes: duplicate members are read as
// encoding/json reads them (the last wins); escapes, invalid JSON and
// values of other types are handled as documented. %U stands for a JSON
// \u escape, %Q for an escaped quote.
func TestParseBrowse_HostileShapes(t *testing.T) {
	p := innertube.TabVideos.Params
	esc := strings.NewReplacer("%U", string(rune(backslash))+"u", "%Q", string(rune(backslash))+`"`).Replace
	const bom = "\xef\xbb\xbf"
	for _, tt := range []struct {
		name string
		body string
		want []string // nil: no IDs
		err  error
	}{
		{"escaped videoId key", tabDoc("true", p, esc(`[{"video%U0049d":"aaaaaaaaaaa"}]`)), []string{"aaaaaaaaaaa"}, nil},
		{"escaped id value", tabDoc("true", p, esc(`[{"videoId":"aaaaaaaaa%U0061a"}]`)), nil, nil},
		{"surrogate in key", tabDoc("true", p, esc(`[{"%Ud83d%Ude00videoId":"aaaaaaaaaaa"}]`)), nil, nil},
		{"selected true then false", tabDoc("true,\"selected\":false", p, `[{"videoId":"aaaaaaaaaaa"}]`), nil, innertube.ErrNoTab},
		{"selected false then true", tabDoc("false,\"selected\":true", p, `[{"videoId":"aaaaaaaaaaa"}]`), []string{"aaaaaaaaaaa"}, nil},
		{"params twice, last other", strings.Replace(tabDoc("true", p, `[{"videoId":"aaaaaaaaaaa"}]`), `"params":"`+p+`"`, `"params":"`+p+`","params":"x"`, 1), nil, innertube.ErrNoTab},
		{"content twice", strings.Replace(tabDoc("true", p, `[{"videoId":"aaaaaaaaaaa"}]`), `"content":`, `"content":[{"videoId":"bbbbbbbbbbb"}],"content":`, 1), []string{"aaaaaaaaaaa"}, nil},
		{"trailing data", tabDoc("true", p, `[]`) + `{"x":1}`, nil, innertube.ErrFormat},
		{"BOM", bom + tabDoc("true", p, `[]`), nil, innertube.ErrFormat},
		{"NaN", tabDoc("true", p, `[NaN]`), nil, innertube.ErrFormat},
		{"selected as a string", tabDoc(`"true"`, p, `[{"videoId":"aaaaaaaaaaa"}]`), nil, innertube.ErrNoTab},
		{"videoId in the header only", `{"header":{"videoId":"aaaaaaaaaaa"},` + tabDoc("true", p, `[]`)[1:], nil, nil},
		{"a value named videoId", tabDoc("true", p, `[{"x":"videoId","videoId":"aaaaaaaaaaa"}]`), []string{"aaaaaaaaaaa"}, nil},
		{"escaped quote before a key", tabDoc("true", p, esc(`[{"a%Q":"videoId","videoId":"aaaaaaaaaaa"}]`)), []string{"aaaaaaaaaaa"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ids, err := innertube.ParseBrowse([]byte(tt.body), jsonType, p, maxIDs)
			if (tt.err == nil) != (err == nil) || (tt.err != nil && !errors.Is(err, tt.err)) {
				t.Fatalf("err = %v, want %v", err, tt.err)
			}
			if !slices.Equal(ids, tt.want) {
				t.Errorf("ids = %v, want %v", ids, tt.want)
			}
		})
	}
}
