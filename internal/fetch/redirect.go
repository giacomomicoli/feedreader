package fetch

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/safeurl"
)

// checkRedirect is the http.Client redirect policy: a redirect loop or more
// than config.MaxRedirects hops is an error, and the redirect of a POST or
// of a request with Request.NoRedirects is not followed but returned as the
// response. via holds the requests already made, oldest first, so len(via)
// is the number of the hop being attempted.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if startedAsPost(via) || refusesRedirects(req.Context()) {
		return http.ErrUseLastResponse
	}
	if s := req.URL.Scheme; s != "http" && s != "https" {
		return &reasonError{
			msg:    fmt.Sprintf("refused redirect to a %s: URL; %v", s, ErrScheme),
			target: ErrScheme,
		}
	}
	if req.URL.Hostname() == "" {
		// As parseTarget: it would dial this machine, and the URL could not
		// be fetched again once stored.
		return &reasonError{msg: "refused redirect to a URL without a host name", target: errInvalidURL}
	}
	key := urlKey(req.URL)
	for _, prev := range via {
		if urlKey(prev.URL) == key {
			return &reasonError{
				msg:    "redirect loop via " + safeurl.Redacted(req.URL),
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

// noRedirectsKey marks the context of a request with Request.NoRedirects.
// net/http gives the requests of a redirect chain the context of the first
// one, so checkRedirect finds the mark on every hop.
type noRedirectsKey struct{}

// withoutRedirects returns ctx marked for a request whose redirects are
// not followed.
func withoutRedirects(ctx context.Context) context.Context {
	return context.WithValue(ctx, noRedirectsKey{}, true)
}

// refusesRedirects reports whether ctx is marked by withoutRedirects.
func refusesRedirects(ctx context.Context) bool {
	refused, _ := ctx.Value(noRedirectsKey{}).(bool)
	return refused
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
