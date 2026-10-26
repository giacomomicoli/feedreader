# Changelog

All notable changes to feedreader are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).
Until 1.0.0, any release may change behaviour, settings or the database schema;
alpha releases are early previews meant for a single trusted user.

## [Unreleased]

### Changed

- Sidebar folders with unread entries now start expanded, so new content is
  visible without opening each folder by hand (#28).

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

[Unreleased]: https://github.com/giacomomicoli/feedreader/compare/v0.1.0-alpha.1...HEAD
[0.1.0-alpha.1]: https://github.com/giacomomicoli/feedreader/releases/tag/v0.1.0-alpha.1
