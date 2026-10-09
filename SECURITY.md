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
| SQL injection | Parameterized queries only. |
| Credentials in feed URLs (`https://user:password@…`, or a token as the user name) leaking into logs | Log lines, error messages and stored fetch errors show such URLs with their whole userinfo replaced by `xxxxx`. |

Out of scope: protection against other people on the same network (there is no
authentication by design).

## Reporting a vulnerability

Please report security issues privately through GitHub's
[private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)
("Security" tab → "Report a vulnerability") rather than opening a public issue.
