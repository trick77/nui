# Integrating a product

For whoever adds Stakeout to another product, an agent included. A product sends one or both:

- **Browser script**: page views, clicks, forms, errors and web vitals of the people on its pages.
- **Server feed**: every request its backend serves, so bots, probes and API callers show up too.

The collector's limits are in the [README](README.md#browser-script); this page says what to build.

## On the Stakeout host (not in the product's repo)

The product's repo cannot do this part. The integrating side hands the operator a `sites.json` entry and, for a
feed, the name of the variable the product reads its key from.

- `domain`: the exact hostname the pages are served from, case-insensitive. `www.example.com` and `example.com` are
  two sites. A page on a host not listed gets 403 and its script switches itself off.
- `product`: the product's name in the dashboard. `kind`: `site` (pages) or `api`.
- `path_rules`: raw paths to templates, first match wins, unmatched paths go to a heuristic. Placeholders: `{int}`,
  `{seg}` (one segment), `{text}` (one segment that is no file name), `{any}` (slashes included), `{ipv4}`,
  `{ipv6}`, `{cidr}`. Literal text matches case-insensitively. Example: [docs/sites.example.json](docs/sites.example.json).
- `feed_key_sha256`: empty without a feed. With one, `docker compose run --rm stakeout -new-feed-key` prints a key (to
  the product's secrets) and its hash (here).
- Stakeout reads the file at start: restart it after the change.

## Browser script

On every page, in `<head>`:

```html
<script src="https://stakeout.example.com/stakeout.js" defer></script>
```

- Single-page apps need nothing more: `pushState`, `replaceState`, `popstate` and `hashchange` are page views.
- The page's Content-Security-Policy needs the Stakeout host in `script-src` and `connect-src`. The script uses no eval,
  no inline script and no `innerHTML` (Trusted Types pass); it is served with `Cross-Origin-Resource-Policy:
  cross-origin`.
- Collected on its own: session start, page views, outbound clicks, downloads, form start and submit (the form's
  id, name and action only), errors (script, resource, unhandled rejection), web vitals (TTFB, FCP, LCP, INP, CLS).
  The only input value ever read is a search term: an `<input type="search">`, or a text input named `q`, `query`,
  `search`, `s` or `term`.
- To tell people from automated browsers it also reads: globals and attributes automation tools leave in the page
  (sent as fixed tokens, never the names), where `navigator.webdriver` is defined, window frame size, plugin count
  and `pdfViewerEnabled`, the notification permission against the Permissions API, the WebGL renderer (an offscreen
  canvas, once per site in each browser, only in an idle period, kept in `localStorage` as `stakeout.gl`; not in
  Safari), mouse moves and presses, input events a script dispatched.
- Local development on a host not in `sites.json` is refused (403) and the script stops: no switch needed. Including
  the tag only in production builds is fine too.

Custom events, for actions the defaults miss (a signup, a plan chosen):

```js
window.stakeout.event("signup", { plan: "pro" });
```

The name is clipped to 128 bytes; props follow the README's limits for `p` (64 keys, 50 array items, 8 levels,
4 KiB). Never put personal data in a name or props. Calls before the script has loaded need a stub, replayed on
load (the first 50):

```html
<script>window.stakeout=window.stakeout||{q:[],event:function(){this.q.push(arguments)}}</script>
```

## Server feed

`POST https://stakeout.example.com/api/feed`, `Authorization: Bearer <key>`, `Content-Type: application/json`:

```json
{"v": 1, "site": "example.com", "events": [
  {"t": 1764000000000, "ip": "203.0.113.7", "method": "GET", "path": "/users/42", "route": "GET /users/{id}",
   "status": 200, "dur_ms": 12, "bytes": 5123, "ua": "Mozilla/5.0 ...", "ref": "https://example.org/",
   "lang": "en-US,en;q=0.9", "ch_ua": "\"Chromium\";v=\"142\"", "sf_site": "cross-site", "sf_mode": "navigate",
   "sf_dest": "document", "sig_agent": "\"https://chatgpt.com\"", "account": "u_8f3a"}
]}
```

`site` is the key's own domain; another is a 400.

| Field | What |
|---|---|
| `t` | Unix milliseconds the request arrived. Over an hour behind or a minute ahead, Stakeout uses the receive time. |
| `ip` | The client's address as the product resolves it, after its own trusted proxies. Required: without one the event is dropped. |
| `method`, `path` | The request line. Query and fragment are dropped: strip them before sending, they can hold tokens. |
| `route` | The router's pattern with the method, as Go's `r.Pattern` gives it (`GET /users/{id}`). Empty for an unmatched request: Stakeout then names the path by the site's rules. |
| `status`, `dur_ms`, `bytes` | Response status, handler time in ms, response body bytes. |
| `ua`, `ref`, `lang` | `User-Agent`, `Referer`, `Accept-Language`. |
| `ch_ua`, `sf_site`, `sf_mode`, `sf_dest` | `Sec-CH-UA`, `Sec-Fetch-Site`, `Sec-Fetch-Mode`, `Sec-Fetch-Dest`. |
| `sig_agent` | `Signature-Agent` (Web Bot Auth): the agent a client says it is, as sent. Empty when absent. |
| `account` | The signed-in account, if any: an opaque id, never an email or a name. |

Send exactly these fields, never raw headers (they carry cookies and credentials). Every request counts, 404s and
401s above all: the probing is what the feed is for. Leave out only the emitter's own traffic and local health
checks.

The emitter must never slow down or break the product:

- Record in a middleware after the handler, into a bounded in-memory queue. A full queue drops the event, never
  blocks the request.
- One background sender: a batch every few seconds or at 500 events, at most 256 KiB, a 30 s timeout.
- 408, 429 and 5xx: send the same batch again after `Retry-After` (or a growing back-off without one). 400, 401 and
  413 will be refused again: drop the batch and log it.
- Stakeout unreachable or the key unset (local development): the product runs as before, the feed is off.
- The key from a `BACKEND_*` variable (`BACKEND_STAKEOUT_FEED_KEY`), the Stakeout URL beside it; neither in code.
