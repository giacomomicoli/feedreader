package innertube_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/innertube"
	"github.com/giacomomicoli/feedreader/internal/innertube/ittest"
)

// fakeFetcher answers every request with respond and records it.
type fakeFetcher struct {
	calls   []fetch.Request
	respond func(r fetch.Request) (*fetch.Result, error)
}

func (f *fakeFetcher) Fetch(_ context.Context, r fetch.Request) (*fetch.Result, error) {
	f.calls = append(f.calls, r)
	return f.respond(r)
}

func answer(body []byte) func(fetch.Request) (*fetch.Result, error) {
	return func(r fetch.Request) (*fetch.Result, error) {
		return &fetch.Result{StatusCode: 200, Body: body, ContentType: jsonType, FinalURL: r.URL}, nil
	}
}

func TestClient_Browse(t *testing.T) {
	ids := ittest.IDs("vid", 20)
	f := &fakeFetcher{respond: answer(ittest.TabAnswer(testChannel, innertube.TabVideos, ids...))}
	got, err := innertube.NewClient(f).Browse(t.Context(), testChannel, innertube.TabVideos, maxIDs)
	if err != nil || !slices.Equal(got, ids[:maxIDs]) {
		t.Fatalf("Browse = %v, %v", got, err)
	}
	if len(f.calls) != 1 || f.calls[0] != innertube.BrowseRequest(innertube.Web, testChannel, innertube.TabVideos) {
		t.Fatalf("requests = %+v", f.calls)
	}
}

func TestClient_Video(t *testing.T) {
	v := testVideo()
	f := &fakeFetcher{respond: answer(ittest.PlayerAnswer(v))}
	got, err := innertube.NewClient(f).Video(t.Context(), v.ID, v.ChannelID)
	if err != nil || got.Title != v.Title || !got.Published.Equal(published) {
		t.Fatalf("Video = %+v, %v", got, err)
	}
	if len(f.calls) != 1 || f.calls[0] != innertube.VideoRequest(v.ID) {
		t.Fatalf("requests = %+v", f.calls)
	}
}

// TestClient_InvalidIDsMakeNoRequest: an ID of the wrong form is never
// sent.
func TestClient_InvalidIDsMakeNoRequest(t *testing.T) {
	f := &fakeFetcher{respond: answer([]byte(`{}`))}
	c := innertube.NewClient(f)
	for _, ch := range []string{"", "UCshort", "UC" + strings.Repeat("a", 23), "XX" + strings.Repeat("a", 22), "UC" + strings.Repeat("a", 21) + "/"} {
		if _, err := c.Browse(t.Context(), ch, innertube.TabVideos, maxIDs); err == nil {
			t.Errorf("Browse(%q) succeeded", ch)
		}
	}
	for _, id := range []string{"", "short", "aaaaaaaaaaaa", "aaaaaaaaa/a", "aaaaaaaaa%3"} {
		if _, err := c.Video(t.Context(), id, testChannel); err == nil {
			t.Errorf("Video(%q) succeeded", id)
		}
	}
	if len(f.calls) != 0 {
		t.Fatalf("%d requests made", len(f.calls))
	}
}

// TestClient_FetchErrors: refusals (401, 403, 429 and a redirect, which a
// POST never follows) are ErrBlocked with the Retry-After reachable; 400
// is ErrRejected; an answer over the cap ErrTooLarge; other failures are
// neither but stay reachable.
func TestClient_FetchErrors(t *testing.T) {
	network := errors.New("connection refused")
	tests := []struct {
		name string
		err  error
		want error
	}{
		{"429", &fetch.StatusError{StatusCode: http.StatusTooManyRequests, RetryAfter: time.Hour}, innertube.ErrBlocked},
		{"403", &fetch.StatusError{StatusCode: http.StatusForbidden}, innertube.ErrBlocked},
		{"401", &fetch.StatusError{StatusCode: http.StatusUnauthorized}, innertube.ErrBlocked},
		{"302 to a consent page", &fetch.StatusError{StatusCode: http.StatusFound}, innertube.ErrBlocked},
		{"307", &fetch.StatusError{StatusCode: http.StatusTemporaryRedirect}, innertube.ErrBlocked},
		{"400", &fetch.StatusError{StatusCode: http.StatusBadRequest}, innertube.ErrRejected},
		{"too large", fmt.Errorf("fetch x: %w", fetch.ErrTooLarge), innertube.ErrTooLarge},
		{"500", &fetch.StatusError{StatusCode: http.StatusInternalServerError}, nil},
		{"network", network, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeFetcher{respond: func(fetch.Request) (*fetch.Result, error) { return nil, tt.err }}
			c := innertube.NewClient(f)
			_, berr := c.Browse(t.Context(), testChannel, innertube.TabVideos, maxIDs)
			_, verr := c.Video(t.Context(), "vid00000001", testChannel)
			for _, err := range []error{berr, verr} {
				if !errors.Is(err, tt.err) {
					t.Errorf("err = %v, does not match the fetch error", err)
				}
				for _, kind := range []error{innertube.ErrBlocked, innertube.ErrRejected, innertube.ErrTooLarge} {
					if errors.Is(err, kind) != (kind == tt.want) {
						t.Errorf("err = %v: matches %v = %v", err, kind, !(kind == tt.want))
					}
				}
				var se *fetch.StatusError
				if errors.As(tt.err, &se) && se.RetryAfter > 0 {
					var got *fetch.StatusError
					if !errors.As(err, &got) || got.RetryAfter != se.RetryAfter {
						t.Errorf("Retry-After lost: %v", err)
					}
				}
			}
		})
	}
}

// TestClient_CanceledContext: a request cut short by ctx returns ctx's
// error.
func TestClient_CanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	f := &fakeFetcher{respond: func(fetch.Request) (*fetch.Result, error) {
		cancel()
		return nil, fmt.Errorf("fetch x: %w", context.Canceled)
	}}
	if _, err := innertube.NewClient(f).Browse(ctx, testChannel, innertube.TabVideos, maxIDs); !errors.Is(err, context.Canceled) ||
		errors.Is(err, innertube.ErrBlocked) {
		t.Fatalf("err = %v", err)
	}
}
