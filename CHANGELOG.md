# Changelog

All notable changes to feedreader are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Until 1.0.0, any release may change behaviour, settings or the database schema;
alpha releases are early previews meant for a single trusted user, and beta
releases are feature-complete candidates for 1.0.0 that only get fixes.

## [Unreleased]

## [1.0.0-beta.1] - 2026-11-06

First beta: feature-complete for 1.0.0, which follows once it has run in
production without surprises; until then only fixes. No database migrations
and no new settings: upgrading from 0.1.0-alpha.2 is a swap of the binary.

### Added

- Switch the entries between the card grid and a list, with two icon-only
  buttons on the right of each top bar; the choice is kept in the browser.
  A list row shows the thumbnail, the title, the description cut to two lines
  and the source, and the quick actions as icon buttons on its right; every
  row has the same height, and pressing a description opens the details with
  all of it (#47).
- Play a YouTube video in the details drawer: its first tab is now Video, a
  player above the full description, with Watch on YouTube in the player's
  frame. The player loads YouTube's privacy-enhanced embed
  (`youtube-nocookie.com`) only when pressed, in a sandboxed frame that sends
  YouTube the site's origin only, and stops when the drawer closes or another
  tab is shown (#47).
- Copy a video's whole transcript to the clipboard from its Transcription
  tab, next to Download (#45).
- Add a source straight into a folder with the + on the right of the folder
  in the sidebar; the add flow starts with that folder picked (#42).
- Move a source to another folder from its page, with the Move button next
  to Feed settings (#43).

### Changed

- A video's thumbnail and title open its details in the drawer, where it
  plays, instead of YouTube in a new tab; Ctrl/Cmd-click still opens the
  page in a new tab. Articles still open the original (#47).
- The README is rewritten as a short introduction; the details of how
  polling, YouTube, transcripts and digests work moved to
  `docs/how-it-works.md`.
- The Content-Security-Policy allows frames from
  `https://www.youtube-nocookie.com` only, for the video player.
- `modernc.org/sqlite` updated from 1.60.1 to 1.61.1. 1.61.1 fixes a
  regression of 1.61.0 that planned prepared statements again on every run,
  which the store uses to insert entries (#48).

### Security

- `golang.org/x/net` updated from 0.61.0 to 0.62.0, fixing GO-2026-6866
  (namespace checks when rendering raw text in HTML) and GO-2026-6860
  (quadratic complexity when coalescing text nodes), both reachable through
  feed parsing and sanitizing.

## [0.1.0-alpha.2] - 2026-10-26

Second alpha: the four open issues of the first one. No database migrations.

### Added

- Download a video's transcript as a Markdown file from its Transcription
  tab: the video's title, channel, link, publication date and caption track,
  then the transcript's paragraphs (#30).

### Changed

- Sidebar folders with unread entries now start expanded, so new content is
  visible without opening each folder by hand (#28).
- The sidebar's Subscriptions are split into a Videos and a Blogs section. A
  folder holding both kinds is listed in each, with only its sources of that
  kind, their unread count and a view of only their entries; folders without
  sources are listed under Empty folders (#31).
- `golang.org/x/net` updated from 0.60.0 to 0.61.0.

### Fixed

- While YouTube's feed is down, a channel followed with everything it uploads
  now also gets the recordings of its past live streams: the fallback reads
  the channel's Live tab as well as its Videos and Shorts tabs (#29).

## [0.1.0-alpha.1] - 2026-10-26

First tagged release: an early alpha of a self-hosted, single-user feed reader
for YouTube channels and blogs.

### Added

- Add a source by URL: a feed URL (RSS, Atom, JSON Feed), a YouTube channel,
  `@handle` or playlist URL, or any website, whose feeds are discovered and
  offered when there are several.
- Card grid for videos and articles alike, with thumbnail, title, source, age
  and read state; YouTube channel avatars on cards and in the sidebar.
- Scopes: All, a folder, a single source, Watch later, Read later, Favourites,
  a tag or a digest, each with an Unread / All filter and "Load more" paging.
- Digests that combine sources, folders and tags in one view, with their own
  unread count and an optional fixed daily fetch time.
- Quick actions on every card: mark read or watched, Watch later / Read later,
  favourite, free-text tags with autocomplete; "Mark all as read" per scope.
- Flat folders and per-source settings: rename, move, custom poll interval,
  fetch status and unsubscribe.
- Follow a YouTube channel's regular videos, its Shorts, or both.
- Entry details in a drawer docked to the right, with the full summary and,
  for videos, a Transcription tab.
- Optional video transcripts (off by default), fetched slowly and best-effort,
  globally or per source.
- Polite polling: every 6 hours by default and hourly for YouTube, conditional
  GET, a quick retry after a one-off error, then exponential backoff honouring
  `Retry-After`; at most 4 background fetches at once. A manual Refresh reloads
  the grid when it is done. One log line per poll.
- YouTube feed outages are ridden out: capped retries, "Subscribe anyway" while
  a feed is down, and, when YouTube's feed of a channel fails, its latest
  videos read from YouTube's web API instead (`FR_YOUTUBE_FALLBACK`, on by
  default).
- Visual design: green-tinted neutrals with the logo's emerald as the single
  accent, Bricolage Grotesque, Figtree and JetBrains Mono self-hosted in the
  binary (SIL Open Font License).
- Light and dark themes with an icon-only sun/moon switch in every top bar;
  the choice is remembered in the browser, and the system setting is followed
  until one is made.
- Logo in the sidebar and favicons that follow the theme.
- "N unread" beside the title of All, folders, sources and digests, the same
  number as the sidebar badge, updated live after every action.
- One static binary with an embedded SQLite database, automatic schema
  migrations, and an example hardened systemd unit and Caddy snippet.

### Fixed

- The database opens when the data directory is a Windows path.

### Security

- Feed HTML is sanitized with an allow-list before it is stored, and the UI is
  served with a strict Content-Security-Policy, host checks and cross-site
  request protection. There is no login: run it on a trusted network only
  (see `SECURITY.md`).
- The whole userinfo of a URL is redacted in logs and errors.
- Built with Go 1.26.10, which fixes GO-2026-6809 (compromised intermediate
  certificates trusted by `crypto/x509`).

[Unreleased]: https://github.com/giacomomicoli/feedreader/compare/v1.0.0-beta.1...HEAD
[1.0.0-beta.1]: https://github.com/giacomomicoli/feedreader/compare/v0.1.0-alpha.2...v1.0.0-beta.1
[0.1.0-alpha.2]: https://github.com/giacomomicoli/feedreader/compare/v0.1.0-alpha.1...v0.1.0-alpha.2
[0.1.0-alpha.1]: https://github.com/giacomomicoli/feedreader/releases/tag/v0.1.0-alpha.1
