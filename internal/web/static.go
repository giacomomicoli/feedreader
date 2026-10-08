package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// contentTypes pins the types of our own assets so they do not depend on the
// host's mime.types (some systems map .js to text/plain).
var contentTypes = map[string]string{
	".css": "text/css; charset=utf-8",
	".js":  "text/javascript; charset=utf-8",
	".svg": "image/svg+xml",
}

// staticFile is an embedded asset held in memory with its content hash.
type staticFile struct {
	data        []byte
	hash        string
	contentType string
}

// staticFS serves embedded assets under /static/ with strong ETags and
// long-lived caching for versioned URLs (?v=<hash>).
type staticFS struct {
	files map[string]staticFile
}

func loadStatic(fsys fs.FS) (*staticFS, error) {
	s := &staticFS{files: make(map[string]staticFile)}
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return fmt.Errorf("read %s: %w", p, err)
		}
		sum := sha256.Sum256(data)
		ct := contentTypes[path.Ext(p)]
		if ct == "" {
			ct = mime.TypeByExtension(path.Ext(p))
		}
		if ct == "" {
			ct = http.DetectContentType(data)
		}
		s.files[p] = staticFile{data: data, hash: hex.EncodeToString(sum[:8]), contentType: ct}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("load static assets: %w", err)
	}
	return s, nil
}

// url returns the cache-busting URL of an asset. Unknown names are returned
// unversioned so a missing file shows up as a 404 rather than a panic.
func (s *staticFS) url(name string) string {
	if f, ok := s.files[name]; ok {
		return "/static/" + name + "?v=" + f.hash
	}
	return "/static/" + name
}

func (s *staticFS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, strings.TrimPrefix(r.URL.Path, "/static/"))
}

// favicon answers /favicon.ico, which browsers request on their own (for
// pages without a <link rel="icon">, such as plain-text error responses),
// with the SVG icon instead of a 404.
func (s *staticFS) favicon(w http.ResponseWriter, r *http.Request) {
	s.serve(w, r, "favicon.svg")
}

// serve writes the embedded asset name, or a 404 when there is none.
func (s *staticFS) serve(w http.ResponseWriter, r *http.Request, name string) {
	f, ok := s.files[name]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.contentType)
	h.Set("ETag", `"`+f.hash+`"`)
	if r.URL.Query().Get("v") == f.hash {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "public, max-age=3600")
	}
	http.ServeContent(w, r, name, time.Time{}, bytes.NewReader(f.data))
}
