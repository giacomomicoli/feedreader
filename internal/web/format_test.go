package web

import (
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/store"
)

func TestRelTimeIncludingFutureDates(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	cases := map[time.Duration]string{
		0:                     "just now",
		-30 * time.Second:     "just now",
		-time.Minute:          "1 minute ago",
		-59 * time.Minute:     "59 minutes ago",
		-3 * time.Hour:        "3 hours ago",
		-24 * time.Hour:       "1 day ago",
		-29 * 24 * time.Hour:  "29 days ago",
		-61 * 24 * time.Hour:  "2 months ago",
		-359 * 24 * time.Hour: "11 months ago",
		-360 * 24 * time.Hour: "1 year ago", // not "12 months ago"
		-364 * 24 * time.Hour: "1 year ago",
		-365 * 24 * time.Hour: "1 year ago",
		-800 * 24 * time.Hour: "2 years ago",
		2 * time.Hour:         "in 2 hours",
		3 * 24 * time.Hour:    "in 3 days",
		362 * 24 * time.Hour:  "in 1 year",
	}
	for d, want := range cases {
		if got := relTime(now.Add(d), now); got != want {
			t.Errorf("relTime(%v) = %q, want %q", d, got, want)
		}
	}
	// Beyond ~292 years time.Duration saturates; a 9999-12-31 placeholder
	// date must not come out as "just now".
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), "in 7973 years"},
		{time.Date(2400, 1, 1, 0, 0, 0, 0, time.UTC), "in 374 years"},
		{time.Date(1600, 1, 1, 0, 0, 0, 0, time.UTC), "426 years ago"},
		{time.Date(2300, 1, 1, 0, 0, 0, 0, time.UTC), "in 273 years"}, // not saturated
	} {
		if got := relTime(c.t, now); got != c.want {
			t.Errorf("relTime(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}

func TestPlaceholderInitialsAndColourFromFeedIDHash(t *testing.T) {
	for in, want := range map[string]string{
		"The Go Blog": "TG", "golang": "G", "": "?", "  -- ": "?",
		"élan vital": "ÉV", "123 Tech": "1T", "日本 ブログ": "日ブ",
		// Combining marks belong to their word and stay on its initial.
		"E\u0301cole Polytechnique": "E\u0301P", // NFD
		"हिन्दी समाचार":             "हिस",
		"தமிழ் செய்திகள்":           "தசெ",
		"«\u0301hello» world":       "HW",
	} {
		if got := initials(in); got != want {
			t.Errorf("initials(%q) = %q, want %q", in, got, want)
		}
	}
	seen := map[string]bool{}
	for id := int64(1); id <= 200; id++ {
		c := paletteClass(id)
		if c != paletteClass(id) {
			t.Fatalf("palette for %d not stable", id)
		}
		if !strings.HasPrefix(c, "ph-") || len(c) != 4 || c[3] < '0' || c[3] >= '0'+paletteSize {
			t.Fatalf("palette class %q out of range", c)
		}
		seen[c] = true
	}
	if len(seen) != paletteSize {
		t.Errorf("only %d of %d palette colours used", len(seen), paletteSize)
	}
}

func TestHTTPURLRejectsNonHTTPSchemes(t *testing.T) {
	ok := []string{"https://example.com/a?b=c", "http://example.com", "HTTPS://EXAMPLE.COM/x"}
	bad := []string{"javascript:alert(1)", "JaVaScRiPt:alert(1)", "data:text/html,x", "/relative", "//example.com/x",
		"vbscript:x", "https://", "", "  ", "ftp://example.com/f", "https://exa mple.com/%zz"}
	for _, u := range ok {
		if httpURL(u) == "" {
			t.Errorf("httpURL(%q) rejected", u)
		}
	}
	for _, u := range bad {
		if got := httpURL(u); got != "" {
			t.Errorf("httpURL(%q) = %q, want rejection", u, got)
		}
	}
}

func TestFirstImageFromSummary(t *testing.T) {
	cases := map[string]string{
		`<p>hi</p>`: "",
		`<img src="data:image/png;base64,AAAA"><img src="https://a.example/2.png">`:           "https://a.example/2.png",
		`<p><IMG SRC="https://a.example/1.jpg" alt=x></p><img src="https://a.example/2.jpg">`: "https://a.example/1.jpg",
		`<img alt="no src"><img src="/relative.png">`:                                         "",
		`<img src="https://a.example/x.png?a=1&amp;b=2">`:                                     "https://a.example/x.png?a=1&b=2",
		`<img src="https://a.example/unterminated`:                                            "",
	}
	for in, want := range cases {
		if got := firstImage(in); got != want {
			t.Errorf("firstImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseIntervalMinutesOrDuration(t *testing.T) {
	good := map[string]time.Duration{"": 0, " 90 ": 90 * time.Minute, "5": 5 * time.Minute,
		"2h": 2 * time.Hour, "1h30m": 90 * time.Minute, "24h": 24 * time.Hour}
	for in, want := range good {
		got, err := parseInterval(in)
		if err != nil || got != want {
			t.Errorf("parseInterval(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"4", "4m59s", "0", "-10", "abc", "10 minutes", "9223372036854775807", "1e3"} {
		if _, err := parseInterval(in); err == nil {
			t.Errorf("parseInterval(%q) accepted", in)
		}
	}
}

func TestFormatInterval(t *testing.T) {
	for d, want := range map[time.Duration]string{
		6 * time.Hour: "6h", 90 * time.Minute: "1h30m", 5 * time.Minute: "5m", 45 * time.Second: "45s",
	} {
		if got := formatInterval(d); got != want {
			t.Errorf("formatInterval(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"  a   b ": "a b", "tab\there": "tab here", "nul\x00byte": "nulbyte", "​": "​", "": "",
	} {
		if got := cleanName(in); got != want {
			t.Errorf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseScopeDefaultsAndValidation(t *testing.T) {
	sc, filter, err := parseScope(url.Values{})
	if err != nil || sc.Kind != store.ScopeAll || filter != filterUnread {
		t.Fatalf("defaults = %+v %q %v", sc, filter, err)
	}
	sc, _, err = parseScope(url.Values{"scope": {"watchlater"}, "id": {"garbage"}})
	if err != nil || sc.ID != 0 {
		t.Errorf("id must be ignored for watchlater: %+v %v", sc, err)
	}
	sc, filter, err = parseScope(url.Values{"scope": {"feed"}, "id": {"12"}, "filter": {"all"}})
	if err != nil || sc != (store.Scope{Kind: store.ScopeFeed, ID: 12}) || filter != filterAll {
		t.Errorf("feed scope = %+v %q %v", sc, filter, err)
	}
	for _, v := range []url.Values{
		{"scope": {"FEED"}, "id": {"1"}}, {"scope": {"tag"}}, {"scope": {"folder"}, "id": {"0"}},
		{"filter": {"read"}}, {"scope": {"feed"}, "id": {"99999999999999999999"}},
	} {
		if _, _, err := parseScope(v); err == nil {
			t.Errorf("parseScope(%v) accepted", v)
		}
	}
	if got := scopeHref(store.Scope{Kind: store.ScopeFeed, ID: 7}, filterAll); got != "/?scope=feed&id=7&filter=all" {
		t.Errorf("scopeHref = %q", got)
	}
	if got := scopeHref(store.Scope{Kind: store.ScopeFavourites, ID: 7}, ""); got != "/?scope=favourites" {
		t.Errorf("scopeHref = %q", got)
	}
}

func TestCursorRoundTrip(t *testing.T) {
	for _, c := range []store.Cursor{
		{PublishedAt: time.Unix(1700000000, 0).UTC(), ID: 1},
		{PublishedAt: time.Unix(-86400, 123456789).UTC(), ID: 42},
		{PublishedAt: time.Date(9999, 12, 31, 0, 0, 0, 0, time.UTC), ID: 7},
	} {
		got, err := decodeCursor(encodeCursor(c))
		if err != nil || got == nil || !got.PublishedAt.Equal(c.PublishedAt) || got.ID != c.ID {
			t.Errorf("round trip %+v = %+v, %v", c, got, err)
		}
	}
	if c, err := decodeCursor(""); c != nil || err != nil {
		t.Errorf("empty cursor = %+v, %v", c, err)
	}
	for _, bad := range []string{"1", "1.2", "a.b.c", "1.2.0", "1.1000000000.3", "1.-1.3", "1.2.3.4"} {
		if _, err := decodeCursor(bad); err == nil {
			t.Errorf("decodeCursor(%q) accepted", bad)
		}
	}
}

func TestSafeBackOnlyAllowsLocalPaths(t *testing.T) {
	for in, want := range map[string]string{
		"/?scope=feed&id=1": "/?scope=feed&id=1",
		"/feeds/3":          "/feeds/3",
		"":                  "/",
		"//evil.example":    "/",
		"/\\evil.example":   "/",
		"https://evil":      "/",
		"evil":              "/",
		"/ok\r\nX: y":       "/",
	} {
		if got := safeBack(in, "/"); got != want {
			t.Errorf("safeBack(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUnsubscribeConfirmText(t *testing.T) {
	v := unsubscribeConfirm(store.Feed{ID: 3, Title: "Chan", Kind: store.KindYouTube}, store.FeedEntryStats{Total: 15, Saved: 1})
	text := strings.Join(v.Lines, " ")
	for _, want := range []string{"all 15 entries", "1 entry saved in Watch later or Favourites", "deleted too", "cannot be undone"} {
		if !strings.Contains(text, want) {
			t.Errorf("confirm text %q lacks %q", text, want)
		}
	}
	if v.Action != "/feeds/3/unsubscribe" || v.CancelHref != "/feeds/3" {
		t.Errorf("confirm = %+v", v)
	}
}
