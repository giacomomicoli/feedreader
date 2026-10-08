// Package resolve turns a URL pasted by the user into one or more feed URLs
// in five steps: (1) direct feeds, (2) YouTube channel/playlist URLs, (3)
// HTML <link rel="alternate"> autodiscovery, (4) well-known fallback paths,
// then (5) "no feed found".
package resolve

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/parse"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Fetcher is the subset of *fetch.Client the resolver needs.
type Fetcher interface {
	Fetch(ctx context.Context, r fetch.Request) (*fetch.Result, error)
}

var _ Fetcher = (*fetch.Client)(nil)

// Candidate is a discovered feed.
type Candidate struct {
	URL   string // absolute feed URL
	Title string // <link title>, may be ""
	Type  string // MIME type from <link type>, may be ""
}

// Result lists the feeds found; the first one is the pre-selected choice.
type Result struct {
	Candidates []Candidate // len >= 1
}

// User-facing errors.
var (
	ErrInvalidURL = errors.New("Please enter a valid http(s) URL")
	ErrNoFeed     = errors.New("No feed found at this URL")
	ErrYouTubeID  = errors.New("Could not resolve channel ID; paste the channel URL with /channel/UC… or the feed URL directly")
)

// errNoFetcher is returned when a resolution step needs HTTP but the
// Resolver was built without a Fetcher.
var errNoFetcher = errors.New("resolve: no fetcher configured")

// Resolver resolves user input to feed URLs. It holds no mutable state and
// is safe for concurrent use when its Fetcher is.
type Resolver struct {
	f Fetcher
}

// New returns a resolver that performs HTTP through f.
func New(f Fetcher) *Resolver { return &Resolver{f: f} }

// Resolve runs the steps listed in the package comment. Input without a
// scheme is treated as https://.
//
// YouTube channel, playlist and feed URLs are answered without any request;
// /@handle, /c/… and /user/… pages are fetched once to read the channel ID.
// Any other URL is fetched once: a feed is used as-is (or at its permanent
// redirect target), an HTML page is searched for <link rel="alternate">
// feeds, and otherwise the well-known fallback paths are probed in order on the
// site origin, stopping at the first one that is a feed.
//
// Errors match ErrInvalidURL, ErrNoFeed or ErrYouTubeID via errors.Is when
// they have a user-facing meaning; network failures are returned wrapped
// (fetch errors, context.Canceled, …) so the caller can tell "the site is
// unreachable" from "no feed here".
func (r *Resolver) Resolve(ctx context.Context, raw string) (*Result, error) {
	u, err := normalizeInput(raw)
	if err != nil {
		return nil, err
	}
	if isYouTubeHost(u) {
		if ref := classifyYouTube(u); ref.kind != ytOther {
			return r.resolveYouTube(ctx, u, ref)
		}
	}
	return r.resolveGeneric(ctx, u)
}

// KindForURL returns store.KindYouTube for YouTube feed URLs
// (youtube.com/feeds/videos.xml) and store.KindRSS otherwise.
func KindForURL(feedURL string) store.Kind {
	u, err := url.Parse(strings.TrimSpace(feedURL))
	if err != nil || !isHTTPURL(u) || !isYouTubeHost(u) || u.Path != ytFeedPath {
		return store.KindRSS
	}
	return store.KindYouTube
}

// resolveGeneric runs steps 1, 3, 4 and 5 for a non-YouTube URL.
func (r *Resolver) resolveGeneric(ctx context.Context, u *url.URL) (*Result, error) {
	input := u.String()
	page, pageErr := r.fetch(ctx, input, fetch.FeedAccept)
	if pageErr != nil && !isURLLevelError(pageErr) {
		return nil, fmt.Errorf("resolve %s: %w", u.Redacted(), pageErr)
	}
	probed := map[string]bool{input: true}
	site := u
	if pageErr == nil {
		if res := fromPage(page, u); res != nil {
			return res, nil
		}
		site = finalURL(page, u)
		probed[site.String()] = true
	}
	// A page that answered with an HTTP error still gets its fallbacks
	// probed: the site may well serve /feed.
	c, found, err := r.probeFallbacks(ctx, site, probed)
	switch {
	case err != nil:
		return nil, fmt.Errorf("resolve %s: %w", u.Redacted(), err)
	case found:
		return single(c), nil
	case pageErr != nil:
		// Step 5, keeping the page's own failure for the logs.
		return nil, fmt.Errorf("resolve %s: %w (page: %w)", u.Redacted(), ErrNoFeed, pageErr)
	default:
		return nil, fmt.Errorf("resolve %s: %w", u.Redacted(), ErrNoFeed)
	}
}

// fromPage applies steps 1 (the URL is a feed) and 3 (HTML autodiscovery)
// to the response for requested; nil means neither found a feed.
func fromPage(page *fetch.Result, requested *url.URL) *Result {
	if c, ok := directFeed(page, requested.String()); ok {
		return single(c)
	}
	if !looksLikeHTML(page) {
		return nil
	}
	cands := discoverLinks(page.Body, page.ContentType, finalURL(page, requested))
	if len(cands) == 0 {
		return nil
	}
	return &Result{Candidates: cands}
}

// fetch performs one GET through the Fetcher, refusing to start once ctx is
// done so that a cancelled resolution stops between requests.
func (r *Resolver) fetch(ctx context.Context, rawURL, accept string) (*fetch.Result, error) {
	if r == nil || r.f == nil {
		return nil, errNoFetcher
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	res, err := r.f.Fetch(ctx, fetch.Request{URL: rawURL, Accept: accept})
	if err != nil {
		return nil, err
	}
	if res == nil {
		return nil, fmt.Errorf("fetch %s: no result", rawURL)
	}
	return res, nil
}

// directFeed reports whether res is itself a feed. The candidate is the
// permanent redirect target when the URL has moved (so the new location is
// what gets stored), else requested.
func directFeed(res *fetch.Result, requested string) (Candidate, bool) {
	if res.NotModified || len(res.Body) == 0 || !parse.Detect(res.Body, res.ContentType) {
		return Candidate{}, false
	}
	if res.PermanentURL != "" {
		return Candidate{URL: res.PermanentURL}, true
	}
	return Candidate{URL: requested}, true
}

// isURLLevelError reports whether err means the server answered but this
// particular URL is unusable (HTTP error status, oversized body, redirect
// loop, redirect to a non-http scheme). Other errors — DNS, connection,
// TLS, timeouts, cancellation — mean further requests to the same site are
// pointless.
func isURLLevelError(err error) bool {
	var se *fetch.StatusError
	return errors.As(err, &se) ||
		errors.Is(err, fetch.ErrTooLarge) ||
		errors.Is(err, fetch.ErrRedirects) ||
		errors.Is(err, fetch.ErrScheme)
}

// finalURL is the URL the response was served from, falling back to the
// requested one when the Fetcher did not report a usable FinalURL.
func finalURL(res *fetch.Result, requested *url.URL) *url.URL {
	if res.FinalURL == "" {
		return requested
	}
	u, err := url.Parse(res.FinalURL)
	if err != nil || !isHTTPURL(u) {
		return requested
	}
	return u
}

func single(c Candidate) *Result { return &Result{Candidates: []Candidate{c}} }
