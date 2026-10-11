// Package fetch is the outbound HTTP client: conditional GET, redirect
// tracking, response size cap, and status classification, plus a bounded
// POST for APIs that need one (see post.go).
package fetch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/safeurl"
)

// FeedAccept is the Accept header sent when fetching feeds.
const FeedAccept = "application/atom+xml, application/rss+xml, application/feed+json, application/json;q=0.9, application/xml;q=0.9, text/xml;q=0.9, */*;q=0.8"

// HTMLAccept is the Accept header sent when fetching web pages for feed
// discovery.
const HTMLAccept = "text/html, application/xhtml+xml;q=0.9, */*;q=0.8"

// Request is one GET, or one POST when Method is http.MethodPost.
type Request struct {
	URL          string
	ETag         string // sent as If-None-Match when non-empty (GET only)
	LastModified string // sent as If-Modified-Since when non-empty (GET only)
	Accept       string // "" = FeedAccept
	// Method is "" or http.MethodGet for a GET, http.MethodPost for a POST
	// of Body with Content-Type ContentType (both unset for a GET). See
	// post.go for how a POST differs. Body is a string so that a Request
	// stays comparable.
	Method      string
	Body        string
	ContentType string
	// NoRedirects returns a redirect as a *StatusError (with its
	// Retry-After) instead of following it, for a GET that must reach no
	// other URL than its own. A POST never follows a redirect (post.go).
	NoRedirects bool
	// MaxBytes, when positive and below the client's cap, is the cap on
	// this response's (decompressed) body instead: for an API whose
	// answers are known to be much smaller than a feed may be.
	MaxBytes int64
}

// Result is a successful (200) or not-modified (304) response.
type Result struct {
	StatusCode   int  // 200 or 304
	NotModified  bool // StatusCode == 304
	Body         []byte
	ContentType  string // raw Content-Type header
	ETag         string
	LastModified string
	// FinalURL is the URL the response was served from, after redirects.
	FinalURL string
	// PermanentURL is non-empty when the requested URL was moved by one or
	// more leading 301/308 hops; it is the URL at the end of that permanent
	// run (a later 302/307 hop does not extend it). Empty when not moved.
	PermanentURL string
	// MaxAge is Cache-Control max-age; 0 when absent or invalid.
	MaxAge time.Duration
}

// StatusError is returned for any final status other than 200 and 304.
type StatusError struct {
	URL        string
	StatusCode int
	// RetryAfter is the parsed Retry-After header (delta-seconds or
	// HTTP-date); 0 when absent or invalid.
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("HTTP %d from %s", e.StatusCode, e.URL)
}

// errInvalidURL is a request or redirect URL that cannot be fetched as it
// is written.
var errInvalidURL = errors.New("invalid URL")

var (
	// ErrTooLarge: the (decompressed) body exceeded the configured cap.
	ErrTooLarge = errors.New("response body exceeds size limit")
	// ErrRedirects: a redirect loop or more than config.MaxRedirects hops.
	ErrRedirects = errors.New("redirect loop or too many redirects")
	// ErrScheme: only http and https URLs can be fetched.
	ErrScheme = errors.New("only http and https URLs are supported")
)

// drainLimit is how much of an unused response body is read before closing
// it, so that small error pages do not prevent connection reuse.
const drainLimit = 4 << 10

// Client performs fetches. It is safe for concurrent use.
//
// Errors are kept short and human-readable because callers store them as a
// feed's last_error: "fetch <url>: <reason>" for transport, redirect, size
// and timeout failures, and *StatusError ("HTTP 404 from <url>") for
// unexpected statuses. URLs in messages have their whole userinfo (user
// name and password) replaced by safeurl.Placeholder, and a URL that does
// not parse is not shown at all. The
// sentinels ErrScheme, ErrRedirects and ErrTooLarge, as well as
// context.Canceled and context.DeadlineExceeded, match via errors.Is.
type Client struct {
	hc        *http.Client
	userAgent string
	maxBody   int64
	timeout   time.Duration
	now       func() time.Time
}

// New returns a client using cfg.UserAgent and cfg.MaxBodyBytes, with
// per-request timeout config.FetchTimeout and redirect limit
// config.MaxRedirects.
//
// An empty cfg.UserAgent falls back to config.DefaultUserAgent and a
// non-positive cfg.MaxBodyBytes to config.DefaultMaxBodyBytes.
func New(cfg config.Config) *Client {
	ua := cfg.UserAgent
	if ua == "" {
		ua = config.DefaultUserAgent()
	}
	maxBody := cfg.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = config.DefaultMaxBodyBytes
	}
	return &Client{
		hc: &http.Client{
			Transport:     newTransport(),
			CheckRedirect: checkRedirect,
		},
		userAgent: ua,
		maxBody:   maxBody,
		timeout:   config.FetchTimeout,
		now:       time.Now,
	}
}

// Fetch performs a GET, or a POST (see Request.Method). Non-200/304 final
// statuses return *StatusError; for a POST only 200 is a success. A
// redirect is followed, unless the request is a POST or sets NoRedirects:
// then it is the final status.
//
// The request carries User-Agent, Accept (r.Accept or FeedAccept) and, when
// set, the If-None-Match / If-Modified-Since validators; the transport adds
// "Accept-Encoding: gzip" and transparently decompresses. The size cap
// applies to the decompressed body and config.FetchTimeout bounds the whole
// exchange, including reading the body.
//
// On 304 the Result has no body; its ETag and LastModified fall back to the
// request's validators when the server omits them, so callers can store the
// Result validators unconditionally.
func (c *Client) Fetch(ctx context.Context, r Request) (*Result, error) {
	target, err := parseTarget(r.URL)
	if err != nil {
		return nil, err
	}
	display := safeurl.Redacted(target)
	method, err := requestMethod(r)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", display, err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if r.NoRedirects {
		reqCtx = withoutRedirects(reqCtx)
	}

	req, err := c.newRequest(reqCtx, target, method, r)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // its URL is shown whole, credentials included
		}
		return nil, fmt.Errorf("fetch %s: %w", display, err)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		if resp != nil {
			// A CheckRedirect failure returns the last response too.
			resp.Body.Close()
		}
		return nil, c.requestError(ctx, reqCtx, display, err)
	}
	defer resp.Body.Close()

	finalURL := target
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL
	}
	notModified := resp.StatusCode == http.StatusNotModified && method == http.MethodGet
	if resp.StatusCode != http.StatusOK && !notModified {
		drain(resp.Body)
		return nil, &StatusError{
			URL:        safeurl.Redacted(finalURL),
			StatusCode: resp.StatusCode,
			RetryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), c.now()),
		}
	}

	res := &Result{
		StatusCode:   resp.StatusCode,
		NotModified:  resp.StatusCode == http.StatusNotModified,
		ContentType:  resp.Header.Get("Content-Type"),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		FinalURL:     finalURL.String(),
		PermanentURL: permanentURL(resp),
		MaxAge:       parseMaxAge(resp.Header.Values("Cache-Control")),
	}
	if res.NotModified {
		if res.ETag == "" {
			res.ETag = r.ETag
		}
		if res.LastModified == "" {
			res.LastModified = r.LastModified
		}
		return res, nil
	}

	body, err := c.readBody(resp, c.capFor(r))
	if err != nil {
		if errors.Is(err, ErrTooLarge) {
			return nil, fmt.Errorf("fetch %s: %w", display, err)
		}
		return nil, c.requestError(ctx, reqCtx, display, err)
	}
	res.Body = body
	return res, nil
}

// parseTarget validates a request URL before anything is dialed.
func parseTarget(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil {
		// Neither raw nor the parser's error is shown: both may quote
		// credentials (a password holding '#' reads as a port), and without
		// a parsed URL they cannot be told apart from the rest.
		return nil, fmt.Errorf("fetch: %w", errInvalidURL)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("fetch %s: %w", safeurl.Redacted(u), ErrScheme)
	}
	if u.Hostname() == "" {
		// Not just an empty Host: "http://:8080/" would dial this machine.
		return nil, fmt.Errorf("fetch %s: %w: missing host", safeurl.Redacted(u), errInvalidURL)
	}
	// The request is built from u.String(), and net/http quotes a string
	// that does not parse whole, credentials included: refuse a URL that
	// does not survive the round trip (a non-ASCII IPv6 zone) here.
	if _, err := url.Parse(u.String()); err != nil {
		return nil, fmt.Errorf("fetch %s: %w", safeurl.Redacted(u), errInvalidURL)
	}
	return u, nil
}

// newRequest builds the GET or POST (method, as checked by requestMethod)
// with identification, content negotiation and conditional headers or the
// body. Accept-Encoding is deliberately left to the transport, which then
// decompresses gzip transparently.
func (c *Client) newRequest(ctx context.Context, target *url.URL, method string, r Request) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, target.String(), requestBody(method, r))
	if err != nil {
		return nil, err
	}
	accept := r.Accept
	if accept == "" {
		accept = FeedAccept
	}
	req.Header.Set("User-Agent", c.userAgent)
	req.Header.Set("Accept", accept)
	if r.ContentType != "" {
		req.Header.Set("Content-Type", r.ContentType)
	}
	if r.ETag != "" {
		req.Header.Set("If-None-Match", r.ETag)
	}
	if r.LastModified != "" {
		req.Header.Set("If-Modified-Since", r.LastModified)
	}
	return req, nil
}

// capFor returns the body cap of request r: the client's, or r.MaxBytes
// when that is lower.
func (c *Client) capFor(r Request) int64 {
	if r.MaxBytes > 0 && r.MaxBytes < c.maxBody {
		return r.MaxBytes
	}
	return c.maxBody
}

// readBody reads the (already decompressed) body, failing with ErrTooLarge
// as soon as it exceeds maxBody so a compression bomb is never inflated in
// full.
func (c *Client) readBody(resp *http.Response, maxBody int64) ([]byte, error) {
	if resp.ContentLength > maxBody {
		return nil, tooLarge(maxBody)
	}
	limit := maxBody
	if limit < math.MaxInt64 {
		limit++ // one extra byte tells "exactly at the cap" from "over it"
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBody {
		return nil, tooLarge(maxBody)
	}
	return body, nil
}

func tooLarge(maxBody int64) error {
	return &reasonError{
		msg:    fmt.Sprintf("%v of %s", ErrTooLarge, formatBytes(maxBody)),
		target: ErrTooLarge,
	}
}

// requestError turns a transport or body-read error into a short message.
// Cancellation of the caller's ctx wins over the per-request timeout, so
// callers shutting down see context.Canceled.
func (c *Client) requestError(parent, reqCtx context.Context, display string, err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err // drop the noisy `Get "<url>":` prefix
	}
	switch {
	case parent.Err() != nil:
		return fmt.Errorf("fetch %s: %w", display, parent.Err())
	case errors.Is(reqCtx.Err(), context.DeadlineExceeded):
		return fmt.Errorf("fetch %s: %w", display, &reasonError{
			msg:    "timed out after " + c.timeout.String(),
			target: context.DeadlineExceeded,
		})
	default:
		return fmt.Errorf("fetch %s: %w", display, err)
	}
}

// reasonError carries a specific human-readable message while still matching
// a sentinel error via errors.Is.
type reasonError struct {
	msg    string
	target error
}

func (e *reasonError) Error() string { return e.msg }
func (e *reasonError) Unwrap() error { return e.target }

// drain discards a little of an unused body so the connection can be reused.
func drain(body io.Reader) {
	_, _ = io.CopyN(io.Discard, body, drainLimit)
}

// formatBytes renders a byte count for error messages ("10 MiB").
func formatBytes(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n >= 1<<10 && n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	default:
		return fmt.Sprintf("%d bytes", n)
	}
}
