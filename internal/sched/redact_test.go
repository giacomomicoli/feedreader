package sched

import (
	"log/slog"
	"strings"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/parse"
)

// TestPoll_LogsNeverShowURLCredentials: a feed URL's user name can be a
// token on its own.
func TestPoll_LogsNeverShowURLCredentials(t *testing.T) {
	for _, u := range []string{"https://tok3n@site.example/feed.xml", "https://reader:s3cret@site.example/feed.xml"} {
		st := openStore(t)
		f := addFeed(t, st, u, t0)
		buf := &syncBuffer{}
		s := New(st, staticFetcher(nil, &fetch.StatusError{URL: "https://site.example/feed.xml", StatusCode: 503}),
			config.Default(), slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
		s.now = newClock(t0).Now
		pollOnce(t, s, st, f.ID)
		out := buf.String()
		if !strings.Contains(out, "feed fetch failed") {
			t.Fatalf("no failure logged:\n%s", out)
		}
		for _, secret := range []string{"tok3n", "reader", "s3cret"} {
			if strings.Contains(out, secret) {
				t.Errorf("%s: log shows %q:\n%s", u, secret, out)
			}
		}
	}
}

// TestSubscriptionTitleNeverShowsURLCredentials: the title of a feed
// without one falls back to its URL, and titles are shown and logged.
func TestSubscriptionTitleNeverShowsURLCredentials(t *testing.T) {
	for _, u := range []string{"http://tok3n@:8080/feed", "https://reader:s3cret@[fe80::1%25é]/feed", "https://tok3n@[::1"} {
		title := subscriptionTitle("", &fetched{feedURL: u, doc: &parse.Feed{}})
		for _, secret := range []string{"tok3n", "reader", "s3cret"} {
			if strings.Contains(title, secret) {
				t.Errorf("%s: title %q shows %q", u, title, secret)
			}
		}
	}
}
