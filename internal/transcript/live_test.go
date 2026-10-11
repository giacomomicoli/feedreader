package transcript

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
)

// liveVideoEnv names a video to fetch the transcript of from YouTube
// itself; the live test is skipped without it. It makes two requests.
const liveVideoEnv = "TRANSCRIPT_LIVE_VIDEO"

// livePause spaces the live test's requests, as callers space theirs:
// YouTube rate-limits clients that ask quickly.
const livePause = 4 * time.Second

// pacedFetcher waits livePause between requests.
type pacedFetcher struct {
	f    Fetcher
	mu   sync.Mutex
	last time.Time
}

func (p *pacedFetcher) Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if wait := time.Until(p.last.Add(livePause)); !p.last.IsZero() && wait > 0 {
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	defer func() { p.last = time.Now() }()
	return p.f.Fetch(ctx, r)
}

// TestLive fetches a real transcript through the real fetch client, to
// check the undocumented API still answers as this package expects:
//
//	TRANSCRIPT_LIVE_VIDEO=<video ID> go test -run TestLive -v ./internal/transcript
func TestLive(t *testing.T) {
	id := os.Getenv(liveVideoEnv)
	if id == "" {
		t.Skip("set " + liveVideoEnv + " to a video ID to fetch its transcript from YouTube")
	}
	tr, err := New(&pacedFetcher{f: fetch.New(config.Default())}).Fetch(t.Context(), id)
	if err != nil {
		t.Fatalf("Fetch(%s): %v", id, err)
	}
	words := strings.Fields(tr.Text)
	t.Logf("lang %s, generated %v, %d runes, %d paragraphs, starting %q",
		tr.Lang, tr.Generated, utf8.RuneCountInString(tr.Text), strings.Count(tr.Text, paragraphBreak)+1,
		strings.Join(words[:min(len(words), liveSampleWords)], " "))
}

// liveSampleWords is how many words of the text the live test shows.
const liveSampleWords = 5
