package web

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"net/url"

	"github.com/giacomomicoli/feedreader/internal/store"
)

// Pages are full documents: the "layout" template with a page-specific
// "main". Each page template set is a clone of the base set, built once at
// startup (html/template sets cannot be cloned after first execution).
var pageNames = []string{"home", "add", "feed", "confirm", "error"}

// renderer holds the parsed templates.
type renderer struct {
	base  *template.Template
	pages map[string]*template.Template
}

func newRenderer(fsys fs.FS, funcs template.FuncMap) (*renderer, error) {
	base, err := template.New("").Funcs(funcs).ParseFS(fsys, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	r := &renderer{pages: make(map[string]*template.Template, len(pageNames))}
	for _, name := range pageNames {
		if base.Lookup("page-"+name) == nil {
			return nil, fmt.Errorf("parse templates: missing page-%s", name)
		}
		t, err := base.Clone()
		if err != nil {
			return nil, fmt.Errorf("clone templates for %s: %w", name, err)
		}
		if _, err := t.New("main").Parse(`{{template "page-` + name + `" .}}`); err != nil {
			return nil, fmt.Errorf("define main for %s: %w", name, err)
		}
		r.pages[name] = t
	}
	r.base = base
	return r, nil
}

// layoutData is what the "layout" template receives.
type layoutData struct {
	Title   string
	Sidebar sidebarView
	Main    any
}

// isHTMX reports whether the request was issued by htmx for a partial. A
// history-restore request needs the full page.
func isHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true" && r.Header.Get("HX-History-Restore-Request") != "true"
}

// writeHTML sends a fully rendered buffer. Rendering always happens into a
// buffer first so that template errors can still produce a clean 500.
func writeHTML(w http.ResponseWriter, status int, body *bytes.Buffer) {
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-cache")
	h.Add("Vary", "HX-Request")
	w.WriteHeader(status)
	_, _ = body.WriteTo(w)
}

// loadSidebarData runs the sidebar queries.
func (s *server) loadSidebarData(ctx context.Context) (sidebarData, error) {
	var d sidebarData
	var err error
	if d.folders, err = s.store.ListFolders(ctx); err != nil {
		return d, fmt.Errorf("list folders: %w", err)
	}
	if d.feeds, err = s.store.ListFeeds(ctx); err != nil {
		return d, fmt.Errorf("list feeds: %w", err)
	}
	if d.tags, err = s.store.ListTags(ctx); err != nil {
		return d, fmt.Errorf("list tags: %w", err)
	}
	if d.counts, err = s.store.UnreadCounts(ctx); err != nil {
		return d, fmt.Errorf("unread counts: %w", err)
	}
	return d, nil
}

// renderPage renders a full document with the sidebar. cur is the scope to
// highlight in the sidebar (zero value: none).
func (s *server) renderPage(w http.ResponseWriter, r *http.Request, status int, page, title string, cur store.Scope, main any) {
	sd, err := s.loadSidebarData(r.Context())
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderPageWith(w, r, status, page, title, buildSidebar(sd, cur), main)
}

func (s *server) renderPageWith(w http.ResponseWriter, r *http.Request, status int, page, title string, sb sidebarView, main any) {
	t, ok := s.tmpl.pages[page]
	if !ok {
		s.fail(w, r, fmt.Errorf("unknown page %q", page))
		return
	}
	var buf bytes.Buffer
	if err := t.ExecuteTemplate(&buf, "layout", layoutData{Title: title, Sidebar: sb, Main: main}); err != nil {
		s.fail(w, r, fmt.Errorf("render page %s: %w", page, err))
		return
	}
	writeHTML(w, status, &buf)
}

// renderFragment renders one partial template. With counts set, the
// out-of-band sidebar badges and tag list are appended (every action
// returns the fragment it changed plus the sidebar counts). A
// successful response also clears the #notice area out of band, so an
// earlier error does not linger next to the result of a later action.
func (s *server) renderFragment(w http.ResponseWriter, r *http.Request, status int, name string, data any, counts bool) {
	var buf bytes.Buffer
	if err := s.tmpl.base.ExecuteTemplate(&buf, name, data); err != nil {
		s.fail(w, r, fmt.Errorf("render %s: %w", name, err))
		return
	}
	if status < http.StatusMultipleChoices && clearsNotice(name) {
		if err := s.tmpl.base.ExecuteTemplate(&buf, "notice-clear", nil); err != nil {
			s.fail(w, r, fmt.Errorf("render notice-clear: %w", err))
			return
		}
	}
	if counts {
		sd, err := s.loadSidebarData(r.Context())
		if err != nil {
			s.fail(w, r, err)
			return
		}
		if err := s.tmpl.base.ExecuteTemplate(&buf, "oob-counts", buildCounts(sd, currentScope(r))); err != nil {
			s.fail(w, r, fmt.Errorf("render oob-counts: %w", err))
			return
		}
	}
	writeHTML(w, status, &buf)
}

// clearsNotice reports whether a successful response with the named
// fragment clears #notice. The notice fragment writes #notice itself, and tag
// autocomplete answers keystrokes, not actions.
func clearsNotice(name string) bool {
	return name != "notice" && name != "tag-options"
}

// redirect sends the browser to target: HX-Redirect for htmx requests (a
// full navigation, so the whole sidebar is fresh), 303 otherwise.
func (s *server) redirect(w http.ResponseWriter, r *http.Request, target string) {
	if isHTMX(r) {
		w.Header().Set("HX-Redirect", target)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// httpError is an error with a status code and a message that is safe to
// show to the user.
type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

func badRequest(msg string) error { return &httpError{status: http.StatusBadRequest, msg: msg} }

// fail reports an error to the client: the status and message of an
// *httpError, 404 for store.ErrNotFound, 413 for oversized bodies, and a
// generic 500 otherwise (details are logged, never shown). htmx requests get
// the message swapped into the page's #notice area.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, msg := http.StatusInternalServerError, "Something went wrong. Try again in a moment."
	var he *httpError
	var mbe *http.MaxBytesError
	switch {
	case errors.As(err, &he):
		status, msg = he.status, he.msg
	case errors.Is(err, store.ErrNotFound):
		status, msg = http.StatusNotFound, "That item does not exist (any more)."
	case errors.Is(err, store.ErrInvalid):
		s.log.Debug("invalid input", "method", r.Method, "path", r.URL.Path, "err", err)
		status, msg = http.StatusBadRequest, "That value is not valid."
	case errors.As(err, &mbe):
		status, msg = http.StatusRequestEntityTooLarge, "The request is too large."
	case errors.Is(err, context.Canceled):
		s.log.Debug("request canceled", "method", r.Method, "path", r.URL.Path)
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	}
	s.writeError(w, r, status, msg)
}

// writeError renders an error response without touching the database
// unless a full page is needed.
func (s *server) writeError(w http.ResponseWriter, r *http.Request, status int, msg string) {
	if isHTMX(r) {
		w.Header().Set("HX-Retarget", "#notice")
		w.Header().Set("HX-Reswap", "innerHTML")
		var buf bytes.Buffer
		if err := s.tmpl.base.ExecuteTemplate(&buf, "notice", noticeView{Text: msg, Error: true}); err != nil {
			http.Error(w, http.StatusText(status), status)
			return
		}
		writeHTML(w, status, &buf)
		return
	}
	ev := errorView{Heading: errorHeading(status), Message: msg}
	var buf bytes.Buffer
	sb := buildSidebar(sidebarData{}, store.Scope{})
	sb.Unavailable = true
	if status != http.StatusInternalServerError {
		if sd, err := s.loadSidebarData(r.Context()); err == nil {
			sb = buildSidebar(sd, store.Scope{})
		}
	}
	if err := s.tmpl.pages["error"].ExecuteTemplate(&buf, "layout", layoutData{Title: ev.Heading, Sidebar: sb, Main: ev}); err != nil {
		http.Error(w, msg, status)
		return
	}
	writeHTML(w, status, &buf)
}

// noticeView is a one-line message in the #notice area.
type noticeView struct {
	Text  string
	Error bool
}

type errorView struct {
	Heading string
	Message string
}

func errorHeading(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "That request did not work"
	case http.StatusNotFound:
		return "Not found"
	case http.StatusConflict:
		return "Already exists"
	case http.StatusRequestEntityTooLarge:
		return "Too large"
	case http.StatusUnprocessableEntity:
		return "Check your input"
	case http.StatusInternalServerError:
		return "Something went wrong"
	}
	return http.StatusText(status)
}

// formError answers an htmx form whose target is its own inline error slot
// with 422 and the message; non-htmx posts get a full error page.
func (s *server) formError(w http.ResponseWriter, r *http.Request, msg string) {
	if !isHTMX(r) {
		s.writeError(w, r, http.StatusUnprocessableEntity, msg)
		return
	}
	s.renderFragment(w, r, http.StatusUnprocessableEntity, "form-error", msg, false)
}

// parseForm parses the request body, mapping oversized bodies to 413.
func parseForm(r *http.Request) error {
	if err := r.ParseForm(); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			return err
		}
		return badRequest("The form could not be read.")
	}
	return nil
}

// currentScope recovers the scope of the page an htmx request came from, so
// out-of-band sidebar updates keep the active tag highlighted.
func currentScope(r *http.Request) store.Scope {
	cur := r.Header.Get("HX-Current-URL")
	if cur == "" {
		return store.Scope{}
	}
	u, err := url.Parse(cur)
	if err != nil || u.Path != "/" {
		return store.Scope{}
	}
	sc, _, err := parseScope(u.Query())
	if err != nil {
		return store.Scope{}
	}
	return sc
}
