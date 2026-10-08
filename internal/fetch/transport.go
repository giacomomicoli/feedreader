package fetch

import (
	"net"
	"net/http"
	"time"
)

// Per-phase transport bounds. They are not config constants: config.FetchTimeout
// bounds the whole request; these only make a stalled phase fail early.
const (
	dialTimeout            = 10 * time.Second
	keepAlive              = 30 * time.Second
	tlsHandshakeTimeout    = 10 * time.Second
	responseHeaderTimeout  = 20 * time.Second
	idleConnTimeout        = 90 * time.Second
	maxResponseHeaderBytes = 1 << 20
)

// newTransport returns a dedicated transport derived from
// http.DefaultTransport (keeping ProxyFromEnvironment and HTTP/2), with
// explicit timeouts. Compression stays enabled: the transport sends
// "Accept-Encoding: gzip" itself and decompresses the body transparently.
func newTransport() *http.Transport {
	var t *http.Transport
	if dt, ok := http.DefaultTransport.(*http.Transport); ok {
		t = dt.Clone()
	} else {
		t = &http.Transport{Proxy: http.ProxyFromEnvironment, ForceAttemptHTTP2: true}
	}
	t.DialContext = (&net.Dialer{Timeout: dialTimeout, KeepAlive: keepAlive}).DialContext
	t.TLSHandshakeTimeout = tlsHandshakeTimeout
	t.ResponseHeaderTimeout = responseHeaderTimeout
	t.IdleConnTimeout = idleConnTimeout
	t.MaxResponseHeaderBytes = maxResponseHeaderBytes
	t.DisableCompression = false
	return t
}
