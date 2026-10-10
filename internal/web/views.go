package web

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/parse"
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

// entryView is one entry's title, source and full summary (GET
// /entries/{id}): the content of the layout's entry dialog, or the main pane
// of a page of its own when the request does not come from htmx.
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
	Dialog    bool   // rendered into the entry dialog rather than as a page
}

func newEntryView(e store.EntryView, now time.Time, dialog bool) entryView {
	v := entryView{
		ID:        e.ID,
		Title:     e.Title,
		Href:      httpURL(e.URL),
		OpenLabel: openLabel(e.FeedKind),
		Avatar:    newAvatar(e.FeedID, e.FeedTitle, e.FeedIconURL),
		FeedTitle: e.FeedTitle,
		FeedHref:  feedHref(e.FeedID, ""),
		RelTime:   relTime(e.PublishedAt, now),
		AbsTime:   absTime(e.PublishedAt),
		ISOTime:   e.PublishedAt.UTC().Format(time.RFC3339),
		Summary:   parse.Text(e.SummaryHTML, config.SummaryFullMaxChars),
		Dialog:    dialog,
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
	Grid         gridView
	OOB          bool // swapped out of band: the grid's reload after a refresh
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
	Active    bool
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

// sidebarView is the whole left column.
type sidebarView struct {
	Nav           []navItem
	Digests       []digestNav
	Tags          tagListView
	Folders       []folderNav
	Uncategorized []feedNav
	HasFeeds      bool
	// Unavailable is set on error pages rendered without database access:
	// the digest, tag and subscription lists are omitted rather than shown
	// empty.
	Unavailable bool
}

// countsView holds every out-of-band sidebar update sent after an action
// (counts are refreshed via htmx after every action).
type countsView struct {
	Badges []badgeView
	Tags   tagListView
}

// sidebarData is everything the sidebar is built from.
type sidebarData struct {
	folders []store.Folder
	feeds   []store.Feed
	digests []store.Digest
	tags    []store.Tag
	counts  store.UnreadCounts
}

func buildSidebar(d sidebarData, cur store.Scope) sidebarView {
	sv := sidebarView{HasFeeds: len(d.feeds) > 0}
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
	byFolder := make(map[int64][]feedNav)
	activeFolder := int64(0)
	for _, f := range d.feeds {
		fn := feedNav{
			ID:        f.ID,
			Title:     f.Title,
			Href:      feedHref(f.ID, ""),
			Avatar:    newAvatar(f.ID, f.Title, f.IconURL),
			Badge:     badgeView{ID: feedBadgeID(f.ID), N: d.counts.ByFeed[f.ID]},
			Warn:      f.ErrorCount >= config.WarnAfterFailures,
			LastError: f.LastError,
			Active:    cur.Kind == store.ScopeFeed && cur.ID == f.ID,
		}
		if fn.Active {
			activeFolder = f.FolderID
		}
		// Feeds whose folder vanished concurrently are shown uncategorized.
		if f.FolderID == 0 || !known[f.FolderID] {
			sv.Uncategorized = append(sv.Uncategorized, fn)
			continue
		}
		byFolder[f.FolderID] = append(byFolder[f.FolderID], fn)
	}
	for _, fo := range d.folders {
		active := cur.Kind == store.ScopeFolder && cur.ID == fo.ID
		sv.Folders = append(sv.Folders, folderNav{
			ID:     fo.ID,
			Name:   fo.Name,
			Href:   scopeHref(store.Scope{Kind: store.ScopeFolder, ID: fo.ID}, ""),
			Badge:  badgeView{ID: folderBadgeID(fo.ID), N: d.counts.ByFolder[fo.ID]},
			Open:   active || (activeFolder != 0 && activeFolder == fo.ID),
			Active: active,
			Feeds:  byFolder[fo.ID],
		})
	}
	return sv
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
	for _, fo := range d.folders {
		cv.Badges = append(cv.Badges, badgeView{ID: folderBadgeID(fo.ID), N: d.counts.ByFolder[fo.ID], OOB: true})
	}
	for _, f := range d.feeds {
		cv.Badges = append(cv.Badges, badgeView{ID: feedBadgeID(f.ID), N: d.counts.ByFeed[f.ID], OOB: true})
	}
	return cv
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
