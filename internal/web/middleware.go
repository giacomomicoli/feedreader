package web

import (
	"bytes"
	"net"
	"net/http"
	"net/netip"
	"runtime/debug"
	"strings"
	"time"
)

// maxRequestBody caps request bodies; the UI only ever posts small forms.
const maxRequestBody = 64 << 10

// contentSecurityPolicy forbids inline scripts and styles (so no hx-on and no
// style attributes in templates); images may come from any http(s) origin
// because thumbnails and feed icons are hot-linked.
const contentSecurityPolicy = "default-src 'self'; img-src 'self' https: http: data:; style-src 'self'; " +
	"script-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

// statusWriter records the status code and body size for logging and lets
// the panic handler know whether a response has already started.
type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (sw *statusWriter) WriteHeader(code int) {
	if sw.status == 0 {
		sw.status = code
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(p []byte) (int, error) {
	if sw.status == 0 {
		sw.status = http.StatusOK
	}
	n, err := sw.ResponseWriter.Write(p)
	sw.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (sw *statusWriter) Unwrap() http.ResponseWriter { return sw.ResponseWriter }

// observe logs every request at debug level and turns panics into a generic
// 500 without leaking details to the client.
func (s *server) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic serving request", "method", r.Method, "path", r.URL.Path,
					"panic", v, "stack", string(debug.Stack()))
				if sw.status == 0 {
					writeInternalError(sw, r)
				}
			}
			s.log.Debug("http request", "method", r.Method, "path", r.URL.Path,
				"status", sw.status, "bytes", sw.bytes, "duration", time.Since(start))
		}()
		next.ServeHTTP(sw, r)
	})
}

// writeInternalError is the last-resort 500 used after a panic.
func writeInternalError(w http.ResponseWriter, r *http.Request) {
	const msg = "Something went wrong. Try again in a moment."
	if isHTMX(r) {
		w.Header().Set("HX-Retarget", "#notice")
		w.Header().Set("HX-Reswap", "innerHTML")
		writeHTML(w, http.StatusInternalServerError,
			bytes.NewBufferString(`<p class="notice notice-error">`+msg+`</p>`))
		return
	}
	http.Error(w, msg, http.StatusInternalServerError)
}

// securityHeaders sets the CSP and related headers on every response.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", contentSecurityPolicy)
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		next.ServeHTTP(w, r)
	})
}

// checkHost rejects requests whose Host header is not a name this server
// answers to, with 421 Misdirected Request, before anything is read or
// changed. It is the DNS-rebinding defence: a page on attacker.example whose
// DNS later points at this server is same-origin with itself, so
// CrossOriginProtection (which compares Origin with Host) lets its requests
// through, and the same-origin policy lets it read the answers. Its requests
// carry Host: attacker.example, which is not in the allowlist.
//
// IP literals cannot be rebound, so they are always accepted; that keeps
// direct access by address (http://192.0.2.10:8080) working without
// configuration. Names must be listed: localhost, the host of FR_LISTEN and
// FR_ALLOWED_HOSTS (the reverse proxy's site name, which Caddy forwards as
// Host). X-Forwarded-Host is deliberately ignored.
func (s *server) checkHost(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := hostName(r.Host)
		if _, err := netip.ParseAddr(host); err == nil || s.hosts[host] {
			next.ServeHTTP(w, r)
			return
		}
		s.log.Warn("request for unknown host rejected; add the name to FR_ALLOWED_HOSTS if it is yours",
			"host", r.Host, "method", r.Method, "path", r.URL.Path)
		http.Error(w, "Misdirected request: this server does not answer to that host name. "+
			"If it is yours, add it to FR_ALLOWED_HOSTS.", http.StatusMisdirectedRequest)
	})
}

// allowedHosts builds checkHost's set of names: localhost, the host of the
// listen address when it is a name, and the configured names.
func allowedHosts(listen string, extra []string) map[string]bool {
	set := map[string]bool{"localhost": true}
	for _, h := range append([]string{listen}, extra...) {
		if h = hostName(strings.TrimSpace(h)); h != "" {
			set[h] = true
		}
	}
	return set
}

// hostName reduces a Host header (or a listen address or configured name)
// to the bare lower-case host: no port, IPv6 brackets or trailing dot.
func hostName(hostport string) string {
	h := hostport
	if host, _, err := net.SplitHostPort(hostport); err == nil {
		h = host
	}
	h = strings.TrimSuffix(strings.TrimPrefix(h, "["), "]")
	return strings.ToLower(strings.TrimSuffix(h, "."))
}

// limitBody caps request bodies at maxRequestBody.
func limitBody(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
		}
		next.ServeHTTP(w, r)
	})
}

// crossOrigin rejects cross-site state-changing requests (CSRF defence via
// Sec-Fetch-Site / Origin, Go 1.25 stdlib).
func (s *server) crossOrigin(next http.Handler) http.Handler {
	cop := http.NewCrossOriginProtection()
	cop.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.log.Warn("cross-origin request rejected", "method", r.Method, "path", r.URL.Path,
			"origin", r.Header.Get("Origin"), "sec_fetch_site", r.Header.Get("Sec-Fetch-Site"))
		http.Error(w, "Cross-origin request rejected.", http.StatusForbidden)
	}))
	return cop.Handler(next)
}
