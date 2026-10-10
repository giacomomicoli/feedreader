// Package web serves the UI: server-rendered html/template pages plus htmx
// partials. Every action handler returns the fragment it changed plus an
// out-of-band swap refreshing the sidebar counts.
package web

import (
	"context"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
	assets "github.com/giacomomicoli/feedreader/web"
)

// Scheduler is the subset of *sched.Scheduler the UI needs.
type Scheduler interface {
	Refresh(ctx context.Context, feedID int64) error
	RefreshAll(ctx context.Context) error
	Preview(ctx context.Context, feedURL string) (*sched.Preview, error)
	Subscribe(ctx context.Context, sub sched.Subscription) (store.Feed, error)
	// ScheduleIngest moves the next fetch of each feed earlier to its next
	// digest ingestion time when that comes first; never later, and never
	// for a feed in failure backoff.
	ScheduleIngest(ctx context.Context, feedIDs []int64) error
}

// rescheduler is implemented by a Scheduler that can pull a feed's next poll
// forward after its interval override changed, so a shorter interval takes
// effect now instead of after the old schedule fires. It never moves a poll
// later and keeps an active failure backoff.
type rescheduler interface {
	Reschedule(ctx context.Context, feedID int64) error
}

// The production scheduler reschedules.
var _ rescheduler = (*sched.Scheduler)(nil)

// Resolver is the subset of *resolve.Resolver the UI needs.
type Resolver interface {
	Resolve(ctx context.Context, raw string) (*resolve.Result, error)
}

// Deps are the server's collaborators.
type Deps struct {
	Config   config.Config
	Store    store.Store
	Sched    Scheduler
	Resolver Resolver
	Log      *slog.Logger
	// AllowedHosts are the host names (FR_ALLOWED_HOSTS) the UI answers to
	// besides localhost, IP literals and the host of Config.Listen, e.g. the
	// site name of the reverse proxy in front of it. See checkHost.
	AllowedHosts []string
}

// server holds the handlers' shared state.
type server struct {
	cfg      config.Config
	store    store.Store
	sched    Scheduler
	resolver Resolver
	log      *slog.Logger
	tmpl     *renderer
	static   *staticFS
	hosts    map[string]bool // allowed Host names besides IP literals
	now      func() time.Time
	// loc is the time zone of digest ingestion times, the server's local
	// time zone (as the scheduler's); replaced in tests.
	loc *time.Location
}

// New returns the complete HTTP handler (routes, static files, security
// headers, Host validation, cross-origin request protection).
func New(d Deps) (http.Handler, error) {
	s, err := newServer(d)
	if err != nil {
		return nil, err
	}
	return s.handler(), nil
}

func newServer(d Deps) (*server, error) {
	if d.Store == nil || d.Sched == nil || d.Resolver == nil {
		return nil, errors.New("web: Store, Sched and Resolver are required")
	}
	s := &server{
		cfg:      d.Config,
		store:    d.Store,
		sched:    d.Sched,
		resolver: d.Resolver,
		log:      d.Log,
		hosts:    allowedHosts(d.Config.Listen, d.AllowedHosts),
		now:      time.Now,
		loc:      time.Local,
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	staticRoot, err := fs.Sub(assets.Static, "static")
	if err != nil {
		return nil, fmt.Errorf("web: static assets: %w", err)
	}
	if s.static, err = loadStatic(staticRoot); err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	funcs := template.FuncMap{
		"asset":   s.static.url,
		"maxName": func() int { return maxNameLen },
		"maxURL":  func() int { return maxURLLen },
	}
	if s.tmpl, err = newRenderer(assets.Templates, funcs); err != nil {
		return nil, fmt.Errorf("web: %w", err)
	}
	return s, nil
}

// handler wires routes and middleware.
func (s *server) handler() http.Handler {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", s.static)
	mux.HandleFunc("GET /favicon.ico", s.static.favicon)
	mux.HandleFunc("GET /{$}", s.handleHome)
	mux.HandleFunc("GET /entries", s.handleEntries)
	mux.HandleFunc("GET /entries/{id}", s.handleEntry)
	mux.HandleFunc("POST /entries/{id}/{action}", s.handleEntryAction)
	mux.HandleFunc("POST /entries/{id}/tags", s.handleAddTag)
	mux.HandleFunc("POST /entries/{id}/tags/{tag}/remove", s.handleRemoveTag)
	mux.HandleFunc("GET /tags/suggest", s.handleSuggestTags)
	mux.HandleFunc("POST /mark-read", s.handleMarkAllRead)
	mux.HandleFunc("POST /refresh", s.handleRefresh)
	mux.HandleFunc("GET /refresh/status", s.handleRefreshStatus)

	mux.HandleFunc("GET /add", s.handleAddPage)
	mux.HandleFunc("POST /add/resolve", s.handleAddResolve)
	mux.HandleFunc("POST /add/preview", s.handleAddPreview)
	mux.HandleFunc("GET /add/folder-field", s.handleAddFolderField)
	mux.HandleFunc("POST /add/subscribe", s.handleAddSubscribe)

	mux.HandleFunc("GET /feeds/{id}", s.handleFeedSettings)
	mux.HandleFunc("POST /feeds/{id}/rename", s.handleFeedRename)
	mux.HandleFunc("POST /feeds/{id}/move", s.handleFeedMove)
	mux.HandleFunc("POST /feeds/{id}/interval", s.handleFeedInterval)
	mux.HandleFunc("GET /feeds/{id}/unsubscribe", s.handleUnsubscribeConfirm)
	mux.HandleFunc("POST /feeds/{id}/unsubscribe", s.handleUnsubscribe)

	mux.HandleFunc("POST /folders", s.handleFolderCreate)
	mux.HandleFunc("POST /folders/{id}/rename", s.handleFolderRename)
	mux.HandleFunc("GET /folders/{id}/delete", s.handleFolderDeleteConfirm)
	mux.HandleFunc("POST /folders/{id}/delete", s.handleFolderDelete)

	mux.HandleFunc("GET /tags/{id}/delete", s.handleTagDeleteConfirm)
	mux.HandleFunc("POST /tags/{id}/delete", s.handleTagDelete)

	mux.HandleFunc("POST /digests", s.handleDigestCreate)
	mux.HandleFunc("GET /digests/{id}", s.handleDigestSettings)
	mux.HandleFunc("POST /digests/{id}/rename", s.handleDigestRename)
	mux.HandleFunc("POST /digests/{id}/schedule", s.handleDigestSchedule)
	mux.HandleFunc("POST /digests/{id}/sources", s.handleDigestSources)
	mux.HandleFunc("GET /digests/{id}/delete", s.handleDigestDeleteConfirm)
	mux.HandleFunc("POST /digests/{id}/delete", s.handleDigestDelete)

	mux.HandleFunc("/", s.handleNotFound)

	var h http.Handler = mux
	h = limitBody(h)
	h = s.crossOrigin(h)
	h = s.checkHost(h)
	h = securityHeaders(h)
	h = s.observe(h)
	return h
}

func (s *server) handleNotFound(w http.ResponseWriter, r *http.Request) {
	s.writeError(w, r, http.StatusNotFound, "There is no page at this address.")
}

// pathID parses a positive id path value.
func pathID(r *http.Request, name string) (int64, error) {
	id, err := parseID(r.PathValue(name))
	if err != nil {
		return 0, badRequest("Invalid id.")
	}
	return id, nil
}
