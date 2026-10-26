package sched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/innertube"
	"github.com/giacomomicoli/feedreader/internal/innertube/ittest"
	"github.com/giacomomicoli/feedreader/internal/parse"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// fakeYouTube serves a channel (chanID) whose feeds fail, through the web
// API the fallback reads: a tab lists its IDs (a tab without IDs is
// absent: YouTube answers with the Home tab), and each video's player
// answer gives its details. browse and player, when set, answer instead.
type fakeYouTube struct {
	mu      sync.Mutex
	feedErr error // the answer of every feed URL
	feed    *fetch.Result
	tabs    map[string][]string // tab params → IDs, newest first
	videos  map[string]ittest.Video
	browse  func(params string) (*fetch.Result, error)
	player  func(id string) (*fetch.Result, error)
}

// newFakeYouTube serves videos and shorts (IDs, newest first, an hour
// apart, the Shorts half an hour after each video) and a feed that
// answers 404.
func newFakeYouTube(videos, shorts []string) *fakeYouTube {
	y := &fakeYouTube{
		feedErr: statusErr(chanFeedURL, http.StatusNotFound),
		tabs:    map[string][]string{innertube.TabVideos.Params: videos, innertube.TabShorts.Params: shorts},
		videos:  map[string]ittest.Video{},
	}
	for i, id := range videos {
		y.videos[id] = testVideoOf(id, t0.Add(-time.Duration(i)*time.Hour))
	}
	for i, id := range shorts {
		y.videos[id] = testVideoOf(id, t0.Add(-time.Duration(i)*time.Hour-30*time.Minute))
	}
	return y
}

func testVideoOf(id string, published time.Time) ittest.Video {
	return ittest.Video{ID: id, ChannelID: chanID, Title: "Video " + id, Description: "About " + id + "\nline two",
		Author: "Test Channel", Published: published}
}

func jsonAnswer(r fetch.Request, body []byte) *fetch.Result {
	return &fetch.Result{StatusCode: 200, Body: body, ContentType: "application/json; charset=UTF-8", FinalURL: r.URL}
}

func (y *fakeYouTube) respond(_ context.Context, r fetch.Request) (*fetch.Result, error) {
	y.mu.Lock()
	defer y.mu.Unlock()
	var body struct {
		BrowseID string `json:"browseId"`
		Params   string `json:"params"`
		VideoID  string `json:"videoId"`
		Context  struct {
			Client struct {
				Name string `json:"clientName"`
			} `json:"client"`
		} `json:"context"`
	}
	switch r.URL {
	case innertube.BrowseURL, innertube.PlayerURL:
		if r.Method != http.MethodPost || json.Unmarshal([]byte(r.Body), &body) != nil || body.Context.Client.Name != innertube.WebName {
			return nil, fmt.Errorf("unexpected request %+v", r)
		}
	case chanPageURL:
		return namedChanPage(), nil
	default:
		var se *fetch.StatusError
		if errors.As(y.feedErr, &se) { // from the feed asked for
			return nil, &fetch.StatusError{URL: r.URL, StatusCode: se.StatusCode, RetryAfter: se.RetryAfter}
		}
		return y.feed, y.feedErr
	}
	if r.URL == innertube.BrowseURL {
		if body.BrowseID != chanID {
			return jsonAnswer(r, ittest.AlertAnswer()), nil
		}
		if y.browse != nil {
			return y.browse(body.Params)
		}
		if ids := y.tabs[body.Params]; len(ids) > 0 {
			tab := innertube.TabVideos
			for _, t := range []innertube.Tab{innertube.TabShorts, innertube.TabLive} {
				if body.Params == t.Params {
					tab = t
				}
			}
			return jsonAnswer(r, ittest.TabAnswer(chanID, tab, ids...)), nil
		}
		return jsonAnswer(r, ittest.HomeAnswer(chanID, ittest.IDs("foreign", 10)...)), nil
	}
	if y.player != nil {
		return y.player(body.VideoID)
	}
	if v, ok := y.videos[body.VideoID]; ok {
		return jsonAnswer(r, ittest.PlayerAnswer(v)), nil
	}
	return jsonAnswer(r, ittest.MissingPlayerAnswer()), nil
}

// innerTubeCalls returns the fallback's requests among calls: the tab
// params browsed and the videos asked about, in order.
func innerTubeCalls(t *testing.T, calls []fetch.Request) (browsed, players []string) {
	t.Helper()
	for _, c := range calls {
		var body struct {
			Params  string `json:"params"`
			VideoID string `json:"videoId"`
		}
		switch c.URL {
		case innertube.BrowseURL:
			mustNoErr(t, json.Unmarshal([]byte(c.Body), &body))
			browsed = append(browsed, body.Params)
		case innertube.PlayerURL:
			mustNoErr(t, json.Unmarshal([]byte(c.Body), &body))
			players = append(players, body.VideoID)
		}
	}
	return browsed, players
}

// addChannelFeed subscribes feed URL u of the test channel in the store,
// due at t0, with validators and one stored video (old).
func addChannelFeed(t *testing.T, st store.Store, u string, entries ...store.NewEntry) store.Feed {
	t.Helper()
	f, err := st.CreateFeed(t.Context(), store.NewFeed{
		Kind: store.KindYouTube, URL: u, Title: "Test Channel", ETag: `"v1"`,
		LastModified: "Wed, 30 Sep 2026 12:00:00 GMT", FetchedAt: t0.Add(-2 * time.Hour), NextFetchAt: t0, TTLSec: 7200,
	}, entries, config.InitialUnread)
	mustNoErr(t, err)
	return f
}

// videoEntry is the stored form of a fake video.
func videoEntry(t *testing.T, v ittest.Video) store.NewEntry {
	t.Helper()
	e, ok := parse.VideoEntry(v.ID, v.Title, v.Description, v.Author, v.Published)
	if !ok {
		t.Fatalf("VideoEntry(%q) refused", v.ID)
	}
	return newEntries([]parse.Entry{e}, t0)[0]
}

// TestFallback_FeedDownStoresTheChannelsVideos: a channel feed answering
// 404 is read from the web API in the same poll: its three tabs (this
// channel has no Live tab), then one
// player request per video not stored yet (the Videos and Shorts tabs in
// turn, newest first, at most config.YouTubeFallbackMaxVideos), and the
// videos are stored unread as the feed would list them, failures cleared,
// validators and <ttl> kept, the next poll one interval away; the poll is
// logged with source=innertube.
func TestFallback_FeedDownStoresTheChannelsVideos(t *testing.T) {
	st := openStore(t)
	videos, shorts := ittest.IDs("vid", 20), ittest.IDs("sho", 20)
	y := newFakeYouTube(videos, shorts)
	old := y.videos[videos[19]]
	f := addChannelFeed(t, st, chanFeedURL, videoEntry(t, old))
	mustNoErr(t, st.RecordFetchError(t.Context(), f.ID, "HTTP 404 from x", t0.Add(-time.Hour), t0))

	fetcher := newFetcher(y.respond)
	clock := newClock(t0)
	s := newClockedScheduler(t, st, fetcher, clock)
	logs := captureLog(t, s)
	if hold := pollOnce(t, s, st, f.ID); !hold.IsZero() {
		t.Fatalf("hold = %v", hold)
	}

	browsed, players := innerTubeCalls(t, fetcher.Calls())
	if want := []string{innertube.TabVideos.Params, innertube.TabShorts.Params, innertube.TabLive.Params}; !slices.Equal(browsed, want) {
		t.Errorf("browsed %v, want %v", browsed, want)
	}
	var want []string
	for i := range config.YouTubeFallbackMaxIDs {
		want = append(want, videos[i], shorts[i])
	}
	want = want[:config.YouTubeFallbackMaxVideos]
	if !slices.Equal(players, want) {
		t.Errorf("players asked about %v, want %v", players, want)
	}
	if fetcher.MaxActive() != 1 {
		t.Errorf("%d requests at once, want 1", fetcher.MaxActive())
	}

	got := getFeed(t, st, f.ID)
	if got.ErrorCount != 0 || got.LastError != "" || got.ETag != `"v1"` || got.LastModified != f.LastModified ||
		got.TTLSec != 7200 || !got.LastFetchedAt.Equal(t0) || !got.NextFetchAt.Equal(t0.Add(2*time.Hour)) {
		t.Errorf("feed after the fallback = %+v", got)
	}
	entries := feedEntries(t, st, f.ID)
	if len(entries) != config.YouTubeFallbackMaxVideos+1 {
		t.Fatalf("%d entries, want %d", len(entries), config.YouTubeFallbackMaxVideos+1)
	}
	for _, id := range want {
		e, ok := entries["yt:video:"+id]
		v := y.videos[id]
		if !ok || e.IsRead || e.URL != "https://www.youtube.com/watch?v="+id || e.Title != v.Title ||
			e.ThumbnailURL != "https://i1.ytimg.com/vi/"+id+"/hqdefault.jpg" || !e.PublishedAt.Equal(v.Published) ||
			e.Author != v.Author || !strings.Contains(e.SummaryHTML, "About "+id) {
			t.Errorf("entry %s = %+v, %v", id, e, ok)
		}
	}
	lines := logs.withMsg("polled feed")
	if len(lines) != 1 || lines[0].Status != http.StatusNotFound || lines[0].New == nil || *lines[0].New != config.YouTubeFallbackMaxVideos ||
		!strings.Contains(logs.String(), `"source":"innertube"`) {
		t.Errorf("poll log = %+v\n%s", lines, logs)
	}
}

// TestFallback_KnownVideosNeedNoPlayerRequest: videos the feed already
// has are not asked about; with all of them known, the fallback is one
// browse per tab and still clears the failures.
func TestFallback_KnownVideosNeedNoPlayerRequest(t *testing.T) {
	st := openStore(t)
	videos := ittest.IDs("vid", 15)
	y := newFakeYouTube(videos, nil)
	var stored []store.NewEntry
	for _, id := range videos[1:] {
		stored = append(stored, videoEntry(t, y.videos[id]))
	}
	f := addChannelFeed(t, st, chanVideosFeedURL(), stored...)
	mustNoErr(t, st.RecordFetchError(t.Context(), f.ID, "HTTP 404 from x", t0.Add(-time.Hour), t0))
	fetcher := newFetcher(y.respond)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))

	pollOnce(t, s, st, f.ID)
	browsed, players := innerTubeCalls(t, fetcher.Calls())
	if !slices.Equal(browsed, []string{innertube.TabVideos.Params}) || !slices.Equal(players, videos[:1]) {
		t.Fatalf("browsed %v, players %v; want the Videos tab, then only %s", browsed, players, videos[0])
	}

	fetcher = newFetcher(y.respond)
	s.fetcher = fetcher
	mustNoErr(t, st.RecordFetchError(t.Context(), f.ID, "HTTP 404 from x", t0, t0))
	pollOnce(t, s, st, f.ID)
	if browsed, players := innerTubeCalls(t, fetcher.Calls()); len(browsed) != 1 || len(players) != 0 {
		t.Fatalf("all known: browsed %v, players %v", browsed, players)
	}
	if got := getFeed(t, st, f.ID); got.ErrorCount != 0 || got.LastError != "" {
		t.Errorf("feed = %+v", got)
	}
}

func chanVideosFeedURL() string { return chanVideosFeed }

// TestFallback_ModesReadTheirOwnTab: the videos-only list reads the Videos
// tab only, the Shorts-only list the Shorts tab only, the channel feed all
// three, live streams included (they are on neither of the other two); a
// tab the channel does not have (the Home tab answered) gives no video and
// no player request, while the other tabs are still read.
func TestFallback_ModesReadTheirOwnTab(t *testing.T) {
	videos, shorts, lives := ittest.IDs("vid", 2), ittest.IDs("sho", 2), ittest.IDs("liv", 2)
	all := []string{innertube.TabVideos.Params, innertube.TabShorts.Params, innertube.TabLive.Params}
	for _, tt := range []struct {
		name, url             string
		videos, shorts, lives []string
		wantTabs, wantP       []string
	}{
		{"videos only", chanVideosFeed, videos, shorts, lives, []string{innertube.TabVideos.Params}, videos},
		{"shorts only", chanShortsFeed, videos, shorts, lives, []string{innertube.TabShorts.Params}, shorts},
		{"all", chanFeedURL, videos, shorts, lives, all,
			[]string{videos[0], shorts[0], lives[0], videos[1], shorts[1], lives[1]}},
		{"all, no Shorts or Live tab", chanFeedURL, videos, nil, nil, all, videos},
		{"all, live streams only", chanFeedURL, nil, nil, lives, all, lives},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			y := newFakeYouTube(tt.videos, tt.shorts)
			y.tabs[innertube.TabLive.Params] = tt.lives
			for i, id := range tt.lives {
				y.videos[id] = testVideoOf(id, t0.Add(-time.Duration(i)*time.Hour-45*time.Minute))
			}
			f := addChannelFeed(t, st, tt.url)
			fetcher := newFetcher(y.respond)
			s := newClockedScheduler(t, st, fetcher, newClock(t0))
			pollOnce(t, s, st, f.ID)
			browsed, players := innerTubeCalls(t, fetcher.Calls())
			if !slices.Equal(browsed, tt.wantTabs) || !slices.Equal(players, tt.wantP) {
				t.Fatalf("browsed %v, players %v; want %v, %v", browsed, players, tt.wantTabs, tt.wantP)
			}
			if n := len(feedEntries(t, st, f.ID)); n != len(tt.wantP) {
				t.Errorf("%d entries, want %d", n, len(tt.wantP))
			}
			if got := getFeed(t, st, f.ID); got.ErrorCount != 0 {
				t.Errorf("feed = %+v", got)
			}
		})
	}
}

// TestFallback_SkipsVideosWithoutTrustworthyDetails: a player answer about
// another channel's video, without a date, with a date too far ahead, for
// a live stream or an upcoming premiere, or of the Android app (no date),
// is skipped (asked about again next time); the other videos are stored.
func TestFallback_SkipsVideosWithoutTrustworthyDetails(t *testing.T) {
	st := openStore(t)
	videos := ittest.IDs("vid", 8)
	y := newFakeYouTube(videos, nil)
	edit := func(i int, f func(*ittest.Video)) {
		v := y.videos[videos[i]]
		f(&v)
		y.videos[videos[i]] = v
	}
	edit(0, func(v *ittest.Video) { v.ChannelID = "UCotherchannel0000000000" })
	edit(1, func(v *ittest.Video) { v.Date = "-" })
	edit(2, func(v *ittest.Video) { v.Published = t0.Add(maxPublishedAhead + time.Hour) })
	edit(3, func(v *ittest.Video) { v.IsLive = true })
	edit(4, func(v *ittest.Video) { v.IsUpcoming = true })
	edit(5, func(v *ittest.Video) { v.Date = "yesterday" })
	android := videos[6]
	y.player = func(id string) (*fetch.Result, error) {
		r := fetch.Request{URL: innertube.PlayerURL}
		if id == android {
			return jsonAnswer(r, ittest.AndroidPlayerAnswer(y.videos[id])), nil
		}
		return jsonAnswer(r, ittest.PlayerAnswer(y.videos[id])), nil
	}
	f := addChannelFeed(t, st, chanVideosFeed)
	fetcher := newFetcher(y.respond)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	pollOnce(t, s, st, f.ID)

	entries := feedEntries(t, st, f.ID)
	if _, ok := entries["yt:video:"+videos[7]]; len(entries) != 1 || !ok {
		t.Fatalf("entries = %v; want only %s", entries, videos[7])
	}
	if got := getFeed(t, st, f.ID); got.ErrorCount != 0 {
		t.Errorf("feed = %+v", got)
	}
	// Skipped videos are asked about again.
	fetcher = newFetcher(y.respond)
	s.fetcher = fetcher
	pollOnce(t, s, st, f.ID)
	if _, players := innerTubeCalls(t, fetcher.Calls()); !slices.Equal(players, videos[:7]) {
		t.Errorf("second fallback asked about %v, want %v", players, videos[:7])
	}
}

// TestFallback_FailureRecordsTheFeedsOwnError: when the fallback fails
// (a server error, a network error, a 400, an error alert, an answer it
// does not understand, no requested tab, a failed player request), the
// poll is the feed's own failure: its 404 counted and stored as
// last_error, retried as a YouTube 404 is, entries untouched; no player
// request follows a failed browse. None of these pauses InnerTube.
func TestFallback_FailureRecordsTheFeedsOwnError(t *testing.T) {
	videos := ittest.IDs("vid", 3)
	browseFails := func(res *fetch.Result, err error) func(*fakeYouTube) {
		return func(y *fakeYouTube) {
			y.browse = func(string) (*fetch.Result, error) { return res, err }
		}
	}
	r := fetch.Request{URL: innertube.BrowseURL}
	for _, tt := range []struct {
		name        string
		setup       func(*fakeYouTube)
		wantPlayers int
	}{
		{"browse 500", browseFails(nil, statusErr(innertube.BrowseURL, http.StatusInternalServerError)), 0},
		{"browse network error", browseFails(nil, errors.New("connection reset by peer")), 0},
		{"browse 400", browseFails(nil, statusErr(innertube.BrowseURL, http.StatusBadRequest)), 0},
		{"alert", browseFails(jsonAnswer(r, ittest.AlertAnswer()), nil), 0},
		{"error object", browseFails(jsonAnswer(r, ittest.ErrorAnswer()), nil), 0},
		{"not JSON", browseFails(&fetch.Result{StatusCode: 200, Body: []byte("x"), ContentType: "text/plain"}, nil), 0},
		{"too large", browseFails(nil, fmt.Errorf("fetch x: %w", fetch.ErrTooLarge)), 0},
		{"no tab", func(y *fakeYouTube) { y.tabs = map[string][]string{} }, 0},
		{"player 503", func(y *fakeYouTube) {
			y.player = func(string) (*fetch.Result, error) { return nil, statusErr(innertube.PlayerURL, 503) }
		}, 1},
		{"player not JSON", func(y *fakeYouTube) {
			y.player = func(string) (*fetch.Result, error) {
				return &fetch.Result{StatusCode: 200, Body: []byte("{"), ContentType: "application/json"}, nil
			}
		}, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			y := newFakeYouTube(videos, nil)
			tt.setup(y)
			f := addChannelFeed(t, st, chanVideosFeed)
			fetcher := newFetcher(y.respond)
			s := newClockedScheduler(t, st, fetcher, newClock(t0))
			pollOnce(t, s, st, f.ID)

			got := getFeed(t, st, f.ID)
			if got.ErrorCount != 1 || got.LastError != "HTTP 404 from "+chanVideosFeed ||
				!got.NextFetchAt.Equal(t0.Add(config.FirstRetryDelay)) || len(feedEntries(t, st, f.ID)) != 0 {
				t.Errorf("feed = %+v", got)
			}
			if _, players := innerTubeCalls(t, fetcher.Calls()); len(players) != tt.wantPlayers {
				t.Errorf("players %v, want %d", players, tt.wantPlayers)
			}
			if p, err := st.InnerTubePause(t.Context()); err != nil || !p.Until.IsZero() || p.Refusals != 0 {
				t.Errorf("pause = %+v, %v", p, err)
			}
		})
	}
}

// TestFallback_RefusalPausesInnerTube: a refusal (429, 403, a redirect a
// POST does not follow, a web page, a bot check) ends the fallback as a
// failure and pauses every InnerTube request, transcripts included, for
// the transcripts' refusal pause (or the Retry-After when longer); while
// paused, a failing feed makes no InnerTube request. The pause written by
// the transcript queue stops the fallback the same way.
func TestFallback_RefusalPausesInnerTube(t *testing.T) {
	videos := ittest.IDs("vid", 3)
	r := fetch.Request{URL: innertube.PlayerURL}
	for _, tt := range []struct {
		name      string
		setup     func(*fakeYouTube)
		wantPause time.Duration
	}{
		{"browse 429", func(y *fakeYouTube) {
			y.browse = func(string) (*fetch.Result, error) {
				return nil, &fetch.StatusError{URL: innertube.BrowseURL, StatusCode: 429, RetryAfter: 3 * time.Hour}
			}
		}, 3 * time.Hour},
		{"browse redirect", func(y *fakeYouTube) {
			y.browse = func(string) (*fetch.Result, error) { return nil, statusErr(innertube.BrowseURL, http.StatusFound) }
		}, config.TranscriptBlockedPause},
		{"browse web page", func(y *fakeYouTube) {
			y.browse = func(string) (*fetch.Result, error) {
				return &fetch.Result{StatusCode: 200, Body: []byte("<html>"), ContentType: "text/html"}, nil
			}
		}, config.TranscriptBlockedPause},
		{"player 403", func(y *fakeYouTube) {
			y.player = func(string) (*fetch.Result, error) { return nil, statusErr(innertube.PlayerURL, http.StatusForbidden) }
		}, config.TranscriptBlockedPause},
		{"player bot check", func(y *fakeYouTube) {
			y.player = func(id string) (*fetch.Result, error) {
				v := y.videos[id]
				v.Status, v.Reason = "LOGIN_REQUIRED", "Sign in to confirm you're not a bot"
				return jsonAnswer(r, ittest.PlayerAnswer(v)), nil
			}
		}, config.TranscriptBlockedPause},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			y := newFakeYouTube(videos, nil)
			tt.setup(y)
			f := addChannelFeed(t, st, chanVideosFeed)
			fetcher := newFetcher(y.respond)
			clock := newClock(t0)
			s := newClockedScheduler(t, st, fetcher, clock)
			pollOnce(t, s, st, f.ID)

			got := getFeed(t, st, f.ID)
			if got.ErrorCount != 1 || !strings.HasPrefix(got.LastError, "HTTP 404 from ") || len(feedEntries(t, st, f.ID)) != 0 {
				t.Errorf("feed = %+v", got)
			}
			p, err := st.InnerTubePause(t.Context())
			if err != nil || !p.Until.Equal(t0.Add(tt.wantPause)) || p.Refusals != 1 {
				t.Fatalf("pause = %+v, %v; want until %v", p, err, t0.Add(tt.wantPause))
			}

			// Paused: the next poll fetches the feed only.
			n := fetcher.CallCount()
			clock.Set(t0.Add(time.Minute))
			pollOnce(t, s, st, f.ID)
			if calls := fetcher.Calls()[n:]; len(calls) != 1 || calls[0].URL != chanVideosFeed {
				t.Errorf("while paused: %d requests", len(calls))
			}
			if got := getFeed(t, st, f.ID); got.ErrorCount != 2 {
				t.Errorf("while paused: feed = %+v", got)
			}
		})
	}

	t.Run("a pause from a clock far ahead is over", func(t *testing.T) {
		st := openStore(t)
		f := addChannelFeed(t, st, chanVideosFeed)
		mustNoErr(t, st.RecordInnerTubeRefusal(t.Context(), t0.Add(maxTranscriptDue+time.Hour)))
		s := newClockedScheduler(t, st, newFetcher(newFakeYouTube(videos, nil).respond), newClock(t0))
		pollOnce(t, s, st, f.ID)
		if got := getFeed(t, st, f.ID); got.ErrorCount != 0 || len(feedEntries(t, st, f.ID)) != len(videos) {
			t.Errorf("feed = %+v", got)
		}
	})

	t.Run("transcript refusal pauses the fallback", func(t *testing.T) {
		st := openStore(t)
		y := newFakeYouTube(videos, nil)
		f := addChannelFeed(t, st, chanVideosFeed)
		mustNoErr(t, st.RecordInnerTubeRefusal(t.Context(), t0.Add(time.Hour)))
		fetcher := newFetcher(y.respond)
		clock := newClock(t0)
		s := newClockedScheduler(t, st, fetcher, clock)
		pollOnce(t, s, st, f.ID)
		if n := len(fetcher.Calls()) - len(feedRequests(fetcher.Calls())); n != 0 {
			t.Fatalf("%d InnerTube requests while paused", n)
		}
		// Once the pause is over the fallback runs again and, answered,
		// ends the streak of refusals (the pause's end is kept).
		clock.Set(t0.Add(time.Hour))
		pollOnce(t, s, st, f.ID)
		if got := getFeed(t, st, f.ID); got.ErrorCount != 0 || len(feedEntries(t, st, f.ID)) != len(videos) {
			t.Errorf("after the pause: feed = %+v", got)
		}
		if p, err := st.InnerTubePause(t.Context()); err != nil || p.Refusals != 0 || !p.Until.Equal(t0.Add(time.Hour)) {
			t.Errorf("pause = %+v, %v", p, err)
		}
	})
}

// TestFallback_ZeroExtraRequests: with FR_YOUTUBE_FALLBACK off, for a
// healthy feed (200, 304), for a YouTube feed failing otherwise (429,
// 403, 410, network, or a 404 or 5xx carrying Retry-After, a rate limit), for a feed that is not a channel's (another
// playlist, a blog) and for a feed that cannot be parsed, no InnerTube
// request is made.
func TestFallback_ZeroExtraRequests(t *testing.T) {
	for _, tt := range []struct {
		name string
		kind store.Kind
		url  string
		res  *fetch.Result
		err  error
		off  bool
	}{
		{"flag off", store.KindYouTube, chanFeedURL, nil, statusErr(chanFeedURL, 404), true},
		{"flag off, 503", store.KindYouTube, chanFeedURL, nil, statusErr(chanFeedURL, 503), true},
		{"200", store.KindYouTube, chanFeedURL, nil, nil, false},
		{"304", store.KindYouTube, chanFeedURL, notModified(chanFeedURL), nil, false},
		{"429", store.KindYouTube, chanFeedURL, nil, statusErr(chanFeedURL, 429), false},
		{"503 + Retry-After", store.KindYouTube, chanFeedURL, nil,
			&fetch.StatusError{URL: chanFeedURL, StatusCode: http.StatusServiceUnavailable, RetryAfter: time.Hour}, false},
		{"404 + Retry-After", store.KindYouTube, chanFeedURL, nil,
			&fetch.StatusError{URL: chanFeedURL, StatusCode: http.StatusNotFound, RetryAfter: time.Minute}, false},
		{"403", store.KindYouTube, chanFeedURL, nil, statusErr(chanFeedURL, 403), false},
		{"410", store.KindYouTube, chanFeedURL, nil, statusErr(chanFeedURL, 410), false},
		{"network", store.KindYouTube, chanFeedURL, nil, errors.New("no route to host"), false},
		{"unparseable", store.KindYouTube, chanFeedURL, &fetch.Result{StatusCode: 200, Body: []byte("<html>"), ContentType: "text/html"}, nil, false},
		{"other playlist", store.KindYouTube, playlistFeed, nil, statusErr(playlistFeed, 404), false},
		{"blog", store.KindRSS, feedURL, nil, statusErr(feedURL, 404), false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			f := addFeedOfKind(t, st, tt.kind, tt.url, t0)
			res := tt.res
			if res == nil && tt.err == nil {
				res = chanFeed(t, tt.url)
			}
			y := newFakeYouTube(ittest.IDs("vid", 3), nil)
			y.feed, y.feedErr = res, tt.err
			fetcher := newFetcher(y.respond)
			s := newClockedScheduler(t, st, fetcher, newClock(t0))
			s.cfg.YouTubeFallback = !tt.off
			s.avatarTried[tt.url] = struct{}{} // no avatar request either
			pollOnce(t, s, st, f.ID)
			if calls := fetcher.Calls(); len(calls) != 1 || calls[0].URL != tt.url {
				t.Errorf("requests %+v; want only the feed", calls)
			}
		})
	}
}

// TestFallback_PendingSubscriptionGetsTheInitialAdd: a pending
// subscription (subscribed while its feed failed) stores its first
// fallback as an initial add: the 5 newest unread, the rest read, no
// transcript queued, the flag cleared.
func TestFallback_PendingSubscriptionGetsTheInitialAdd(t *testing.T) {
	st := openStore(t)
	mustNoErr(t, st.SetTranscriptsSetting(t.Context(), true, t0))
	videos := ittest.IDs("vid", 8)
	y := newFakeYouTube(videos, nil)
	f, err := st.CreateFeed(t.Context(), store.NewFeed{Kind: store.KindYouTube, URL: chanVideosFeed, Title: "T",
		FetchedAt: t0.Add(-time.Hour), NextFetchAt: t0, InitialPending: true, LastError: "HTTP 404 from x"}, nil, 0)
	mustNoErr(t, err)
	fetcher := newFetcher(y.respond)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	pollOnce(t, s, st, f.ID)

	entries := feedEntries(t, st, f.ID)
	unread := unreadGUIDs(entries)
	if len(entries) != len(videos) || len(unread) != config.InitialUnread {
		t.Fatalf("%d entries, %d unread", len(entries), len(unread))
	}
	for _, id := range videos[:config.InitialUnread] {
		if !unread["yt:video:"+id] {
			t.Errorf("%s is read; want the newest unread", id)
		}
	}
	if got := getFeed(t, st, f.ID); got.InitialPending || got.ErrorCount != 0 {
		t.Errorf("feed = %+v", got)
	}
	if job, ok, err := st.NextTranscript(t.Context()); err != nil || ok {
		t.Errorf("transcript queued: %+v, %v, %v", job, ok, err)
	}
}

// TestFallback_QueuesTranscriptsOfNewVideos: on a regular poll, the
// videos the fallback stores are queued for their transcripts like a
// poll's new entries.
func TestFallback_QueuesTranscriptsOfNewVideos(t *testing.T) {
	st := openStore(t)
	mustNoErr(t, st.SetTranscriptsSetting(t.Context(), true, t0))
	y := newFakeYouTube(ittest.IDs("vid", 2), nil)
	f := addChannelFeed(t, st, chanVideosFeed)
	s := newClockedScheduler(t, st, newFetcher(y.respond), newClock(t0))
	pollOnce(t, s, st, f.ID)
	if job, ok, err := st.NextTranscript(t.Context()); err != nil || !ok || !strings.Contains(job.URL, "watch?v=vid") {
		t.Errorf("NextTranscript = %+v, %v, %v; want a queued video", job, ok, err)
	}
}

// TestFallback_FeedComingBackAddsNoDuplicate: once the feed answers again,
// the videos the fallback stored are the feed's entries: nothing new, and
// their read state is kept.
func TestFallback_FeedComingBackAddsNoDuplicate(t *testing.T) {
	st := openStore(t)
	videos := ittest.IDs("vid", 4)
	y := newFakeYouTube(videos, nil)
	f := addChannelFeed(t, st, chanFeedURL)
	fetcher := newFetcher(y.respond)
	clock := newClock(t0)
	s := newClockedScheduler(t, st, fetcher, clock)
	pollOnce(t, s, st, f.ID)
	entries := feedEntries(t, st, f.ID)
	if len(entries) != len(videos) {
		t.Fatalf("%d entries after the fallback", len(entries))
	}
	read := entries["yt:video:"+videos[0]]
	mustNoErr(t, st.SetRead(t.Context(), read.ID, true, t0))

	var atom strings.Builder
	atom.WriteString(`<?xml version="1.0" encoding="UTF-8"?><feed xmlns:yt="http://www.youtube.com/xml/schemas/2015" ` +
		`xmlns:media="http://search.yahoo.com/mrss/" xmlns="http://www.w3.org/2005/Atom"><title>Test Channel</title>`)
	for _, id := range videos {
		v := y.videos[id]
		fmt.Fprintf(&atom, `<entry><id>yt:video:%[1]s</id><yt:videoId>%[1]s</yt:videoId><title>%[2]s</title>`+
			`<link rel="alternate" href="https://www.youtube.com/watch?v=%[1]s"/><author><name>%[3]s</name></author>`+
			`<published>%[4]s</published><updated>%[4]s</updated><media:group>`+
			`<media:thumbnail url="https://i3.ytimg.com/vi/%[1]s/hqdefault.jpg"/><media:description>%[5]s</media:description>`+
			`</media:group></entry>`, id, v.Title, v.Author, v.Published.Format(time.RFC3339), v.Description)
	}
	atom.WriteString(`</feed>`)
	y.mu.Lock()
	y.feed, y.feedErr = ok(chanFeedURL, []byte(atom.String())), nil
	y.mu.Unlock()
	logs := captureLog(t, s)
	clock.Advance(time.Hour)
	pollOnce(t, s, st, f.ID)

	if lines := logs.withMsg("polled feed"); len(lines) != 1 || lines[0].New == nil || *lines[0].New != 0 {
		t.Fatalf("poll log = %+v", lines)
	}
	after := feedEntries(t, st, f.ID)
	if len(after) != len(videos) || !after["yt:video:"+videos[0]].IsRead {
		t.Errorf("entries after the feed came back = %+v", after)
	}
	for g, e := range after {
		if b := entries[g]; e.Title != b.Title || e.SummaryHTML != b.SummaryHTML || e.URL != b.URL || !e.PublishedAt.Equal(b.PublishedAt) {
			t.Errorf("%s changed: %+v → %+v", g, b, e)
		}
	}
}

// TestFallback_ManualRefresh: a Refresh click runs the fallback too; when
// it fails, the failure is the manual one (not counted).
func TestFallback_ManualRefresh(t *testing.T) {
	st := openStore(t)
	y := newFakeYouTube(ittest.IDs("vid", 2), nil)
	f := addChannelFeed(t, st, chanVideosFeed)
	mustNoErr(t, st.RecordFetchError(t.Context(), f.ID, "HTTP 404 from x", t0.Add(-time.Hour), t0.Add(time.Hour)))
	fetcher := newFetcher(y.respond)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	y.browse = func(string) (*fetch.Result, error) { return nil, statusErr(innertube.BrowseURL, 500) }
	s.pollWith(t.Context(), getFeed(t, st, f.ID), &manualRefresh{})
	if got := getFeed(t, st, f.ID); got.ErrorCount != 1 || !strings.HasPrefix(got.LastError, "HTTP 404 from "+chanVideosFeed) {
		t.Fatalf("after a failed manual fallback: %+v", got)
	}
	y.mu.Lock()
	y.browse = nil
	y.mu.Unlock()
	s.pollWith(t.Context(), getFeed(t, st, f.ID), &manualRefresh{})
	if got := getFeed(t, st, f.ID); got.ErrorCount != 0 || len(feedEntries(t, st, f.ID)) != 2 {
		t.Fatalf("after a manual fallback: %+v", got)
	}
}

// TestFallback_ShutdownRecordsNothing: a fallback cut short by shutdown
// stores nothing.
func TestFallback_ShutdownRecordsNothing(t *testing.T) {
	st := openStore(t)
	y := newFakeYouTube(ittest.IDs("vid", 3), nil)
	f := addChannelFeed(t, st, chanVideosFeed)
	ctx, cancel := context.WithCancel(t.Context())
	y.player = func(string) (*fetch.Result, error) {
		cancel()
		return nil, fmt.Errorf("fetch x: %w", context.Canceled)
	}
	s := newClockedScheduler(t, st, newFetcher(y.respond), newClock(t0))
	if hold := s.poll(ctx, getFeed(t, st, f.ID)); !hold.IsZero() {
		t.Fatalf("hold = %v", hold)
	}
	if got := getFeed(t, st, f.ID); got.ErrorCount != 0 || got.LastError != "" || !got.NextFetchAt.Equal(t0) {
		t.Errorf("feed = %+v", got)
	}
	if p, _ := st.InnerTubePause(t.Context()); !p.Until.IsZero() {
		t.Errorf("pause = %+v", p)
	}
}

// TestFallback_RequestsAreSpaced: fallback requests are sent one after
// another, at least fallbackSpacing apart, across feeds polled at once.
func TestFallback_RequestsAreSpaced(t *testing.T) {
	const spacing = 30 * time.Millisecond
	st := openStore(t)
	y := newFakeYouTube(ittest.IDs("vid", 2), ittest.IDs("sho", 2))
	var (
		mu    sync.Mutex
		times []time.Time
	)
	fetcher := newFetcher(func(ctx context.Context, r fetch.Request) (*fetch.Result, error) {
		if r.URL == innertube.BrowseURL || r.URL == innertube.PlayerURL {
			mu.Lock()
			times = append(times, time.Now())
			mu.Unlock()
		}
		return y.respond(ctx, r)
	})
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	s.fallbackSpacing = spacing
	a := addChannelFeed(t, st, chanVideosFeed)
	b := addChannelFeed(t, st, chanShortsFeed)
	var wg sync.WaitGroup
	for _, id := range []int64{a.ID, b.ID} {
		wg.Go(func() { pollOnce(t, s, st, id) })
	}
	wg.Wait()
	if len(times) != 6 {
		t.Fatalf("%d fallback requests, want 6", len(times))
	}
	slices.SortFunc(times, func(x, y time.Time) int { return x.Compare(y) })
	for i := 1; i < len(times); i++ {
		if d := times[i].Sub(times[i-1]); d < spacing-time.Millisecond {
			t.Errorf("requests %d and %d %v apart, want at least %v", i-1, i, d, spacing)
		}
	}
}

func TestInterleave(t *testing.T) {
	got := interleave([][]string{{"a", "b", "c"}, {"x", "a"}, nil})
	if want := []string{"a", "x", "b", "c"}; !slices.Equal(got, want) {
		t.Errorf("interleave = %v, want %v", got, want)
	}
}

// TestPreview_FallbackWhenTheFeedIsDown: the add flow reads a channel
// whose feed answers 404 from the web API: the preview is a fallback one,
// titled with the channel's name, and subscribing stores its videos as an
// initial add (the newest config.InitialUnread unread) from the cached
// preview, without another request; the feed is not pending.
func TestPreview_FallbackWhenTheFeedIsDown(t *testing.T) {
	st := openStore(t)
	videos, shorts := ittest.IDs("vid", 10), ittest.IDs("sho", 10)
	y := newFakeYouTube(videos, shorts)
	fetcher := newFetcher(y.respond)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	p, err := s.Preview(t.Context(), chanFeedURL)
	mustNoErr(t, err)
	want := Preview{FeedURL: chanFeedURL, Kind: store.KindYouTube, Title: "Test Channel",
		EntryCount: config.YouTubeFallbackMaxVideos, Fallback: true}
	if *p != want {
		t.Fatalf("preview = %+v, want %+v", *p, want)
	}
	if browsed, players := innerTubeCalls(t, fetcher.Calls()); len(browsed) != 3 || len(players) != config.YouTubeFallbackMaxVideos {
		t.Errorf("browsed %d tabs, asked about %d videos", len(browsed), len(players))
	}

	n := fetcher.CallCount()
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanFeedURL, Title: p.Title})
	mustNoErr(t, err)
	if calls := fetcher.Calls()[n:]; len(feedRequests(calls)) != len(calls) {
		t.Errorf("subscribe made fallback requests: %v", callURLs(fetcher)[n:])
	}
	if feed.InitialPending || feed.ErrorCount != 0 || feed.Title != "Test Channel" {
		t.Errorf("feed = %+v", feed)
	}
	entries := feedEntries(t, st, feed.ID)
	unread := unreadGUIDs(entries)
	if len(entries) != config.YouTubeFallbackMaxVideos || len(unread) != config.InitialUnread {
		t.Fatalf("%d entries, %d unread", len(entries), len(unread))
	}
	for _, id := range []string{videos[0], shorts[0], videos[1], shorts[1], videos[2]} {
		if !unread["yt:video:"+id] {
			t.Errorf("%s is read; want the newest unread", id)
		}
	}
}

// TestSubscribe_FallbackWithoutAPreview: Subscribe without a cached
// preview falls back the same way, for a mode's list too (its own tab).
func TestSubscribe_FallbackWithoutAPreview(t *testing.T) {
	st := openStore(t)
	y := newFakeYouTube(ittest.IDs("vid", 3), ittest.IDs("sho", 3))
	fetcher := newFetcher(y.respond)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanShortsFeed})
	mustNoErr(t, err)
	if browsed, _ := innerTubeCalls(t, fetcher.Calls()); !slices.Equal(browsed, []string{innertube.TabShorts.Params}) {
		t.Errorf("browsed %v", browsed)
	}
	entries := feedEntries(t, st, feed.ID)
	if _, ok := entries["yt:video:"+ittest.IDs("sho", 1)[0]]; len(entries) != 3 || !ok || feed.InitialPending {
		t.Errorf("feed %+v, entries %v", feed, entries)
	}
}

// TestPreview_PendingWhenTheFallbackCannotRun: with the fallback off, or
// paused after a refusal, or failing, a channel whose feed is down is
// offered as a pending subscription as before; off or paused, no fallback
// request is made.
func TestPreview_PendingWhenTheFallbackCannotRun(t *testing.T) {
	for _, tt := range []struct {
		name          string
		setup         func(*Scheduler, *fakeYouTube, store.Store)
		wantInnerTube int
	}{
		{"off", func(s *Scheduler, _ *fakeYouTube, _ store.Store) { s.cfg.YouTubeFallback = false }, 0},
		{"paused", func(_ *Scheduler, _ *fakeYouTube, st store.Store) {
			mustNoErr(t, st.RecordInnerTubeRefusal(t.Context(), t0.Add(time.Minute)))
		}, 0},
		{"failing", func(_ *Scheduler, y *fakeYouTube, _ store.Store) {
			y.browse = func(string) (*fetch.Result, error) { return nil, statusErr(innertube.BrowseURL, 500) }
		}, 1}, // the first failure ends it
	} {
		t.Run(tt.name, func(t *testing.T) {
			st := openStore(t)
			y := newFakeYouTube(ittest.IDs("vid", 3), nil)
			fetcher := newFetcher(y.respond)
			s := newClockedScheduler(t, st, fetcher, newClock(t0))
			tt.setup(s, y, st)
			p, err := s.Preview(t.Context(), chanFeedURL)
			mustNoErr(t, err)
			if !p.Pending || p.Fallback || p.EntryCount != 0 {
				t.Errorf("preview = %+v, want pending", p)
			}
			if n := len(fetcher.Calls()) - len(feedRequests(fetcher.Calls())); n != tt.wantInnerTube {
				t.Errorf("%d fallback requests, want %d", n, tt.wantInnerTube)
			}
		})
	}
}

// TestFallback_OlderVideosAreStoredRead: a tab lists more, and older,
// videos than the feed did; those published before the newest entry the
// feed has are not new: they are stored read and not queued for
// transcripts, so they are not asked about again, while newer ones are
// unread.
func TestFallback_OlderVideosAreStoredRead(t *testing.T) {
	st := openStore(t)
	mustNoErr(t, st.SetTranscriptsSetting(t.Context(), true, t0))
	videos := ittest.IDs("vid", 6) // t0, t0-1h, … t0-5h
	y := newFakeYouTube(videos, nil)
	f := addChannelFeed(t, st, chanVideosFeed, videoEntry(t, y.videos[videos[2]]))
	fetcher := newFetcher(y.respond)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	pollOnce(t, s, st, f.ID)

	entries := feedEntries(t, st, f.ID)
	for i, id := range videos {
		e, ok := entries["yt:video:"+id]
		if !ok || e.IsRead != (i > 2) {
			t.Errorf("%s (%d hours old): %+v, %v; want stored, read only when older than the newest stored", id, i, e, ok)
		}
	}
	job, ok, err := st.NextTranscript(t.Context())
	mustNoErr(t, err)
	if !ok || (!strings.Contains(job.URL, videos[0]) && !strings.Contains(job.URL, videos[1])) {
		t.Errorf("next transcript = %+v, %v", job, ok)
	}
	fetcher = newFetcher(y.respond)
	s.fetcher = fetcher
	pollOnce(t, s, st, f.ID)
	if _, players := innerTubeCalls(t, fetcher.Calls()); len(players) != 0 {
		t.Errorf("asked again about %v", players)
	}
}

// TestFallback_RefusalAfterAnAnswerKeepsTheStreak: a refusal met after
// an answered request in the same fallback adds to the streak of
// refusals; it is not ended by the answer before it.
func TestFallback_RefusalAfterAnAnswerKeepsTheStreak(t *testing.T) {
	st := openStore(t)
	mustNoErr(t, st.RecordInnerTubeRefusal(t.Context(), t0.Add(-2*time.Hour)))
	mustNoErr(t, st.RecordInnerTubeRefusal(t.Context(), t0.Add(-time.Hour)))
	y := newFakeYouTube(ittest.IDs("vid", 2), nil)
	y.player = func(string) (*fetch.Result, error) {
		return nil, statusErr(innertube.PlayerURL, http.StatusTooManyRequests)
	}
	f := addChannelFeed(t, st, chanVideosFeed)
	s := newClockedScheduler(t, st, newFetcher(y.respond), newClock(t0))
	pollOnce(t, s, st, f.ID)
	p, err := st.InnerTubePause(t.Context())
	if want := t0.Add(s.blockedPause(statusErr(innertube.PlayerURL, 429), 2)); err != nil || p.Refusals != 3 || !p.Until.Equal(want) {
		t.Errorf("pause = %+v, %v; want 3 refusals, until %v", p, err, want)
	}
}

// TestFallback_RateLimitedFeedRunsNoFallback: a 404 or 5xx carrying
// Retry-After is a rate limit: the host asked to back off, so the
// fallback, on the same host, is not run, and the failure is recorded as
// that rate limit.
func TestFallback_RateLimitedFeedRunsNoFallback(t *testing.T) {
	st := openStore(t)
	y := newFakeYouTube(ittest.IDs("vid", 3), nil)
	y.feedErr = &fetch.StatusError{URL: chanVideosFeed, StatusCode: http.StatusServiceUnavailable, RetryAfter: 3 * time.Hour}
	f := addChannelFeed(t, st, chanVideosFeed)
	fetcher := newFetcher(y.respond)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	pollOnce(t, s, st, f.ID)
	if browsed, players := innerTubeCalls(t, fetcher.Calls()); len(browsed) != 0 || len(players) != 0 {
		t.Fatalf("browsed %v, players %v; want no fallback", browsed, players)
	}
	if got := getFeed(t, st, f.ID); got.ErrorCount != 1 || !got.NextFetchAt.Equal(t0.Add(3*time.Hour)) || len(feedEntries(t, st, f.ID)) != 0 {
		t.Errorf("feed = %+v", got)
	}
}

// TestFallback_FutureStoredEntryDoesNotBuryNewVideos: a stored entry
// dated after the poll (a premiere the feed listed, a bad date) is no
// reference for what is old: the fallback's new videos stay unread and
// are queued for transcripts.
func TestFallback_FutureStoredEntryDoesNotBuryNewVideos(t *testing.T) {
	st := openStore(t)
	mustNoErr(t, st.SetTranscriptsSetting(t.Context(), true, t0))
	videos := ittest.IDs("vid", 3) // t0, t0-1h, t0-2h: all new
	y := newFakeYouTube(videos, nil)
	future := testVideoOf("fuuuuuuuuuu", t0.Add(30*24*time.Hour))
	f := addChannelFeed(t, st, chanVideosFeed, videoEntry(t, future))
	s := newClockedScheduler(t, st, newFetcher(y.respond), newClock(t0))
	pollOnce(t, s, st, f.ID)
	entries := feedEntries(t, st, f.ID)
	for _, id := range videos {
		if e, ok := entries["yt:video:"+id]; !ok || e.IsRead {
			t.Errorf("new video %s: %+v, %v; want stored unread", id, e, ok)
		}
	}
	if _, ok, err := st.NextTranscript(t.Context()); err != nil || !ok {
		t.Errorf("no transcript queued for the new videos (%v)", err)
	}
}

// TestFallback_PublishDateSlack: a video dated more than maxPublishedAhead
// after the poll is skipped; within it (clock skew), it is stored.
func TestFallback_PublishDateSlack(t *testing.T) {
	if maxPublishedAhead > time.Hour {
		t.Fatalf("maxPublishedAhead = %v; a publication date is the upload time, an hour covers clock skew", maxPublishedAhead)
	}
	st := openStore(t)
	videos := ittest.IDs("vid", 2)
	y := newFakeYouTube(videos, nil)
	ahead := y.videos[videos[0]]
	ahead.Published = t0.Add(maxPublishedAhead + time.Minute)
	y.videos[videos[0]] = ahead
	skew := y.videos[videos[1]]
	skew.Published = t0.Add(maxPublishedAhead - time.Minute)
	y.videos[videos[1]] = skew
	f := addChannelFeed(t, st, chanVideosFeed)
	s := newClockedScheduler(t, st, newFetcher(y.respond), newClock(t0))
	pollOnce(t, s, st, f.ID)
	entries := feedEntries(t, st, f.ID)
	if _, ok := entries["yt:video:"+videos[1]]; len(entries) != 1 || !ok {
		t.Errorf("entries = %v; want only the one within the slack", entries)
	}
}

// TestPreview_FallbackWithoutUsableVideosOffersPending: an add-flow
// fallback whose videos give no details to store (all live, upcoming or
// undated) did not help: the channel is offered as a pending subscription
// with its name and avatar from its page, as without the fallback.
func TestPreview_FallbackWithoutUsableVideosOffersPending(t *testing.T) {
	st := openStore(t)
	videos := ittest.IDs("vid", 3)
	y := newFakeYouTube(videos, nil)
	for _, id := range videos {
		v := y.videos[id]
		v.IsUpcoming = true
		y.videos[id] = v
	}
	fetcher := newFetcher(y.respond)
	s := newClockedScheduler(t, st, fetcher, newClock(t0))
	p, err := s.Preview(t.Context(), chanFeedURL)
	mustNoErr(t, err)
	if !p.Pending || p.Fallback || p.Title != chanTitle || p.EntryCount != 0 {
		t.Fatalf("preview = %+v, want pending with the channel page's name", p)
	}
	feed, err := s.Subscribe(t.Context(), Subscription{FeedURL: chanFeedURL, AllowPending: true})
	mustNoErr(t, err)
	if !feed.InitialPending || feed.Title != chanTitle {
		t.Errorf("feed = %+v, want a pending subscription named after the channel", feed)
	}
}
