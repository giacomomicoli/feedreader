package fetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
)

const testUA = "feedreader-test/1.0 (+https://example.invalid/repo)"

const sampleFeed = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>Sample</title><id>urn:x</id></feed>`

// newTestClient returns a client with a small body cap so size tests stay
// cheap; idle connections are closed when the test ends.
func newTestClient(t *testing.T, maxBody int64) *Client {
	t.Helper()
	cfg := config.Default()
	cfg.UserAgent = testUA
	cfg.MaxBodyBytes = maxBody
	c := New(cfg)
	t.Cleanup(c.hc.CloseIdleConnections)
	return c
}

func newServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// roundTripFunc lets a test observe whether the transport is ever reached.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetch_SendsUserAgentAndDefaultFeedAccept(t *testing.T) {
	var got http.Header
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, sampleFeed)
	})
	c := newTestClient(t, 1<<20)

	if _, err := c.Fetch(t.Context(), Request{URL: srv.URL}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if ua := got.Get("User-Agent"); ua != testUA {
		t.Errorf("User-Agent = %q, want %q", ua, testUA)
	}
	if a := got.Get("Accept"); a != FeedAccept {
		t.Errorf("Accept = %q, want FeedAccept", a)
	}
	for _, h := range []string{"If-None-Match", "If-Modified-Since"} {
		if v, ok := got[h]; ok {
			t.Errorf("%s sent without a stored validator: %q", h, v)
		}
	}
}

func TestFetch_CustomAcceptForHTMLDiscovery(t *testing.T) {
	var accept string
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		accept = r.Header.Get("Accept")
		fmt.Fprint(w, "<html></html>")
	})
	c := newTestClient(t, 1<<20)

	if _, err := c.Fetch(t.Context(), Request{URL: srv.URL, Accept: HTMLAccept}); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if accept != HTMLAccept {
		t.Errorf("Accept = %q, want HTMLAccept", accept)
	}
}

func TestNew_ZeroConfigFallsBackToDefaults(t *testing.T) {
	c := New(config.Config{})
	if c.userAgent != config.DefaultUserAgent() {
		t.Errorf("userAgent = %q, want %q", c.userAgent, config.DefaultUserAgent())
	}
	if c.maxBody != config.DefaultMaxBodyBytes {
		t.Errorf("maxBody = %d, want %d", c.maxBody, config.DefaultMaxBodyBytes)
	}
	if c.timeout != config.FetchTimeout {
		t.Errorf("timeout = %v, want %v", c.timeout, config.FetchTimeout)
	}
}

func TestNew_DedicatedTransportWithTimeoutsAndProxy(t *testing.T) {
	c := New(config.Default())
	tr, ok := c.hc.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("Transport is %T, want *http.Transport", c.hc.Transport)
	}
	if tr == http.DefaultTransport {
		t.Error("client shares http.DefaultTransport")
	}
	if tr.TLSHandshakeTimeout <= 0 || tr.ResponseHeaderTimeout <= 0 || tr.IdleConnTimeout <= 0 {
		t.Errorf("missing timeouts: tls=%v header=%v idle=%v",
			tr.TLSHandshakeTimeout, tr.ResponseHeaderTimeout, tr.IdleConnTimeout)
	}
	if tr.Proxy == nil {
		t.Error("Proxy is nil, want ProxyFromEnvironment")
	}
	if tr.DisableCompression {
		t.Error("DisableCompression = true, want transparent gzip")
	}
	if c.hc.CheckRedirect == nil {
		t.Error("CheckRedirect not set")
	}
}

func TestFetch_ConditionalGET_SendsStoredValidators(t *testing.T) {
	const etag = `"abc123"`
	const lastMod = "Wed, 07 Oct 2026 10:00:00 GMT"
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != etag || r.Header.Get("If-Modified-Since") != lastMod {
			t.Errorf("validators = %q / %q", r.Header.Get("If-None-Match"), r.Header.Get("If-Modified-Since"))
		}
		w.WriteHeader(http.StatusNotModified)
	})
	c := newTestClient(t, 1<<20)

	res, err := c.Fetch(t.Context(), Request{URL: srv.URL, ETag: etag, LastModified: lastMod})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !res.NotModified || res.StatusCode != http.StatusNotModified {
		t.Errorf("NotModified=%v StatusCode=%d, want true/304", res.NotModified, res.StatusCode)
	}
	if len(res.Body) != 0 {
		t.Errorf("Body = %q, want empty on 304", res.Body)
	}
}

func TestFetch_304_KeepsStoredValidatorsWhenOmitted(t *testing.T) {
	const oldETag = `"v1"`
	const oldLM = "Wed, 07 Oct 2026 10:00:00 GMT"
	tests := []struct {
		name             string
		respETag, respLM string
		wantETag, wantLM string
	}{
		{name: "server omits validators", wantETag: oldETag, wantLM: oldLM},
		{name: "server sends fresh ETag", respETag: `"v2"`, wantETag: `"v2"`, wantLM: oldLM},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.respETag != "" {
					w.Header().Set("ETag", tt.respETag)
				}
				if tt.respLM != "" {
					w.Header().Set("Last-Modified", tt.respLM)
				}
				w.WriteHeader(http.StatusNotModified)
			})
			c := newTestClient(t, 1<<20)
			res, err := c.Fetch(t.Context(), Request{URL: srv.URL, ETag: oldETag, LastModified: oldLM})
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if res.ETag != tt.wantETag || res.LastModified != tt.wantLM {
				t.Errorf("validators = %q / %q, want %q / %q", res.ETag, res.LastModified, tt.wantETag, tt.wantLM)
			}
		})
	}
}

func TestFetch_200_CapturesBodyAndValidators(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
		w.Header().Set("ETag", `W/"xyz"`)
		w.Header().Set("Last-Modified", "Thu, 08 Oct 2026 09:00:00 GMT")
		fmt.Fprint(w, sampleFeed)
	})
	c := newTestClient(t, 1<<20)

	res, err := c.Fetch(t.Context(), Request{URL: srv.URL + "/feed.xml"})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	want := Result{
		StatusCode:   http.StatusOK,
		ContentType:  "application/atom+xml; charset=utf-8",
		ETag:         `W/"xyz"`,
		LastModified: "Thu, 08 Oct 2026 09:00:00 GMT",
		FinalURL:     srv.URL + "/feed.xml",
	}
	if res.StatusCode != want.StatusCode || res.NotModified || res.ContentType != want.ContentType ||
		res.ETag != want.ETag || res.LastModified != want.LastModified ||
		res.FinalURL != want.FinalURL || res.PermanentURL != "" || res.MaxAge != 0 {
		t.Errorf("Result = %+v\nwant   %+v", *res, want)
	}
	if string(res.Body) != sampleFeed {
		t.Errorf("Body = %q", res.Body)
	}
}

func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestFetch_AcceptEncodingGzip_BodyIsDecompressed(t *testing.T) {
	compressed := gzipBytes(t, []byte(sampleFeed))
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if ae := r.Header.Get("Accept-Encoding"); ae != "gzip" {
			t.Errorf("Accept-Encoding = %q, want gzip", ae)
		}
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Type", "application/atom+xml")
		w.Write(compressed)
	})
	c := newTestClient(t, 1<<20)

	res, err := c.Fetch(t.Context(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if string(res.Body) != sampleFeed {
		t.Errorf("Body = %q, want decompressed feed", res.Body)
	}
}

func TestFetch_BodySizeCap(t *testing.T) {
	const limit = 1024
	bomb := gzipBytes(t, make([]byte, 256<<10)) // 256 KiB of zeros
	if len(bomb) >= limit {
		t.Fatalf("test setup: compressed bomb is %d bytes, must be under the %d cap", len(bomb), limit)
	}
	tests := []struct {
		name    string
		handler http.HandlerFunc
		wantErr bool
	}{
		{
			name: "exactly at the cap is accepted",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Write(bytes.Repeat([]byte("a"), limit))
			},
		},
		{
			name: "declared Content-Length over the cap",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Length", fmt.Sprint(limit+1))
				w.Write(bytes.Repeat([]byte("a"), limit+1))
			},
			wantErr: true,
		},
		{
			name: "chunked body over the cap",
			handler: func(w http.ResponseWriter, r *http.Request) {
				for range 4 {
					w.Write(bytes.Repeat([]byte("a"), limit/2))
					w.(http.Flusher).Flush()
				}
			},
			wantErr: true,
		},
		{
			name: "gzip bomb is capped after decompression",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Encoding", "gzip")
				w.Write(bomb)
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, tt.handler)
			c := newTestClient(t, limit)
			res, err := c.Fetch(t.Context(), Request{URL: srv.URL + "/big"})
			if !tt.wantErr {
				if err != nil {
					t.Fatalf("Fetch: %v", err)
				}
				if len(res.Body) != limit {
					t.Fatalf("len(Body) = %d, want %d", len(res.Body), limit)
				}
				return
			}
			if !errors.Is(err, ErrTooLarge) {
				t.Fatalf("err = %v, want ErrTooLarge", err)
			}
			want := "fetch " + srv.URL + "/big: response body exceeds size limit of 1 KiB"
			if err.Error() != want {
				t.Errorf("message = %q, want %q", err.Error(), want)
			}
		})
	}
}

func TestFetch_UnexpectedStatusReturnsStatusError(t *testing.T) {
	for _, code := range []int{
		http.StatusNotFound, http.StatusGone, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusServiceUnavailable,
		http.StatusNoContent, http.StatusMultipleChoices,
	} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(code)
				fmt.Fprint(w, "error page")
			})
			c := newTestClient(t, 1<<20)
			_, err := c.Fetch(t.Context(), Request{URL: srv.URL + "/feed"})
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v (%T), want *StatusError", err, err)
			}
			if se.StatusCode != code || se.URL != srv.URL+"/feed" || se.RetryAfter != 0 {
				t.Errorf("StatusError = %+v", *se)
			}
			if want := fmt.Sprintf("HTTP %d from %s/feed", code, srv.URL); err.Error() != want {
				t.Errorf("message = %q, want %q", err.Error(), want)
			}
		})
	}
}

func TestFetch_StatusErrorReportsURLAfterRedirect(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/old" {
			http.Redirect(w, r, "/gone", http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusGone)
	})
	c := newTestClient(t, 1<<20)
	_, err := c.Fetch(t.Context(), Request{URL: srv.URL + "/old"})
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusGone || se.URL != srv.URL+"/gone" {
		t.Fatalf("err = %v, want HTTP 410 from %s/gone", err, srv.URL)
	}
}

func TestFetch_ErrorMessagesRedactPasswords(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	c := newTestClient(t, 1<<20)
	u := strings.Replace(srv.URL, "http://", "http://user:s3cret@", 1)
	_, err := c.Fetch(t.Context(), Request{URL: u})
	var se *StatusError
	if !errors.As(err, &se) {
		t.Fatalf("err = %v, want *StatusError", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("password leaked into message %q", err.Error())
	}
}

func TestFetch_RetryAfterOnBackoffStatuses(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		status int
		header string
		want   time.Duration
	}{
		{"429 delta-seconds", http.StatusTooManyRequests, "120", 2 * time.Minute},
		{"503 HTTP-date", http.StatusServiceUnavailable, now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second},
		{"429 date in the past", http.StatusTooManyRequests, now.Add(-time.Hour).Format(http.TimeFormat), 0},
		{"503 negative", http.StatusServiceUnavailable, "-5", 0},
		{"429 garbage", http.StatusTooManyRequests, "soon", 0},
		{"503 absent", http.StatusServiceUnavailable, "", 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				if tt.header != "" {
					w.Header().Set("Retry-After", tt.header)
				}
				w.WriteHeader(tt.status)
			})
			c := newTestClient(t, 1<<20)
			c.now = func() time.Time { return now }
			_, err := c.Fetch(t.Context(), Request{URL: srv.URL})
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *StatusError", err)
			}
			if se.StatusCode != tt.status || se.RetryAfter != tt.want {
				t.Errorf("StatusCode=%d RetryAfter=%v, want %d/%v", se.StatusCode, se.RetryAfter, tt.status, tt.want)
			}
		})
	}
}

func TestFetch_MaxAgeFromCacheControl(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Cache-Control", "public")
		w.Header().Add("Cache-Control", "max-age=3600")
		fmt.Fprint(w, sampleFeed)
	})
	c := newTestClient(t, 1<<20)
	res, err := c.Fetch(t.Context(), Request{URL: srv.URL})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.MaxAge != time.Hour {
		t.Errorf("MaxAge = %v, want 1h", res.MaxAge)
	}
}

func TestFetch_RejectsNonHTTPSchemesBeforeDialing(t *testing.T) {
	for _, raw := range []string{
		"ftp://example.com/feed",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"gopher://example.com/",
		"example.com/feed",
		"",
	} {
		t.Run(raw, func(t *testing.T) {
			c := newTestClient(t, 1<<20)
			c.hc.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				t.Errorf("transport reached for %s", r.URL)
				return nil, errors.New("unreachable")
			})
			_, err := c.Fetch(t.Context(), Request{URL: raw})
			if !errors.Is(err, ErrScheme) {
				t.Fatalf("err = %v, want ErrScheme", err)
			}
		})
	}
}

func TestFetch_RejectsMalformedURLsBeforeDialing(t *testing.T) {
	for _, raw := range []string{"http:///feed", "https://exa mple.com/", "http://[::1/feed"} {
		t.Run(raw, func(t *testing.T) {
			c := newTestClient(t, 1<<20)
			c.hc.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				t.Errorf("transport reached for %s", r.URL)
				return nil, errors.New("unreachable")
			})
			if _, err := c.Fetch(t.Context(), Request{URL: raw}); err == nil {
				t.Fatal("Fetch succeeded, want an error")
			}
		})
	}
}

func TestFetch_AcceptsUppercaseSchemeAndSurroundingSpace(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, sampleFeed) })
	c := newTestClient(t, 1<<20)
	raw := "  HTTP://" + strings.TrimPrefix(srv.URL, "http://") + "/feed \n"
	res, err := c.Fetch(t.Context(), Request{URL: raw})
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if res.FinalURL != srv.URL+"/feed" {
		t.Errorf("FinalURL = %q", res.FinalURL)
	}
}

// blockingServer accepts a request, optionally writes part of a body, then
// stalls until the client goes away. started is closed once the handler runs.
func blockingServer(t *testing.T, partialBody bool) (srv *httptest.Server, started <-chan struct{}) {
	t.Helper()
	ch := make(chan struct{})
	var once sync.Once
	srv = newServer(t, func(w http.ResponseWriter, r *http.Request) {
		if partialBody {
			w.Write([]byte("<rss><channel>"))
			w.(http.Flusher).Flush()
		}
		once.Do(func() { close(ch) })
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
	})
	return srv, ch
}

func TestFetch_ContextCancelAbortsRequest(t *testing.T) {
	srv, started := blockingServer(t, false)
	c := newTestClient(t, 1<<20)
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-started
		cancel()
	}()

	begin := time.Now()
	_, err := c.Fetch(ctx, Request{URL: srv.URL})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(begin); elapsed > 5*time.Second {
		t.Errorf("Fetch took %v after cancel", elapsed)
	}
}

func TestFetch_CanceledContextMakesNoRequest(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	})
	c := newTestClient(t, 1<<20)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	if _, err := c.Fetch(ctx, Request{URL: srv.URL}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Errorf("server saw %d requests, want 0", hits)
	}
}

func TestFetch_PerRequestTimeout(t *testing.T) {
	for _, tt := range []struct {
		name        string
		partialBody bool
	}{
		{"waiting for headers", false},
		{"covers reading the body", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			srv, _ := blockingServer(t, tt.partialBody)
			c := newTestClient(t, 1<<20)
			c.timeout = 100 * time.Millisecond

			_, err := c.Fetch(t.Context(), Request{URL: srv.URL})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("err = %v, want context.DeadlineExceeded", err)
			}
			if want := "fetch " + srv.URL + ": timed out after 100ms"; err.Error() != want {
				t.Errorf("message = %q, want %q", err.Error(), want)
			}
		})
	}
}

func TestFetch_TransportErrorMessageIsShort(t *testing.T) {
	c := newTestClient(t, 1<<20)
	c.hc.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection refused")
	})
	_, err := c.Fetch(t.Context(), Request{URL: "https://feeds.example.invalid/rss"})
	if want := "fetch https://feeds.example.invalid/rss: connection refused"; err == nil || err.Error() != want {
		t.Errorf("err = %v, want %q", err, want)
	}
}

func TestFetch_SafeForConcurrentUse(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"`+r.URL.Path+`"`)
		io.WriteString(w, r.URL.Path)
	})
	c := newTestClient(t, 1<<20)

	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			path := fmt.Sprintf("/feed/%d", i)
			res, err := c.Fetch(t.Context(), Request{URL: srv.URL + path})
			if err != nil {
				t.Errorf("Fetch %s: %v", path, err)
				return
			}
			if string(res.Body) != path || res.ETag != `"`+path+`"` {
				t.Errorf("Fetch %s: body %q etag %q", path, res.Body, res.ETag)
			}
		})
	}
	wg.Wait()
}

func TestFormatBytes(t *testing.T) {
	for n, want := range map[int64]string{
		config.DefaultMaxBodyBytes: "10 MiB",
		1024:                       "1 KiB",
		1500:                       "1500 bytes",
	} {
		if got := formatBytes(n); got != want {
			t.Errorf("formatBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

// TestFetch_ErrorsNeverShowURLCredentials: a feed URL's user name can be a
// token on its own; error messages end up in logs and in last_error.
func TestFetch_ErrorsNeverShowURLCredentials(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loop":
			http.Redirect(w, r, "http://tok3n@"+r.Host+"/loop", http.StatusFound)
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	closed := httptest.NewServer(http.NotFoundHandler())
	closed.Close()
	withCreds := func(base, userinfo, path string) string {
		return strings.Replace(base, "://", "://"+userinfo+"@", 1) + path
	}
	c := newTestClient(t, 1<<20)
	for _, userinfo := range []string{"tok3n", "reader:s3cret"} {
		for name, raw := range map[string]string{
			"status":      withCreds(srv.URL, userinfo, "/feed"),
			"redirect":    withCreds(srv.URL, userinfo, "/loop"),
			"unreachable": withCreds(closed.URL, userinfo, "/feed"),
			"scheme":      "ftp://" + userinfo + "@files.example/feed",
			"no host":     "https://" + userinfo + "@/feed",
			"unparsable":  "https://" + userinfo + "@[::1/feed",
			// Parses, but its String() does not: net/http would quote it.
			"zone":           "https://" + userinfo + "@[fe80::1%25é]/feed",
			"empty hostname": "http://" + userinfo + "@:8080/feed",
		} {
			_, err := c.Fetch(t.Context(), Request{URL: raw})
			if err == nil {
				t.Fatalf("%s %s: no error", userinfo, name)
			}
			msg := err.Error()
			var se *StatusError
			if errors.As(err, &se) {
				msg += " " + se.URL
			}
			for _, secret := range []string{"tok3n", "reader", "s3cret"} {
				if strings.Contains(msg, secret) {
					t.Errorf("%s %s: %q shows %q", userinfo, name, msg, secret)
				}
			}
		}
	}
	// A password with '#', '?' or '/' in it makes url.Parse read it as a
	// port, and its error quotes that port.
	for _, raw := range []string{
		"https://reader:s3cret#x@files.example/feed",
		"https://reader:s3cret?x@files.example/feed",
		"https://reader:s3/cret@files.example/feed",
	} {
		_, err := c.Fetch(t.Context(), Request{URL: raw})
		if err == nil || strings.Contains(err.Error(), "s3") {
			t.Errorf("%s: err = %v; want an error without the password", raw, err)
		}
	}
}

func TestFetch_URLWithoutHostNameIsRefusedWithoutRequest(t *testing.T) {
	c := newTestClient(t, 1<<20)
	c.hc.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Error("transport reached")
		return nil, errors.New("unreachable")
	})
	for _, raw := range []string{"http://:8080/feed", "https:///feed"} {
		if _, err := c.Fetch(t.Context(), Request{URL: raw}); err == nil || !strings.Contains(err.Error(), "missing host") {
			t.Errorf("%s: err = %v; want missing host", raw, err)
		}
	}
}

func TestFetch_RedirectToURLWithoutHostNameIsRefused(t *testing.T) {
	var dialed bool
	local := newServer(t, func(w http.ResponseWriter, r *http.Request) { dialed = true })
	port := local.URL[strings.LastIndex(local.URL, ":"):]
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://tok3n@"+port+"/feed", http.StatusMovedPermanently)
	})
	c := newTestClient(t, 1<<20)
	_, err := c.Fetch(t.Context(), Request{URL: srv.URL + "/feed"})
	if err == nil || !strings.Contains(err.Error(), "without a host name") || strings.Contains(err.Error(), "tok3n") {
		t.Errorf("err = %v; want a refused redirect without the user name", err)
	}
	if dialed {
		t.Error("the host-less redirect target was fetched")
	}
}

func TestFetch_InvalidURLMessagesNameFetchOnce(t *testing.T) {
	c := newTestClient(t, 1<<20)
	for _, raw := range []string{"https://tok3n@[::1/feed", "https://tok3n@[fe80::1%25é]/feed", "http://:8080/feed"} {
		_, err := c.Fetch(t.Context(), Request{URL: raw})
		if err == nil || strings.Count(err.Error(), "fetch") != 1 || !strings.Contains(err.Error(), "invalid URL") {
			t.Errorf("%s: err = %v; want one \"fetch\" and \"invalid URL\"", raw, err)
		}
	}
}
