package transcript

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/safeurl"
)

func TestFetch_TwoRequestsAndTheTranscript(t *testing.T) {
	yt := okYouTube(t)
	got, err := New(yt).Fetch(t.Context(), testVideoID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := Transcript{
		Lang:      "en",
		Generated: false,
		Text:      "Synthetic line one wrapped [Narrator] second cue.\n\n>> A new speaker & styled words",
	}
	if got != want {
		t.Errorf("Fetch = %+v, want %+v", got, want)
	}

	calls := yt.requests()
	if len(calls) != 2 {
		t.Fatalf("%d requests, want 2: %+v", len(calls), calls)
	}
	if p := calls[0]; p.Method != http.MethodPost || p.URL != playerURL || p.ContentType != jsonType {
		t.Errorf("first request = %s %s (%s), want the player POST", p.Method, p.URL, p.ContentType)
	}
	// The caption URL is the authored English track's, with only its
	// format changed and its parameters in YouTube's order.
	wantURL := strings.Replace(syntheticCaptionURLFromFixture(t, 1), "&fmt=srv3", "&fmt="+captionFormat, 1)
	if c := calls[1]; c.Method != "" || c.URL != wantURL || c.Accept != jsonType || c.Body != "" || !c.NoRedirects {
		t.Errorf("second request = %+v, want GET %s without redirects", c, wantURL)
	}
}

// syntheticCaptionURLFromFixture returns the baseUrl of track i of the
// dubbed player fixture.
func syntheticCaptionURLFromFixture(t *testing.T, i int) string {
	t.Helper()
	p, err := decodePlayer(fixture(t, "player_ok_dubbed.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p.Captions.Renderer.CaptionTracks[i].BaseURL
}

func TestFetch_AutomaticCaptions(t *testing.T) {
	yt := &fakeYouTube{
		player:   jsonReply(fixture(t, "player_ok_asr_only.json")),
		captions: jsonReply(fixture(t, "json3_asr.json")),
	}
	got, err := New(yt).Fetch(t.Context(), testVideoID)
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got.Lang != "en" || !got.Generated || got.Text != "alpha beta gamma delta.\n\n>> epsilon zeta" {
		t.Errorf("Fetch = %+v", got)
	}
}

func TestFetch_InvalidIDMakesNoRequest(t *testing.T) {
	for _, id := range []string{"", "short", "vid000000012", "vid0000000/", "vid00000001\n", `vid"00000001`} {
		yt := okYouTube(t)
		if _, err := New(yt).Fetch(t.Context(), id); err == nil {
			t.Errorf("Fetch(%q): no error", id)
		}
		if n := len(yt.requests()); n != 0 {
			t.Errorf("Fetch(%q) made %d requests", id, n)
		}
	}
}

func TestFetch_Outcomes(t *testing.T) {
	ok := func() *fakeYouTube { return okYouTube(t) }
	withPlayer := func(r reply) *fakeYouTube { y := ok(); y.player = r; return y }
	withCaptions := func(r reply) *fakeYouTube { y := ok(); y.captions = r; return y }
	captionsRes := func(ctype string, body []byte, finalURL string) reply {
		return reply{res: &fetch.Result{StatusCode: http.StatusOK, ContentType: ctype, Body: body, FinalURL: finalURL}}
	}
	tests := []struct {
		name     string
		yt       *fakeYouTube
		want     error // nil: neither ErrNoCaptions nor ErrBlocked
		requests int
	}{
		{"no captions", withPlayer(jsonReply(fixture(t, "player_no_captions.json"))), ErrNoCaptions, 1},
		{"unavailable video", withPlayer(jsonReply(fixture(t, "player_unavailable.json"))), ErrUnavailable, 1},
		{"bot check", withPlayer(jsonReply(fixture(t, "player_bot_check.json"))), ErrBlocked, 1},
		{"proof-of-origin captions", withPlayer(jsonReply(fixture(t, "player_pot.json"))), ErrBlocked, 1},
		{"upcoming premiere", withPlayer(jsonReply(fixture(t, "player_upcoming.json"))), ErrNotReady, 1},
		{"live stream", withPlayer(jsonReply(fixture(t, "player_live.json"))), ErrNotReady, 1},
		{"captcha instead of the player", withPlayer(captionsRes(htmlUTF8, []byte(`<div class="g-recaptcha"></div>`), "")), ErrBlocked, 1},
		{"player too large", withPlayer(jsonReply([]byte(`{"x":"` + strings.Repeat("a", maxPlayerBytes) + `"}`))), ErrTooLarge, 1},
		{"empty caption body", withCaptions(captionsRes(htmlUTF8, nil, "")), ErrBlocked, 2},
		{"empty caption body, JSON type", withCaptions(captionsRes(jsonUTF8, []byte{}, "")), ErrBlocked, 2},
		{"HTML caption body", withCaptions(captionsRes(htmlUTF8, []byte(`<html>consent</html>`), "")), ErrBlocked, 2},
		{"captions redirected to a consent page", withCaptions(captionsRes(jsonUTF8, fixture(t, "json3_manual.json"), "https://consent.youtube.com/m?continue=x")), ErrBlocked, 2},
		{"captions redirected to another path", withCaptions(captionsRes(jsonUTF8, fixture(t, "json3_manual.json"), "https://www.youtube.com/sorry/index")), ErrBlocked, 2},
		{"captions of another type", withCaptions(captionsRes("text/xml", []byte(`<transcript/>`), "")), nil, 2},
		{"captions too large", withCaptions(captionsRes(jsonUTF8, []byte(`{"events":[],"x":"`+strings.Repeat("a", maxCaptionBytes)+`"}`), "")), ErrTooLarge, 2},
		{"renamed track URL", withPlayer(jsonReply(renamed(t, "player_ok_dubbed.json", "baseUrl", "url"))), nil, 1},
		{"renamed caption text", withCaptions(jsonReply(renamed(t, "json3_manual.json", "utf8", "text"))), nil, 2},
		{"captions without text", withCaptions(jsonReply(json3(cue(" ")))), ErrNoCaptions, 2},
		{"music video", withCaptions(jsonReply(fixture(t, "json3_annotations_only.json"))), ErrNoCaptions, 2},
		{"malformed captions", withCaptions(jsonReply([]byte(`{"events":[`))), nil, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := New(tt.yt).Fetch(t.Context(), testVideoID)
			if err == nil {
				t.Fatalf("Fetch = %+v, want an error", got)
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if tt.want == nil && (errors.Is(err, ErrNoCaptions) || errors.Is(err, ErrBlocked) || errors.Is(err, ErrTooLarge)) {
				t.Errorf("err = %v, want none of ErrNoCaptions, ErrBlocked and ErrTooLarge", err)
			}
			if errors.Is(err, ErrTooLarge) && (errors.Is(err, ErrNoCaptions) || errors.Is(err, ErrBlocked)) {
				t.Errorf("err = %v matches another outcome than ErrTooLarge", err)
			}
			checkNotReady(t, err, tt.want == ErrNotReady)
			if n := len(tt.yt.requests()); n != tt.requests {
				t.Errorf("%d requests, want %d", n, tt.requests)
			}
			if !strings.HasPrefix(err.Error(), "transcript: ") {
				t.Errorf("message %q does not name the package", err)
			}
		})
	}
}

func TestFetch_TransportErrors(t *testing.T) {
	const retryAfter = 10 * time.Minute
	status := func(code int, d time.Duration) error {
		return &fetch.StatusError{URL: "https://www.youtube.com/x", StatusCode: code, RetryAfter: d}
	}
	tests := []struct {
		name    string
		player  error
		caption error
		want    error // nil: neither ErrNoCaptions nor ErrBlocked
	}{
		{"429 on the player", status(http.StatusTooManyRequests, retryAfter), nil, ErrBlocked},
		{"403 on the player", status(http.StatusForbidden, 0), nil, ErrBlocked},
		{"401 on the player", status(http.StatusUnauthorized, 0), nil, ErrBlocked},
		{"302 on the player", status(http.StatusFound, 0), nil, ErrBlocked},
		{"303 on the player", status(http.StatusSeeOther, 0), nil, ErrBlocked},
		{"500 on the player", status(http.StatusInternalServerError, 0), nil, nil},
		{"400 on the player", status(http.StatusBadRequest, 0), nil, nil},
		{"429 on the captions", nil, status(http.StatusTooManyRequests, retryAfter), ErrBlocked},
		{"403 on the captions", nil, status(http.StatusForbidden, 0), ErrBlocked},
		{"404 on the captions", nil, status(http.StatusNotFound, 0), nil},
		// A long video's caption track over the fetch size cap is so
		// every time it is asked for.
		{"captions too large", nil, fmt.Errorf("fetch x: %w", fetch.ErrTooLarge), ErrTooLarge},
		{"player too large", fmt.Errorf("fetch x: %w", fetch.ErrTooLarge), nil, ErrTooLarge},
		{"network", errors.New("fetch x: connection refused"), nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			yt := okYouTube(t)
			if tt.player != nil {
				yt.player = errReply(tt.player)
			}
			if tt.caption != nil {
				yt.captions = errReply(tt.caption)
			}
			_, err := New(yt).Fetch(t.Context(), testVideoID)
			if err == nil {
				t.Fatal("no error")
			}
			if tt.want != nil && !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
			if tt.want == nil && (errors.Is(err, ErrNoCaptions) || errors.Is(err, ErrBlocked)) {
				t.Errorf("err = %v, want neither ErrNoCaptions nor ErrBlocked", err)
			}
			cause := tt.player
			if cause == nil {
				cause = tt.caption
			}
			if !errors.Is(err, cause) {
				t.Errorf("err = %v does not wrap the Fetcher's error", err)
			}
			var se *fetch.StatusError
			if errors.As(cause, &se) {
				var got *fetch.StatusError
				if !errors.As(err, &got) || got.RetryAfter != se.RetryAfter {
					t.Errorf("Retry-After not reachable through %v", err)
				}
			}
			if n, want := len(yt.requests()), map[bool]int{true: 1, false: 2}[tt.player != nil]; n != want {
				t.Errorf("%d requests, want %d", n, want)
			}
		})
	}
}

// TestFetch_ErrorsDoNotShowTheCaptionURL: the caption URL is long and
// signed; messages name its endpoint only.
func TestFetch_ErrorsDoNotShowTheCaptionURL(t *testing.T) {
	yt := okYouTube(t)
	signed := strings.Replace(syntheticCaptionURLFromFixture(t, 1), "&fmt=srv3", "&fmt="+captionFormat, 1)
	for _, cause := range []error{
		&fetch.StatusError{URL: safeurl.String(signed), StatusCode: http.StatusTooManyRequests},
		&fetch.StatusError{URL: safeurl.String(signed), StatusCode: http.StatusInternalServerError},
		fmt.Errorf("fetch %s: %w", safeurl.String(signed), context.DeadlineExceeded),
	} {
		yt.captions = errReply(cause)
		_, err := New(yt).Fetch(t.Context(), testVideoID)
		if err == nil || strings.Contains(err.Error(), "signature") || strings.Contains(err.Error(), "?") {
			t.Errorf("err = %v; want no caption query in it", err)
		}
		if err != nil && !strings.Contains(err.Error(), "caption track") {
			t.Errorf("err = %v; want the request named", err)
		}
	}
	// A status may come from a captcha page the request was redirected to,
	// whose URL carries the caption URL.
	sorry := "https://www.google.com/sorry/index?continue=" + url.QueryEscape(signed) + "&q=synthetic"
	yt.captions = errReply(&fetch.StatusError{URL: sorry, StatusCode: http.StatusTooManyRequests})
	_, err := New(yt).Fetch(t.Context(), testVideoID)
	if !errors.Is(err, ErrBlocked) || strings.Contains(err.Error(), "sorry") || strings.Contains(err.Error(), "signature") {
		t.Errorf("err = %v; want ErrBlocked without the captcha URL", err)
	}
	// Other errors name the endpoint instead of the URL.
	yt.captions = errReply(fmt.Errorf("fetch %s: %w", signed, context.DeadlineExceeded))
	if _, err := New(yt).Fetch(t.Context(), testVideoID); err == nil || !strings.Contains(err.Error(), "fetch "+captionEndpoint+": ") {
		t.Errorf("err = %v; want the endpoint named", err)
	}
}

func TestFetch_CancelledContext(t *testing.T) {
	t.Run("before the first request", func(t *testing.T) {
		yt := okYouTube(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err := New(yt).Fetch(ctx, testVideoID)
		if err != context.Canceled {
			t.Errorf("err = %v, want context.Canceled itself", err)
		}
		if n := len(yt.requests()); n != 0 {
			t.Errorf("%d requests", n)
		}
	})
	for _, at := range []int{1, 2} {
		t.Run(fmt.Sprintf("during request %d", at), func(t *testing.T) {
			yt := okYouTube(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			yt.onFetch = func(n int) {
				if n == at {
					cancel()
				}
			}
			_, err := New(yt).Fetch(ctx, testVideoID)
			if err != context.Canceled {
				t.Errorf("err = %v, want context.Canceled itself", err)
			}
			if n := len(yt.requests()); n != at {
				t.Errorf("%d requests, want %d", n, at)
			}
		})
	}
	t.Run("a deadline", func(t *testing.T) {
		yt := okYouTube(t)
		ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
		defer cancel()
		if _, err := New(yt).Fetch(ctx, testVideoID); err != context.DeadlineExceeded {
			t.Errorf("err = %v, want context.DeadlineExceeded", err)
		}
	})
}

func TestFetch_SafeForConcurrentUse(t *testing.T) {
	const concurrent = 8
	c := New(okYouTube(t))
	errs := make(chan error, concurrent)
	for range cap(errs) {
		go func() {
			_, err := c.Fetch(t.Context(), testVideoID)
			errs <- err
		}()
	}
	for range cap(errs) {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}
