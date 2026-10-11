package fetch

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// A POST (Request.Method http.MethodPost) is for the few APIs that cannot
// be read with a GET. It goes through the same client as a GET, with the
// same User-Agent, timeout, response size cap and status classification,
// and differs in three ways, all so that a POST is sent only where the
// caller chose:
//
//   - It carries Body, at most maxPostBodyBytes, with Content-Type
//     ContentType, and never the conditional headers (If-None-Match,
//     If-Modified-Since): a POST with Request.ETag or LastModified is
//     refused.
//   - Redirects are not followed. net/http would turn a 301, 302 or 303
//     into a GET of the new location and re-send the body on a 307 or 308;
//     the redirect response is returned as a *StatusError instead (with
//     its Retry-After).
//   - A 304 is a *StatusError too, since nothing was conditional.

// maxPostBodyBytes bounds the body of a POST. Callers send small JSON
// queries of a few hundred bytes.
const maxPostBodyBytes = 64 << 10

// errInvalidRequest is a Request whose fields do not make a GET or a POST.
var errInvalidRequest = errors.New("invalid request")

// requestMethod checks the method-specific fields of r and returns the HTTP
// method to use.
func requestMethod(r Request) (string, error) {
	switch r.Method {
	case "", http.MethodGet:
		if r.Body != "" || r.ContentType != "" {
			return "", fmt.Errorf("%w: a GET has no body", errInvalidRequest)
		}
		return http.MethodGet, nil
	case http.MethodPost:
		switch {
		case r.ContentType == "":
			return "", fmt.Errorf("%w: a POST needs a content type", errInvalidRequest)
		case len(r.Body) > maxPostBodyBytes:
			return "", fmt.Errorf("%w: POST body exceeds %s", errInvalidRequest, formatBytes(maxPostBodyBytes))
		case r.ETag != "" || r.LastModified != "":
			return "", fmt.Errorf("%w: a POST cannot be conditional", errInvalidRequest)
		}
		return http.MethodPost, nil
	default:
		return "", fmt.Errorf("%w: only GET and POST are supported", errInvalidRequest)
	}
}

// requestBody returns the body to send with method.
func requestBody(method string, r Request) io.Reader {
	if method != http.MethodPost {
		return nil
	}
	return strings.NewReader(r.Body)
}

// startedAsPost reports whether the requests already made in a redirect
// chain (oldest first) began with a POST, whose redirects are not followed.
func startedAsPost(via []*http.Request) bool {
	return len(via) > 0 && via[0].Method == http.MethodPost
}
