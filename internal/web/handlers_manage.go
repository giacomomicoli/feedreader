package web

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Organizing: feed settings, folders and tags. Small forms post
// with htmx into their own inline error slot and navigate with HX-Redirect
// on success, so the sidebar is rebuilt; without JS they post normally and
// get a 303.

// feedSettingsView is the feed settings page.
type feedSettingsView struct {
	ID              int64
	Title           string
	OriginalTitle   string
	KindLabel       string
	ScopeHref       string
	FeedURL         string
	FeedLink        string
	SiteURL         string
	SiteLink        string
	Folders         []folderOption
	IntervalValue   string
	DefaultInterval string
	MinInterval     string
	LastFetch       string
	LastFetchRel    string
	NextFetch       string
	NextFetchRel    string
	ErrorCount      int
	LastError       string
	Warn            bool
}

// feedForPath loads the feed named by the {id} path value.
func (s *server) feedForPath(r *http.Request) (store.Feed, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return store.Feed{}, err
	}
	f, err := s.store.GetFeed(r.Context(), id)
	if err != nil {
		return store.Feed{}, fmt.Errorf("get feed %d: %w", id, err)
	}
	return f, nil
}

// handleFeedSettings renders /feeds/{id}: rename, move, poll interval,
// fetch status, refresh and unsubscribe.
func (s *server) handleFeedSettings(w http.ResponseWriter, r *http.Request) {
	f, err := s.feedForPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	folders, err := s.store.ListFolders(r.Context())
	if err != nil {
		s.fail(w, r, fmt.Errorf("list folders: %w", err))
		return
	}
	now := s.now()
	v := feedSettingsView{
		ID:              f.ID,
		Title:           f.Title,
		KindLabel:       kindLabel(f.Kind),
		ScopeHref:       feedHref(f.ID, ""),
		FeedURL:         f.URL,
		FeedLink:        httpURL(f.URL),
		SiteURL:         f.SiteURL,
		SiteLink:        httpURL(f.SiteURL),
		Folders:         folderOptions(folders, f.FolderID),
		DefaultInterval: formatInterval(sched.DefaultInterval(s.cfg, f.Kind)),
		MinInterval:     formatInterval(config.MinPollInterval),
		LastFetch:       absTime(f.LastFetchedAt),
		NextFetch:       absTime(f.NextFetchAt),
		ErrorCount:      f.ErrorCount,
		LastError:       f.LastError,
		Warn:            f.ErrorCount >= config.WarnAfterFailures,
	}
	if f.OriginalTitle != "" && f.OriginalTitle != f.Title {
		v.OriginalTitle = f.OriginalTitle
	}
	if f.IntervalSec > 0 {
		v.IntervalValue = formatInterval(time.Duration(f.IntervalSec) * time.Second)
	}
	if !f.LastFetchedAt.IsZero() {
		v.LastFetchRel = relTime(f.LastFetchedAt, now)
	}
	if !f.NextFetchAt.IsZero() {
		v.NextFetchRel = relTime(f.NextFetchAt, now)
	}
	s.renderPage(w, r, http.StatusOK, "feed", f.Title, store.Scope{Kind: store.ScopeFeed, ID: f.ID}, v)
}

func settingsHref(id int64) string { return "/feeds/" + strconv.FormatInt(id, 10) }

// handleFeedRename sets the feed's user-editable title.
func (s *server) handleFeedRename(w http.ResponseWriter, r *http.Request) {
	f, err := s.feedForPath(r)
	if err == nil {
		err = parseForm(r)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	title := cleanName(r.PostForm.Get("title"))
	switch {
	case title == "":
		s.formError(w, r, "Enter a title.")
		return
	case tooLong(title):
		s.formError(w, r, fmt.Sprintf("Titles can be at most %d characters.", maxNameLen))
		return
	}
	if err := s.store.RenameFeed(r.Context(), f.ID, title); err != nil {
		s.fail(w, r, fmt.Errorf("rename feed %d: %w", f.ID, err))
		return
	}
	s.redirect(w, r, settingsHref(f.ID))
}

// handleFeedMove moves the feed to a folder (0 = uncategorized).
func (s *server) handleFeedMove(w http.ResponseWriter, r *http.Request) {
	f, err := s.feedForPath(r)
	if err == nil {
		err = parseForm(r)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var folderID int64
	if sel := r.PostForm.Get("folder"); sel != "" && sel != "0" {
		if folderID, err = parseID(sel); err != nil {
			s.fail(w, r, badRequest("Invalid folder."))
			return
		}
		if _, err := s.store.GetFolder(r.Context(), folderID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				s.formError(w, r, "That folder no longer exists. Pick another one.")
				return
			}
			s.fail(w, r, fmt.Errorf("get folder %d: %w", folderID, err))
			return
		}
	}
	if err := s.store.MoveFeed(r.Context(), f.ID, folderID); err != nil {
		s.fail(w, r, fmt.Errorf("move feed %d: %w", f.ID, err))
		return
	}
	s.redirect(w, r, settingsHref(f.ID))
}

// handleFeedInterval sets or clears the per-feed poll interval override.
func (s *server) handleFeedInterval(w http.ResponseWriter, r *http.Request) {
	f, err := s.feedForPath(r)
	if err == nil {
		err = parseForm(r)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	d, err := parseInterval(r.PostForm.Get("interval"))
	if err != nil {
		s.formError(w, r, err.Error())
		return
	}
	if err := s.store.SetFeedInterval(r.Context(), f.ID, int(d/time.Second)); err != nil {
		s.fail(w, r, fmt.Errorf("set interval of feed %d: %w", f.ID, err))
		return
	}
	// A shorter interval (or clearing the override back to a shorter
	// default) applies now, not only after the old schedule fires. The
	// interval is stored either way, so a failure here is not fatal: the
	// next poll uses it.
	if rs, ok := s.sched.(rescheduler); ok {
		if err := rs.Reschedule(r.Context(), f.ID); err != nil {
			s.log.Warn("could not reschedule feed after interval change", "feed", f.ID, "err", err)
		}
	}
	s.redirect(w, r, settingsHref(f.ID))
}

// parseInterval reads a poll interval typed as minutes ("90") or as a Go
// duration ("2h", "1h30m"). Empty means "use the default of the feed's kind" (0).
func parseInterval(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}
	invalid := &httpError{status: http.StatusUnprocessableEntity, msg: fmt.Sprintf(
		"Enter minutes (like 90) or a duration (like 2h or 1h30m), at least %s.", formatInterval(config.MinPollInterval))}
	var d time.Duration
	if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
		if n <= 0 || n > math.MaxInt64/int64(time.Minute) {
			return 0, invalid
		}
		d = time.Duration(n) * time.Minute
	} else if d, err = time.ParseDuration(raw); err != nil {
		return 0, invalid
	}
	if d < config.MinPollInterval {
		return 0, &httpError{status: http.StatusUnprocessableEntity,
			msg: fmt.Sprintf("The interval must be at least %s.", formatInterval(config.MinPollInterval))}
	}
	return d, nil
}

// handleUnsubscribeConfirm is the first step of unsubscribing: it states
// how many entries, including saved ones, will be deleted.
func (s *server) handleUnsubscribeConfirm(w http.ResponseWriter, r *http.Request) {
	f, err := s.feedForPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	st, err := s.store.FeedEntryStats(r.Context(), f.ID)
	if err != nil {
		s.fail(w, r, fmt.Errorf("feed entry stats %d: %w", f.ID, err))
		return
	}
	s.renderConfirm(w, r, unsubscribeConfirm(f, st), store.Scope{Kind: store.ScopeFeed, ID: f.ID})
}

// handleUnsubscribe deletes the feed, its entries and their tags.
func (s *server) handleUnsubscribe(w http.ResponseWriter, r *http.Request) {
	f, err := s.feedForPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.DeleteFeed(r.Context(), f.ID); err != nil {
		s.fail(w, r, fmt.Errorf("delete feed %d: %w", f.ID, err))
		return
	}
	s.log.Info("unsubscribed", "feed", f.ID, "url", redactURL(f.URL))
	s.redirect(w, r, "/")
}

// renderConfirm shows a confirmation panel: inline for htmx, as a page
// otherwise.
func (s *server) renderConfirm(w http.ResponseWriter, r *http.Request, v confirmView, cur store.Scope) {
	if isHTMX(r) {
		s.renderFragment(w, r, http.StatusOK, "confirm-panel", v, false)
		return
	}
	s.renderPage(w, r, http.StatusOK, "confirm", v.Heading, cur, v)
}

// --- Folders ---

// validName checks a cleaned folder name and reports a message if invalid.
func validName(name, what string) string {
	switch {
	case name == "":
		return "Enter a " + what + " name."
	case tooLong(name):
		return fmt.Sprintf("%s names can be at most %d characters.", capitalize(what), maxNameLen)
	}
	return ""
}

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func folderHref(id int64) string {
	return scopeHref(store.Scope{Kind: store.ScopeFolder, ID: id}, "")
}

// handleFolderCreate creates a folder from the sidebar form.
func (s *server) handleFolderCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		s.fail(w, r, err)
		return
	}
	name := cleanName(r.PostForm.Get("name"))
	if msg := validName(name, "folder"); msg != "" {
		s.formError(w, r, msg)
		return
	}
	f, err := s.store.CreateFolder(r.Context(), name)
	if errors.Is(err, store.ErrConflict) {
		s.formError(w, r, "A folder with that name already exists.")
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("create folder: %w", err))
		return
	}
	s.redirect(w, r, folderHref(f.ID))
}

// folderForPath loads the folder named by the {id} path value.
func (s *server) folderForPath(r *http.Request) (store.Folder, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return store.Folder{}, err
	}
	f, err := s.store.GetFolder(r.Context(), id)
	if err != nil {
		return store.Folder{}, fmt.Errorf("get folder %d: %w", id, err)
	}
	return f, nil
}

// handleFolderRename renames a folder.
func (s *server) handleFolderRename(w http.ResponseWriter, r *http.Request) {
	f, err := s.folderForPath(r)
	if err == nil {
		err = parseForm(r)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := cleanName(r.PostForm.Get("name"))
	if msg := validName(name, "folder"); msg != "" {
		s.formError(w, r, msg)
		return
	}
	err = s.store.RenameFolder(r.Context(), f.ID, name)
	if errors.Is(err, store.ErrConflict) {
		s.formError(w, r, "A folder with that name already exists.")
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("rename folder %d: %w", f.ID, err))
		return
	}
	s.redirect(w, r, folderHref(f.ID))
}

// handleFolderDeleteConfirm asks before deleting a folder.
func (s *server) handleFolderDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	f, err := s.folderForPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	feeds, err := s.store.ListFeeds(r.Context())
	if err != nil {
		s.fail(w, r, fmt.Errorf("list feeds: %w", err))
		return
	}
	n := 0
	for _, fd := range feeds {
		if fd.FolderID == f.ID {
			n++
		}
	}
	line := "The folder is empty."
	switch n {
	case 0:
	case 1:
		line = "Its feed stays subscribed and moves to the list of feeds without a folder."
	default:
		line = fmt.Sprintf("Its %d feeds stay subscribed and move to the list of feeds without a folder.", n)
	}
	v := confirmView{
		Heading:    fmt.Sprintf("Delete folder “%s”?", f.Name),
		Lines:      []string{line},
		Action:     "/folders/" + strconv.FormatInt(f.ID, 10) + "/delete",
		Button:     "Delete folder",
		CancelHref: folderHref(f.ID),
	}
	s.renderConfirm(w, r, v, store.Scope{Kind: store.ScopeFolder, ID: f.ID})
}

// handleFolderDelete deletes a folder; its feeds become uncategorized.
func (s *server) handleFolderDelete(w http.ResponseWriter, r *http.Request) {
	f, err := s.folderForPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.DeleteFolder(r.Context(), f.ID); err != nil {
		s.fail(w, r, fmt.Errorf("delete folder %d: %w", f.ID, err))
		return
	}
	s.redirect(w, r, "/")
}

// --- Tags ---

// tagForPath loads the tag named by the {id} path value, with its count.
func (s *server) tagForPath(r *http.Request) (store.Tag, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return store.Tag{}, err
	}
	t, err := s.store.GetTag(r.Context(), id)
	if err != nil {
		return store.Tag{}, fmt.Errorf("get tag %d: %w", id, err)
	}
	return t, nil
}

// handleTagDeleteConfirm asks before deleting a tag.
func (s *server) handleTagDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	t, err := s.tagForPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tags, err := s.store.ListTags(r.Context())
	if err != nil {
		s.fail(w, r, fmt.Errorf("list tags: %w", err))
		return
	}
	for _, x := range tags {
		if x.ID == t.ID {
			t.Count = x.Count
		}
	}
	v := confirmView{
		Heading:    fmt.Sprintf("Delete tag “%s”?", t.Name),
		Lines:      []string{fmt.Sprintf("It is removed from %s. The entries themselves are kept.", pluralEntries(t.Count))},
		Action:     "/tags/" + strconv.FormatInt(t.ID, 10) + "/delete",
		Button:     "Delete tag",
		CancelHref: scopeHref(store.Scope{Kind: store.ScopeTag, ID: t.ID}, ""),
	}
	s.renderConfirm(w, r, v, store.Scope{Kind: store.ScopeTag, ID: t.ID})
}

// handleTagDelete removes a tag from all entries (only entry_tags rows go
// away; the entries stay).
func (s *server) handleTagDelete(w http.ResponseWriter, r *http.Request) {
	t, err := s.tagForPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.DeleteTag(r.Context(), t.ID); err != nil {
		s.fail(w, r, fmt.Errorf("delete tag %d: %w", t.ID, err))
		return
	}
	s.redirect(w, r, "/")
}
