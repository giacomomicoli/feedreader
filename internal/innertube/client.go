package innertube

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/giacomomicoli/feedreader/internal/fetch"
)

// Fetcher is the subset of *fetch.Client the Client needs.
type Fetcher interface {
	Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error)
}

var _ Fetcher = (*fetch.Client)(nil)

// Client reads a channel's videos from YouTube's web API, as YouTube's web
// client (Web): the IDs a tab of the channel's page lists (Browse), then
// each video's details (Video). It makes exactly one request per call,
// none for an invalid ID, to the fixed endpoints BrowseURL and PlayerURL,
// never following a redirect; the pacing and the number of calls are the
// caller's. It holds no state and is safe for concurrent use when its
// Fetcher is.
//
// Errors match ErrBlocked, ErrRejected, ErrAlert, ErrNoTab, ErrTooLarge,
// ErrFormat or ErrSkipVideo via errors.Is when they mean that, and keep
// the Fetcher's error reachable (a *fetch.StatusError keeps its
// Retry-After). Messages hold nothing from the answer but a status token.
type Client struct {
	f Fetcher
}

// NewClient returns a Client that performs HTTP through f.
func NewClient(f Fetcher) *Client { return &Client{f: f} }

// Browse returns the IDs of the newest videos tab of channel channelID
// lists, at most max (see ParseBrowse).
func (c *Client) Browse(ctx context.Context, channelID string, tab Tab, max int) ([]string, error) {
	if !ChannelIDPattern.MatchString(channelID) {
		return nil, errInvalidID
	}
	res, err := c.f.Fetch(ctx, BrowseRequest(Web, channelID, tab))
	if err != nil {
		return nil, requestError(ctx, "browse request", err)
	}
	return ParseBrowse(res.Body, res.ContentType, tab.Params, max)
}

// Video returns the details of video id of channel channelID (see
// ParseVideo).
func (c *Client) Video(ctx context.Context, id, channelID string) (Video, error) {
	if !VideoIDPattern.MatchString(id) || !ChannelIDPattern.MatchString(channelID) {
		return Video{}, errInvalidID
	}
	res, err := c.f.Fetch(ctx, VideoRequest(id))
	if err != nil {
		return Video{}, requestError(ctx, "player request", err)
	}
	return ParseVideo(res.Body, res.ContentType, id, channelID)
}

// requestError classifies an error of the Fetcher for request step:
// statuses that mean YouTube refuses this client are ErrBlocked, a 400
// ErrRejected, an answer over its cap ErrTooLarge. A cancelled ctx returns
// ctx's error.
func requestError(ctx context.Context, step string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var kind error
	var se *fetch.StatusError
	switch {
	case errors.As(err, &se) && BlockedStatus(se.StatusCode):
		kind = ErrBlocked
	case errors.As(err, &se) && se.StatusCode == http.StatusBadRequest:
		kind = ErrRejected
	case errors.Is(err, fetch.ErrTooLarge):
		kind = ErrTooLarge
	}
	if kind == nil {
		return fmt.Errorf("innertube: %s: %w", step, err)
	}
	return fmt.Errorf("%w: %s: %w", kind, step, err)
}
