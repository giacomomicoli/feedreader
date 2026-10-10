package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/giacomomicoli/feedreader/internal/config"
	"github.com/giacomomicoli/feedreader/internal/sched"
	"github.com/giacomomicoli/feedreader/internal/store"
)

// Digests: custom views over several folders, feeds and tags, with an
// optional daily ingestion time. Like the feed settings, the forms post with
// htmx into their own inline error slot and navigate with HX-Redirect on
// success, so the sidebar and its counts are rebuilt; without JS they post
// normally and get a 303.

// Form field names of the digest sources form: one value per picked id,
// and the fingerprint of the sources the form offered
// (store.SourcesFingerprint).
const (
	sourceFolder  = "folder"
	sourceFeed    = "feed"
	sourceTag     = "tag"
	sourceOffered = "offered"
)

// staleSources answers a sources form posted after the folders, feeds or
// tags it offered changed, for example after one was deleted and another
// created (which may reuse its id) in another tab.
const staleSources = "The folders, sources or tags changed since this page was loaded, so nothing was saved. " +
	"Here they are as they are now: pick again and save."

// ingestTimeLayouts are the value formats of an <input type="time">: HH:MM,
// or HH:MM:SS when the input allows seconds (they are dropped).
var ingestTimeLayouts = []string{"15:04", "15:04:05"}

// badIngestTime answers an ingestion time that does not parse.
const badIngestTime = "Enter a time like 07:00, or leave it empty for no fixed time."

// digestSettingsView is the digest settings page.
type digestSettingsView struct {
	ID        int64
	Name      string
	ScopeHref string
	// IngestValue is the ingestion time as HH:MM, "" for none.
	IngestValue   string
	Zone          string // abbreviation of the server's time zone, e.g. CET
	NextIngest    string
	NextIngestRel string
	FetchSummary  string
	Folders       []sourceOption
	Feeds         []sourceOption
	Tags          []sourceOption
	// Offered is the fingerprint of Folders, Feeds and Tags that the
	// sources form posts back (store.SourcesFingerprint).
	Offered string
	// SourcesError is shown in the sources form's error slot when the form
	// is rendered again in answer to a save.
	SourcesError string
}

// sourceOption is one checkbox of the digest sources form.
type sourceOption struct {
	Field   string // the form field: sourceFolder, sourceFeed or sourceTag
	ID      int64
	Label   string
	Detail  string
	Checked bool
	Avatar  *avatarView // feeds only
	// Warn and LastError flag a feed whose fetches keep failing.
	Warn      bool
	LastError string
}

func digestHref(id int64) string { return "/digests/" + strconv.FormatInt(id, 10) }

// zoneName is the abbreviation of the server's time zone now, the zone of
// digest ingestion times.
func (s *server) zoneName() string { return s.now().In(s.loc).Format("MST") }

// digestForPath loads the digest named by the {id} path value.
func (s *server) digestForPath(r *http.Request) (store.Digest, error) {
	id, err := pathID(r, "id")
	if err != nil {
		return store.Digest{}, err
	}
	d, err := s.store.GetDigest(r.Context(), id)
	if err != nil {
		return store.Digest{}, fmt.Errorf("get digest %d: %w", id, err)
	}
	return d, nil
}

// handleDigestCreate creates a digest from the sidebar form and opens its
// settings, where its sources are picked.
func (s *server) handleDigestCreate(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		s.fail(w, r, err)
		return
	}
	name := cleanName(r.PostForm.Get("name"))
	if msg := validName(name, "digest"); msg != "" {
		s.formError(w, r, msg)
		return
	}
	d, err := s.store.CreateDigest(r.Context(), name)
	if errors.Is(err, store.ErrConflict) {
		s.formError(w, r, "A digest with that name already exists.")
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("create digest: %w", err))
		return
	}
	s.redirect(w, r, digestHref(d.ID))
}

// handleDigestSettings renders /digests/{id}: name, ingestion time,
// sources and delete.
func (s *server) handleDigestSettings(w http.ResponseWriter, r *http.Request) {
	d, err := s.digestForPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v, err := s.digestSettings(r.Context(), d)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.renderPage(w, r, http.StatusOK, "digest", d.Name, store.Scope{Kind: store.ScopeDigest, ID: d.ID}, v)
}

// digestSettings builds the settings page of d.
func (s *server) digestSettings(ctx context.Context, d store.Digest) (digestSettingsView, error) {
	src, err := s.store.DigestSources(ctx, d.ID)
	if err != nil {
		return digestSettingsView{}, fmt.Errorf("sources of digest %d: %w", d.ID, err)
	}
	folders, err := s.store.ListFolders(ctx)
	if err != nil {
		return digestSettingsView{}, fmt.Errorf("list folders: %w", err)
	}
	feeds, err := s.store.ListFeeds(ctx)
	if err != nil {
		return digestSettingsView{}, fmt.Errorf("list feeds: %w", err)
	}
	tags, err := s.store.ListTags(ctx)
	if err != nil {
		return digestSettingsView{}, fmt.Errorf("list tags: %w", err)
	}
	v := digestSettingsView{
		ID:        d.ID,
		Name:      d.Name,
		ScopeHref: scopeHref(store.Scope{Kind: store.ScopeDigest, ID: d.ID}, ""),
		Zone:      s.zoneName(),
		Offered:   store.SourcesFingerprint(folders, feeds, tags),
	}
	if d.HasIngest {
		now := s.now()
		next := sched.NextIngest(now, d.IngestMinute, s.loc)
		v.IngestValue = clockTime(d.IngestMinute)
		v.NextIngest = next.In(s.loc).Format(absTimeLayout)
		v.NextIngestRel = relTime(next, now)
		ids, err := s.store.DigestFeedIDs(ctx, d.ID)
		if err != nil {
			return digestSettingsView{}, fmt.Errorf("feeds of digest %d: %w", d.ID, err)
		}
		v.FetchSummary = fetchSummary(len(ids))
	}

	picked := func(list []int64) map[int64]bool {
		m := make(map[int64]bool, len(list))
		for _, id := range list {
			m[id] = true
		}
		return m
	}
	pickedFolders, pickedFeeds, pickedTags := picked(src.FolderIDs), picked(src.FeedIDs), picked(src.TagIDs)
	folderNames := make(map[int64]string, len(folders))
	inFolder := make(map[int64]int, len(folders))
	for _, fo := range folders {
		folderNames[fo.ID] = fo.Name
	}
	for _, f := range feeds {
		inFolder[f.FolderID]++
	}
	for _, fo := range folders {
		v.Folders = append(v.Folders, sourceOption{Field: sourceFolder, ID: fo.ID, Label: fo.Name,
			Detail: pluralSources(inFolder[fo.ID]), Checked: pickedFolders[fo.ID]})
	}
	for _, f := range feeds {
		avatar := newAvatar(f.ID, f.Title, f.IconURL)
		v.Feeds = append(v.Feeds, sourceOption{Field: sourceFeed, ID: f.ID, Label: f.Title, Detail: folderNames[f.FolderID],
			Checked: pickedFeeds[f.ID], Avatar: &avatar,
			Warn: f.ErrorCount >= config.WarnAfterFailures, LastError: f.LastError})
	}
	for _, t := range tags {
		v.Tags = append(v.Tags, sourceOption{Field: sourceTag, ID: t.ID, Label: t.Name, Detail: pluralEntries(t.Count),
			Checked: pickedTags[t.ID]})
	}
	return v, nil
}

// fetchSummary says how many feeds the ingestion time fetches.
func fetchSummary(n int) string {
	switch n {
	case 0:
		return "No source is fetched at that time yet: pick sources or folders below."
	case 1:
		return "1 source is fetched at that time, besides its regular schedule."
	}
	return strconv.Itoa(n) + " sources are fetched at that time, besides their regular schedule."
}

func pluralSources(n int) string {
	if n == 1 {
		return "1 source"
	}
	return strconv.Itoa(n) + " sources"
}

// handleDigestRename renames a digest.
func (s *server) handleDigestRename(w http.ResponseWriter, r *http.Request) {
	d, err := s.digestForPath(r)
	if err == nil {
		err = parseForm(r)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	name := cleanName(r.PostForm.Get("name"))
	if msg := validName(name, "digest"); msg != "" {
		s.formError(w, r, msg)
		return
	}
	err = s.store.RenameDigest(r.Context(), d.ID, name)
	if errors.Is(err, store.ErrConflict) {
		s.formError(w, r, "A digest with that name already exists.")
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("rename digest %d: %w", d.ID, err))
		return
	}
	s.redirect(w, r, digestHref(d.ID))
}

// parseIngestTime reads an ingestion time typed into an <input
// type="time">: a minute of the day, or set false for an empty value (no
// fixed time).
func parseIngestTime(raw string) (minute int, set bool, err error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false, nil
	}
	for _, layout := range ingestTimeLayouts {
		if t, err := time.Parse(layout, raw); err == nil {
			return t.Hour()*minutesPerHour + t.Minute(), true, nil
		}
	}
	return 0, false, &httpError{status: http.StatusUnprocessableEntity, msg: badIngestTime}
}

// handleDigestSchedule sets or clears the digest's daily ingestion time.
func (s *server) handleDigestSchedule(w http.ResponseWriter, r *http.Request) {
	d, err := s.digestForPath(r)
	if err == nil {
		err = parseForm(r)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	minute, set, err := parseIngestTime(r.PostForm.Get("ingest"))
	if err != nil {
		s.formError(w, r, err.Error())
		return
	}
	if err := s.store.SetDigestIngest(r.Context(), d.ID, minute, set); err != nil {
		s.fail(w, r, fmt.Errorf("set ingestion time of digest %d: %w", d.ID, err))
		return
	}
	if set {
		s.scheduleDigest(r.Context(), d.ID)
	}
	s.redirect(w, r, digestHref(d.ID))
}

// handleDigestSources replaces the digest's folders, feeds and tags with
// those picked in the form. A form whose offered sources have changed since
// it was rendered saves nothing: it is answered with 409 and the form as it
// is now (see staleSourcesForm).
func (s *server) handleDigestSources(w http.ResponseWriter, r *http.Request) {
	d, err := s.digestForPath(r)
	if err == nil {
		err = parseForm(r)
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f := r.PostForm
	if n := len(f[sourceFolder]) + len(f[sourceFeed]) + len(f[sourceTag]); n > store.MaxDigestSources {
		s.formError(w, r, fmt.Sprintf(
			"A digest can have at most %d sources. Pick folders to include many sources at once.", store.MaxDigestSources))
		return
	}
	var src store.DigestSources
	for _, field := range []struct {
		name string
		list *[]int64
	}{{sourceFolder, &src.FolderIDs}, {sourceFeed, &src.FeedIDs}, {sourceTag, &src.TagIDs}} {
		for _, raw := range f[field.name] {
			id, err := parseID(raw)
			if err != nil {
				s.fail(w, r, badRequest("Invalid source."))
				return
			}
			*field.list = append(*field.list, id)
		}
	}
	err = s.store.SetDigestSources(r.Context(), d.ID, src, f.Get(sourceOffered))
	if errors.Is(err, store.ErrConflict) {
		s.staleSourcesForm(w, r, d)
		return
	}
	if errors.Is(err, store.ErrNotFound) {
		s.formError(w, r, "Some of the picked sources no longer exist. Reload the page and pick again.")
		return
	}
	if err != nil {
		s.fail(w, r, fmt.Errorf("set sources of digest %d: %w", d.ID, err))
		return
	}
	if d.HasIngest {
		s.scheduleDigest(r.Context(), d.ID)
	}
	s.redirect(w, r, digestHref(d.ID))
}

// staleSourcesForm answers a sources form whose offered sources have
// changed (feed, folder and tag ids are reused, so its picks may name other
// sources than it showed) with 409 and the form rendered again with the
// sources and picks as they are now: in place of the posted form for htmx,
// as the settings page otherwise.
func (s *server) staleSourcesForm(w http.ResponseWriter, r *http.Request, d store.Digest) {
	v, err := s.digestSettings(r.Context(), d)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	v.SourcesError = staleSources
	if !isHTMX(r) {
		s.renderPage(w, r, http.StatusConflict, "digest", d.Name, store.Scope{Kind: store.ScopeDigest, ID: d.ID}, v)
		return
	}
	w.Header().Set("HX-Retarget", "#digest-sources")
	w.Header().Set("HX-Reswap", "outerHTML")
	s.renderFragment(w, r, http.StatusConflict, "digest-sources", v, false)
}

// scheduleDigest moves the next fetch of the digest's feeds earlier when
// its ingestion time now comes first. The change is stored either way, so a
// failure is only logged: the feeds' next polls schedule the digest time.
func (s *server) scheduleDigest(ctx context.Context, id int64) {
	ids, err := s.store.DigestFeedIDs(ctx, id)
	if err == nil {
		err = s.sched.ScheduleIngest(ctx, ids)
	}
	if err != nil {
		s.log.Warn("could not schedule the digest's ingestion time", "digest", id, "err", err)
	}
}

// handleDigestDeleteConfirm asks before deleting a digest.
func (s *server) handleDigestDeleteConfirm(w http.ResponseWriter, r *http.Request) {
	d, err := s.digestForPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	lines := []string{"Its folders, sources and tags stay as they are, and so do their entries."}
	if d.HasIngest {
		lines = append(lines, fmt.Sprintf("Its sources are no longer fetched at %s besides their regular schedule.",
			clockTime(d.IngestMinute)))
	}
	v := confirmView{
		Heading:    fmt.Sprintf("Delete digest “%s”?", d.Name),
		Lines:      lines,
		Action:     digestHref(d.ID) + "/delete",
		Button:     "Delete digest",
		CancelHref: digestHref(d.ID),
	}
	s.renderConfirm(w, r, v, store.Scope{Kind: store.ScopeDigest, ID: d.ID})
}

// handleDigestDelete deletes a digest; only its memberships go with it.
func (s *server) handleDigestDelete(w http.ResponseWriter, r *http.Request) {
	d, err := s.digestForPath(r)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if err := s.store.DeleteDigest(r.Context(), d.ID); err != nil {
		s.fail(w, r, fmt.Errorf("delete digest %d: %w", d.ID, err))
		return
	}
	s.redirect(w, r, "/")
}
