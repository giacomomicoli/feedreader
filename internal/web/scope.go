package web

import (
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Read filters for the card grid (default Unread only, togglable to All).
const (
	filterUnread = "unread"
	filterAll    = "all"
)

var errBadScope = errors.New("invalid scope")

// parseScope reads scope, id and filter from query or form values. Missing
// values default to the All scope with the Unread filter.
func parseScope(v url.Values) (store.Scope, string, error) {
	kind := store.ScopeKind(strings.TrimSpace(v.Get("scope")))
	if kind == "" {
		kind = store.ScopeAll
	}
	sc := store.Scope{Kind: kind}
	switch kind {
	case store.ScopeFolder, store.ScopeFeed, store.ScopeTag:
		id, err := parseID(v.Get("id"))
		if err != nil {
			return store.Scope{}, "", fmt.Errorf("%w: %s needs a valid id", errBadScope, kind)
		}
		sc.ID = id
	case store.ScopeAll, store.ScopeWatchLater, store.ScopeReadLater, store.ScopeFavourites:
	default:
		return store.Scope{}, "", fmt.Errorf("%w: unknown scope %q", errBadScope, kind)
	}

	filter := strings.TrimSpace(v.Get("filter"))
	switch filter {
	case "":
		filter = filterUnread
	case filterUnread, filterAll:
	default:
		return store.Scope{}, "", fmt.Errorf("%w: unknown filter %q", errBadScope, filter)
	}
	return sc, filter, nil
}

// parseID parses a positive database id.
func parseID(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid id %q", s)
	}
	return id, nil
}

// scopeHasID reports whether the scope kind refers to a folder, feed or tag.
func scopeHasID(k store.ScopeKind) bool {
	return k == store.ScopeFolder || k == store.ScopeFeed || k == store.ScopeTag
}

// scopeParams returns the query string (without "?") selecting scope and
// filter, in a stable, readable order: scope, id, filter.
func scopeParams(sc store.Scope, filter string) string {
	var b strings.Builder
	b.WriteString("scope=")
	b.WriteString(url.QueryEscape(string(sc.Kind)))
	if scopeHasID(sc.Kind) {
		b.WriteString("&id=")
		b.WriteString(strconv.FormatInt(sc.ID, 10))
	}
	if filter != "" {
		b.WriteString("&filter=")
		b.WriteString(url.QueryEscape(filter))
	}
	return b.String()
}

// scopeHref is the page URL of a scope with the given filter.
func scopeHref(sc store.Scope, filter string) string {
	return "/?" + scopeParams(sc, filter)
}

// feedHref is the grid URL of a single feed.
func feedHref(id int64, filter string) string {
	return scopeHref(store.Scope{Kind: store.ScopeFeed, ID: id}, filter)
}

// pageSize is the "Load more" block size for a scope.
func pageSize(sc store.Scope) int {
	if sc.Kind == store.ScopeFeed {
		return config.FeedPageSize
	}
	return config.ScopePageSize
}

// encodeCursor serializes a keyset position as "unixSeconds.nanos.id".
func encodeCursor(c store.Cursor) string {
	return strconv.FormatInt(c.PublishedAt.Unix(), 10) + "." +
		strconv.Itoa(c.PublishedAt.Nanosecond()) + "." +
		strconv.FormatInt(c.ID, 10)
}

// decodeCursor parses the output of encodeCursor; "" means the first page.
func decodeCursor(s string) (*store.Cursor, error) {
	if s == "" {
		return nil, nil
	}
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid cursor %q", s)
	}
	sec, err1 := strconv.ParseInt(parts[0], 10, 64)
	nsec, err2 := strconv.ParseInt(parts[1], 10, 64)
	id, err3 := strconv.ParseInt(parts[2], 10, 64)
	if err := errors.Join(err1, err2, err3); err != nil || nsec < 0 || nsec >= int64(time.Second) || id <= 0 {
		return nil, fmt.Errorf("invalid cursor %q", s)
	}
	return &store.Cursor{PublishedAt: time.Unix(sec, nsec).UTC(), ID: id}, nil
}

// safeBack validates a same-site path used to redirect non-htmx form posts
// back to the page they came from. Anything else yields fallback.
func safeBack(raw, fallback string) string {
	if raw == "" || len(raw) > 2048 || !strings.HasPrefix(raw, "/") ||
		strings.HasPrefix(raw, "//") || strings.ContainsAny(raw, "\\\r\n\t") {
		return fallback
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil {
		return fallback
	}
	return raw
}
