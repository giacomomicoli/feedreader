# feedreader

A small, self-hosted, single-user feed reader for **YouTube channels and blogs** — a lightweight replacement for Feedly.

- One static Go binary, an SQLite file, server-rendered pages with [htmx](https://htmx.org). No JavaScript build step, no external services.
- No accounts, API keys or OAuth: YouTube channel feeds and blog feeds are public, so feedreader simply polls them.
- Designed to run on a home server or a small VM on your **private network**.

> [!WARNING]
> feedreader has **no login**. Anyone who can reach it can read and change everything.
> Run it on a trusted network only (see [Deployment](#deployment) and [SECURITY.md](SECURITY.md)) — never expose it directly to the internet.

## Features

- **Add anything by URL** — paste a feed URL (RSS, Atom, JSON Feed), a YouTube channel / `@handle` / playlist URL, or any website: feedreader finds its feed(s) and lets you pick when there are several.
- **Card grid** like the YouTube home page, for videos and articles alike: thumbnail, title, source, age, read state.
- **Scopes**: All, a folder, a single source, Watch later, Read later, Favourites, or a tag — with an Unread/All filter and "Load more" paging.
- **Quick actions** on every card: mark read/watched, Watch later / Read later, favourite, free-text tags with autocomplete. "Mark all as read" for the current scope.
- **Folders** (flat), per-source settings (rename, move, custom poll interval, fetch status, unsubscribe).
- **Polite polling**: every 6 hours by default (YouTube feeds every hour), conditional GET (`ETag` / `Last-Modified`), exponential backoff on errors honouring `Retry-After`, at most 4 background fetches at once. Nothing is fetched until you add your first source.
- **Safe content**: all feed HTML is sanitized with an allow-list before it is stored; the UI uses a strict Content-Security-Policy.

Not included (yet): push updates (WebSub), full-text article extraction, embedded video player, search, OPML import/export, multiple users.

## Quick start

You need [Go](https://go.dev/dl/) 1.26 or newer (the exact patched toolchain pinned in `go.mod` is downloaded automatically).

```sh
git clone https://github.com/giacomomicoli/feedreader.git
cd feedreader
make run
```

Open <http://127.0.0.1:8080>, click **Add source** and paste a URL. Data is stored in `./data/feedreader.db`.

To build a standalone binary instead:

```sh
make build          # static, CGO-free binary: ./bin/feedreader
./bin/feedreader    # listens on 127.0.0.1:8080, data in ./data
```

Building for another machine: set the target platform, e.g. `GOOS=linux GOARCH=amd64 make build` (or `arm64` for a Raspberry Pi 4/5).

## Using feedreader

**Adding a source.** Click **Add source** and paste one of:

| You paste | feedreader subscribes to |
| --- | --- |
| A feed URL (`…/feed`, `…/atom.xml`, `…/feed.json`) | that feed |
| `youtube.com/channel/UC…`, `youtube.com/@handle`, `/c/…`, `/user/…` | the channel's video feed |
| `youtube.com/playlist?list=…` | the playlist's video feed |
| Any web page | the feeds the page advertises (you choose if there are several), or common feed locations such as `/feed` and `/rss.xml` |

YouTube channels show their avatar, read from the channel's page on YouTube. For an `@handle`, `/c/…` or `/user/…` URL that page is already fetched to find the channel ID, so the avatar costs no extra request; otherwise, or when that page shows none, subscribing takes one extra request. A channel still without an avatar (subscribed with an older version, or its page could not be read) gets one at its next successful check, with one try per channel each time feedreader starts. Playlists and legacy `feeds/videos.xml?user=…` feeds show their initials, as does any source whose icon is missing or fails to load.

You can edit the title and pick (or create) a folder before confirming. The **5 newest** entries start as unread; older ones are kept as already read, and you can page back through them with **Load more**. A feed only contains its most recent items (YouTube: the last 15 videos), so feedreader cannot show anything older than what the feed offered when you subscribed.

**Reading.** Click a thumbnail or title to open the original (video on youtube.com, article on its site) in a new tab. Opening something never marks it read — use the card's **⋮** menu: *Mark read / watched*, *Read later / Watch later*, *Add to favourites*, *Add tag*. Hover or expand **Summary** for a short excerpt.

**Organizing.** Use the sidebar to switch between All, Watch later (videos), Read later (articles), Favourites, tags and folders. Create folders at the bottom of the sidebar; rename or delete them from the folder's view (deleting a folder keeps its sources). Open **Feed settings** on a source to rename it, move it, change how often it is checked, see when it was last fetched and any error, or unsubscribe (which deletes its entries, including saved ones).

**Refreshing.** Sources are checked automatically: by default every 6 hours, and every hour for YouTube channels and playlists, whose feeds only list the latest 15 videos. **Refresh** checks the current source — or every source — right away. A ⚠ icon next to a source means its last 3 fetches failed; the source settings show the error.

## Configuration

Settings come from environment variables, or from a file of `KEY=VALUE` lines passed with `-config <file>` (environment variables win). [.env.example](.env.example) lists them all.

| Variable | Default | Meaning |
| --- | --- | --- |
| `FR_LISTEN` | `127.0.0.1:8080` | HTTP listen address |
| `FR_ALLOWED_HOSTS` | (empty) | Comma-separated host names the UI answers to besides `localhost`, IP addresses and the `FR_LISTEN` host — e.g. the name of your reverse-proxy site (`feeds.lan.example`). Other `Host` headers get `421` (DNS-rebinding protection). |
| `FR_DATA_DIR` | `./data` | Directory holding `feedreader.db` |
| `FR_POLL_INTERVAL` | `6h` | Default interval between checks of a source (Go duration, at least `5m`); YouTube feeds use `FR_POLL_INTERVAL_YOUTUBE` |
| `FR_POLL_INTERVAL_YOUTUBE` | `1h` | Default interval between checks of a YouTube channel or playlist (Go duration, at least `5m`) |
| `FR_FETCH_WORKERS` | `4` | Maximum concurrent background fetches (1–64) |
| `FR_FETCH_MAX_BODY` | `10485760` | Maximum feed size in bytes (after decompression) |
| `FR_USER_AGENT` | `feedreader/<version> (+https://github.com/giacomomicoli/feedreader)` | User-Agent sent to sites |
| `FR_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |

`feedreader -version` prints the version.

## Deployment

feedreader is meant for a single person on a private network:

1. Build the binary (`make build`) and copy it to the server, e.g. `/usr/local/bin/feedreader`.
2. Run it as an unprivileged service. [deploy/feedreader.service](deploy/feedreader.service) is a hardened systemd unit: it expects a `feedreader` system user, settings in `/etc/feedreader/env` (copy `.env.example`) and stores data in `/var/lib/feedreader`.
3. Put a reverse proxy in front that only admits clients from your network — [deploy/Caddyfile.snippet](deploy/Caddyfile.snippet) is an example for [Caddy](https://caddyserver.com). Add the proxy's site name to `FR_ALLOWED_HOSTS`, otherwise feedreader answers `421 Misdirected Request`. Make sure only the proxy can reach feedreader's port: bind `FR_LISTEN` to the right interface and firewall it.
4. For access away from home, use a VPN (e.g. Tailscale or WireGuard) rather than exposing it publicly.

**Backups:** copy the SQLite file. It runs in WAL mode, so either stop the service first or take an online copy with the `sqlite3` command-line tool (a separate package on most distributions): `sqlite3 feedreader.db ".backup backup.db"`.
**Upgrades:** replace the binary and restart; database migrations run automatically at start.

## Development

The tests use Go's race detector, which needs a C compiler (gcc or clang; `build-essential` on Debian/Ubuntu). Without one, run `go test ./...` instead of `make test`.

```sh
make run        # run from source with ./data as data directory
make test       # go test -race ./...
make check      # gofmt check, go vet, tests
make build      # static binary in ./bin/feedreader
```

```
cmd/feedreader/     entry point: wires config, store, scheduler and HTTP server
internal/config/    configuration and all tunable defaults
internal/store/     repository interface + SQLite implementation
internal/resolve/   pasted URL → feed URL(s): YouTube, autodiscovery, fallbacks
internal/fetch/     HTTP client: conditional GET, redirects, size limit
internal/parse/     RSS/Atom/JSON Feed parsing (gofeed) and HTML sanitizing (bluemonday)
internal/sched/     polling scheduler and the add-source pipeline
internal/web/       HTTP handlers, htmx partials, security headers
internal/e2e/       end-to-end tests of the whole stack
web/                embedded templates and static files (htmx, CSS, a small error-handling script)
migrations/         embedded SQL schema migrations
deploy/             example systemd unit and reverse-proxy config
```

Issues and pull requests are welcome. Please report security problems privately — see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). Includes [htmx](https://htmx.org) (`web/static/htmx.min.js`, Zero-Clause BSD).
