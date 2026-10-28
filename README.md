# feedreader

A small, self-hosted feed reader for **YouTube channels and blogs**, built for
one person. I wrote it as a lightweight replacement for Feedly: you paste a
URL, and new videos and articles show up in a card grid, much like the
YouTube home page.

It's a single static Go binary with an SQLite file next to it. Pages are
rendered on the server with [htmx](https://htmx.org), so there's no
JavaScript build step, and it needs no accounts, API keys or external
services: YouTube channel feeds and blog feeds are public, and feedreader
simply polls them.

> [!WARNING]
> feedreader has **no login**. Anyone who can reach it can read and change
> everything. Run it on a home server or a VM on your private network, never
> directly on the internet — see [Deployment](#deployment) and
> [SECURITY.md](SECURITY.md).

## What it does

- **Add anything by URL.** A feed (RSS, Atom, JSON Feed), a YouTube channel,
  `@handle` or playlist, or just a website: feedreader finds the feed and
  lets you choose when there are several.
- **One grid for videos and articles**, with thumbnail, title, source, age
  and read state.
- **Organize** with folders, tags, Favourites, Watch later (videos) and Read
  later (articles). The sidebar keeps YouTube channels under *Videos* and
  everything else under *Blogs*.
- **Digests**: your own views that mix sources, folders and tags — three
  channels and two blogs as a morning newsletter, say — optionally fetched
  every day at a fixed time.
- **Videos, Shorts or both**: follow a channel's regular videos, only its
  Shorts, or everything it uploads.
- **Video transcripts** (optional, off by default), readable in the app and
  downloadable as Markdown.
- **Keeps working when YouTube's feeds don't**: during YouTube's frequent feed
  outages, a channel's latest videos are read from its website instead.
- **Polite polling**: every 6 hours (YouTube every hour), conditional
  requests, backoff on errors, a few fetches at a time.
- **Safe content**: feed HTML is sanitized with an allow-list before it's
  stored, and the UI uses a strict Content-Security-Policy.

Not there (yet): push updates (WebSub), full-text article extraction, an
embedded video player, search, OPML import/export, multiple users.

## Quick start

You need [Go](https://go.dev/dl/) 1.26 or newer; the exact toolchain pinned in
`go.mod` is downloaded automatically.

```sh
git clone https://github.com/giacomomicoli/feedreader.git
cd feedreader
make run
```

Open <http://127.0.0.1:8080>, click **Add source** and paste a URL. Your data
lives in `./data/feedreader.db`.

To get a standalone binary instead:

```sh
make build          # static, CGO-free binary in ./bin/feedreader
./bin/feedreader    # listens on 127.0.0.1:8080, data in ./data
```

To build for another machine, set the target platform, for example
`GOOS=linux GOARCH=amd64 make build` (or `GOARCH=arm64` for a Raspberry Pi
4/5).

## Using it

**Adding sources.** Paste a URL into **Add source**. Before confirming you
can rename it, put it in a folder and, for a YouTube channel, choose between
videos, Shorts or both. The 5 newest entries start as unread and the older
ones as read; **Load more** pages back through them. A feed only carries its
latest items (YouTube: the last 15 videos), so that's as far back as you can
go.

**Reading.** Clicking a card opens the video or article on its own site in a
new tab. Opening something never marks it read: use the card's **⋮** menu to
mark it read, save it for later, favourite or tag it. Hover a title for a
short excerpt, or click **Details** for a side panel with the full summary
and, for videos, the transcript. **Mark all as read** works on whatever view
you're in.

**Organizing.** The sidebar switches between All, Watch later, Read later,
Favourites, your digests, tags, folders and single sources, each with an
Unread / All filter. Folders are flat; a folder holding both channels and
blogs appears in both sidebar sections, showing only its sources of that
kind. Create folders and digests at the bottom of the sidebar. **Feed
settings** on a source lets you rename or move it, change how often it's
checked, see when it was last fetched and any error, or unsubscribe (which
deletes its entries, saved ones included).

**Refreshing.** Sources are checked on their own schedule; **Refresh** checks
the current source, or all of them, right away. A ⚠ next to a source means its
last 3 checks failed — its settings show why.

**Transcripts.** Off until you tick **Fetch transcripts** at the bottom of the
sidebar. From then on, new YouTube videos get their transcript in the
**Transcription** tab of their details, where **Download** saves it as
Markdown. YouTube has no public API for this, so it's unofficial,
deliberately slow and may break when YouTube changes things.

The details — what each kind of URL resolves to, retry and backoff rules,
how the YouTube fallback and transcripts behave — are in
[docs/how-it-works.md](docs/how-it-works.md).

## Configuration

Settings come from environment variables, or from a file of `KEY=VALUE` lines
passed with `-config <file>` (environment variables win).
[.env.example](.env.example) lists them all.

| Variable | Default | Meaning |
| --- | --- | --- |
| `FR_LISTEN` | `127.0.0.1:8080` | HTTP listen address. |
| `FR_ALLOWED_HOSTS` | (empty) | Comma-separated host names the UI answers to besides `localhost`, IP addresses and the `FR_LISTEN` host, e.g. your reverse proxy's site name (`feeds.lan.example`). Any other `Host` gets `421`, as a guard against DNS rebinding. |
| `FR_DATA_DIR` | `./data` | Directory holding `feedreader.db`. |
| `FR_POLL_INTERVAL` | `6h` | Default time between checks of a source (Go duration, at least `5m`). |
| `FR_POLL_INTERVAL_YOUTUBE` | `1h` | The same for YouTube channels and playlists. |
| `FR_FETCH_WORKERS` | `4` | Maximum concurrent background fetches (1–64). Transcripts, when on, add one request at a time. |
| `FR_FETCH_MAX_BODY` | `10485760` | Maximum size of a feed in bytes, after decompression. It also caps each transcript request, so a video with a larger caption track gets no transcript. |
| `FR_USER_AGENT` | `feedreader/<version> (+https://github.com/giacomomicoli/feedreader)` | User-Agent sent to sites. |
| `FR_TRANSCRIPTS` | `true` | Server-wide kill switch for transcripts. `true` lets you turn them on from the sidebar; `false` means none is ever requested (queued videos wait until it's `true` again). |
| `FR_YOUTUBE_FALLBACK` | `true` | Read a channel's latest videos from YouTube's website when its feed answers `404` or `5xx`. `false` only retries the feed. |
| `FR_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. At `info`, every check of a source and of a transcript is logged with its outcome. |

**Time zone.** Digest ingestion times and the times shown in the UI use the
zone named by the standard `TZ` variable (e.g. `TZ=Europe/Rome`), or the
system's when it's unset. Set it in the service's environment — with the
systemd unit below, in `/etc/feedreader/env` — since a `-config` file only
holds `FR_*` settings. The time zone database is embedded in the binary, so
this works on hosts without one; the startup log shows the zone in use
(`time_zone=`).

`feedreader -version` prints the version.

## Deployment

feedreader is meant for one person on a private network. A typical setup:

1. Build the binary with `make build` and copy it to the server, e.g. to
   `/usr/local/bin/feedreader`.
2. Run it as an unprivileged service.
   [deploy/feedreader.service](deploy/feedreader.service) is a hardened
   systemd unit: it expects a `feedreader` system user, reads its settings
   from `/etc/feedreader/env` (start from `.env.example`) and keeps data in
   `/var/lib/feedreader`.
3. Put a reverse proxy in front that only admits clients from your network;
   [deploy/Caddyfile.snippet](deploy/Caddyfile.snippet) is an example for
   [Caddy](https://caddyserver.com). Add the proxy's site name to
   `FR_ALLOWED_HOSTS`, or feedreader will answer `421 Misdirected Request`.
   Make sure only the proxy can reach feedreader's port: bind `FR_LISTEN` to
   the right interface and firewall it.
4. To use it away from home, go through a VPN such as Tailscale or WireGuard
   rather than exposing it publicly.

**Backups.** Copy the SQLite file. It runs in WAL mode, so either stop the
service first or take an online copy with the `sqlite3` command-line tool (a
separate package on most distributions):
`sqlite3 feedreader.db ".backup backup.db"`.

**Upgrades.** Replace the binary and restart; database migrations run
automatically at startup.

## Development

```sh
make run        # run from source, with ./data as data directory
make test       # go test -race ./...
make check      # gofmt check, go vet and tests
make build      # static binary in ./bin/feedreader
```

The race detector used by `make test` needs a C compiler (gcc or clang;
`build-essential` on Debian/Ubuntu). Without one, run `go test ./...`
instead.

How the code is laid out:

```
cmd/feedreader/       entry point: wires config, store, scheduler and HTTP server
internal/config/      configuration and all tunable defaults
internal/store/       repository interface and SQLite implementation
internal/resolve/     pasted URL → feed URL(s): YouTube, autodiscovery, fallbacks
internal/fetch/       HTTP client: conditional GET, redirects, size limit, bounded POST
internal/parse/       RSS/Atom/JSON Feed parsing (gofeed) and HTML sanitizing (bluemonday)
internal/sched/       polling scheduler, the add-source pipeline and the transcript queue
internal/transcript/  YouTube transcripts: player API → caption track, no API key
internal/innertube/   YouTube's InnerTube API: requests, client profiles, refusals, bounded JSON
internal/web/         HTTP handlers, htmx partials, security headers
internal/e2e/         end-to-end tests of the whole stack
web/                  embedded templates and static files (htmx, CSS, a small script)
migrations/           embedded SQL schema migrations
deploy/               example systemd unit and reverse-proxy config
docs/                 detailed behaviour, beyond this README
```

Issues and pull requests are welcome. Please report security problems
privately — see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). Includes [htmx](https://htmx.org)
(`web/static/htmx.min.js`, Zero-Clause BSD).
