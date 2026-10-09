package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/giacomomicoli/feedreader/internal/store"
)

// suggestLimit is how many tags the autocomplete offers.
const suggestLimit = 10

// scopeMeta is a validated scope with its display title.
type scopeMeta struct {
	scope    store.Scope
	title    string
	subtitle string
}

// loadScope validates that the scope's folder, feed or tag exists and
// returns its title. Unknown ids yield store.ErrNotFound.
func (s *server) loadScope(ctx context.Context, sc store.Scope) (scopeMeta, error) {
	m := scopeMeta{scope: sc}
	switch sc.Kind {
	case store.ScopeAll:
		m.title = "All"
	case store.ScopeWatchLater:
		m.title, m.subtitle = "Watch later", "Videos you saved, read or not"
	case store.ScopeReadLater:
		m.title, m.subtitle = "Read later", "Articles you saved, read or not"
	case store.ScopeFavourites:
		m.title = "Favourites"
	case store.ScopeFolder:
		f, err := s.store.GetFolder(ctx, sc.ID)
		if err != nil {
			return m, fmt.Errorf("get folder %d: %w", sc.ID, err)
		}
		m.title, m.subtitle = f.Name, "Folder"
	case store.ScopeFeed:
		f, err := s.store.GetFeed(ctx, sc.ID)
		if err != nil {
			return m, fmt.Errorf("get feed %d: %w", sc.ID, err)
		}
		m.title, m.subtitle = f.Title, kindLabel(f.Kind)
	case store.ScopeTag:
		t, err := s.store.GetTag(ctx, sc.ID)
		if err != nil {
			return m, fmt.Errorf("get tag %d: %w", sc.ID, err)
		}
		m.title, m.subtitle = t.Name, "Tag"
	default:
		return m, badRequest("Unknown scope.")
	}
	return m, nil
}

// requestScope parses and validates the scope of a request.
func (s *server) requestScope(ctx context.Context, v url.Values) (scopeMeta, string, error) {
	sc, filter, err := parseScope(v)
	if err != nil {
		return scopeMeta{}, "", badRequest("Unknown scope or filter.")
	}
	m, err := s.loadScope(ctx, sc)
	return m, filter, err
}

// listQuery builds the store query for a scope page.
func listQuery(sc store.Scope, filter string, after *store.Cursor) store.ListQuery {
	return store.ListQuery{
		Scope:      sc,
		UnreadOnly: filter == filterUnread && !sc.IgnoresReadFilter(),
		Limit:      pageSize(sc),
		After:      after,
	}
}

// loadGrid fetches one page of cards. back is the page cards return to
// after non-htmx actions.
func (s *server) loadGrid(ctx context.Context, sc store.Scope, filter string, after *store.Cursor, back string) (gridView, error) {
	page, err := s.store.ListEntries(ctx, listQuery(sc, filter, after))
	if err != nil {
		return gridView{}, fmt.Errorf("list entries: %w", err)
	}
	now := s.now()
	g := gridView{Cards: make([]cardView, 0, len(page.Entries))}
	for _, e := range page.Entries {
		g.Cards = append(g.Cards, newCard(e, now, back))
	}
	if page.HasMore && len(page.Entries) > 0 {
		params := scopeParams(sc, filter) + "&after=" + url.QueryEscape(encodeCursor(page.Next))
		g.More = &moreView{Href: "/?" + params, HxHref: "/entries?" + params}
	}
	if len(g.Cards) == 0 {
		g.EmptyText, g.EmptyHint, g.EmptyLink = emptyMessage(sc, filter)
	}
	return g, nil
}

// buildScopeView assembles the main pane of the home page.
func (s *server) buildScopeView(ctx context.Context, m scopeMeta, filter string, after *store.Cursor) (scopeView, error) {
	sc := m.scope
	v := scopeView{
		Title:      m.title,
		Subtitle:   m.subtitle,
		Kind:       sc.Kind,
		ID:         sc.ID,
		Filter:     filter,
		ShowFilter: !sc.IgnoresReadFilter(),
		UnreadOnly: filter == filterUnread,
		UnreadHref: scopeHref(sc, filterUnread),
		AllHref:    scopeHref(sc, filterAll),
		Href:       scopeHref(sc, filter),
		IsFeed:     sc.Kind == store.ScopeFeed,
		IsFolder:   sc.Kind == store.ScopeFolder,
		IsTag:      sc.Kind == store.ScopeTag,
	}
	if v.IsFeed {
		v.SettingsHref = "/feeds/" + strconv.FormatInt(sc.ID, 10)
	}
	feeds, err := s.store.ListFeeds(ctx)
	if err != nil {
		return v, fmt.Errorf("list feeds: %w", err)
	}
	v.HasFeeds = len(feeds) > 0
	if !v.HasFeeds {
		// With no feeds, the empty state is a single "Add your first source" card.
		v.Grid = gridView{NoFeeds: true}
		return v, nil
	}
	// The watermark is read before the entries are listed: every entry
	// stored after this point has a larger id, so "Mark all as read" leaves
	// it unread whatever its fetch time or the clock says.
	if v.UpTo, err = s.store.MaxEntryID(ctx); err != nil {
		return v, fmt.Errorf("max entry id: %w", err)
	}
	v.Grid, err = s.loadGrid(ctx, sc, filter, after, v.Href)
	return v, err
}

// handleHome renders the card grid page for ?scope=…&id=…&filter=….
func (s *server) handleHome(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	m, filter, err := s.requestScope(r.Context(), q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	after, err := decodeCursor(q.Get("after"))
	if err != nil {
		s.fail(w, r, badRequest("Invalid page position."))
		return
	}
	v, err := s.buildScopeView(r.Context(), m, filter, after)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderPage(w, r, http.StatusOK, "home", m.title, m.scope, v)
}

// handleEntries returns the next "Load more" block: cards plus a new button
// (none when the scope is exhausted).
func (s *server) handleEntries(w http.ResponseWriter, r *http.Request) {
	if !isHTMX(r) {
		http.Redirect(w, r, "/?"+r.URL.RawQuery, http.StatusSeeOther)
		return
	}
	q := r.URL.Query()
	m, filter, err := s.requestScope(r.Context(), q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	after, err := decodeCursor(q.Get("after"))
	if err != nil {
		s.fail(w, r, badRequest("Invalid page position."))
		return
	}
	g, err := s.loadGrid(r.Context(), m.scope, filter, after, scopeHref(m.scope, filter))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderFragment(w, r, http.StatusOK, "grid-items", g, false)
}

// handleEntry shows one entry's full summary: the content of the layout's
// entry dialog for htmx (the card's Summary link), a page of its own
// otherwise, so the link also works without JavaScript. It is read-only:
// opening a summary never marks the entry read.
func (s *server) handleEntry(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	e, err := s.store.GetEntry(r.Context(), id)
	if err != nil {
		s.fail(w, r, fmt.Errorf("get entry %d: %w", id, err))
		return
	}
	v := newEntryView(e, s.now(), isHTMX(r))
	if v.Dialog {
		s.renderFragment(w, r, http.StatusOK, "entry-detail", v, false)
		return
	}
	s.renderPage(w, r, http.StatusOK, "entry", v.Title, store.Scope{}, v)
}

// handleEntryAction applies one quick action and returns the re-rendered
// card plus out-of-band sidebar counts.
func (s *server) handleEntryAction(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := parseForm(r); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	switch r.PathValue("action") {
	case "read":
		err = s.store.SetRead(ctx, id, true, s.now())
	case "unread":
		err = s.store.SetRead(ctx, id, false, time.Time{})
	case "later":
		err = s.store.SetLater(ctx, id, true)
	case "unlater":
		err = s.store.SetLater(ctx, id, false)
	case "favourite":
		err = s.store.SetFavourite(ctx, id, true)
	case "unfavourite":
		err = s.store.SetFavourite(ctx, id, false)
	default:
		s.handleNotFound(w, r)
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("entry %d %s: %w", id, r.PathValue("action"), err))
		return
	}
	s.respondCard(w, r, id)
}

// handleAddTag attaches a free-text tag to an entry.
func (s *server) handleAddTag(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := parseForm(r); err != nil {
		s.fail(w, r, err)
		return
	}
	name := cleanName(r.PostForm.Get("tag"))
	switch {
	case name == "":
		s.fail(w, r, &httpError{status: http.StatusUnprocessableEntity, msg: "Enter a tag name."})
		return
	case tooLong(name):
		s.fail(w, r, &httpError{status: http.StatusUnprocessableEntity,
			msg: fmt.Sprintf("Tag names can be at most %d characters.", maxNameLen)})
		return
	}
	ctx := r.Context()
	if _, err := s.store.GetEntry(ctx, id); err != nil {
		s.fail(w, r, fmt.Errorf("get entry %d: %w", id, err))
		return
	}
	if _, err := s.store.AddEntryTag(ctx, id, name); err != nil {
		s.fail(w, r, fmt.Errorf("tag entry %d: %w", id, err))
		return
	}
	s.respondCard(w, r, id)
}

// handleRemoveTag detaches a tag from an entry.
func (s *server) handleRemoveTag(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r, "id")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tagID, err := pathID(r, "tag")
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := parseForm(r); err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.RemoveEntryTag(r.Context(), id, tagID); err != nil {
		s.fail(w, r, fmt.Errorf("untag entry %d: %w", id, err))
		return
	}
	s.respondCard(w, r, id)
}

// respondCard answers a per-entry action: the card plus OOB counts for htmx,
// a 303 back to the originating page otherwise.
func (s *server) respondCard(w http.ResponseWriter, r *http.Request, id int64) {
	e, err := s.store.GetEntry(r.Context(), id)
	if err != nil {
		s.fail(w, r, fmt.Errorf("get entry %d: %w", id, err))
		return
	}
	back := safeBack(r.PostForm.Get("back"), "/")
	if !isHTMX(r) {
		http.Redirect(w, r, back, http.StatusSeeOther)
		return
	}
	s.renderFragment(w, r, http.StatusOK, "card", newCard(e, s.now(), back), true)
}

// handleSuggestTags feeds the tag input's <datalist> (autocomplete on
// existing tags).
func (s *server) handleSuggestTags(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	prefix := q.Get("q")
	if prefix == "" {
		prefix = q.Get("tag") // the name htmx sends from the card's tag input
	}
	prefix = cleanName(prefix)
	if prefix == "" || tooLong(prefix) {
		s.renderFragment(w, r, http.StatusOK, "tag-options", []store.Tag(nil), false)
		return
	}
	tags, err := s.store.SuggestTags(r.Context(), prefix, suggestLimit)
	if err != nil {
		s.fail(w, r, fmt.Errorf("suggest tags: %w", err))
		return
	}
	s.renderFragment(w, r, http.StatusOK, "tag-options", tags, false)
}

// handleMarkAllRead marks the scope read up to the page's up_to watermark
// (the largest entry id when the page was rendered, see buildScopeView), so
// entries stored after the user loaded the page stay unread, and returns the
// re-rendered scope view plus OOB counts.
func (s *server) handleMarkAllRead(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	m, filter, err := s.requestScope(ctx, r.PostForm)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	upTo, err := strconv.ParseInt(strings.TrimSpace(r.PostForm.Get("up_to")), 10, 64)
	if err != nil || upTo < 0 {
		s.fail(w, r, badRequest("Missing or invalid page watermark. Reload the page and try again."))
		return
	}
	n, err := s.store.MarkScopeReadUpTo(ctx, m.scope, upTo, s.now())
	if err != nil {
		s.fail(w, r, fmt.Errorf("mark scope read: %w", err))
		return
	}
	s.log.Debug("marked scope read", "scope", m.scope.Kind, "id", m.scope.ID, "rows", n)
	if !isHTMX(r) {
		http.Redirect(w, r, scopeHref(m.scope, filter), http.StatusSeeOther)
		return
	}
	v, err := s.buildScopeView(ctx, m, filter, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderFragment(w, r, http.StatusOK, "scope-view", v, true)
}

// handleRefresh forces a fetch of one feed (feed scope) or of all feeds.
func (s *server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	sc, filter, err := parseScope(r.PostForm)
	if err != nil {
		s.fail(w, r, badRequest("Unknown scope."))
		return
	}
	if sc.Kind == store.ScopeFeed {
		if _, err := s.store.GetFeed(ctx, sc.ID); err != nil {
			s.fail(w, r, fmt.Errorf("get feed %d: %w", sc.ID, err))
			return
		}
		err = s.sched.Refresh(ctx, sc.ID)
	} else {
		err = s.sched.RefreshAll(ctx)
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("refresh: %w", err))
		return
	}
	if !isHTMX(r) {
		http.Redirect(w, r, safeBack(r.PostForm.Get("back"), scopeHref(sc, filter)), http.StatusSeeOther)
		return
	}
	s.renderFragment(w, r, http.StatusOK, "notice", noticeView{Text: "Refresh started — reload in a moment."}, true)
}
