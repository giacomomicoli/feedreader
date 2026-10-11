package fetch

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testJSONType = "application/json"
	testJSONBody = `{"query":"synthetic"}`
)

func postRequest(rawURL string) Request {
	return Request{
		URL:         rawURL,
		Method:      http.MethodPost,
		Body:        testJSONBody,
		ContentType: testJSONType,
		Accept:      testJSONType,
	}
}

func TestFetch_PostSendsBodyAndHeaders(t *testing.T) {
	var (
		method string
		header http.Header
		body   []byte
		length int64
	)
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		method, header, length = r.Method, r.Header.Clone(), r.ContentLength
		body, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", testJSONType)
		fmt.Fprint(w, `{"ok":true}`)
	})
	c := newTestClient(t, 1<<20)

	res, err := c.Fetch(t.Context(), postRequest(srv.URL+"/api"))
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if method != http.MethodPost || string(body) != testJSONBody || length != int64(len(testJSONBody)) {
		t.Errorf("server got %s %q (Content-Length %d)", method, body, length)
	}
	for name, want := range map[string]string{
		"Content-Type": testJSONType,
		"Accept":       testJSONType,
		"User-Agent":   testUA,
	} {
		if got := header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	for _, h := range []string{"If-None-Match", "If-Modified-Since"} {
		if v, ok := header[h]; ok {
			t.Errorf("%s sent on a POST: %q", h, v)
		}
	}
	if res.StatusCode != http.StatusOK || string(res.Body) != `{"ok":true}` || res.ContentType != testJSONType {
		t.Errorf("Result = %+v", res)
	}
}

func TestFetch_GetIsStillTheDefaultMethod(t *testing.T) {
	var method string
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		fmt.Fprint(w, sampleFeed)
	})
	c := newTestClient(t, 1<<20)
	for _, m := range []string{"", http.MethodGet} {
		if _, err := c.Fetch(t.Context(), Request{URL: srv.URL, Method: m}); err != nil {
			t.Fatalf("Fetch(Method %q): %v", m, err)
		}
		if method != http.MethodGet {
			t.Errorf("Method %q sent %s", m, method)
		}
	}
}

// TestFetch_PostRedirectIsNotFollowed: net/http would turn a 301/302/303
// into a GET of the new location and re-send the body on a 307/308; a POST
// returns the redirect as a StatusError instead.
func TestFetch_PostRedirectIsNotFollowed(t *testing.T) {
	const retryAfter = 2 * time.Minute
	for _, code := range []int{
		http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
	} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var followed atomic.Int32
			srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/target" {
					followed.Add(1)
					fmt.Fprint(w, `{}`)
					return
				}
				w.Header().Set("Location", "/target")
				w.Header().Set("Retry-After", fmt.Sprint(int(retryAfter.Seconds())))
				w.WriteHeader(code)
			})
			c := newTestClient(t, 1<<20)

			_, err := c.Fetch(t.Context(), postRequest(srv.URL+"/api"))
			var se *StatusError
			if !errors.As(err, &se) {
				t.Fatalf("err = %v, want *StatusError", err)
			}
			if se.StatusCode != code || se.URL != srv.URL+"/api" || se.RetryAfter != retryAfter {
				t.Errorf("StatusError = %+v", *se)
			}
			if n := followed.Load(); n != 0 {
				t.Errorf("redirect target requested %d times", n)
			}
		})
	}
}

func TestFetch_GetRedirectStillFollowedAfterAPost(t *testing.T) {
	srv := redirectServer(t, map[string]hop{"/a": {http.StatusFound, "/b"}})
	c := newTestClient(t, 1<<20)
	if _, err := c.Fetch(t.Context(), postRequest(srv.URL+"/a")); err == nil {
		t.Fatal("POST redirect: no error")
	}
	res, err := c.Fetch(t.Context(), Request{URL: srv.URL + "/a"})
	if err != nil || res.FinalURL != srv.URL+"/b" {
		t.Fatalf("GET = %+v, %v; want the redirect followed", res, err)
	}
}

func TestFetch_Post304IsAnError(t *testing.T) {
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	c := newTestClient(t, 1<<20)
	_, err := c.Fetch(t.Context(), postRequest(srv.URL))
	var se *StatusError
	if !errors.As(err, &se) || se.StatusCode != http.StatusNotModified {
		t.Fatalf("err = %v, want HTTP 304 as a StatusError", err)
	}
}

func TestFetch_PostResponseSizeCap(t *testing.T) {
	const maxBody = 1 << 10
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("x", maxBody+1))
	})
	c := newTestClient(t, maxBody)
	if _, err := c.Fetch(t.Context(), postRequest(srv.URL)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
}

func TestFetch_InvalidMethodFieldsRefusedBeforeDialing(t *testing.T) {
	const target = "https://api.example.invalid/v1"
	post := postRequest(target)
	withETag, withLastModified, noType, huge := post, post, post, post
	withETag.ETag = `"v1"`
	withLastModified.LastModified = "Sat, 10 Oct 2026 00:00:00 GMT"
	noType.ContentType = ""
	huge.Body = strings.Repeat("x", maxPostBodyBytes+1)
	for name, r := range map[string]Request{
		"GET with a body":         {URL: target, Body: "x"},
		"GET with a content type": {URL: target, ContentType: testJSONType},
		"conditional POST (ETag)": withETag,
		"conditional POST (date)": withLastModified,
		"POST without a type":     noType,
		"POST body over the cap":  huge,
		"other method":            {URL: target, Method: http.MethodPut},
		"lower-case method":       {URL: target, Method: "post", Body: "x", ContentType: testJSONType},
	} {
		t.Run(name, func(t *testing.T) {
			c := newTestClient(t, 1<<20)
			c.hc.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
				t.Errorf("transport reached for %s %s", r.Method, r.URL)
				return nil, errors.New("unreachable")
			})
			_, err := c.Fetch(t.Context(), r)
			if !errors.Is(err, errInvalidRequest) {
				t.Fatalf("err = %v, want errInvalidRequest", err)
			}
			if !strings.HasPrefix(err.Error(), "fetch "+target+": ") {
				t.Errorf("message = %q", err.Error())
			}
		})
	}
}

func TestFetch_PostBodyAtTheCapIsSent(t *testing.T) {
	var got int
	srv := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = len(b)
	})
	c := newTestClient(t, 1<<20)
	r := postRequest(srv.URL)
	r.Body = strings.Repeat("x", maxPostBodyBytes)
	if _, err := c.Fetch(t.Context(), r); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if got != maxPostBodyBytes {
		t.Errorf("server read %d bytes, want %d", got, maxPostBodyBytes)
	}
}
