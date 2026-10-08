package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
)

// syncBuffer is a bytes.Buffer safe for the concurrent writes of a logger
// and reads of a test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// isolateEnv clears FR_* variables inherited from the developer's shell so
// that only what a test sets is in effect.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(k, "FR_") {
			t.Setenv(k, "") // restores the original value at cleanup
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestRun_VersionFlagPrintsVersion(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if err := run(t.Context(), []string{"-version"}, &stdout, &stderr); err != nil {
		t.Fatalf("run -version: %v", err)
	}
	if got, want := stdout.String(), config.AppName+" "+config.Version+"\n"; got != want {
		t.Errorf("stdout = %q, want %q", got, want)
	}
}

func TestRun_HelpFlagIsNotAnError(t *testing.T) {
	var stderr bytes.Buffer
	if err := run(t.Context(), []string{"-h"}, io.Discard, &stderr); err != nil {
		t.Fatalf("run -h: %v", err)
	}
	if !strings.Contains(stderr.String(), "-config") {
		t.Errorf("usage text lacks -config:\n%s", stderr.String())
	}
}

func TestRun_BadArgumentsAreUsageErrors(t *testing.T) {
	for _, args := range [][]string{{"-nope"}, {"serve"}, {"-config"}} {
		var ue usageError
		err := run(t.Context(), args, io.Discard, io.Discard)
		if !errors.As(err, &ue) {
			t.Errorf("run %q = %v, want a usage error", args, err)
		}
	}
}

func TestRun_InvalidConfigurationFails(t *testing.T) {
	isolateEnv(t)
	t.Run("environment", func(t *testing.T) {
		t.Setenv("FR_POLL_INTERVAL", "1s")
		err := run(t.Context(), nil, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "FR_POLL_INTERVAL") {
			t.Fatalf("run = %v, want an FR_POLL_INTERVAL error", err)
		}
	})
	t.Run("missing config file", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "absent.env")
		if err := run(t.Context(), []string{"-config", missing}, io.Discard, io.Discard); err == nil {
			t.Fatalf("run with a missing config file succeeded")
		}
	})
}

func TestRun_ListenFailureIsFatal(t *testing.T) {
	isolateEnv(t)
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	t.Setenv("FR_DATA_DIR", t.TempDir())
	t.Setenv("FR_LISTEN", busy.Addr().String())

	err = run(t.Context(), nil, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "listen") {
		t.Fatalf("run on a busy port = %v, want a listen error", err)
	}
}

var listenRE = regexp.MustCompile(`listen=(http://\S+)`)

// TestRun_ServesUntilCancelledThenShutsDownCleanly starts the whole program
// with settings from a -config file, checks the HTTP surface, and stops it
// the way a signal does (by cancelling its context).
func TestRun_ServesUntilCancelledThenShutsDownCleanly(t *testing.T) {
	isolateEnv(t)
	dataDir := filepath.Join(t.TempDir(), "nested", "data")
	cfgFile := filepath.Join(t.TempDir(), "feedreader.env")
	cfgText := "# test configuration\nFR_DATA_DIR=" + dataDir + "\nFR_LISTEN=127.0.0.1:0\nFR_LOG_LEVEL=debug\n" +
		"FR_ALLOWED_HOSTS=feeds.lan.example, other.lan.example\n"
	if err := os.WriteFile(cfgFile, []byte(cfgText), 0o600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var logs syncBuffer
	done := make(chan error, 1)
	go func() { done <- run(ctx, []string{"-config", cfgFile}, io.Discard, &logs) }()

	base := waitForListen(t, &logs, done)
	hc := &http.Client{Timeout: 5 * time.Second}

	resp := mustDo(t, hc, http.MethodGet, base+"/", nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Security-Policy") == "" {
		t.Errorf("GET / = %d, CSP %q", resp.StatusCode, resp.Header.Get("Content-Security-Policy"))
	}
	if resp := mustDo(t, hc, http.MethodGet, base+"/static/htmx.min.js", nil); resp.StatusCode != http.StatusOK {
		t.Errorf("GET /static/htmx.min.js = %d", resp.StatusCode)
	}
	crossSite := http.Header{"Sec-Fetch-Site": {"cross-site"}, "Content-Type": {"application/x-www-form-urlencoded"}}
	if resp := mustDo(t, hc, http.MethodPost, base+"/folders", crossSite); resp.StatusCode != http.StatusForbidden {
		t.Errorf("cross-site POST = %d, want 403", resp.StatusCode)
	}
	// FR_ALLOWED_HOSTS reaches the Host check: the reverse proxy's site name
	// (forwarded by Caddy as Host) is served, any other name is refused.
	for host, want := range map[string]int{
		"feeds.lan.example":        http.StatusOK,
		"Other.LAN.example:443":    http.StatusOK,
		"rebound.attacker.example": http.StatusMisdirectedRequest,
	} {
		if resp := mustDo(t, hc, http.MethodGet, base+"/", http.Header{"Host": {host}}); resp.StatusCode != want {
			t.Errorf("GET / with Host %q = %d, want %d", host, resp.StatusCode, want)
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run after cancel: %v\nlogs:\n%s", err, logs.String())
		}
	case <-time.After(shutdownTimeout + schedStopTimeout + 5*time.Second):
		t.Fatalf("run did not return after cancel; logs:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "feedreader stopped") {
		t.Errorf("no clean-stop log line:\n%s", logs.String())
	}
	if _, err := os.Stat(filepath.Join(dataDir, "feedreader.db")); err != nil {
		t.Errorf("database not created in the data directory: %v", err)
	}
}

// waitForListen waits for the startup log line and returns the base URL.
func waitForListen(t *testing.T, logs *syncBuffer, done <-chan error) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if m := listenRE.FindStringSubmatch(logs.String()); m != nil {
			return m[1]
		}
		select {
		case err := <-done:
			t.Fatalf("run returned before listening: %v\nlogs:\n%s", err, logs.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	t.Fatalf("no startup log line; logs:\n%s", logs.String())
	return ""
}

func mustDo(t *testing.T, hc *http.Client, method, url string, h http.Header) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader("name=x"))
	if err != nil {
		t.Fatal(err)
	}
	for k, vs := range h {
		req.Header[k] = vs
	}
	if host := h.Get("Host"); host != "" {
		req.Host = host
	}
	resp, err := hc.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp
}

func TestWithRequestTimeout_HandlerContextHasDeadline(t *testing.T) {
	var deadline time.Time
	var ok bool
	h := withRequestTimeout(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		deadline, ok = r.Context().Deadline()
	}), time.Minute)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !ok || time.Until(deadline) > time.Minute || time.Until(deadline) < 50*time.Second {
		t.Errorf("handler deadline = %v (set %v), want about a minute from now", deadline, ok)
	}
	if requestTimeout >= writeTimeout {
		t.Errorf("requestTimeout %s must be shorter than writeTimeout %s", requestTimeout, writeTimeout)
	}
}

// startTestServer serves h on a loopback port with the production server
// settings and returns the server and its base URL. The server is closed at
// cleanup.
func startTestServer(t *testing.T, h http.Handler) (*http.Server, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := newHTTPServer(h, slog.New(slog.DiscardHandler))
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return srv, "http://" + ln.Addr().String()
}

// fakeScheduler stands in for scheduler.Run as serve starts it: done is
// closed stopDelay after stop is called, like a Run whose in-flight fetches
// take that long to notice the cancellation. A negative stopDelay never
// stops. ctx is the scheduler's context.
func fakeScheduler(t *testing.T, stopDelay time.Duration) (ctx context.Context, stop context.CancelFunc, done <-chan struct{}) {
	t.Helper()
	ctx, stop = context.WithCancel(context.Background())
	t.Cleanup(stop)
	ch := make(chan struct{})
	go func() {
		<-ctx.Done()
		if stopDelay < 0 {
			return
		}
		time.Sleep(stopDelay)
		close(ch)
	}()
	return ctx, stop, ch
}

// slowRequest sends a request to base that the handler holds open until the
// test ends; it returns once the handler is running. handler must close
// entered and then block on release.
func slowRequest(t *testing.T, base string, entered <-chan struct{}) {
	t.Helper()
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		if resp, err := http.Get(base + "/slow"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	t.Cleanup(func() { <-clientDone })
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow request never reached the handler")
	}
}

// TestShutdown_SlowRequestLeavesTheSchedulerItsOwnBudget: a request still
// running when the HTTP drain budget runs out must not use up the time the
// scheduler gets to stop. Otherwise a scheduler that stops promptly is
// reported as stuck and the database is closed under its fetch workers.
func TestShutdown_SlowRequestLeavesTheSchedulerItsOwnBudget(t *testing.T) {
	release := make(chan struct{})
	entered := make(chan struct{})
	srv, base := startTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
	}))
	slowRequest(t, base, entered)
	t.Cleanup(func() { close(release) }) // runs before the client wait above

	const httpBudget = 100 * time.Millisecond
	_, stopSched, schedDone := fakeScheduler(t, 20*time.Millisecond)
	err := shutdown(srv, stopSched, schedDone, httpBudget, 5*time.Second)

	if err == nil || !strings.Contains(err.Error(), "http shutdown") {
		t.Errorf("shutdown = %v, want the undrained request reported", err)
	}
	if err != nil && strings.Contains(err.Error(), "scheduler") {
		t.Errorf("shutdown = %v, want no scheduler error: it stops 20ms after being told to", err)
	}
	select {
	case <-schedDone:
	default:
		t.Errorf("shutdown returned before the scheduler stopped; the database would be closed under it")
	}
}

// TestShutdown_StuckSchedulerIsReportedWithItsBudget: a scheduler that does
// not stop is given up on after its own budget, and the error says how long
// it had.
func TestShutdown_StuckSchedulerIsReportedWithItsBudget(t *testing.T) {
	srv, _ := startTestServer(t, http.NotFoundHandler())
	_, stopSched, schedDone := fakeScheduler(t, -1)

	const schedBudget = 50 * time.Millisecond
	start := time.Now()
	err := shutdown(srv, stopSched, schedDone, 5*time.Second, schedBudget)
	took := time.Since(start)

	if want := "scheduler did not stop within " + schedBudget.String(); err == nil || err.Error() != want {
		t.Errorf("shutdown = %v, want %q", err, want)
	}
	if took < schedBudget || took > 5*time.Second {
		t.Errorf("shutdown took %s, want about the scheduler budget %s", took, schedBudget)
	}
}

// TestShutdown_SchedulerRunsUntilRequestsAreDrained: requests still in
// flight when shutdown starts can trigger refreshes, so the scheduler is
// stopped only after the HTTP server has drained.
func TestShutdown_SchedulerRunsUntilRequestsAreDrained(t *testing.T) {
	schedCtx, stopSched, schedDone := fakeScheduler(t, 0)
	release := make(chan struct{})
	entered := make(chan struct{})
	schedStoppedEarly := make(chan bool, 1)
	srv, base := startTestServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(entered)
		<-release
		schedStoppedEarly <- schedCtx.Err() != nil
	}))
	slowRequest(t, base, entered)

	errc := make(chan error, 1)
	go func() { errc <- shutdown(srv, stopSched, schedDone, 5*time.Second, 5*time.Second) }()
	time.Sleep(50 * time.Millisecond) // let shutdown start draining
	close(release)

	if err := <-errc; err != nil {
		t.Errorf("shutdown = %v", err)
	}
	if <-schedStoppedEarly {
		t.Errorf("scheduler was stopped while a request was still in flight")
	}
}
