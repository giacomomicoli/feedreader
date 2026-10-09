package web

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// reloadGrid is the value of the reload field of the card grid's Refresh
// form: the answer then also starts the checks that reload the grid once the
// fetches are done. The feed settings page has no grid and sends none.
const reloadGrid = "grid"

// refreshSinceWindow bounds how far from now the since of a refresh check
// may be. The checks of one refresh end after config.RefreshMaxChecks, about
// a minute, but browsers run timers late in background tabs.
const refreshSinceWindow = time.Hour

// refreshPollView is one check of a Refresh from the card grid.
type refreshPollView struct {
	URL   string // GET /refresh/status with the scope, since, up_to and attempt
	Delay string // how long htmx waits before asking, e.g. "2000ms"
	OOB   bool   // the first check, swapped into the idle element out of band
}

// refreshStartedView answers a Refresh from the card grid.
type refreshStartedView struct {
	Notice noticeView
	Poll   refreshPollView
}

// refreshDoneView ends the checks of a refresh that brought something the
// grid can show: the final notice and the reloaded scope view, both swapped
// out of band. With nothing new, the checks end with the notice alone
// (fragment refresh-quiet), so the grid on screen is left as it is.
type refreshDoneView struct {
	Notice noticeView
	View   scopeView
}

// handleRefresh forces a fetch of one feed (feed scope) or of all feeds.
// From the card grid (reload=grid, with the grid's up_to watermark) the htmx
// answer also starts the checks that reload the grid once those fetches are
// done, if they brought something it can show (handleRefreshStatus).
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
	reload := r.PostForm.Get("reload")
	if reload != "" && reload != reloadGrid {
		s.fail(w, r, badRequest("Unknown reload option."))
		return
	}
	// The largest entry id when the grid was rendered: entries above it are
	// not on screen, so only they make the grid worth reloading.
	var upTo int64
	if reload == reloadGrid {
		var ok bool
		if upTo, ok = parseUpTo(r.PostForm.Get("up_to")); !ok {
			s.fail(w, r, badRequest(badWatermark))
			return
		}
	}
	// Read before the refresh is stored, so that every fetch it asks for
	// ends after this time (see refreshedSince).
	accepted := s.now()
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
	if reload != reloadGrid {
		s.renderFragment(w, r, http.StatusOK, "notice", noticeView{Text: "Refresh started — reload in a moment."}, true)
		return
	}
	poll := newRefreshPoll(sc, filter, accepted, upTo, 0)
	poll.OOB = true
	s.renderFragment(w, r, http.StatusOK, "refresh-started", refreshStartedView{
		Notice: noticeView{Text: "Refresh started — new entries show up when it is done."},
		Poll:   poll,
	}, true)
}

// handleRefreshStatus answers one check of a Refresh from the card grid
// (GET /refresh/status, see newRefreshPoll) and never changes state. Until
// every feed the refresh asked for has been fetched since it was accepted,
// it answers with the next check, at most config.RefreshMaxChecks in all.
// Then it answers with a final notice and the sidebar counts, plus, when the
// first page of the scope now holds an entry the grid did not have (an id
// above the grid's up_to), the scope view reloaded like a fresh load (first
// page, same filter). Otherwise the grid stays as it is, with its loaded
// pages, open menus, typed text and focus: Watch later, Read later,
// Favourites and tags never gain entries from a fetch. Invalid parameters
// get the usual error answer in #notice, which ends the checks.
func (s *server) handleRefreshStatus(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if !isHTMX(r) {
		sc, filter, err := parseScope(q)
		if err != nil {
			s.fail(w, r, badRequest("Unknown scope or filter."))
			return
		}
		http.Redirect(w, r, scopeHref(sc, filter), http.StatusSeeOther)
		return
	}
	since, upTo, attempt, err := s.parseRefreshCheck(q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	ctx := r.Context()
	m, filter, err := s.requestScope(ctx, q)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	done, err := s.refreshDone(ctx, m.scope, since)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if next := attempt + 1; !done && next < config.RefreshMaxChecks {
		s.renderFragment(w, r, http.StatusOK, "refresh-poll", newRefreshPoll(m.scope, filter, since, upTo, next), false)
		return
	}
	v, err := s.buildScopeView(ctx, m, filter, nil)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !hasCardAfter(v.Grid.Cards, upTo) {
		notice := "Refresh done — nothing new here."
		if !done {
			notice = refreshSlowNotice(m.scope)
		}
		s.renderFragment(w, r, http.StatusOK, "refresh-quiet", noticeView{Text: notice}, true)
		return
	}
	v.OOB = true
	notice := "Refresh done."
	if !done {
		notice = refreshSlowNotice(m.scope)
	}
	s.renderFragment(w, r, http.StatusOK, "refresh-done", refreshDoneView{Notice: noticeView{Text: notice}, View: v}, true)
}

// hasCardAfter reports whether cards hold an entry stored after the
// watermark upTo (entry ids only grow and are never reused).
func hasCardAfter(cards []cardView, upTo int64) bool {
	for _, c := range cards {
		if c.ID > upTo {
			return true
		}
	}
	return false
}

// newRefreshPoll is check number attempt (counting from 0) of a refresh of
// scope sc with filter, accepted at since, from a grid with watermark upTo.
func newRefreshPoll(sc store.Scope, filter string, since time.Time, upTo int64, attempt int) refreshPollView {
	return refreshPollView{
		URL: "/refresh/status?" + scopeParams(sc, filter) +
			"&since=" + strconv.FormatInt(since.Unix(), 10) + "&up_to=" + strconv.FormatInt(upTo, 10) +
			"&attempt=" + strconv.Itoa(attempt),
		Delay: strconv.FormatInt(config.RefreshCheckInterval.Milliseconds(), 10) + "ms",
	}
}

// parseRefreshCheck reads since, the Unix second the refresh was accepted,
// at most refreshSinceWindow away from now, upTo, the watermark of the grid
// the refresh was asked from (see parseUpTo), and attempt, the number of
// checks made before this one (0 to config.RefreshMaxChecks-1).
func (s *server) parseRefreshCheck(q url.Values) (since time.Time, upTo int64, attempt int, err error) {
	sec, errSince := strconv.ParseInt(q.Get("since"), 10, 64)
	upTo, okUpTo := parseUpTo(q.Get("up_to"))
	attempt, errAttempt := strconv.Atoi(q.Get("attempt"))
	since = time.Unix(sec, 0)
	age := s.now().Sub(since)
	if errSince != nil || !okUpTo || errAttempt != nil || attempt < 0 || attempt >= config.RefreshMaxChecks ||
		age > refreshSinceWindow || age < -refreshSinceWindow {
		return time.Time{}, 0, 0, badRequest("This refresh can no longer be followed. Reload the page to see new entries.")
	}
	return since, upTo, attempt, nil
}

// refreshDone reports whether every feed that a refresh of scope sc,
// accepted at since, asked for has been fetched since then: the feed of a
// feed scope, every feed otherwise (as handleRefresh asks).
func (s *server) refreshDone(ctx context.Context, sc store.Scope, since time.Time) (bool, error) {
	var feeds []store.Feed
	if sc.Kind == store.ScopeFeed {
		f, err := s.store.GetFeed(ctx, sc.ID)
		if err != nil {
			return false, fmt.Errorf("get feed %d: %w", sc.ID, err)
		}
		feeds = []store.Feed{f}
	} else {
		var err error
		if feeds, err = s.store.ListFeeds(ctx); err != nil {
			return false, fmt.Errorf("list feeds: %w", err)
		}
	}
	for _, f := range feeds {
		if !refreshedSince(f, since) {
			return false, nil
		}
	}
	return true, nil
}

// refreshedSince reports whether feed f has been fetched for a refresh
// accepted at since (stored times have one-second precision). Its last fetch,
// failed ones included, must be no older than since, and its next fetch must
// lie after since: a fetch that was in flight at the click also ends after
// since, but it keeps the schedule the refresh stored, so the scheduler
// fetches the feed once more. A feed subscribed at or after since was not
// part of the refresh; it was fetched when it was added.
func refreshedSince(f store.Feed, since time.Time) bool {
	if !f.CreatedAt.Before(since) {
		return true
	}
	return !f.LastFetchedAt.Before(since) && f.NextFetchAt.After(since)
}

// refreshSlowNotice is the final notice when the checks give up before every
// feed of the refresh has been fetched.
func refreshSlowNotice(sc store.Scope) string {
	if sc.Kind == store.ScopeFeed {
		return "This source has not been fetched yet. Reload the page later to see what it brings."
	}
	return "Some sources have not been fetched yet. Reload the page later to see what they bring."
}
