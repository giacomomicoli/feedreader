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
- **Scopes**: All, a folder, a single source, Watch later, Read later, Favourites, a tag, or a digest — with an Unread/All filter and "Load more" paging.
- **Digests**: your own views that combine several sources, folders and tags (say, three YouTube channels and two blogs as a morning newsletter), each with its own unread count and, optionally, a fixed daily time at which their sources are fetched.
- **Quick actions** on every card: mark read/watched, Watch later / Read later, favourite, free-text tags with autocomplete. "Mark all as read" for the current scope.
- **Videos, Shorts or both**: follow a YouTube channel's regular videos only, its Shorts only, or everything it uploads.
- **Folders** (flat), per-source settings (rename, move, custom poll interval, videos/Shorts choice, fetch status, unsubscribe).
- **Polite polling**: every 6 hours by default (YouTube feeds every hour), conditional GET (`ETag` / `Last-Modified`), a quick retry after a one-off error, then exponential backoff honouring `Retry-After` (except during YouTube's feed outages, see [Using feedreader](#using-feedreader)), at most 4 background fetches at once (plus at most one transcript request, when video transcripts are on). Nothing is fetched until you add your first source.
- **Video transcripts** (optional, off until you turn them on): the transcript of each new YouTube video, in the **Transcription** tab of its details — fetched slowly and best-effort, see *Transcripts* under [Using feedreader](#using-feedreader).
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
| A channel's video feed in another spelling (`m.youtube.com`, no `www`, extra parameters) | the same feed at `https://www.youtube.com/feeds/videos.xml?channel_id=UC…`, so a channel is never subscribed twice (sources added with an older version keep their URL) |
| A channel's *Videos only* or *Shorts only* feed (`feeds/videos.xml?playlist_id=UULF…` or `UUSH…`) in any spelling | that feed at `https://www.youtube.com/feeds/videos.xml?playlist_id=…` (see *Videos and Shorts* below) |
| `youtube.com/playlist?list=…` | the playlist's video feed |
| Any web page | the feeds the page advertises (you choose if there are several), or common feed locations such as `/feed` and `/rss.xml` |

YouTube channels show their avatar, read from the channel's page on YouTube. For an `@handle`, `/c/…` or `/user/…` URL that page is already fetched to find the channel ID, so the avatar costs no extra request; otherwise, or when that page shows none, subscribing takes one extra request. A channel still without an avatar (subscribed with an older version, or its page could not be read) gets one at its next successful check, with one try per channel each time feedreader starts. A channel followed as *Videos only* or *Shorts only* shows its channel's avatar too; other playlists and legacy `feeds/videos.xml?user=…` feeds show their initials, as does any source whose icon is missing or fails to load.

**When YouTube's feed is down.** YouTube's feed server sometimes answers `404` or a `5xx` error for working channels, for minutes or hours at a time, while the channel's page keeps working. If that happens while you add a channel feed (`feeds/videos.xml?channel_id=UC…`, which every `@handle`, `/c/…`, `/user/…` and `/channel/UC…` URL resolves to), feedreader checks with the channel's page on YouTube that the channel exists — the page it already fetched for an `@handle`, `/c/…` or `/user/…` URL, otherwise one request — and offers to subscribe anyway, with the channel's name and avatar from that page. The source then shows *Waiting for the first fetch* (an hourglass in the sidebar instead of the ⚠) and is retried after 15 minutes, then every hour; when the feed answers, its videos arrive as if you had just added it (the 5 newest unread). This is never offered for playlists (a channel's *Videos only* and *Shorts only* feeds included: pick those in the source's settings once it is subscribed), other sites, rate limits (`429`) or network errors, and the server checks it again when you confirm, whatever the form says. A channel whose feed never comes back keeps waiting until you unsubscribe.

**Videos and Shorts.** A YouTube channel can be followed in three ways, picked under **Follow** before you confirm and changed at any time in the source's settings: *Videos and Shorts* (the default: the channel's feed, everything it uploads), *Videos only, no Shorts*, or *Shorts only*. The last two subscribe to the lists YouTube keeps of a channel's regular videos and of its Shorts (`feeds/videos.xml?playlist_id=UULF…` and `UUSH…`, the channel ID with `UC` replaced by `UULF` or `UUSH`). These lists are not documented by YouTube and could stop working, and live streams may be left out of the regular-videos list. When the list of your choice does not answer (a channel without Shorts may have no Shorts list), it counts as a failure of that source, under the same rules as a channel feed that is down, and shows the ⚠ after 3 failures (*Waiting for the first fetch* is only for a channel subscribed while its feed was down that has not answered yet): feedreader never falls back to the channel's feed, which would bring back what you chose to leave out. Switching an existing source keeps its title, folder, interval and the entries it already has (nothing is hidden or deleted) and fetches the new list at once, unless YouTube is rate-limiting the source (its last check failed with `429` or a `Retry-After`), in which case it waits until that check was due and keeps showing those failures, so no later setting change can fetch it sooner; of the videos it brings that were not stored yet, the 5 newest are unread and the rest are kept as read, as when adding a source. A switch to a list you already follow as another source is refused.

You can edit the title and pick (or create) a folder before confirming. The **5 newest** entries start as unread; older ones are kept as already read, and you can page back through them with **Load more**. A feed only contains its most recent items (YouTube: the last 15 videos), so feedreader cannot show anything older than what the feed offered when you subscribed.

**Reading.** Click a thumbnail or title to open the original (video on youtube.com, article on its site) in a new tab. Opening something never marks it read — use the card's **⋮** menu: *Mark read / watched*, *Read later / Watch later*, *Add to favourites*, *Add tag*. Hover over a title for a short excerpt of its summary, or click **Details** to open a panel that slides in from the right with two tabs: **Summary**, up to 5,000 characters of the summary — a video's full description fits — with its paragraphs and line breaks, and **Transcription**, a video's transcript once one was fetched (the tab is greyed out until then). Esc, **Close** or a click beside the panel dismisses it, and Ctrl/Cmd-click opens the details as a page in a new tab. Details are shown for every card with a summary, and for every YouTube video. Opening them never marks anything read.

**Transcripts (optional).** feedreader can keep the transcript of YouTube videos and show it in the **Transcription** tab of a video's details. It is off until you tick **Fetch transcripts** under *Video transcripts* at the bottom of the sidebar and press **Save**; each YouTube source can also be left out with the **Video transcripts** switch in its **Feed settings** (on by default). `FR_TRANSCRIPTS=false` is the server's kill switch: then no transcript is ever requested and the sidebar switch is greyed out. Turning transcripts on never fetches anything for the videos already stored: only videos that a check finds new from then on get a transcript — never the videos already in a channel's feed when you add it, nor those a channel added while its feed was down delivers at its first answer, nor those the new list brings at its first answer after you switch a channel between videos and Shorts. For any other video, open its details: the line under the tabs says where its transcript stands (not requested, queued and when it is next checked, waiting because YouTube is limiting requests, no captions, given up and why), with a **Fetch transcript** (or **Check now**, **Try again**) button that queues it. The button never makes a request sooner than the rules below allow. Turning a switch off freezes the queue (nothing is requested, nothing is lost); turning it back on resumes it. YouTube has no public API for transcripts, so feedreader asks for the captions the way YouTube's Android app does, without an API key or cookies: two requests to `www.youtube.com` each time it checks a video, one to its player API and one for the caption track in the video's original language (authored captions before automatic ones); neither request follows a redirect. This is unofficial and best-effort, and may stop working whenever YouTube changes that API. It works from a home internet connection at a low rate, while requests from data-centre addresses, or too many at once, get refused — so feedreader is deliberately slow about it: one video at a time, 10 seconds apart; when YouTube refuses a request, feedreader waits an hour before the next one (twice as long after each further refusal in a row, or longer if YouTube asks for that, up to a day), even across a restart. A video found without captions is checked once more 6 hours later, since YouTube makes automatic captions some time after a video is published. Premieres and live streams are checked every 6 hours until they have aired, so their transcript is fetched after the broadcast. A transcript that cannot be fetched for another reason (a network or YouTube outage, a video YouTube will not play to this client, or an answer in a form feedreader does not understand, say) is tried again 2 hours later, then 4 hours after that: 3 tries spread over hours. A caption track larger than `FR_FETCH_MAX_BODY` is not asked for again. A video still queued a week after it was queued (or after transcripts were turned back on) is given up on. A video without captions, or whose transcript could not be fetched, simply shows its description alone.

**Organizing.** Use the sidebar to switch between All, Watch later (videos), Read later (articles), Favourites, digests, tags and folders. Create folders at the bottom of the sidebar; rename or delete them from the folder's view (deleting a folder keeps its sources). Open **Feed settings** on a source to rename it, move it, choose videos and/or Shorts (YouTube channels), change how often it is checked, see when it was last fetched and any error, or unsubscribe (which deletes its entries, including saved ones).

**Digests.** A digest gathers entries from several places in one view: the sources you pick, every source in the folders you pick (including ones added to them later), and every entry carrying the tags you pick — each entry once. Create one with **New digest** at the bottom of the sidebar, then pick its folders, sources and tags in **Digest settings**; it appears under *Digests* in the sidebar with its unread count, and reads like any other scope. A digest can also have a **daily ingestion time** (in the time zone that the `TZ` environment variable names, or the system's when it is unset — see [Configuration](#configuration); the settings page shows it): its sources, and those in its folders, are then fetched every day at that time on top of their regular checks, so the digest is ready by then — for example 07:00 for a morning read. Those fetches come in addition to the usual schedule, never instead of it, and also mean a failing source is retried at that time rather than backing off past it, unless the server asked for a longer wait with `Retry-After`. Deleting a digest leaves its sources, folders, tags and entries alone; deleting a folder or a tag removes it from the digests that name it, which its confirmation lists.

**Refreshing.** Sources are checked automatically: by default every 6 hours, and every hour for YouTube channels and playlists, whose feeds only list the latest 15 videos. **Refresh** checks the current source — or every source — right away. Once those checks are done, the grid reloads itself if new entries arrived that it can show; otherwise it stays as it is (with the pages you loaded) and says there is nothing new. It stops waiting after about a minute and shows what has arrived; reload the page later for sources that take longer. A failed check is retried after 15 minutes, then less and less often, down to once a day. YouTube feeds that answer `404` or a `5xx` error — YouTube's feed server does this for working channels, sometimes for hours — are retried after 15 minutes and then every hour (their interval), not less and less often; rate limits (`429`) and network errors still back off. A ⚠ icon next to a source means its last 3 scheduled fetches failed; the source settings show the error. A check you start with **Refresh** that fails shows its error but does not count as one more failure, and never moves the next scheduled check later (unless the server asks for a longer wait with `Retry-After`). Until the source's next successful check, a changed check interval or digest ingestion time leaves its next check where it is, so a server's `Retry-After` is honoured.

## Configuration

Settings come from environment variables, or from a file of `KEY=VALUE` lines passed with `-config <file>` (environment variables win). [.env.example](.env.example) lists them all.

| Variable | Default | Meaning |
| --- | --- | --- |
| `FR_LISTEN` | `127.0.0.1:8080` | HTTP listen address |
| `FR_ALLOWED_HOSTS` | (empty) | Comma-separated host names the UI answers to besides `localhost`, IP addresses and the `FR_LISTEN` host — e.g. the name of your reverse-proxy site (`feeds.lan.example`). Other `Host` headers get `421` (DNS-rebinding protection). |
| `FR_DATA_DIR` | `./data` | Directory holding `feedreader.db` |
| `FR_POLL_INTERVAL` | `6h` | Default interval between checks of a source (Go duration, at least `5m`); YouTube feeds use `FR_POLL_INTERVAL_YOUTUBE` |
| `FR_POLL_INTERVAL_YOUTUBE` | `1h` | Default interval between checks of a YouTube channel or playlist (Go duration, at least `5m`) |
| `FR_FETCH_WORKERS` | `4` | Maximum concurrent background fetches (1–64). Video transcripts, when on, add one more request at a time |
| `FR_FETCH_MAX_BODY` | `10485760` | Maximum feed size in bytes (after decompression). It also caps each answer to a transcript request, and so the length of a transcript: a video whose caption track is larger gets none |
| `FR_USER_AGENT` | `feedreader/<version> (+https://github.com/giacomomicoli/feedreader)` | User-Agent sent to sites |
| `FR_TRANSCRIPTS` | `true` | The server's kill switch for video transcripts. `true` lets the switch at the bottom of the sidebar turn them on (they start off); `false` means no transcript is ever requested; videos already queued wait and resume when it is `true` again (unofficial and best-effort, see *Transcripts* under [Using feedreader](#using-feedreader)) |
| `FR_LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error`. At `info` every check of a source is logged: one line with its HTTP status, new entries, duration and next check, or a warning when it failed. So is every check of a video's transcript: one line with its outcome and, unless it was fetched, the reason |

Digest ingestion times, and the times the UI shows, use the time zone that the standard `TZ` environment variable names (e.g. `TZ=Europe/Rome`), or the system's when it is unset. `TZ` must be set in the service's environment — with the systemd unit below, in `/etc/feedreader/env` — since a `-config` file only holds `FR_*` settings. The binary embeds the time zone database, so `TZ` works even on hosts without one; the startup log line shows the zone in use (`time_zone=`).

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
internal/fetch/     HTTP client: conditional GET, redirects, size limit, bounded POST
internal/parse/     RSS/Atom/JSON Feed parsing (gofeed) and HTML sanitizing (bluemonday)
internal/sched/     polling scheduler, the add-source pipeline and the transcript queue
internal/transcript/ YouTube video transcripts: player API → caption track, no API key
internal/innertube/ YouTube's InnerTube API: requests, client profiles, refusals, bounded JSON reading
internal/web/       HTTP handlers, htmx partials, security headers
internal/e2e/       end-to-end tests of the whole stack
web/                embedded templates and static files (htmx, CSS, a small script for error notices, the entry drawer and its tabs, and broken icons)
migrations/         embedded SQL schema migrations
deploy/             example systemd unit and reverse-proxy config
```

Issues and pull requests are welcome. Please report security problems privately — see [SECURITY.md](SECURITY.md).

## License

[MIT](LICENSE). Includes [htmx](https://htmx.org) (`web/static/htmx.min.js`, Zero-Clause BSD).
