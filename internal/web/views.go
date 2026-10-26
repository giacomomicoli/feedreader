package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/parse"
	"github.com/giacomomicoli/feedreader/internal/resolve"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// View models. Templates only ever receive plain strings, numbers and
// booleans; feed-controlled text is never converted to template.HTML/URL.

// avatarView is a feed icon with an initials fallback underneath it.
type avatarView struct {
	IconURL  string
	Initials string
	Palette  string
}

func newAvatar(feedID int64, title, iconURL string) avatarView {
	return avatarView{IconURL: httpURL(iconURL), Initials: initials(title), Palette: paletteClass(feedID)}
}

// badgeView is an unread badge with a stable element id so that htmx can
// refresh it out of band. N == 0 renders an empty (hidden) badge.
type badgeView struct {
	ID  string
	N   int
	OOB bool
}

func feedBadgeID(id int64) string   { return "badge-feed-" + strconv.FormatInt(id, 10) }
func folderBadgeID(id int64) string { return "badge-folder-" + strconv.FormatInt(id, 10) }

// folderKindBadgeID is the badge of a folder in the sidebar section of a
// kind: a folder with videos and blogs is listed, and counted, in both.
func folderKindBadgeID(k store.FolderKind) string {
	return folderBadgeID(k.FolderID) + "-" + string(k.Kind)
}
func digestBadgeID(id int64) string { return "badge-digest-" + strconv.FormatInt(id, 10) }

const allBadgeID = "badge-all"

// actionView is one POST button in a card's overflow menu.
type actionView struct {
	Path  string
	Label string
}

// tagChip is a tag shown on a card.
type tagChip struct {
	ID         int64
	Name       string
	Href       string
	RemovePath string
}

// untitled stands in for an empty entry title.
const untitled = "(untitled)"

// cardView is one entry in the card grid.
type cardView struct {
	ID          int64
	Title       string
	Href        string // http(s) URL of the original, or ""
	Thumb       string // image URL, or "" for the initials placeholder
	ThumbIsIcon bool   // Thumb is the feed icon, not an entry image
	Avatar      avatarView
	FeedTitle   string
	FeedHref    string
	RelTime     string
	AbsTime     string
	ISOTime     string
	Excerpt     string
	// HasDetails: the card's Details button opens the entry drawer, which
	// has something to show: a summary, a fetched transcript, or the
	// transcript status of a YouTube video.
	HasDetails  bool
	Kind        store.Kind
	IsRead      bool
	IsLater     bool
	IsFavourite bool
	StatusLabel string
	LaterLabel  string
	Actions     []actionView
	Tags        []tagChip
	Back        string // page to return to after a non-htmx action
}

func newCard(e store.EntryView, now time.Time, back string) cardView {
	id := strconv.FormatInt(e.ID, 10)
	base := "/entries/" + id + "/"
	c := cardView{
		ID:          e.ID,
		Title:       e.Title,
		Href:        httpURL(e.URL),
		Avatar:      newAvatar(e.FeedID, e.FeedTitle, e.FeedIconURL),
		FeedTitle:   e.FeedTitle,
		FeedHref:    feedHref(e.FeedID, ""),
		RelTime:     relTime(e.PublishedAt, now),
		AbsTime:     absTime(e.PublishedAt),
		ISOTime:     e.PublishedAt.UTC().Format(time.RFC3339),
		Excerpt:     parse.Excerpt(e.SummaryHTML, config.SummaryMaxChars),
		Kind:        e.FeedKind,
		IsRead:      e.IsRead,
		IsLater:     e.IsLater,
		IsFavourite: e.IsFavourite,
		StatusLabel: statusLabel(e.FeedKind, e.IsRead),
		LaterLabel:  laterLabel(e.FeedKind),
		Back:        back,
	}
	if c.Title == "" {
		c.Title = untitled
	}
	c.HasDetails = c.Excerpt != "" || e.HasTranscript || isVideo(e)
	c.Thumb = httpURL(e.ThumbnailURL)
	if c.Thumb == "" {
		c.Thumb = firstImage(e.SummaryHTML)
	}
	if c.Thumb == "" && c.Avatar.IconURL != "" {
		c.Thumb, c.ThumbIsIcon = c.Avatar.IconURL, true
	}

	readPath, laterPath, favPath := base+"read", base+"later", base+"favourite"
	laterText := c.LaterLabel
	favText := "Add to favourites"
	if e.IsRead {
		readPath = base + "unread"
	}
	if e.IsLater {
		laterPath, laterText = base+"unlater", "Remove from "+c.LaterLabel
	}
	if e.IsFavourite {
		favPath, favText = base+"unfavourite", "Remove from favourites"
	}
	c.Actions = []actionView{
		{Path: readPath, Label: readActionLabel(e.FeedKind, e.IsRead)},
		{Path: laterPath, Label: laterText},
		{Path: favPath, Label: favText},
	}
	for _, t := range e.Tags {
		c.Tags = append(c.Tags, tagChip{
			ID:         t.ID,
			Name:       t.Name,
			Href:       scopeHref(store.Scope{Kind: store.ScopeTag, ID: t.ID}, ""),
			RemovePath: base + "tags/" + strconv.FormatInt(t.ID, 10) + "/remove",
		})
	}
	return c
}

// The tabs of an entry's details.
const (
	tabSummary    = "summary"
	tabTranscript = "transcript"
)

// entryView is one entry's details (GET /entries/{id}): title, source,
// and the tabs Summary (its full summary) and Transcription (a video's
// transcript, when one was fetched), with a video's transcript status
// under the tabs. It is the content of the layout's entry drawer, or the
// main pane of a page of its own when the request does not come from htmx.
type entryView struct {
	ID        int64
	Title     string
	Href      string // http(s) URL of the original, or ""
	OpenLabel string
	Avatar    avatarView
	FeedTitle string
	FeedHref  string
	RelTime   string
	AbsTime   string
	ISOTime   string
	Summary   string // plain text with its line breaks (parse.Text), or ""
	Tab       string // the selected tab: tabSummary or tabTranscript
	// HasTranscript enables the Transcription tab: the video's transcript
	// was fetched. Its text is loaded when the tab is first shown, unless
	// Transcript already holds it (the page with that tab selected).
	HasTranscript bool
	Transcript    *transcriptView
	// Status is the transcript status line of a YouTube video, or nil.
	Status *transcriptStatusView
	Drawer bool // rendered into the entry drawer rather than as a page
}

// transcriptView is a video's transcript in the Transcription tab.
type transcriptView struct {
	EntryID int64  // for the download link
	Label   string // "Transcript (en, auto-generated)"
	Lang    string // language code of Text (lang attribute), or ""
	Text    string // plain text with its line breaks
}

// newTranscriptView labels t, the transcript of entry id, with its language
// code and whether YouTube generated it: "Transcript (en, auto-generated)",
// "Transcript (de)", or "Transcript".
func newTranscriptView(id int64, t store.Transcript) *transcriptView {
	var notes []string
	if t.Lang != "" {
		notes = append(notes, t.Lang)
	}
	if t.Generated {
		notes = append(notes, "auto-generated")
	}
	label := "Transcript"
	if len(notes) > 0 {
		label += " (" + strings.Join(notes, ", ") + ")"
	}
	return &transcriptView{EntryID: id, Label: label, Lang: t.Lang, Text: t.Text}
}

func newEntryView(e store.EntryView, now time.Time, drawer bool) entryView {
	v := entryView{
		ID:            e.ID,
		Title:         e.Title,
		Href:          httpURL(e.URL),
		OpenLabel:     openLabel(e.FeedKind),
		Avatar:        newAvatar(e.FeedID, e.FeedTitle, e.FeedIconURL),
		FeedTitle:     e.FeedTitle,
		FeedHref:      feedHref(e.FeedID, ""),
		RelTime:       relTime(e.PublishedAt, now),
		AbsTime:       absTime(e.PublishedAt),
		ISOTime:       e.PublishedAt.UTC().Format(time.RFC3339),
		Summary:       parse.Text(e.SummaryHTML, config.SummaryFullMaxChars),
		Tab:           tabSummary,
		HasTranscript: e.HasTranscript,
		Drawer:        drawer,
	}
	if v.Title == "" {
		v.Title = untitled
	}
	return v
}

// moreView is the "Load more" button: Href is the full-page fallback,
// HxHref the fragment URL used by htmx.
type moreView struct {
	Href   string
	HxHref string
}

// gridView is the card grid of one scope page.
type gridView struct {
	NoFeeds   bool // nothing subscribed yet: show the "Add your first source" card
	Cards     []cardView
	More      *moreView
	EmptyText string
	EmptyLink string
	EmptyHint string
}

// scopeView is the main pane of the home page: top bar plus grid.
type scopeView struct {
	Title        string
	Subtitle     string
	Kind         store.ScopeKind
	ID           int64
	FeedKind     store.Kind // a folder narrowed to one kind, or ""
	Filter       string
	ShowFilter   bool
	UnreadOnly   bool
	UnreadHref   string
	AllHref      string
	Href         string // this page (scope + filter), for redirects
	UpTo         int64  // largest entry id when the page was rendered, for Mark all as read
	HasFeeds     bool
	IsFeed       bool
	IsFolder     bool
	IsTag        bool
	IsDigest     bool
	SettingsHref string
	// Count is the scope's unread count beside the title, the same number
	// as its sidebar badge; nil for scopes without a badge (Watch later,
	// Read later, Favourites, tags).
	Count *scopeCountView
	Grid  gridView
	OOB   bool // swapped out of band: the grid's reload after a refresh
}

// scopeCountView is the "N unread" beside a scope page's title. It keeps
// its id when the count is 0 (empty), so that later out-of-band updates
// (countsView.Scope) still find it.
type scopeCountView struct {
	N   int
	OOB bool
}

// scopeUnread returns the unread count of sc as its sidebar badge shows
// it, and false for scopes that have no badge.
func scopeUnread(c store.UnreadCounts, sc store.Scope) (int, bool) {
	switch sc.Kind {
	case store.ScopeAll:
		return c.All, true
	case store.ScopeFeed:
		return c.ByFeed[sc.ID], true
	case store.ScopeFolder:
		if sc.FeedKind != "" {
			return c.ByFolderKind[store.FolderKind{FolderID: sc.ID, Kind: sc.FeedKind}], true
		}
		return c.ByFolder[sc.ID], true
	case store.ScopeDigest:
		return c.ByDigest[sc.ID], true
	}
	return 0, false
}

// navItem is a fixed sidebar entry (All, Watch later, Read later, Favourites).
type navItem struct {
	Label  string
	Href   string
	Icon   string
	Active bool
	Badge  *badgeView
}

type feedNav struct {
	ID        int64
	Title     string
	Href      string
	Avatar    avatarView
	Badge     badgeView
	Warn      bool
	LastError string
	// Waiting marks a subscription whose feed has not answered yet.
	Waiting bool
	Active  bool
}

type folderNav struct {
	ID     int64
	Name   string
	Href   string
	Badge  badgeView
	Open   bool
	Active bool
	Feeds  []feedNav
}

type digestNav struct {
	ID     int64
	Name   string
	Href   string
	Badge  badgeView
	Active bool
	// Schedule is the tooltip of a digest with an ingestion time, "" for
	// none.
	Schedule string
}

type tagNav struct {
	ID     int64
	Name   string
	Count  int
	Href   string
	Active bool
}

// tagListView is the sidebar tag list, also swapped out of band.
type tagListView struct {
	Tags []tagNav
	OOB  bool
}

// sidebarSection is one kind's part of the subscriptions: its folders (each
// with only its feeds of that kind, linking to the folder narrowed to it),
// then its feeds outside any folder.
type sidebarSection struct {
	Label         string // "Videos", "Blogs"
	Folders       []folderNav
	Uncategorized []feedNav
}

// sidebarKinds are the feed kinds in the order of their sidebar sections.
var sidebarKinds = []store.Kind{store.KindYouTube, store.KindRSS}

// sectionLabel names the sidebar section of feed kind k.
func sectionLabel(k store.Kind) string {
	if k == store.KindYouTube {
		return "Videos"
	}
	return "Blogs"
}

// sidebarView is the whole left column.
type sidebarView struct {
	Nav      []navItem
	Digests  []digestNav
	Tags     tagListView
	Sections []sidebarSection // only the kinds that have feeds
	// EmptyFolders are the folders without any feed, which belong to no
	// section: listed apart so they can still be opened, renamed or deleted.
	EmptyFolders []folderNav
	HasFeeds     bool
	// Transcripts is the global transcript switch; nil on error pages.
	Transcripts *transcriptsSettingView
	// Unavailable is set on error pages rendered without database access:
	// the digest, tag and subscription lists are omitted rather than shown
	// empty.
	Unavailable bool
}

// countsView holds every out-of-band sidebar update sent after an action
// (counts are refreshed via htmx after every action), and the count beside
// the title when the action came from a scope page that shows one.
type countsView struct {
	Badges []badgeView
	Tags   tagListView
	Scope  *scopeCountView
}

// sidebarData is everything the sidebar is built from.
type sidebarData struct {
	folders []store.Folder
	feeds   []store.Feed
	digests []store.Digest
	tags    []store.Tag
	counts  store.UnreadCounts
	// transcripts is the global transcript switch (nil when not loaded).
	transcripts *transcriptsSettingView
}

func buildSidebar(d sidebarData, cur store.Scope) sidebarView {
	sv := sidebarView{HasFeeds: len(d.feeds) > 0, Transcripts: d.transcripts}
	sv.Nav = []navItem{
		{Label: "All", Href: scopeHref(store.Scope{Kind: store.ScopeAll}, ""), Icon: "all",
			Active: cur.Kind == store.ScopeAll, Badge: &badgeView{ID: allBadgeID, N: d.counts.All}},
		{Label: "Watch later", Href: scopeHref(store.Scope{Kind: store.ScopeWatchLater}, ""), Icon: "later",
			Active: cur.Kind == store.ScopeWatchLater},
		{Label: "Read later", Href: scopeHref(store.Scope{Kind: store.ScopeReadLater}, ""), Icon: "bookmark",
			Active: cur.Kind == store.ScopeReadLater},
		{Label: "Favourites", Href: scopeHref(store.Scope{Kind: store.ScopeFavourites}, ""), Icon: "star",
			Active: cur.Kind == store.ScopeFavourites},
	}
	for _, dg := range d.digests {
		dn := digestNav{
			ID:     dg.ID,
			Name:   dg.Name,
			Href:   scopeHref(store.Scope{Kind: store.ScopeDigest, ID: dg.ID}, ""),
			Badge:  badgeView{ID: digestBadgeID(dg.ID), N: d.counts.ByDigest[dg.ID]},
			Active: cur.Kind == store.ScopeDigest && cur.ID == dg.ID,
		}
		if dg.HasIngest {
			dn.Schedule = "Fetched daily at " + clockTime(dg.IngestMinute)
		}
		sv.Digests = append(sv.Digests, dn)
	}
	sv.Tags = buildTagList(d.tags, cur, false)

	known := make(map[int64]bool, len(d.folders))
	for _, fo := range d.folders {
		known[fo.ID] = true
	}
	byFolder := make(map[store.FolderKind][]feedNav)
	uncategorized := make(map[store.Kind][]feedNav)
	activeFolder := store.FolderKind{}
	for _, f := range d.feeds {
		kind := f.Kind
		fn := feedNav{
			ID:        f.ID,
			Title:     f.Title,
			Href:      feedHref(f.ID, ""),
			Avatar:    newAvatar(f.ID, f.Title, f.IconURL),
			Badge:     badgeView{ID: feedBadgeID(f.ID), N: d.counts.ByFeed[f.ID]},
			Warn:      warnFeed(f),
			LastError: f.LastError,
			Waiting:   waitingFeed(f),
			Active:    cur.Kind == store.ScopeFeed && cur.ID == f.ID,
		}
		key := store.FolderKind{FolderID: f.FolderID, Kind: kind}
		if fn.Active {
			activeFolder = key
		}
		// Feeds whose folder vanished concurrently are shown uncategorized.
		if f.FolderID == 0 || !known[f.FolderID] {
			uncategorized[kind] = append(uncategorized[kind], fn)
			continue
		}
		byFolder[key] = append(byFolder[key], fn)
	}
	for _, kind := range sidebarKinds {
		sec := sidebarSection{Label: sectionLabel(kind), Uncategorized: uncategorized[kind]}
		for _, fo := range d.folders {
			key := store.FolderKind{FolderID: fo.ID, Kind: kind}
			feeds := byFolder[key]
			if len(feeds) == 0 {
				continue
			}
			// The unnarrowed folder page is active in every section.
			active := cur.Kind == store.ScopeFolder && cur.ID == fo.ID && (cur.FeedKind == "" || cur.FeedKind == kind)
			unread := d.counts.ByFolderKind[key]
			sec.Folders = append(sec.Folders, folderNav{
				ID:    fo.ID,
				Name:  fo.Name,
				Href:  scopeHref(store.Scope{Kind: store.ScopeFolder, ID: fo.ID, FeedKind: kind}, ""),
				Badge: badgeView{ID: folderKindBadgeID(key), N: unread},
				// A folder with unread entries starts expanded, so new content
				// shows without opening every folder by hand.
				Open:   active || unread > 0 || activeFolder == key,
				Active: active,
				Feeds:  feeds,
			})
		}
		if len(sec.Folders) > 0 || len(sec.Uncategorized) > 0 {
			sv.Sections = append(sv.Sections, sec)
		}
	}
	for _, fo := range d.folders {
		if hasFolderFeeds(d.feeds, fo.ID) {
			continue
		}
		active := cur.Kind == store.ScopeFolder && cur.ID == fo.ID
		sv.EmptyFolders = append(sv.EmptyFolders, folderNav{
			ID:     fo.ID,
			Name:   fo.Name,
			Href:   scopeHref(store.Scope{Kind: store.ScopeFolder, ID: fo.ID}, ""),
			Badge:  badgeView{ID: folderBadgeID(fo.ID)},
			Open:   active,
			Active: active,
		})
	}
	return sv
}

// hasFolderFeeds reports whether any of feeds is in folder id.
func hasFolderFeeds(feeds []store.Feed, id int64) bool {
	for _, f := range feeds {
		if f.FolderID == id {
			return true
		}
	}
	return false
}

func buildTagList(tags []store.Tag, cur store.Scope, oob bool) tagListView {
	tl := tagListView{OOB: oob}
	for _, t := range tags {
		tl.Tags = append(tl.Tags, tagNav{
			ID:     t.ID,
			Name:   t.Name,
			Count:  t.Count,
			Href:   scopeHref(store.Scope{Kind: store.ScopeTag, ID: t.ID}, ""),
			Active: cur.Kind == store.ScopeTag && cur.ID == t.ID,
		})
	}
	return tl
}

func buildCounts(d sidebarData, cur store.Scope) countsView {
	cv := countsView{Tags: buildTagList(d.tags, cur, true)}
	cv.Badges = append(cv.Badges, badgeView{ID: allBadgeID, N: d.counts.All, OOB: true})
	for _, dg := range d.digests {
		cv.Badges = append(cv.Badges, badgeView{ID: digestBadgeID(dg.ID), N: d.counts.ByDigest[dg.ID], OOB: true})
	}
	// One badge per folder and kind it has feeds of, as buildSidebar shows
	// them; a folder without feeds has a badge of its own, always empty.
	seen := map[store.FolderKind]bool{}
	for _, f := range d.feeds {
		key := store.FolderKind{FolderID: f.FolderID, Kind: f.Kind}
		if f.FolderID == 0 || seen[key] {
			continue
		}
		seen[key] = true
		cv.Badges = append(cv.Badges, badgeView{ID: folderKindBadgeID(key), N: d.counts.ByFolderKind[key], OOB: true})
	}
	for _, fo := range d.folders {
		if !hasFolderFeeds(d.feeds, fo.ID) {
			cv.Badges = append(cv.Badges, badgeView{ID: folderBadgeID(fo.ID), OOB: true})
		}
	}
	for _, f := range d.feeds {
		cv.Badges = append(cv.Badges, badgeView{ID: feedBadgeID(f.ID), N: d.counts.ByFeed[f.ID], OOB: true})
	}
	// cur is empty unless the request came from a scope page (currentScope).
	if n, ok := scopeUnread(d.counts, cur); ok {
		cv.Scope = &scopeCountView{N: n, OOB: true}
	}
	return cv
}

// warnFeed reports whether the UI flags f as broken: config.WarnAfterFailures
// failures in a row, unless it is a subscription still waiting for its
// feed's first answer (waitingFeed), which is shown as waiting instead.
func warnFeed(f store.Feed) bool {
	return !waitingFeed(f) && f.ErrorCount >= config.WarnAfterFailures
}

// waitingFeed reports whether f is a subscription whose feed has not
// answered since it was subscribed: one stored while its YouTube channel's
// feed was down (only a channel's own feed is ever stored so), still
// without entries. Its failures are shown as waiting, not as a broken feed.
//
// The pending flag alone does not say this: switching a channel between
// its modes (see resolve.Mode) sets it too, so that the new URL's first
// document follows the initial-add rule. Such a feed has answered before
// and keeps its entries; a mode list that keeps failing (a channel without
// Shorts) is a failing feed. A channel feed switched to while it still had
// no entry at all (its feed never answered, or never listed anything) is
// shown as waiting until its next successful fetch.
func waitingFeed(f store.Feed) bool {
	return f.InitialPending && !f.HasEntries && resolve.IsChannelFeed(f.URL)
}

// waitingText is the status of a subscription whose feed has not answered
// yet; templates use the "waiting-text" template, which says the same.
const waitingText = "Waiting for the first fetch"

// pendingEmpty explains the empty grid of a subscription whose feed has not
// answered yet, with its next try (the feed's next fetch) as a clock time in
// loc.
func pendingEmpty(next time.Time, loc *time.Location) (text, hint string) {
	text = waitingText + "."
	hint = "Videos will appear when YouTube's feed answers."
	if !next.IsZero() {
		hint += " Next try about " + next.In(loc).Format(clockLayout) + "."
	}
	return text, hint
}

// emptyMessage explains an empty grid and points at the next useful step.
func emptyMessage(sc store.Scope, filter string) (text, hint, link string) {
	switch sc.Kind {
	case store.ScopeWatchLater:
		return "Nothing in Watch later.", "Use a video's menu to save it here.", ""
	case store.ScopeReadLater:
		return "Nothing in Read later.", "Use an article's menu to save it here.", ""
	case store.ScopeFavourites:
		return "No favourites yet.", "Use an entry's menu to add it to your favourites.", ""
	}
	if filter == filterUnread {
		return "You're all caught up.", "", scopeHref(sc, filterAll)
	}
	if sc.Kind == store.ScopeTag {
		return "No entries carry this tag.", "", ""
	}
	return "No entries yet.", "New entries appear after the next fetch.", ""
}

// confirmView is a two-step confirmation for destructive actions.
type confirmView struct {
	Heading    string
	Lines      []string
	Action     string
	Button     string
	CancelHref string
}

// unsubscribeConfirm states what unsubscribing deletes (later/favourite
// entries are deleted too and the dialog says so).
func unsubscribeConfirm(f store.Feed, st store.FeedEntryStats) confirmView {
	later := laterLabel(f.Kind)
	lines := []string{
		fmt.Sprintf("This deletes the source and all %s it has delivered.", pluralEntries(st.Total)),
	}
	switch st.Saved {
	case 0:
		lines = append(lines, fmt.Sprintf("None of them are in %s or Favourites.", later))
	default:
		lines = append(lines, fmt.Sprintf("That includes %s saved in %s or Favourites, which will be deleted too.",
			pluralEntries(st.Saved), later))
	}
	lines = append(lines, "This cannot be undone.")
	return confirmView{
		Heading:    fmt.Sprintf("Unsubscribe from “%s”?", f.Title),
		Lines:      lines,
		Action:     "/feeds/" + strconv.FormatInt(f.ID, 10) + "/unsubscribe",
		Button:     "Unsubscribe and delete entries",
		CancelHref: "/feeds/" + strconv.FormatInt(f.ID, 10),
	}
}

func pluralEntries(n int) string {
	if n == 1 {
		return "1 entry"
	}
	return strconv.Itoa(n) + " entries"
}

// folderOption is one <option> of a folder <select>.
type folderOption struct {
	ID       int64
	Name     string
	Selected bool
}

func folderOptions(folders []store.Folder, selected int64) []folderOption {
	opts := make([]folderOption, 0, len(folders))
	for _, f := range folders {
		opts = append(opts, folderOption{ID: f.ID, Name: f.Name, Selected: f.ID == selected})
	}
	return opts
}

// findFolderByName returns the folder whose name matches case-insensitively.
func findFolderByName(folders []store.Folder, name string) (store.Folder, bool) {
	for _, f := range folders {
		if strings.EqualFold(f.Name, name) {
			return f, true
		}
	}
	return store.Folder{}, false
}
