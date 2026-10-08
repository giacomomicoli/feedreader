package web

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/fetch"
	"github.com/giacomomicoli/feedreader/internal/parse"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Add-source flow: URL → resolve → (pick a candidate) → preview →
// confirm title and folder → subscribe.

const (
	stepURL     = "url"
	stepChoose  = "choose"
	stepConfirm = "confirm"

	folderNew = "new" // <select> value revealing the new-folder name input
)

// addView drives the "add-flow" template; Step selects the form shown.
type addView struct {
	Step        string
	URL         string // what the user typed
	Error       string
	ShowDirect  bool // offer "enter the feed URL directly"
	DirectURL   string
	Conflict    *conflictView
	Candidates  []candidateView
	Preview     previewView
	Title       string
	Folders     []folderOption
	NewSelected bool
	NewFolder   string
}

// conflictView links to a feed the user is already subscribed to.
type conflictView struct {
	Title string
	Href  string
}

type candidateView struct {
	URL   string
	Label string
	Type  string
}

// previewView is the result of the immediate first fetch.
type previewView struct {
	FeedURL    string
	Kind       store.Kind
	KindLabel  string
	SiteURL    string
	EntryCount int
	CountText  string
	UnreadNote string
}

func newPreviewView(feedURL string, kind store.Kind, siteURL string, n int) previewView {
	if kind != store.KindYouTube {
		kind = store.KindRSS
	}
	p := previewView{
		FeedURL:    feedURL,
		Kind:       kind,
		KindLabel:  kindLabel(kind),
		SiteURL:    httpURL(siteURL),
		EntryCount: n,
		CountText:  pluralEntries(n) + " found.",
	}
	switch {
	case n == 0:
		p.CountText = "The feed has no entries yet."
	case n <= config.InitialUnread:
		p.UnreadNote = "All of them will be marked unread."
	default:
		p.UnreadNote = fmt.Sprintf("The %d newest will be marked unread; older ones are kept as read.", config.InitialUnread)
	}
	return p
}

// renderAdd sends the add-flow section (htmx) or the whole add page.
func (s *server) renderAdd(w http.ResponseWriter, r *http.Request, status int, v addView) {
	if isHTMX(r) {
		s.renderFragment(w, r, status, "add-flow", v, false)
		return
	}
	s.renderPage(w, r, status, "add", "Add source", store.Scope{}, v)
}

// handleAddPage shows the URL input.
func (s *server) handleAddPage(w http.ResponseWriter, r *http.Request) {
	s.renderPage(w, r, http.StatusOK, "add", "Add source", store.Scope{}, addView{Step: stepURL})
}

// handleAddResolve resolves the pasted URL to one or more feeds.
func (s *server) handleAddResolve(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		s.fail(w, r, err)
		return
	}
	input := strings.TrimSpace(r.PostForm.Get("url"))
	v := addView{Step: stepURL, URL: input}
	if input == "" || len(input) > maxURLLen {
		v.Error = resolve.ErrInvalidURL.Error()
		s.renderAdd(w, r, http.StatusUnprocessableEntity, v)
		return
	}
	res, err := s.resolver.Resolve(r.Context(), input)
	if err == nil && (res == nil || len(res.Candidates) == 0) {
		err = resolve.ErrNoFeed
	}
	if err != nil {
		v.Error = s.userMessage(r, err, "Could not check this URL.")
		v.ShowDirect, v.DirectURL = true, input
		s.renderAdd(w, r, http.StatusUnprocessableEntity, v)
		return
	}
	if len(res.Candidates) == 1 {
		s.previewStep(w, r, res.Candidates[0].URL, input)
		return
	}
	v.Step = stepChoose
	for _, c := range res.Candidates {
		label := c.Title
		if label == "" {
			label = c.URL
		}
		v.Candidates = append(v.Candidates, candidateView{URL: c.URL, Label: label, Type: c.Type})
	}
	s.renderAdd(w, r, http.StatusOK, v)
}

// handleAddPreview fetches a chosen or directly entered feed URL once and
// shows the confirmation form.
func (s *server) handleAddPreview(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		s.fail(w, r, err)
		return
	}
	input := strings.TrimSpace(r.PostForm.Get("url"))
	feedURL := normalizeFeedURL(r.PostForm.Get("feed_url"))
	if feedURL == "" {
		v := addView{Step: stepURL, URL: input, Error: resolve.ErrInvalidURL.Error(),
			ShowDirect: true, DirectURL: r.PostForm.Get("feed_url")}
		s.renderAdd(w, r, http.StatusUnprocessableEntity, v)
		return
	}
	if input == "" {
		input = feedURL
	}
	s.previewStep(w, r, feedURL, input)
}

// normalizeFeedURL accepts a typed feed URL, defaulting to https:// when
// the scheme is missing; anything but http(s) yields "".
func normalizeFeedURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > maxURLLen {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	return httpURL(raw)
}

// previewStep shows "already subscribed", an inline fetch error, or the
// confirmation form for feedURL.
func (s *server) previewStep(w http.ResponseWriter, r *http.Request, feedURL, input string) {
	ctx := r.Context()
	v := addView{Step: stepURL, URL: input}
	if s.renderIfSubscribed(w, r, feedURL, v) {
		return
	}
	p, err := s.sched.Preview(ctx, feedURL)
	if err == nil && p == nil {
		err = parse.ErrNotAFeed
	}
	if err != nil {
		if errors.Is(err, store.ErrConflict) && s.renderIfSubscribed(w, r, feedURL, v) {
			return
		}
		v.Error = s.userMessage(r, err, "Could not load this feed.")
		v.ShowDirect, v.DirectURL = true, feedURL
		s.renderAdd(w, r, http.StatusUnprocessableEntity, v)
		return
	}
	finalURL := p.FeedURL
	if finalURL == "" {
		finalURL = feedURL
	}
	if finalURL != feedURL && s.renderIfSubscribed(w, r, finalURL, v) {
		return
	}
	folders, err := s.store.ListFolders(ctx)
	if err != nil {
		s.fail(w, r, fmt.Errorf("list folders: %w", err))
		return
	}
	v.Step = stepConfirm
	v.Preview = newPreviewView(finalURL, p.Kind, p.SiteURL, p.EntryCount)
	v.Title = p.Title
	v.Folders = folderOptions(folders, 0)
	s.renderAdd(w, r, http.StatusOK, v)
}

// renderIfSubscribed answers 409 with a link to the existing feed when
// feedURL is already subscribed. It reports whether it wrote a response.
func (s *server) renderIfSubscribed(w http.ResponseWriter, r *http.Request, feedURL string, v addView) bool {
	f, err := s.store.GetFeedByURL(r.Context(), feedURL)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return false
	case err != nil:
		s.fail(w, r, fmt.Errorf("get feed by url: %w", err))
		return true
	}
	v.Step = stepURL
	v.Conflict = &conflictView{Title: f.Title, Href: feedHref(f.ID, "")}
	s.renderAdd(w, r, http.StatusConflict, v)
	return true
}

// handleAddFolderField reveals the new-folder name input when "New folder…"
// is selected (htmx swap on change).
func (s *server) handleAddFolderField(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("folder") != folderNew {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		return
	}
	s.renderFragment(w, r, http.StatusOK, "add-new-folder", addView{}, false)
}

// handleAddSubscribe stores the feed and redirects to its grid.
func (s *server) handleAddSubscribe(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	f := r.PostForm
	feedURL := normalizeFeedURL(f.Get("feed_url"))
	if feedURL == "" {
		s.renderAdd(w, r, http.StatusUnprocessableEntity, addView{Step: stepURL,
			Error: resolve.ErrInvalidURL.Error(), ShowDirect: true, DirectURL: f.Get("feed_url")})
		return
	}
	entries, _ := strconv.Atoi(f.Get("entries"))
	v := addView{
		Step:        stepConfirm,
		Preview:     newPreviewView(feedURL, store.Kind(f.Get("kind")), f.Get("site_url"), max(entries, 0)),
		Title:       cleanName(f.Get("title")),
		NewSelected: f.Get("folder") == folderNew,
		NewFolder:   cleanName(f.Get("new_folder")),
	}
	folders, err := s.store.ListFolders(ctx)
	if err != nil {
		s.fail(w, r, fmt.Errorf("list folders: %w", err))
		return
	}
	selected, _ := strconv.ParseInt(f.Get("folder"), 10, 64)
	v.Folders = folderOptions(folders, selected)
	if tooLong(v.Title) {
		v.Error = fmt.Sprintf("Titles can be at most %d characters.", maxNameLen)
		s.renderAdd(w, r, http.StatusUnprocessableEntity, v)
		return
	}

	folderID, created, err := s.resolveFolder(ctx, folders, f.Get("folder"), v.NewFolder)
	if err != nil {
		var he *httpError
		if errors.As(err, &he) {
			v.Error = he.msg
			s.renderAdd(w, r, he.status, v)
			return
		}
		s.fail(w, r, err)
		return
	}

	feed, err := s.sched.Subscribe(ctx, sched.Subscription{FeedURL: feedURL, Title: v.Title, FolderID: folderID})
	if err != nil {
		if created {
			s.rollbackFolder(context.WithoutCancel(ctx), folderID)
		}
		if errors.Is(err, store.ErrConflict) && s.renderIfSubscribed(w, r, feedURL, addView{URL: feedURL}) {
			return
		}
		if msg, ok := userFacing(err, s.cfg.MaxBodyBytes); ok {
			v.Error = msg
			s.renderAdd(w, r, http.StatusUnprocessableEntity, v)
			return
		}
		s.fail(w, r, fmt.Errorf("subscribe %s: %w", redactURL(feedURL), err))
		return
	}
	s.log.Info("subscribed", "feed", feed.ID, "url", redactURL(feed.URL))
	s.redirect(w, r, feedHref(feed.ID, filterAll))
}

// emptyFolderDeleter is implemented by a store that can delete a folder only
// while no feed is in it, atomically.
type emptyFolderDeleter interface {
	DeleteFolderIfEmpty(ctx context.Context, id int64) (deleted bool, err error)
}

// The production store takes the atomic path.
var _ emptyFolderDeleter = (*store.SQLite)(nil)

// rollbackFolder deletes the folder that "New folder…" created for a
// subscription that then failed, unless it is no longer empty: from the
// moment it was created another request may have reused it (a concurrent
// add with the same new-folder name) or moved a feed into it, and deleting
// it would silently make those feeds uncategorized.
func (s *server) rollbackFolder(ctx context.Context, folderID int64) {
	if d, ok := s.store.(emptyFolderDeleter); ok {
		if _, err := d.DeleteFolderIfEmpty(ctx, folderID); err != nil {
			s.log.Warn("could not roll back new folder", "folder", folderID, "err", err)
		}
		return
	}
	feeds, err := s.store.ListFeeds(ctx)
	if err != nil {
		s.log.Warn("could not roll back new folder", "folder", folderID, "err", err)
		return
	}
	for _, f := range feeds {
		if f.FolderID == folderID {
			return // in use: keep it
		}
	}
	if err := s.store.DeleteFolder(ctx, folderID); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.log.Warn("could not roll back new folder", "folder", folderID, "err", err)
	}
}

// resolveFolder turns the folder <select> value into a folder id, creating
// the folder for "New folder…" (or reusing an existing one with the same
// name). created reports whether a new folder was inserted.
func (s *server) resolveFolder(ctx context.Context, folders []store.Folder, sel, newName string) (id int64, created bool, err error) {
	switch sel {
	case "", "0":
		return 0, false, nil
	case folderNew:
		if newName == "" {
			return 0, false, &httpError{status: http.StatusUnprocessableEntity, msg: "Enter a name for the new folder."}
		}
		if tooLong(newName) {
			return 0, false, &httpError{status: http.StatusUnprocessableEntity,
				msg: fmt.Sprintf("Folder names can be at most %d characters.", maxNameLen)}
		}
		if f, ok := findFolderByName(folders, newName); ok {
			return f.ID, false, nil
		}
		f, err := s.store.CreateFolder(ctx, newName)
		if errors.Is(err, store.ErrConflict) {
			// Created concurrently (or differs only in case): reuse it.
			all, lerr := s.store.ListFolders(ctx)
			if lerr != nil {
				return 0, false, fmt.Errorf("list folders: %w", lerr)
			}
			if existing, ok := findFolderByName(all, newName); ok {
				return existing.ID, false, nil
			}
		}
		if err != nil {
			return 0, false, fmt.Errorf("create folder: %w", err)
		}
		return f.ID, true, nil
	}
	fid, err := parseID(sel)
	if err != nil {
		return 0, false, badRequest("Invalid folder.")
	}
	if _, err := s.store.GetFolder(ctx, fid); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return 0, false, &httpError{status: http.StatusUnprocessableEntity, msg: "That folder no longer exists. Pick another one."}
		}
		return 0, false, fmt.Errorf("get folder %d: %w", fid, err)
	}
	return fid, false, nil
}

// userMessage converts resolve/fetch/parse errors into a message for the
// inline error under the URL input; anything unexpected is logged and
// replaced by fallback.
func (s *server) userMessage(r *http.Request, err error, fallback string) string {
	if msg, ok := userFacing(err, s.cfg.MaxBodyBytes); ok {
		s.log.Debug("add source failed", "path", r.URL.Path, "err", err)
		return msg
	}
	s.log.Warn("add source failed", "path", r.URL.Path, "err", err)
	return fallback + " Try again, or enter the feed URL directly."
}

// userFacing maps the errors of the add pipeline to plain-language messages.
func userFacing(err error, maxBody int64) (string, bool) {
	for _, sentinel := range []error{resolve.ErrInvalidURL, resolve.ErrNoFeed, resolve.ErrYouTubeID} {
		if errors.Is(err, sentinel) {
			return sentinel.Error(), true
		}
	}
	var se *fetch.StatusError
	var dnsErr *net.DNSError
	var netErr net.Error
	switch {
	case errors.Is(err, parse.ErrNotAFeed):
		return "This URL does not return an RSS, Atom or JSON feed.", true
	case errors.As(err, &se):
		return fmt.Sprintf("The site answered with HTTP %d.", se.StatusCode), true
	case errors.Is(err, fetch.ErrTooLarge):
		if maxBody >= 1<<20 {
			return fmt.Sprintf("The response is larger than the %d MB limit.", maxBody>>20), true
		}
		return "The response is larger than the size limit.", true
	case errors.Is(err, fetch.ErrRedirects):
		return "The URL redirects too many times or in a loop.", true
	case errors.Is(err, fetch.ErrScheme):
		return "Only http and https URLs are supported.", true
	case errors.Is(err, context.DeadlineExceeded):
		return "The site took too long to respond.", true
	case errors.As(err, &dnsErr):
		return "Could not find that host name.", true
	case errors.As(err, &netErr):
		return "Could not connect to the site.", true
	}
	return "", false
}
