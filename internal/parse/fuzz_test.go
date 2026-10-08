package parse

import (
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unicode/utf8"
)

// addFixtureSeeds adds every testdata file to the fuzz corpus.
func addFixtureSeeds(f *testing.F, add func(b []byte)) {
	f.Helper()
	files, err := filepath.Glob(filepath.Join("testdata", "*"))
	if err != nil {
		f.Fatal(err)
	}
	for _, file := range files {
		if info, err := os.Stat(file); err != nil || info.IsDir() {
			continue // e.g. testdata/fuzz, the fuzzing corpus
		}
		b, err := os.ReadFile(file)
		if err != nil {
			f.Fatal(err)
		}
		add(b)
	}
}

// FuzzParse checks that Parse never panics and that every successful result
// honors the Entry contract.
func FuzzParse(f *testing.F) {
	addFixtureSeeds(f, func(b []byte) { f.Add(b, "application/xml") })
	f.Add([]byte(`<rss><channel><item><title>&</title></item></channel></rss>`), "text/xml; charset=windows-1252")
	f.Add([]byte(`{"version":"https://jsonfeed.org/version/1","items":[{"id":1}]}`), "application/feed+json")
	f.Add([]byte(`<rss><channel><link>//::</link></channel></rss>`), "")
	f.Fuzz(func(t *testing.T, body []byte, contentType string) {
		feed, err := Parse(body, contentType, "https://fuzz.example.com/feed")
		if err != nil {
			return
		}
		checkHTTPURL(t, "SiteURL", feed.SiteURL)
		checkHTTPURL(t, "IconURL", feed.IconURL)
		checkRunes(t, "feed Title", feed.Title)
		if feed.TTL < 0 {
			t.Errorf("negative TTL %v", feed.TTL)
		}
		seen := map[string]bool{}
		for _, e := range feed.Entries {
			if e.GUID == "" || e.Title == "" {
				t.Errorf("empty GUID or Title: %+v", e)
			}
			if seen[e.GUID] {
				t.Errorf("duplicate GUID %q", e.GUID)
			}
			seen[e.GUID] = true
			checkHTTPURL(t, "URL", e.URL)
			checkHTTPURL(t, "ThumbnailURL", e.ThumbnailURL)
			checkRunes(t, "Title", e.Title)
			checkRunes(t, "Author", e.Author)
			checkUTC(t, e.PublishedAt)
			checkUTC(t, e.UpdatedAt)
			assertSafeHTML(t, e.SummaryHTML)
			if len(e.SummaryHTML) > renderLimit(maxFieldBytes) {
				t.Errorf("SummaryHTML is %d bytes", len(e.SummaryHTML))
			}
		}
	})
}

// FuzzSanitize checks the allow-list holds for arbitrary input.
func FuzzSanitize(f *testing.F) {
	for _, s := range []string{
		`<a href="/x" onclick="y">x</a><img src=y onerror=z>`,
		`<svg><style><img src=x onerror=alert(1)>`,
		`<table><td><plaintext><b>`,
	} {
		f.Add(s)
	}
	addFixtureSeeds(f, func(b []byte) { f.Add(string(b)) })
	f.Fuzz(func(t *testing.T, in string) {
		out := Sanitize(in, "https://fuzz.example.com/a/b")
		assertSafeHTML(t, out)
		if len(out) > renderLimit(maxFieldBytes) {
			t.Errorf("output is %d bytes", len(out))
		}
	})
}

// FuzzExcerpt checks Excerpt's length and encoding guarantees.
func FuzzExcerpt(f *testing.F) {
	f.Add("<p>héllo <b>wörld</b></p>", 5)
	f.Add("\xff\xfe broken", 3)
	f.Fuzz(func(t *testing.T, in string, n int) {
		if n > 1<<16 {
			n = 1 << 16
		}
		got := Excerpt(in, n)
		if n <= 0 && got != "" {
			t.Errorf("Excerpt(_, %d) = %q, want empty", n, got)
		}
		if c := utf8.RuneCountInString(got); n > 0 && c > n+1 {
			t.Errorf("Excerpt(_, %d) has %d runes", n, c)
		}
	})
}

// checkHTTPURL requires "" or an absolute http(s) URL of at most
// maxURLBytes bytes.
func checkHTTPURL(t *testing.T, field, s string) {
	t.Helper()
	if s == "" {
		return
	}
	u, err := url.Parse(s)
	if err != nil || !isHTTP(u) || len(s) > maxURLBytes {
		t.Errorf("%s = %.80q, want an absolute http(s) URL of at most %d bytes", field, s, maxURLBytes)
	}
}

// checkRunes requires at most maxTitleRunes runes.
func checkRunes(t *testing.T, field, s string) {
	t.Helper()
	if n := utf8.RuneCountInString(s); n > maxTitleRunes {
		t.Errorf("%s has %d runes", field, n)
	}
}

// checkUTC requires zero or UTC times.
func checkUTC(t *testing.T, v time.Time) {
	t.Helper()
	if !v.IsZero() && v.Location() != time.UTC {
		t.Errorf("time %v is not UTC", v)
	}
}
