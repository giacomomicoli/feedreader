package web

import (
	"hash/fnv"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/giacomomicoli/feedreader/internal/store"
)

// paletteSize is the number of placeholder colour classes (.ph-0 … .ph-7)
// defined in web/static/app.css.
const paletteSize = 8

// maxNameLen bounds user-entered names (folders, tags, feed titles), in runes.
const maxNameLen = 200

// maxURLLen bounds URLs typed into the add-source form.
const maxURLLen = 2048

// paletteClass picks a placeholder colour class from a hash of the feed id,
// so a feed keeps the same colour.
func paletteClass(feedID int64) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(strconv.FormatInt(feedID, 10)))
	return "ph-" + strconv.Itoa(int(h.Sum32()%paletteSize))
}

// initials returns the upper-cased initials of a title's first two words,
// or "?". Combining marks belong to their word, and an initial keeps the
// marks that follow its first letter, so decomposed accents (NFD "É") and
// Indic vowel signs ("हि") stay intact instead of splitting words.
func initials(title string) string {
	words := strings.FieldsFunc(title, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && !unicode.IsMark(r)
	})
	var b strings.Builder
	n := 0
	for _, w := range words {
		w = strings.TrimLeftFunc(w, unicode.IsMark) // stray marks after punctuation
		if w == "" {
			continue
		}
		r, size := utf8.DecodeRuneInString(w)
		b.WriteRune(unicode.ToUpper(r))
		for _, m := range w[size:] {
			if !unicode.IsMark(m) {
				break
			}
			b.WriteRune(m)
		}
		if n++; n == 2 {
			break
		}
	}
	if n == 0 {
		return "?"
	}
	return b.String()
}

// httpURL returns raw when it is an absolute http(s) URL with a host, and ""
// otherwise. It is a defence in depth on top of html/template's own URL
// filtering: feed-controlled links never become javascript:, data: or
// relative URLs.
func httpURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return raw
	}
	return ""
}

// redactURL hides the password of a URL (https://user:xxxxx@host/…) for
// logs and error messages; feed URLs may embed credentials.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(invalid URL)"
	}
	return u.Redacted()
}

// firstImage returns the src of the first <img> with an absolute http(s)
// URL in sanitized summary HTML, or "" (article thumbnails use the entry's
// first image).
func firstImage(summary string) string {
	if !strings.Contains(summary, "<img") && !strings.Contains(summary, "<IMG") {
		return ""
	}
	z := html.NewTokenizer(strings.NewReader(summary))
	for {
		switch z.Next() {
		case html.ErrorToken:
			return ""
		case html.StartTagToken, html.SelfClosingTagToken:
			name, hasAttr := z.TagName()
			if string(name) != "img" || !hasAttr {
				continue
			}
			for {
				key, val, more := z.TagAttr()
				if string(key) == "src" {
					if u := httpURL(string(val)); u != "" {
						return u
					}
				}
				if !more {
					break
				}
			}
		}
	}
}

// relTime renders t relative to now: "just now", "5 minutes ago",
// "in 3 days" for future dates (shown as such, not clamped to now).
func relTime(t, now time.Time) string {
	future := t.After(now)
	var s string
	if d := now.Sub(t); d == math.MinInt64 || d == math.MaxInt64 {
		// Over ~292 years apart (a 9999-12-31 placeholder date): Sub
		// saturated and -d would overflow, so count calendar years.
		s = plural(max(t.Year()-now.Year(), now.Year()-t.Year()), "year")
	} else {
		if future {
			d = -d
		}
		if d < time.Minute {
			return "just now"
		}
		s = span(d)
	}
	if future {
		return "in " + s
	}
	return s + " ago"
}

// span renders a duration of at least a minute in its largest whole unit.
func span(d time.Duration) string {
	const (
		day   = 24 * time.Hour
		month = 30 * day
		year  = 365 * day
	)
	switch {
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < day:
		return plural(int(d/time.Hour), "hour")
	case d < month:
		return plural(int(d/day), "day")
	case d < year && d/month < 12:
		return plural(int(d/month), "month")
	case d < year:
		return plural(1, "year") // 360–364 days, not "12 months"
	default:
		return plural(int(d/year), "year")
	}
}

func plural(n int, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return strconv.Itoa(n) + " " + unit + "s"
}

// absTime is the full timestamp shown in tooltips and on the settings page.
func absTime(t time.Time) string {
	if t.IsZero() {
		return "Never"
	}
	return t.Local().Format("Mon 2 Jan 2006, 15:04 MST")
}

// formatInterval renders a poll interval compactly: 90m, 6h, 1h30m.
func formatInterval(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// cleanName normalizes a user-entered name: control characters dropped,
// whitespace runs collapsed, surrounding space trimmed.
func cleanName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && !unicode.IsSpace(r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// tooLong reports whether a cleaned name exceeds maxNameLen runes.
func tooLong(s string) bool { return utf8.RuneCountInString(s) > maxNameLen }

// Kind-dependent labels (Watched/Unwatched for youtube, Read/Unread for rss;
// Watch later vs Read later share the is_later flag).

func statusLabel(k store.Kind, read bool) string {
	switch {
	case k == store.KindYouTube && read:
		return "Watched"
	case k == store.KindYouTube:
		return "Unwatched"
	case read:
		return "Read"
	default:
		return "Unread"
	}
}

func readActionLabel(k store.Kind, read bool) string {
	switch {
	case k == store.KindYouTube && read:
		return "Mark unwatched"
	case k == store.KindYouTube:
		return "Mark watched"
	case read:
		return "Mark unread"
	default:
		return "Mark read"
	}
}

func laterLabel(k store.Kind) string {
	if k == store.KindYouTube {
		return "Watch later"
	}
	return "Read later"
}

// openLabel names the link to an entry's original.
func openLabel(k store.Kind) string {
	if k == store.KindYouTube {
		return "Open video"
	}
	return "Open article"
}

func kindLabel(k store.Kind) string {
	if k == store.KindYouTube {
		return "YouTube"
	}
	return "Website feed"
}
