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
// time, the default poll interval and rendering (labels, thumbnails).
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

// MinutesPerDay is the number of minutes in a day. A digest's ingestion
// time is a minute of the day, 0 to MinutesPerDay-1.
const MinutesPerDay = int(24 * time.Hour / time.Minute)

// MaxDigestSources bounds how many folders, feeds and tags one digest names,
// all together (duplicates collapse first). A digest that needs more can
// name the folders that hold them.
const MaxDigestSources = 1000

// Digest is a custom view over several sources: the entries of the feeds it
// names, of the feeds in the folders it names, and the entries carrying any
// of the tags it names, each entry once. Names are unique
// case-insensitively, like folder names. Ids are never reused.
type Digest struct {
	ID   int64
	Name string
	// IngestMinute is the minute of the day (0 to MinutesPerDay-1), in the
	// server's local time zone, at which the digest's feeds (those it names
	// and those in its folders) are fetched every day on top of their
	// regular schedule. It is only meaningful when HasIngest is set.
	IngestMinute int
	HasIngest    bool
	Position     int
	CreatedAt    time.Time
}

// DigestSources are what a digest is made of, each list sorted by id.
type DigestSources struct {
	FolderIDs []int64
	FeedIDs   []int64
	TagIDs    []int64
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
	IntervalSec   int // 0 = use the default interval of the feed's kind
	ErrorCount    int // consecutive failures; reset on success or 304
	LastError     string
	// TTLSec is the RSS <ttl> of the last parsed document, in seconds
	// (0 = none). It still applies after a 304, which carries no document.
	TTLSec int
	// InitialPending is set for a subscription stored before its feed could
	// be fetched: its first stored document follows the initial-add rule
	// (see FetchSuccess.InitialUnread), which clears the flag.
	InitialPending bool
	// HasEntries reports whether any entry of the feed is stored. It is
	// read with the row, not a column: a pending subscription that never
	// answered has none, while a feed switched to another URL (see
	// ChangeFeedURL) keeps the entries it had.
	HasEntries bool

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
	// HasTranscript is set when the transcript of the entry's video was
	// fetched (GetTranscript returns it).
	HasTranscript bool
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
	// InitialPending stores a subscription whose feed could not be fetched
	// yet (Feed.InitialPending). It takes no entries.
	InitialPending bool
	// LastError, when not empty, records the failed fetch the subscription
	// was stored after: error_count = 1 and last_error = LastError.
	LastError string
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
	// InitialUnread applies the initial-add rule when the feed's row is
	// InitialPending as the outcome is stored: of the entries this fetch
	// inserts, the InitialUnread with the newest PublishedAt are unread and
	// the others are stored read (read_at = FetchedAt), and the flag is
	// cleared. It is ignored for a feed that is not pending.
	InitialUnread int

	// QueueTranscripts queues the transcript of every entry this fetch
	// inserts, due at FetchedAt (see NextTranscript), when the global
	// transcript setting and the feed's own switch are on as the
	// transaction reads them: set for the polls of YouTube feeds when
	// FR_TRANSCRIPTS is on. CreateFeed, the initial add, never queues.
	QueueTranscripts bool
}

// Transcript is the text of a video's caption track (FR_TRANSCRIPTS).
type Transcript struct {
	Lang      string // BCP 47 code of the caption track; "" = unknown
	Generated bool   // YouTube's automatic speech recognition, not authored captions
	Text      string // plain text, never HTML
}

// TranscriptJob is an entry whose transcript is queued (NextTranscript).
type TranscriptJob struct {
	EntryID  int64
	URL      string // the entry's URL: the video's watch page
	Attempts int    // failed attempts so far (TranscriptRetry)
	// Rechecked is set once a check found no captions and queued the entry
	// for one more (TranscriptRecheck).
	Rechecked bool
	// QueuedAt is when a poll queued it, or when an outcome kept it queued
	// if that is earlier: the clock was set back since (RecordTranscript).
	QueuedAt time.Time
	DueAt    time.Time // when it may be fetched, not before
	// Refusals is how many requests YouTube refused in a row before this
	// job was read (TranscriptBlocked); the pause after another refusal
	// grows with it.
	Refusals int
}

// TranscriptOutcome is what one attempt at a queued entry's transcript came
// to (RecordTranscript).
type TranscriptOutcome string

const (
	// TranscriptOK: the transcript was fetched; GetTranscript returns it.
	TranscriptOK TranscriptOutcome = "ok"
	// TranscriptNoCaptions: the video has no captions. Final.
	TranscriptNoCaptions TranscriptOutcome = "no_captions"
	// TranscriptRetry: the attempt failed. The entry stays queued, with one
	// more attempt counted, until RetryAt.
	TranscriptRetry TranscriptOutcome = "retry"
	// TranscriptDeferred: the attempt told nothing final about the video
	// (it has not aired yet). The entry stays queued, its attempts
	// unchanged, until RetryAt.
	TranscriptDeferred TranscriptOutcome = "deferred"
	// TranscriptBlocked: YouTube refused the request. The entry stays
	// queued as with TranscriptDeferred, and no queued entry is due before
	// RetryAt, the end of the pause that follows (NextTranscript), also
	// after a restart. The refusal adds to the streak of refusals in a row
	// (TranscriptJob.Refusals), which is stored too.
	TranscriptBlocked TranscriptOutcome = "blocked"
	// TranscriptRecheck: the video had no captions, but YouTube may still
	// be making them (a new upload, a stream that has just ended). The
	// entry stays queued, its attempts unchanged, until RetryAt, and is
	// marked rechecked: its next check without captions is final.
	TranscriptRecheck TranscriptOutcome = "recheck"
	// TranscriptFailed: the last attempt failed too; the entry leaves the
	// queue with one more attempt counted. Final.
	TranscriptFailed TranscriptOutcome = "failed"
)

// staysQueued reports whether the entry stays queued after outcome o, due
// again at TranscriptResult.RetryAt.
func (o TranscriptOutcome) staysQueued() bool {
	return o == TranscriptRetry || o == TranscriptDeferred || o == TranscriptBlocked || o == TranscriptRecheck
}

// TranscriptResult is the outcome RecordTranscript stores.
type TranscriptResult struct {
	Outcome    TranscriptOutcome
	Transcript Transcript // the transcript when Outcome is TranscriptOK
	// RetryAt is when an entry that stays queued (TranscriptRetry,
	// TranscriptDeferred, TranscriptBlocked, TranscriptRecheck) falls due
	// again; required for those outcomes, ignored for the others.
	RetryAt time.Time
	// At is when the attempt ended (updated_at), by the caller's clock;
	// zero means now. An entry that stays queued and was queued after At
	// (while the clock was ahead) counts as queued at At from then on.
	At time.Time
	// Requested is set when the outcome follows a request. Unless YouTube
	// refused it (TranscriptBlocked, which adds to the streak), it ends the
	// streak of refusals in a row (TranscriptJob.Refusals). An outcome
	// reached without a request leaves the streak as it is, and a
	// TranscriptFailed without one counts no attempt.
	Requested bool
	// Reason is why the entry is left as it is, stored with any outcome
	// but TranscriptOK (which clears it): see TranscriptReason. "" stores
	// none.
	Reason TranscriptReason
}

// TranscriptReason is why the last check of an entry's transcript left it
// pending or done without a transcript, for the UI to say so.
type TranscriptReason string

const (
	ReasonNoCaptions  TranscriptReason = "no_captions" // no captions (yet)
	ReasonNotReady    TranscriptReason = "not_ready"   // a premiere or live stream that has not aired
	ReasonRefused     TranscriptReason = "refused"     // YouTube refused the request
	ReasonError       TranscriptReason = "error"       // a failed attempt (network, format, ...)
	ReasonUnavailable TranscriptReason = "unavailable" // YouTube does not play the video to this client
	ReasonTooLarge    TranscriptReason = "too_large"   // an answer over the size caps
	ReasonNotAVideo   TranscriptReason = "not_a_video" // the entry's URL names no video
	ReasonQueuedLong  TranscriptReason = "queued_too_long"
	ReasonExhausted   TranscriptReason = "attempts_exhausted"
)

// valid reports whether r may be stored ("" stores NULL).
func (r TranscriptReason) valid() bool {
	switch r {
	case "", ReasonNoCaptions, ReasonNotReady, ReasonRefused, ReasonError, ReasonUnavailable,
		ReasonTooLarge, ReasonNotAVideo, ReasonQueuedLong, ReasonExhausted:
		return true
	}
	return false
}

// TranscriptStatus is the stored state of an entry's transcript.
type TranscriptStatus string

const (
	StatusNone       TranscriptStatus = ""            // never queued
	StatusPending    TranscriptStatus = "pending"     // queued
	StatusOK         TranscriptStatus = "ok"          // fetched
	StatusNoCaptions TranscriptStatus = "no_captions" // checked, no captions
	StatusFailed     TranscriptStatus = "failed"      // given up on
)

// TranscriptState is what the UI shows about an entry's transcript
// (TranscriptStateOf): its stored state, and the pause that delays every
// queued one.
type TranscriptState struct {
	Status    TranscriptStatus
	Reason    TranscriptReason
	Attempts  int
	Rechecked bool
	Lang      string // of a fetched transcript
	Generated bool   // of a fetched transcript
	QueuedAt  time.Time
	DueAt     time.Time // as stored, before the pause
	// PausedUntil is the end of the pause after YouTube's last refusal
	// (zero when none was stored): no queued entry is fetched before it.
	PausedUntil time.Time
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
	ScopeDigest     ScopeKind = "digest" // see Digest
)

// Scope is a scope kind plus the folder, feed, tag or digest id it refers to
// (0 for the others).
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
	// ByDigest maps a digest id to its unread entries, each counted once
	// however many of its sources it belongs to (absent = 0).
	ByDigest map[int64]int
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
	// A pending subscription (f.InitialPending) takes no entries
	// (ErrInvalid otherwise).
	CreateFeed(ctx context.Context, f NewFeed, entries []NewEntry, initialUnread int) (Feed, error)
	RenameFeed(ctx context.Context, id int64, title string) error
	// MoveFeed sets the feed's folder; folderID 0 = uncategorized.
	MoveFeed(ctx context.Context, id, folderID int64) error
	// SetFeedInterval sets the per-feed poll interval; 0 = the default of the
	// feed's kind.
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
	// AdvanceNextFetch moves the feed's next fetch to at only when at is
	// earlier than the stored one, in one statement, so it never undoes an
	// earlier fetch requested meanwhile (a Refresh). moved reports whether
	// it did; a feed that is due now (no next fetch stored) stays due.
	AdvanceNextFetch(ctx context.Context, id int64, at time.Time) (moved bool, err error)
	// RecordFetchSuccess upserts entries and updates fetch state in one
	// transaction: error_count = 0, last_error = NULL, last_fetched_at,
	// next_fetch_at, etag, last_modified, ttl_sec, and non-empty metadata fields.
	// Entries whose (feed_id, guid) is new are inserted unread, except on a
	// pending subscription (see FetchSuccess.InitialUnread); otherwise their
	// transcripts are queued when r.QueueTranscripts is set. Existing ones
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
	// RecordFetchErrorKeepCount is RecordFetchError for a failure that does
	// not count as one more in a row (a manual refresh): it sets last_error,
	// last_fetched_at and next_fetch_at and leaves error_count as it is.
	RecordFetchErrorKeepCount(ctx context.Context, id int64, msg string, fetchedAt, next time.Time) error
	// UpdateFeedURL stores a permanently redirected feed URL. Returns
	// ErrConflict if another feed already uses that URL.
	UpdateFeedURL(ctx context.Context, id int64, url string) error
	// ChangeFeedURL makes the feed follow the URL to instead of from, as a
	// new subscription there whose first fetch is due at next: validators,
	// <ttl> and failures are cleared and the feed becomes pending
	// (Feed.InitialPending); title, folder, icon, interval and entries stay.
	// Returns ErrNotFound when the feed does not exist or its URL is no
	// longer from, ErrConflict when another feed already uses to. With
	// keepFailures the failure count and last error stay instead (the feed
	// is waiting out a rate limit until next).
	ChangeFeedURL(ctx context.Context, id int64, from, to string, next time.Time, keepFailures bool) error
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

	// --- Digests ---

	// ListDigests returns all digests ordered by position, then name
	// (case-insensitive).
	ListDigests(ctx context.Context) ([]Digest, error)
	GetDigest(ctx context.Context, id int64) (Digest, error)
	// CreateDigest appends a digest without sources or ingestion time.
	// Returns ErrConflict if the name is taken (case-insensitive, as
	// strings.EqualFold) and ErrInvalid if it is empty.
	CreateDigest(ctx context.Context, name string) (Digest, error)
	// RenameDigest returns ErrConflict if the name is taken, ErrInvalid if it
	// is empty.
	RenameDigest(ctx context.Context, id int64, name string) error
	// SetDigestIngest sets the digest's daily ingestion time to minute (0 to
	// MinutesPerDay-1, else ErrInvalid) when on is true, and clears it when
	// on is false (minute is then ignored).
	SetDigestIngest(ctx context.Context, id int64, minute int, on bool) error
	// DeleteDigest removes the digest and its memberships; feeds, folders,
	// tags and entries are kept.
	DeleteDigest(ctx context.Context, id int64) error
	// DigestSources returns the folders, feeds and tags the digest names.
	DigestSources(ctx context.Context, id int64) (DigestSources, error)
	// SetDigestSources replaces all of the digest's sources atomically.
	// Duplicate ids collapse. offered is the SourcesFingerprint of the
	// folders, feeds and tags the settings form offered: when they have
	// changed since (ids are reused), it returns ErrConflict. Returns
	// ErrInvalid for a non-positive id or more than MaxDigestSources
	// sources, and ErrNotFound when the digest or any of the sources does
	// not exist. Nothing is changed on any error.
	SetDigestSources(ctx context.Context, id int64, src DigestSources, offered string) error
	// DigestsNamingFolder and DigestsNamingTag return the digests that name
	// the folder or the tag, ordered as ListDigests: those that deleting it
	// removes it from.
	DigestsNamingFolder(ctx context.Context, id int64) ([]Digest, error)
	DigestsNamingTag(ctx context.Context, id int64) ([]Digest, error)
	// DigestFeedIDs returns, sorted, the feeds a digest's ingestion time
	// fetches: those it names and those in the folders it names.
	DigestFeedIDs(ctx context.Context, id int64) ([]int64, error)
	// IngestMinutes returns, sorted and distinct, the ingestion times
	// (minutes of the day) of the digests that name feedID or folderID; 0
	// matches nothing. These are the times at which a feed in that folder
	// must be fetched besides its regular schedule.
	IngestMinutes(ctx context.Context, feedID, folderID int64) ([]int, error)

	// --- Transcripts ---

	// NextTranscript returns the queued entry whose transcript is due
	// first: the one with the earliest DueAt, then the one with fewer
	// attempts, then the lowest id, with the streak of refusals so far.
	// Its DueAt may still lie ahead: callers fetch it only once that time
	// has come. Entries of feeds whose transcript switch is off are
	// skipped (frozen). ok is false when none is queued.
	NextTranscript(ctx context.Context) (job TranscriptJob, ok bool, err error)
	// RecordTranscript stores the outcome of an attempt at a queued entry's
	// transcript, and its effect on the streak of refusals (see
	// TranscriptResult.Requested), which it keeps even when the entry is
	// gone. It returns ErrNotFound when the entry is not queued (it was
	// deleted meanwhile, or its transcript is done) and ErrInvalid for an
	// unknown outcome, a TranscriptOK without text, or an outcome that
	// keeps the entry queued without a RetryAt.
	RecordTranscript(ctx context.Context, entryID int64, r TranscriptResult) error
	// GetTranscript returns the entry's transcript, or ErrNotFound unless
	// one was fetched (TranscriptOK).
	GetTranscript(ctx context.Context, entryID int64) (Transcript, error)
	// TranscriptStateOf returns the stored state of the entry's transcript;
	// Status is StatusNone when it was never queued (whether or not the
	// entry exists).
	TranscriptStateOf(ctx context.Context, entryID int64) (TranscriptState, error)
	// RequestTranscript queues the entry's transcript as of at, due at once
	// (the user asked for it): a new row, or a done one (no captions,
	// given up on) queued again from scratch (no attempts, not rechecked,
	// queued at at); a pending one is made due at at and counts as queued
	// at at, its attempts kept. The pause after a refusal still applies
	// (NextTranscript). It returns ErrNotFound when the entry does not exist
	// and ErrConflict when its transcript was fetched already.
	RequestTranscript(ctx context.Context, entryID int64, at time.Time) error

	// --- Settings ---

	// TranscriptsSetting returns the global transcript switch: false until
	// it was turned on.
	TranscriptsSetting(ctx context.Context) (bool, error)
	// SetTranscriptsSetting stores the global transcript switch. Turning it
	// on (from off) restarts the queue age of every pending transcript at
	// at (they were frozen meanwhile); it never queues anything.
	SetTranscriptsSetting(ctx context.Context, on bool, at time.Time) error
	// ServerTranscripts returns the server's transcript switch
	// (FR_TRANSCRIPTS) as the last RecordServerTranscripts stored it: on
	// until recorded off.
	ServerTranscripts(ctx context.Context) (bool, error)
	// RecordServerTranscripts stores the server's transcript switch, as the
	// transcript worker finds it when it starts. Recording it on after off
	// restarts the queue age of every pending transcript at at (they were
	// frozen meanwhile); it never queues anything.
	RecordServerTranscripts(ctx context.Context, on bool, at time.Time) error
	// FeedTranscripts returns the feed's transcript switch (on by default),
	// or ErrNotFound.
	FeedTranscripts(ctx context.Context, feedID int64) (bool, error)
	// SetFeedTranscripts stores the feed's transcript switch. Turning it on
	// (from off) restarts the queue age of the feed's pending transcripts
	// at at; it never queues anything. ErrNotFound when the feed is gone.
	SetFeedTranscripts(ctx context.Context, feedID int64, on bool, at time.Time) error

	// --- Counts ---

	// UnreadCounts computes all sidebar badges with a single query.
	UnreadCounts(ctx context.Context) (UnreadCounts, error)
}
