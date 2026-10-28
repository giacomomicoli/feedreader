# How feedreader works

The [README](../README.md) covers the everyday use of feedreader. This page is
the fine print: exactly what happens when you add a source, how often things
are checked, and how the YouTube-specific features behave when YouTube
doesn't cooperate.

- [Adding a source](#adding-a-source)
- [Polling and retries](#polling-and-retries)
- [Digests](#digests)
- [YouTube](#youtube)
  - [Channel avatars](#channel-avatars)
  - [Videos and Shorts](#videos-and-shorts)
  - [When YouTube's feed is down](#when-youtubes-feed-is-down)
  - [Transcripts](#transcripts)

## Adding a source

Click **Add source** and paste one of these:

| You paste | feedreader subscribes to |
| --- | --- |
| A feed URL (`…/feed`, `…/atom.xml`, `…/feed.json`) | that feed |
| `youtube.com/channel/UC…`, `youtube.com/@handle`, `/c/…`, `/user/…` | the channel's video feed |
| `youtube.com/playlist?list=…` | the playlist's video feed |
| Any web page | the feeds the page advertises (you choose if there are several), or common locations such as `/feed` and `/rss.xml` |

YouTube feed URLs are normalised, so the same channel is never subscribed
twice: a channel feed in any spelling (`m.youtube.com`, no `www`, extra
parameters) becomes `https://www.youtube.com/feeds/videos.xml?channel_id=UC…`,
and a channel's *Videos only* or *Shorts only* list becomes
`https://www.youtube.com/feeds/videos.xml?playlist_id=…`. Sources added with
an older version keep the URL they had.

Before confirming you can edit the title, pick or create a folder and, for a
YouTube channel, choose what to follow (see [Videos and Shorts](#videos-and-shorts)).

The **5 newest** entries start as unread; older ones are stored as already
read, and you can page back to them with **Load more**. A feed only contains
its most recent items (on YouTube, the last 15 videos), so feedreader can't
show anything older than what the feed offered when you subscribed.

## Polling and retries

**Schedule.** Every source is checked every 6 hours by default
(`FR_POLL_INTERVAL`), and YouTube channels and playlists every hour
(`FR_POLL_INTERVAL_YOUTUBE`), since their feeds only list the latest 15
videos. Each source can have its own interval in its **Feed settings**. At
most 4 fetches run at once (`FR_FETCH_WORKERS`), and nothing is fetched until
you add your first source. Requests are conditional (`ETag` /
`Last-Modified`), so an unchanged feed costs the site almost nothing.

**Refresh.** The **Refresh** button checks the current source — or every
source — right away. When the checks are done, the grid reloads if new
entries arrived that it can show; otherwise it stays as it is (including the
pages you loaded) and tells you there's nothing new. It gives up waiting
after about a minute and shows what has arrived so far; reload the page later
for slower sources.

**Failures.** A failed check is retried after 15 minutes, then less and less
often, down to once a day, and a server's `Retry-After` is always honoured.
YouTube feeds that answer `404` or a `5xx` error are an exception: YouTube's
feed server does this for working channels, sometimes for hours, so those are
retried after 15 minutes and then every hour (their normal interval) instead
of backing off. Rate limits (`429`) and network errors still back off.

A ⚠ next to a source means its last 3 scheduled checks failed; the source's
settings show the error. A check you start yourself with **Refresh** shows its
error if it fails, but doesn't count as one more failure and never pushes the
next scheduled check later (unless the server asks for a longer wait with
`Retry-After`).

Until a failing source succeeds again, changing its check interval or a
digest's ingestion time leaves its next check where it is, so a server's
`Retry-After` is still respected.

## Digests

A digest gathers in one view the sources you pick, every source in the
folders you pick (including ones added to those folders later), and every
entry carrying the tags you pick — each entry only once.

A digest can have a **daily ingestion time**: its sources, and those in its
folders, are then fetched every day at that time, so the digest is ready when
you sit down to read it (say 07:00 for a morning read). These fetches come on
top of the regular schedule, never instead of it. They also mean a failing
source is retried at that time rather than backing off past it, unless the
server asked for a longer wait with `Retry-After`.

The time is in the zone named by the `TZ` environment variable, or the
system's zone when it's unset; the digest's settings page shows which one is
in use (see [Configuration](../README.md#configuration)).

Deleting a digest leaves its sources, folders, tags and entries alone.
Deleting a folder or a tag removes it from every digest that names it; the
confirmation lists which ones.

## YouTube

### Channel avatars

YouTube channels show their avatar, read from the channel's page on YouTube.

- For an `@handle`, `/c/…` or `/user/…` URL, that page is already fetched to
  find the channel ID, so the avatar costs no extra request. Otherwise (or
  when that page has none) subscribing takes one extra request.
- A channel still without an avatar — subscribed with an older version, or
  whose page couldn't be read — gets one at its next successful check, with
  one try per channel each time feedreader starts.
- A channel followed as *Videos only* or *Shorts only* shows the channel's
  avatar too. Other playlists, legacy `feeds/videos.xml?user=…` feeds and any
  source whose icon is missing or broken show their initials instead.

### Videos and Shorts

A YouTube channel can be followed in three ways, picked under **Follow** when
you add it and changeable at any time in its settings:

- **Videos and Shorts** (the default) — the channel's own feed, everything it
  uploads.
- **Videos only, no Shorts** — the list YouTube keeps of the channel's regular
  videos (`feeds/videos.xml?playlist_id=UULF…`).
- **Shorts only** — the list of its Shorts (`…playlist_id=UUSH…`).

The two lists use the channel ID with `UC` replaced by `UULF` or `UUSH`. They
aren't documented by YouTube and could stop working, and live streams may be
missing from the regular-videos list.

If the list you chose doesn't answer (a channel without Shorts may have no
Shorts list at all), that counts as a failure of the source, like any feed
that's down, and the ⚠ appears after 3 failures. feedreader never falls back
to the channel's full feed, which would bring back exactly what you chose to
leave out. While a list is down, the [fallback](#when-youtubes-feed-is-down)
reads only the matching tab of the channel's page.

**Switching** an existing source keeps its title, folder, interval and
entries (nothing is hidden or deleted) and fetches the new list right away —
unless YouTube is rate-limiting the source (its last check failed with `429`
or a `Retry-After`), in which case it waits until that check was due anyway.
Of the videos the new list brings that weren't stored yet, the 5 newest are
unread and the rest are stored as read, just like when adding a source.
Switching to a list you already follow as another source is refused.

### When YouTube's feed is down

YouTube's feed server sometimes answers `404` or `5xx` for perfectly working
channels, for minutes or hours, while the rest of YouTube is fine. When that
happens feedreader reads the channel's latest videos from YouTube's website
instead. This is on by default; set `FR_YOUTUBE_FALLBACK=false` to only retry
the feed.

**When it kicks in.** A check of a channel feed, or of a *Videos only* /
*Shorts only* list, fails with `404` or `5xx` *without* a `Retry-After`
header. With a `Retry-After`, YouTube is limiting requests and nothing more is
asked of it.

**What it reads.** feedreader asks YouTube's web API for the channel's
*Videos* tab, its *Shorts* tab, or — for a channel followed with everything it
uploads — both of those plus its *Live* tab, which holds recordings of past
live streams. It takes the 15 newest of each (never further pages), then
fetches the details of each video it hasn't stored yet (at most 15), one
request at a time, at least a second apart.

**What gets stored.** Videos are stored exactly as the feed would list them
(same link, title, description and date), so nothing is duplicated when the
feed comes back. The source's failures are cleared and the check is logged
with `source=innertube`. A few things are left for a later check: videos with
no publication date (or one more than an hour in the future), live streams,
and premieres that haven't aired yet. Since a tab can list more videos than
the feed did, new videos older than the newest one the source already has are
stored as read.

**When YouTube refuses.** feedreader stops using the web API for an hour,
twice as long after each further refusal in a row (or longer if YouTube asks),
up to a day. This pause is shared with [transcripts](#transcripts): a refusal
of either stops both. When the fallback can't help — paused, or failing — the
check counts as the feed's own failure, under the
[usual rules](#polling-and-retries).

**Adding a channel while its feed is down.** The preview lists the videos
read from YouTube's website, with a note saying so, and subscribing stores
them as usual (the 5 newest unread). If that finds no video it can store, it
counts as failed.

If the fallback fails too (or is turned off), and you're adding a channel feed
— which every `@handle`, `/c/…`, `/user/…` and `/channel/UC…` URL resolves to
— feedreader checks the channel's page to confirm the channel exists and
offers to **subscribe anyway**, using the name and avatar from that page. The
source then shows *Waiting for the first fetch* (an hourglass in the sidebar
instead of the ⚠) and is retried after 15 minutes, then every hour. When the
feed or the fallback finally answers, its videos arrive as if you'd just added
it. A channel whose feed never comes back keeps waiting until you unsubscribe.

This "subscribe anyway" option is never offered for playlists (including a
channel's *Videos only* / *Shorts only* lists — pick those in the source's
settings once it's subscribed), for other sites, or for rate limits (`429`)
and network errors. The server re-checks it when you confirm, whatever the
form says.

### Transcripts

feedreader can keep the transcript of YouTube videos and show it in the
**Transcription** tab of a video's details. **Download** saves it as a
Markdown file named `<title> - transcript.md`, with the video's title,
channel, link, publication date and caption track (language, automatic or
authored), followed by the transcript's paragraphs. Timestamps aren't kept.

**Turning it on.** Transcripts are off until you tick **Fetch transcripts**
under *Video transcripts* at the bottom of the sidebar and press **Save**.
Each YouTube source can be left out with the **Video transcripts** switch in
its **Feed settings** (on by default). `FR_TRANSCRIPTS=false` is the server's
kill switch: no transcript is ever requested, and the sidebar switch is
greyed out. Turning a switch off freezes the queue (nothing is requested,
nothing is lost); turning it back on resumes it.

**Which videos.** Only videos that a check finds *new* after transcripts are
turned on are queued automatically. That excludes the videos already stored,
those already in a channel's feed when you add it, those delivered by the
first answer of a channel subscribed while its feed was down, and those a
list brings at its first answer after you switch between videos and Shorts.

For any other video, open its details: the line under the tabs says where its
transcript stands (not requested, queued and when it's next checked, waiting
because YouTube is limiting requests, no captions, or given up and why), with
a **Fetch transcript**, **Check now** or **Try again** button that queues it.
The button never makes a request sooner than the rules below allow.

**How it's fetched.** YouTube has no public API for transcripts, so
feedreader asks for captions the way YouTube's Android app does, with no API
key or cookies: two requests to `www.youtube.com` per check, one to its
player API and one for the caption track in the video's original language
(authored captions before automatic ones). Neither request follows a
redirect. This is unofficial and best-effort, and may break whenever YouTube
changes that API.

It works from a home internet connection at a low rate, while requests from
data-centre addresses, or too many at once, get refused. So feedreader is
deliberately slow:

- one video at a time, 10 seconds apart;
- after a refusal, an hour's pause before the next request, twice as long
  after each further refusal in a row (or longer if YouTube asks), up to a
  day — even across a restart. The pause is shared with the
  [fallback](#when-youtubes-feed-is-down).

**Retries.**

- A video without captions is checked once more 6 hours later, since YouTube
  adds automatic captions some time after publishing.
- Premieres and live streams are checked every 6 hours until they've aired,
  so the transcript is fetched after the broadcast.
- Any other failure (a network or YouTube outage, a video YouTube won't play
  to this client, an answer feedreader doesn't understand) is retried 2 hours
  later, then 4 hours after that: 3 tries in all.
- A caption track larger than `FR_FETCH_MAX_BODY` isn't asked for again.
- A video still queued a week after it was queued (or after transcripts were
  turned back on) is given up on.

A video without a transcript simply shows its description alone.
