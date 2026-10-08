package resolve

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
)

// TestResolve_WithRealFetchClient runs the resolver over the real fetch
// client against a local test server, so redirects, Accept headers and
// FinalURL/PermanentURL reporting are exercised end to end.
func TestResolve_WithRealFetchClient(t *testing.T) {
	var (
		mu   sync.Mutex
		hits []string
	)
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, r.URL.RequestURI())
		mu.Unlock()
		switch r.URL.RequestURI() {
		case "/blog/":
			w.Header().Set("Content-Type", ctHTML)
			_, _ = w.Write([]byte(htmlPage(
				`<base href="/blog/static/"><link rel="alternate" type="application/atom+xml" title="Atom" href="../atom.xml">`)))
		case "/moved":
			http.Redirect(w, r, "/feeds/main.xml", http.StatusMovedPermanently)
		case "/temporary":
			http.Redirect(w, r, "/feeds/main.xml", http.StatusFound)
		case "/feeds/main.xml", "/rss.xml":
			w.Header().Set("Content-Type", ctRSS)
			_, _ = w.Write([]byte(rssDoc))
		case "/bare":
			w.Header().Set("Content-Type", ctHTML)
			_, _ = w.Write([]byte(htmlPage("")))
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	r := New(fetch.New(config.Default()))
	resolve := func(path string) []string {
		t.Helper()
		res, err := r.Resolve(context.Background(), srv.URL+path)
		if err != nil {
			t.Fatalf("Resolve(%s): %v", path, err)
		}
		return candidateURLs(res)
	}

	t.Run("permanent redirect to a feed", func(t *testing.T) {
		if got := resolve("/moved"); !slices.Equal(got, []string{srv.URL + "/feeds/main.xml"}) {
			t.Errorf("candidates = %v", got)
		}
	})
	t.Run("temporary redirect to a feed", func(t *testing.T) {
		if got := resolve("/temporary"); !slices.Equal(got, []string{srv.URL + "/temporary"}) {
			t.Errorf("candidates = %v", got)
		}
	})
	t.Run("autodiscovery with base href", func(t *testing.T) {
		if got := resolve("/blog/"); !slices.Equal(got, []string{srv.URL + "/blog/atom.xml"}) {
			t.Errorf("candidates = %v", got)
		}
	})
	t.Run("fallback paths", func(t *testing.T) {
		mu.Lock()
		hits = nil
		mu.Unlock()
		if got := resolve("/bare"); !slices.Equal(got, []string{srv.URL + "/rss.xml"}) {
			t.Errorf("candidates = %v", got)
		}
		mu.Lock()
		defer mu.Unlock()
		want := []string{"/bare", "/feed", "/feed/", "/rss", "/rss.xml"}
		if !slices.Equal(hits, want) {
			t.Errorf("server saw %v; want %v", hits, want)
		}
	})
}
