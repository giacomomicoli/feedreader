# Security

## Threat model

feedreader is a **single-user application for a private network** and has **no
authentication** by design. Anyone who can reach its HTTP port can read and
change everything. Its security therefore rests on where you run it:

- Put it behind a reverse proxy that only admits clients from your network
  (example: [deploy/Caddyfile.snippet](deploy/Caddyfile.snippet)), and make sure
  only the proxy can reach feedreader's port: bind `FR_LISTEN` to the right
  interface and firewall the port. Then check from another machine on your
  network that the port really is closed. The default listen address is
  `127.0.0.1:8080`.
- Never expose it directly to the internet. For remote access use a VPN such as
  Tailscale or WireGuard.

Within that model the application defends against attacks that reach a LAN
service through the user's own browser or through hostile feed content:

| Threat | Mitigation |
| --- | --- |
| Malicious feed HTML (XSS) | All feed HTML is sanitized with an allow-list (bluemonday) before storage; the UI renders summaries only as escaped plain text; `html/template` auto-escaping and URL filtering; strict Content-Security-Policy (`script-src 'self'`, no inline script or style). |
| Cross-site request forgery from other websites | Go's `http.CrossOriginProtection` rejects cross-origin state-changing requests (`Sec-Fetch-Site` / `Origin` checks); all state changes use POST. |
| DNS rebinding (a hostile web page whose host name is pointed at your server) | Host allowlist: requests whose `Host` is not `localhost`, an IP literal, the `FR_LISTEN` host or a name in `FR_ALLOWED_HOSTS` are refused with 421 before any handler runs; `X-Forwarded-Host` is ignored. |
| Clickjacking | `frame-ancestors 'none'` and `X-Frame-Options: DENY`. |
| Leaking your server's host name to third parties | `Referrer-Policy: no-referrer`; outbound links use `rel="noopener noreferrer"`. |
| Oversized or hostile feeds | 10 MB cap on the decompressed body (gzip-bomb safe), request timeouts, at most 5 redirects, http/https only. Because parsing can need far more memory than the document's size, documents are also checked against structural limits before parsing (nesting depth 100, 200,000 XML elements and attributes, 64 namespaces in scope, 10,000 items; 200,000 JSON values), and each HTML field is bounded before and after sanitizing. Titles are capped at 200 characters and URLs at 2048 bytes. |
| Abusing "subscribe anyway" (a YouTube channel whose feed is down) to store a source that was never checked | The confirm form only carries the user's consent; the server decides. It stores such a subscription only for the canonical YouTube channel feed URL, built from a channel ID matching the `UC` + 22 characters format, after its own fetch of that feed answered `404` or `5xx`, and after a page of that channel served from YouTube named the same channel: `https://www.youtube.com/channel/<id>` (built from that ID alone, one request), or the channel's own `@handle`, `/c/…` or `/user/…` page fetched moments before to resolve the URL. The channel name read from that page is reduced to one line of plain text of at most 200 characters and rendered escaped. |
| Hostile answers to transcript requests (off until turned on in the UI; `FR_TRANSCRIPTS=false` forbids them) | At most two requests each time a video is checked, both to `www.youtube.com`: a POST to its player API, and a GET of the caption URL from that answer, made only when it is `https://www.youtube.com/api/timedtext` for the same video. Neither follows a redirect, so the signed caption URL reaches no other host. Each time a video is queued (by a poll, or by the user’s button) it is checked a bounded number of times, hours apart: at most 3 failed tries (one when its caption track is over the size cap), one more check after an answer without captions, and every 6 hours for at most a week while it has not aired; after a refusal nothing is requested for at least an hour, also after a restart. Answers get the same size cap and timeouts as feeds and are decoded within fixed limits (4 MB player answer, 200 caption tracks); the transcript is cut at 200,000 characters, stored as plain text and shown only as escaped text, loaded only when its tab is opened. Requests are made only while all three switches allow them (`FR_TRANSCRIPTS`, the global setting, the source's), checked when a video is queued and again before each request; turning a switch on queues nothing. The "Fetch transcript" button (a POST, behind the same cross-origin protection as every action) only queues a video of a YouTube source: it cannot make requests faster than the queue's 10-second spacing and refusal pause, and a fetched transcript is never requested again. |
| SQL injection | Parameterized queries only. |
| Credentials in feed URLs (`https://user:password@…`, or a token as the user name) leaking into logs | Log lines, error messages and stored fetch errors show such URLs with their whole userinfo replaced by `xxxxx`. |

Out of scope: protection against other people on the same network (there is no
authentication by design).

## Reporting a vulnerability

Please report security issues privately through GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
("Security" tab → "Report a vulnerability") rather than opening a public issue.
