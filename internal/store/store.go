// Package store defines the repository interface for all persistent state
// and its SQLite implementation. No other package may talk to the database
// directly.
//
// Time values: a zero time.Time means NULL / "never". The SQLite
// implementation stores timestamps as UTC Unix epoch seconds.
package store

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors. Implementations wrap or return these so callers can use
// errors.Is.
var (
	ErrNotFound = errors.New("store: not found")
	ErrConflict = errors.New("store: already exists")
	// ErrInvalid reports an argument the store refuses to persist, such as
	// an empty name, an unknown feed kind or scope, or a non-positive page
	// size.
	ErrInvalid = errors.New("store: invalid argument")
)

// Kind is a feed kind. It only affects YouTube channel-ID resolution at add
// time and rendering (labels, thumbnails).
type Kind string

const (
	KindRSS     Kind = "rss"
	KindYouTube Kind = "youtube"
)

// Folder is a flat (non-nested) group of feeds. Names are unique
// case-insensitively for all of Unicode: two names clash exactly when
// strings.EqualFold reports them equal.
type Folder struct {
	ID       int64
	Name     string
	Position int
}

// Feed is a subscription plus its fetch state.
type Feed struct {
	ID            int64
	Kind          Kind
	URL           string
	SiteURL       string
	Title         string // user-editable
	OriginalTitle string // as published by the feed
	IconURL       string
	FolderID      int64 // 0 = uncategorized

	ETag          string
	LastModified  string
	LastFetchedAt time.Time // zero = never
	NextFetchAt   time.Time
	IntervalSec   int // 0 = use the global poll interval
	ErrorCount    int // consecutive failures; reset on success or 304
	LastError     string
	// TTLSec is the RSS <ttl> of the last parsed document, in seconds
	// (0 = none). It still applies after a 304, which carries no document.
	TTLSec int

	CreatedAt time.Time
}

// Entry is one stored feed item. Unique key: (FeedID, GUID).
type Entry struct {
	ID           int64 // grows with every insert and is never reused
	FeedID       int64
	GUID         string
	URL          string
	Title        string
	SummaryHTML  string // sanitized before storage
	Author       string
	ThumbnailURL string
	PublishedAt  time.Time
	UpdatedAt    time.Time // zero = NULL
	// FetchedAt is when the response that first delivered this entry was
	// fetched (NewFeed.FetchedAt or FetchSuccess.FetchedAt). It is not the
	// time the entry was stored: a preview or a slow poll can store it
	// later, after entries with a later FetchedAt. To tell which entries
	// were stored after a given moment, compare IDs (see MaxEntryID).
	FetchedAt time.Time

	IsRead      bool
	IsLater     bool
	IsFavourite bool
	ReadAt      time.Time // zero = NULL
}

// EntryView is an entry joined with what the card grid needs from its feed,
// plus its tags (ordered by name).
type EntryView struct {
	Entry
	FeedTitle   string
	FeedKind    Kind
	FeedIconURL string
	Tags        []Tag
}

// Tag is a free-text label; names are unique case-insensitively for all of
// Unicode (two names clash exactly when strings.EqualFold reports them equal).
type Tag struct {
	ID    int64
	Name  string
	Count int // number of entries carrying the tag; filled by ListTags only
}

// NewEntry is a parsed entry ready to be inserted or updated.
type NewEntry struct {
	GUID         string
	URL          string
	Title        string
	SummaryHTML  string
	Author       string
	ThumbnailURL string
	// PublishedAt must be non-zero: callers apply the fallback
	// published → updated → fetched_at before calling the store.
	PublishedAt time.Time
	UpdatedAt   time.Time // zero = NULL
}

// NewFeed describes a feed being subscribed to.
type NewFeed struct {
	Kind          Kind
	URL           string
	SiteURL       string
	Title         string
	OriginalTitle string
	IconURL       string
	FolderID      int64 // 0 = uncategorized
	ETag          string
	LastModified  string
	FetchedAt     time.Time // time of the initial fetch
	NextFetchAt   time.Time
	TTLSec        int // RSS <ttl> of the initial document, in seconds (0 = none)
}

// FetchSuccess is the outcome of a 200 response that parsed successfully.
type FetchSuccess struct {
	FeedID       int64
	FetchedAt    time.Time
	NextFetchAt  time.Time
	ETag         string // stored as-is (empty clears it)
	LastModified string // stored as-is (empty clears it)
	TTLSec       int    // RSS <ttl> of the document, in seconds; replaces the stored value (0 = none)
	// Feed metadata refreshed from the document; "" keeps the stored value.
	SiteURL       string
	IconURL       string
	OriginalTitle string
	Entries       []NewEntry
}

// ScopeKind selects which entries the card grid shows.
type ScopeKind string

const (
	ScopeAll        ScopeKind = "all"
	ScopeFolder     ScopeKind = "folder"
	ScopeFeed       ScopeKind = "feed"
	ScopeWatchLater ScopeKind = "watchlater" // is_later on youtube feeds
	ScopeReadLater  ScopeKind = "readlater"  // is_later on rss feeds
	ScopeFavourites ScopeKind = "favourites" // is_favourite, both kinds
	ScopeTag        ScopeKind = "tag"
)

// Scope is a scope kind plus the folder, feed or tag id it refers to (0 for
// the others).
type Scope struct {
	Kind ScopeKind
	ID   int64
}

// IgnoresReadFilter reports whether the scope lists entries regardless of
// read state (Watch later, Read later, Favourites).
func (s Scope) IgnoresReadFilter() bool {
	switch s.Kind {
	case ScopeWatchLater, ScopeReadLater, ScopeFavourites:
		return true
	}
	return false
}

// Cursor is a keyset pagination position: the next page holds entries
// strictly after (PublishedAt DESC, ID DESC) this position.
type Cursor struct {
	PublishedAt time.Time
	ID          int64
}

// ListQuery selects a page of the card grid. Sort: published_at DESC, id DESC.
type ListQuery struct {
	Scope      Scope
	UnreadOnly bool    // ignored when Scope.IgnoresReadFilter()
	Limit      int     // page size, > 0
	After      *Cursor // nil = first page
}

// Page is one block of cards.
type Page struct {
	Entries []EntryView
	HasMore bool   // true when at least one more entry matches after Next
	Next    Cursor // position of the last entry in Entries; valid when HasMore
}

// UnreadCounts holds sidebar badges.
type UnreadCounts struct {
	All      int
	ByFeed   map[int64]int // feed id → unread entries (absent = 0)
	ByFolder map[int64]int // folder id → sum over its feeds (absent = 0)
}

// FeedEntryStats is shown in the unsubscribe confirmation dialog.
type FeedEntryStats struct {
	Total int // all entries of the feed
	Saved int // entries with is_later or is_favourite set
}

// Store is the repository for all persistent state. Methods are safe for
// concurrent use.
//
// Every method that reads or changes one row by id returns an error wrapping
// ErrNotFound when that row does not exist; methods that create or rename
// return an error wrapping ErrConflict on a uniqueness violation.
type Store interface {
	Close() error

	// --- Folders ---

	// ListFolders returns all folders ordered by position, then name
	// (case-insensitive).
	ListFolders(ctx context.Context) ([]Folder, error)
	GetFolder(ctx context.Context, id int64) (Folder, error)
	// CreateFolder returns ErrConflict if the name is taken (case-insensitive,
	// as strings.EqualFold).
	CreateFolder(ctx context.Context, name string) (Folder, error)
	// RenameFolder returns ErrConflict if the name is taken, ErrNotFound if
	// the folder does not exist.
	RenameFolder(ctx context.Context, id int64, name string) error
	// DeleteFolder removes the folder; its feeds become uncategorized.
	DeleteFolder(ctx context.Context, id int64) error

	// --- Feeds ---

	// ListFeeds returns all feeds ordered by title (case-insensitive).
	ListFeeds(ctx context.Context) ([]Feed, error)
	GetFeed(ctx context.Context, id int64) (Feed, error)
	GetFeedByURL(ctx context.Context, url string) (Feed, error)
	// CreateFeed atomically inserts the feed and all its entries. The
	// initialUnread entries with the newest PublishedAt are unread; all other
	// entries are stored read (read_at = f.FetchedAt). Entries sharing a GUID
	// are collapsed (last one wins). Returns ErrConflict if the URL exists.
	CreateFeed(ctx context.Context, f NewFeed, entries []NewEntry, initialUnread int) (Feed, error)
	RenameFeed(ctx context.Context, id int64, title string) error
	// MoveFeed sets the feed's folder; folderID 0 = uncategorized.
	MoveFeed(ctx context.Context, id, folderID int64) error
	// SetFeedInterval sets the per-feed poll interval; 0 = global default.
	SetFeedInterval(ctx context.Context, id int64, sec int) error
	// DeleteFeed removes the feed, all its entries (including later and
	// favourite ones) and their entry_tags rows.
	DeleteFeed(ctx context.Context, id int64) error
	FeedEntryStats(ctx context.Context, id int64) (FeedEntryStats, error)

	// --- Polling state ---

	// DueFeeds returns feeds with next_fetch_at <= now, ordered by
	// next_fetch_at.
	DueFeeds(ctx context.Context, now time.Time) ([]Feed, error)
	// NextFetchAfter returns the earliest next_fetch_at strictly after t, and
	// false if there is none.
	NextFetchAfter(ctx context.Context, t time.Time) (time.Time, bool, error)
	SetNextFetch(ctx context.Context, id int64, at time.Time) error
	SetAllNextFetch(ctx context.Context, at time.Time) error
	// RecordFetchSuccess upserts entries and updates fetch state in one
	// transaction: error_count = 0, last_error = NULL, last_fetched_at,
	// next_fetch_at, etag, last_modified, ttl_sec, and non-empty metadata fields.
	// Entries whose (feed_id, guid) is new are inserted unread. Existing ones
	// get title, summary_html, url, thumbnail_url and updated_at refreshed;
	// is_read, is_later, is_favourite, read_at, published_at and fetched_at
	// are never touched. Returns the number of inserted entries.
	RecordFetchSuccess(ctx context.Context, r FetchSuccess) (inserted int, err error)
	// RecordNotModified handles a 304: last_fetched_at, next_fetch_at,
	// error_count = 0, last_error = NULL.
	RecordNotModified(ctx context.Context, id int64, fetchedAt, next time.Time) error
	// RecordFetchError increments error_count and sets last_error,
	// last_fetched_at and next_fetch_at. Entries are left untouched.
	RecordFetchError(ctx context.Context, id int64, msg string, fetchedAt, next time.Time) error
	// UpdateFeedURL stores a permanently redirected feed URL. Returns
	// ErrConflict if another feed already uses that URL.
	UpdateFeedURL(ctx context.Context, id int64, url string) error
	// SetFeedIcon stores an icon found for the feed outside its document (a
	// YouTube channel's avatar). Polls keep it while the document has none.
	SetFeedIcon(ctx context.Context, id int64, iconURL string) error

	// --- Entries ---

	ListEntries(ctx context.Context, q ListQuery) (Page, error)
	GetEntry(ctx context.Context, id int64) (EntryView, error)
	// SetRead marks one entry read (read_at = at) or unread (read_at = NULL).
	SetRead(ctx context.Context, id int64, read bool, at time.Time) error
	// SetLater sets is_later; it never changes the read state.
	SetLater(ctx context.Context, id int64, on bool) error
	SetFavourite(ctx context.Context, id int64, on bool) error
	// MaxEntryID returns the largest entry id stored so far (0 if none).
	// Ids are never reused and writers are serialized, so every entry stored
	// after this call gets a larger id. Read it before listing a scope and
	// pass it back to MarkScopeReadUpTo.
	MaxEntryID(ctx context.Context) (int64, error)
	// MarkScopeReadUpTo marks every unread entry in the scope whose id is
	// <= maxID as read (read_at = at), so entries stored after the user
	// loaded the page are not swept away. A non-positive maxID matches
	// nothing. Returns rows changed.
	MarkScopeReadUpTo(ctx context.Context, s Scope, maxID int64, at time.Time) (int64, error)

	// --- Tags ---

	// ListTags returns all tags with Count, ordered by name (case-insensitive).
	ListTags(ctx context.Context) ([]Tag, error)
	GetTag(ctx context.Context, id int64) (Tag, error)
	// SuggestTags returns up to limit tags whose name starts with prefix
	// (case-insensitive, as strings.EqualFold), for autocomplete.
	SuggestTags(ctx context.Context, prefix string, limit int) ([]Tag, error)
	// AddEntryTag attaches the tag named name (created if missing, matched
	// case-insensitively as strings.EqualFold, keeping the existing spelling)
	// to the entry. Idempotent.
	AddEntryTag(ctx context.Context, entryID int64, name string) (Tag, error)
	RemoveEntryTag(ctx context.Context, entryID, tagID int64) error
	// DeleteTag removes the tag and its entry_tags rows; entries are kept.
	DeleteTag(ctx context.Context, id int64) error

	// --- Counts ---

	// UnreadCounts computes all sidebar badges with a single query.
	UnreadCounts(ctx context.Context) (UnreadCounts, error)
}
