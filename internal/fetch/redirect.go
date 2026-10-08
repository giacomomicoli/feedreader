package fetch

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/giacomomicoli/feedreader/internal/config"
)

// checkRedirect is the http.Client redirect policy: a redirect loop or more
// than config.MaxRedirects hops is an error. via holds the requests already
// made, oldest first, so len(via) is the number of the hop being attempted.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if s := req.URL.Scheme; s != "http" && s != "https" {
		return &reasonError{
			msg:    fmt.Sprintf("refused redirect to a %s: URL; %v", s, ErrScheme),
			target: ErrScheme,
		}
	}
	key := urlKey(req.URL)
	for _, prev := range via {
		if urlKey(prev.URL) == key {
			return &reasonError{
				msg:    "redirect loop via " + req.URL.Redacted(),
				target: ErrRedirects,
			}
		}
	}
	if len(via) > config.MaxRedirects {
		return &reasonError{
			msg:    fmt.Sprintf("more than %d redirects", config.MaxRedirects),
			target: ErrRedirects,
		}
	}
	return nil
}

// urlKey identifies a URL for loop detection: the fragment is never sent and
// the host is case-insensitive.
func urlKey(u *url.URL) string {
	k := *u
	k.Fragment, k.RawFragment = "", ""
	k.Host = strings.ToLower(k.Host)
	return k.String()
}

// permanentURL returns the URL at the end of the leading run of 301/308
// hops that led to resp, or "" when the first hop (if any) was temporary.
// A 302/303/307 ends the run; permanent hops after it do not count, since
// the original URL itself was not permanently moved.
//
// The chain is rebuilt from the response: each redirect-created request
// links to the redirect response that caused it (Request.Response), whose
// Request is the previous hop.
func permanentURL(resp *http.Response) string {
	var hops []*http.Request // newest first
	for req := resp.Request; req != nil && req.Response != nil; req = req.Response.Request {
		hops = append(hops, req)
	}
	moved := ""
	for i := len(hops) - 1; i >= 0; i-- {
		if !isPermanentRedirect(hops[i].Response.StatusCode) {
			break
		}
		moved = hops[i].URL.String()
	}
	return moved
}

func isPermanentRedirect(status int) bool {
	return status == http.StatusMovedPermanently || status == http.StatusPermanentRedirect
}
