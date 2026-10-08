package e2e

import (
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// Paths served by the fake feed site.
const (
	blogPagePath     = "/blog/"
	blogFeedPath     = "/blog/feed.xml"
	blogMovedPath    = "/blog/feed-v2.xml" // where the feed moves permanently
	blogCommentsPath = "/blog/comments.xml"
	videosFeedPath   = "/feeds/videos.xml"
	videosFeedQuery  = "?channel_id=UCe2eTestChannel0000000000"
)

// Fixture sizes.
const (
	initialPosts   = 8 // RSS items when the blog is first subscribed
	publishedLater = 2 // items the blog publishes before the next poll
	videoCount     = 3 // fewer than config.InitialUnread: all start unread
)

// post is one blog item. Posts are numbered from 1 (oldest).
type post struct {
	n         int
	title     string
	published time.Time
}

// hit is one request the site received.
type hit struct {
	path           string
	status         int
	ifNoneMatch    string
	userAgent      string
	acceptEncoding string
}

// feedSite is a fake website on a local httptest server: a blog page that
// advertises two RSS feeds via <link rel="alternate">, the blog's RSS feed
// (with ETag-based conditional GET and gzip), a comments feed, and a
// YouTube-style Atom channel feed. It records every request.
type feedSite struct {
	srv  *httptest.Server
	base time.Time // publication time of post 0

	mu      sync.Mutex
	posts   []post // oldest first
	version int    // bumped whenever the blog feed changes; drives the ETag
	moved   bool   // blogFeedPath answers 301 to blogMovedPath
	gone    bool   // the blog feed answers 404 at both paths
	hits    []hit
}

func newFeedSite(t *testing.T) *feedSite {
	t.Helper()
	s := &feedSite{
		base:    time.Now().UTC().Add(-48 * time.Hour).Truncate(time.Second),
		version: 1,
	}
	for n := 1; n <= initialPosts; n++ {
		s.posts = append(s.posts, s.newPost(n))
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+blogPagePath+"{$}", s.serveBlogPage)
	mux.HandleFunc("GET "+blogFeedPath, s.serveBlogFeed)
	mux.HandleFunc("GET "+blogMovedPath, s.serveBlogFeed)
	mux.HandleFunc("GET "+blogCommentsPath, s.serveComments)
	mux.HandleFunc("GET "+videosFeedPath, s.serveVideos)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		s.record(r, http.StatusNotFound)
		http.NotFound(w, r)
	})
	s.srv = httptest.NewServer(mux)
	t.Cleanup(s.srv.Close)
	return s
}

func (s *feedSite) newPost(n int) post {
	return post{n: n, title: postTitle(n), published: s.base.Add(time.Duration(n) * time.Hour)}
}

// postTitle is the original title of post n.
func postTitle(n int) string { return fmt.Sprintf("Post %d", n) }

// postTitles lists the titles of posts newest down to oldest, newest first.
func postTitles(newest, oldest int) []string {
	var out []string
	for n := newest; n >= oldest; n-- {
		out = append(out, postTitle(n))
	}
	return out
}

// url returns the absolute URL of a path on the site.
func (s *feedSite) url(path string) string { return s.srv.URL + path }

// publish adds count new posts and edits the title of the newest existing
// one, as a blog does between two polls.
func (s *feedSite) publish(count int, editedTitle string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.posts[len(s.posts)-1].title = editedTitle
	for range count {
		s.posts = append(s.posts, s.newPost(len(s.posts)+1))
	}
	s.version++
}

// moveFeed makes the blog feed move permanently to blogMovedPath.
func (s *feedSite) moveFeed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.moved = true
}

// setGone makes the blog feed answer 404 (gone=true) or come back.
func (s *feedSite) setGone(gone bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gone = gone
}

func (s *feedSite) etag() string { return fmt.Sprintf(`"posts-v%d"`, s.version) }

// requests returns how many requests path received.
func (s *feedSite) requests(path string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, h := range s.hits {
		if h.path == path {
			n++
		}
	}
	return n
}

// total returns how many requests the site received.
func (s *feedSite) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.hits)
}

// lastHit returns the most recent request to path.
func (s *feedSite) lastHit(t *testing.T, path string) hit {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.hits) - 1; i >= 0; i-- {
		if s.hits[i].path == path {
			return s.hits[i]
		}
	}
	t.Fatalf("site: no request to %s", path)
	return hit{}
}

func (s *feedSite) record(r *http.Request, status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recordLocked(r, status)
}

func (s *feedSite) recordLocked(r *http.Request, status int) {
	s.hits = append(s.hits, hit{
		path:           r.URL.Path,
		status:         status,
		ifNoneMatch:    r.Header.Get("If-None-Match"),
		userAgent:      r.Header.Get("User-Agent"),
		acceptEncoding: r.Header.Get("Accept-Encoding"),
	})
}

func (s *feedSite) serveBlogPage(w http.ResponseWriter, r *http.Request) {
	s.record(r, http.StatusOK)
	// The posts feed is linked relatively, the comments feed by absolute
	// path; both must come back as absolute candidate URLs.
	write(w, r, "text/html; charset=utf-8", `<!doctype html>
<html><head>
<meta charset="utf-8">
<title>E2E Blog</title>
<link rel="alternate" type="application/rss+xml" title="E2E Blog posts" href="feed.xml">
<link rel="alternate" type="application/rss+xml" title="E2E Blog comments" href="/blog/comments.xml">
<link rel="stylesheet" href="/style.css">
</head><body><h1>E2E Blog</h1><p>Welcome.</p></body></html>`)
}

// serveBlogFeed serves the RSS feed with an ETag and answers a matching
// If-None-Match with 304. It also plays a moved (301) or gone (404) feed.
func (s *feedSite) serveBlogFeed(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	switch {
	case s.gone:
		s.recordLocked(r, http.StatusNotFound)
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	case s.moved && r.URL.Path == blogFeedPath:
		s.recordLocked(r, http.StatusMovedPermanently)
		s.mu.Unlock()
		http.Redirect(w, r, blogMovedPath, http.StatusMovedPermanently)
		return
	}
	etag := s.etag()
	if r.Header.Get("If-None-Match") == etag {
		s.recordLocked(r, http.StatusNotModified)
		s.mu.Unlock()
		w.Header().Set("ETag", etag)
		w.WriteHeader(http.StatusNotModified)
		return
	}
	s.recordLocked(r, http.StatusOK)
	body := s.blogRSSLocked()
	s.mu.Unlock()
	w.Header().Set("ETag", etag)
	write(w, r, "application/rss+xml; charset=utf-8", body)
}

// blogRSSLocked renders the posts newest first. Each summary carries a
// relative link and image (to be made absolute) and a script (to be
// stripped by the sanitizer).
func (s *feedSite) blogRSSLocked() string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0"><channel>
<title>E2E Blog</title>
<link>` + s.url(blogPagePath) + `</link>
<description>A blog served by the end-to-end test.</description>
`)
	for i := len(s.posts) - 1; i >= 0; i-- {
		p := s.posts[i]
		fmt.Fprintf(&b, `<item>
<title>%s</title>
<link>%s</link>
<guid isPermaLink="false">e2e-post-%d</guid>
<pubDate>%s</pubDate>
<description><![CDATA[<p>Summary of post %d with <a href="/posts/%d">a relative link</a>.</p><img src="/img/%d.png" alt=""><script>alert("xss-%d")</script>]]></description>
</item>
`, p.title, s.url(fmt.Sprintf("/posts/%d", p.n)), p.n, p.published.Format(time.RFC1123Z), p.n, p.n, p.n, p.n)
	}
	b.WriteString("</channel></rss>\n")
	return b.String()
}

func (s *feedSite) serveComments(w http.ResponseWriter, r *http.Request) {
	s.record(r, http.StatusOK)
	write(w, r, "application/rss+xml", `<?xml version="1.0"?>
<rss version="2.0"><channel><title>E2E Blog comments</title><link>`+s.url(blogPagePath)+`</link>
<item><title>A comment</title><guid>e2e-comment-1</guid></item>
</channel></rss>`)
}

// serveVideos serves a YouTube-style channel feed (Atom with yt: and
// media: extensions).
func (s *feedSite) serveVideos(w http.ResponseWriter, r *http.Request) {
	s.record(r, http.StatusOK)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>
<feed xmlns:yt="http://www.youtube.com/xml/schemas/2015" xmlns:media="http://search.yahoo.com/mrss/" xmlns="http://www.w3.org/2005/Atom">
 <id>yt:channel:e2eTestChannel0000000000</id>
 <yt:channelId>e2eTestChannel0000000000</yt:channelId>
 <title>E2E Channel</title>
 <link rel="alternate" href="https://www.youtube.com/channel/UCe2eTestChannel0000000000"/>
 <author><name>E2E Channel</name></author>
`)
	for n := videoCount; n >= 1; n-- {
		published := s.base.Add(time.Duration(n) * 90 * time.Minute).Format(time.RFC3339)
		fmt.Fprintf(&b, ` <entry>
  <id>yt:video:e2evideo%04d</id>
  <yt:videoId>e2evideo%04d</yt:videoId>
  <title>Video %d</title>
  <link rel="alternate" href="https://www.youtube.com/watch?v=e2evideo%04d"/>
  <published>%s</published>
  <updated>%s</updated>
  <media:group>
   <media:title>Video %d</media:title>
   <media:thumbnail url="https://i.ytimg.com/vi/e2evideo%04d/hqdefault.jpg" width="480" height="360"/>
   <media:description>Description of video %d.</media:description>
  </media:group>
 </entry>
`, n, n, n, n, published, published, n, n, n)
	}
	b.WriteString("</feed>\n")
	write(w, r, "application/atom+xml; charset=utf-8", b.String())
}

// write sends body, gzip-compressed when the client accepts it.
func write(w http.ResponseWriter, r *http.Request, contentType, body string) {
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Vary", "Accept-Encoding")
	if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		_, _ = w.Write([]byte(body))
		return
	}
	w.Header().Set("Content-Encoding", "gzip")
	gz := gzip.NewWriter(w)
	_, _ = gz.Write([]byte(body))
	_ = gz.Close()
}
