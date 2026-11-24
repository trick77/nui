# Integrating a product

For whoever adds Trakk to another product, an agent included. A product sends one or both:

- **Browser script**: page views, clicks, forms, errors and web vitals of the people on its pages.
- **Server feed**: every request its backend serves, so bots, probes and API callers show up too.

The collector's limits are in the [README](README.md#browser-script); this page says what to build.

## On the Trakk host (not in the product's repo)

The product's repo cannot do this part. The integrating side hands the operator a `sites.json` entry and, for a
feed, a place for the key in the product's `.env` ([Settings](#settings)).

- `domain`: the exact hostname the pages are served from, case-insensitive. `www.example.com` and `example.com` are
  two sites. A page on a host not listed gets 403 and its script switches itself off.
- `product`: the product's name in the dashboard. `kind`: `site` (pages) or `api`.
- `path_rules`: raw paths to templates, first match wins, unmatched paths go to a heuristic. Placeholders: `{int}`,
  `{seg}` (one segment), `{text}` (one segment that is no file name), `{any}` (slashes included), `{ipv4}`,
  `{ipv6}`, `{cidr}`. Literal text matches case-insensitively. Example: [docs/sites.example.json](docs/sites.example.json).
- `feed_key_sha256`: empty without a feed. With one, `docker compose run --rm trakk -new-feed-key` prints a key (to
  the product's secrets) and its hash (here).
- Trakk reads the file at start: restart it after the change.

## Browser script

On every page, in `<head>`:

```html
<script src="https://trakk.example.com/trakk.js" defer></script>
```

- Single-page apps need nothing more: `pushState`, `replaceState`, `popstate` and `hashchange` are page views.
- The page's Content-Security-Policy needs the Trakk host in `script-src` and `connect-src`. The script uses no eval,
  no inline script and no `innerHTML` (Trusted Types pass); it is served with `Cross-Origin-Resource-Policy:
  cross-origin`.
- Collected on its own: session start, page views, outbound clicks, downloads, form start and submit (the form's
  id, name and action only), errors (script, resource, unhandled rejection), web vitals (TTFB, FCP, LCP, INP, CLS).
  The only input value ever read is a search term: an `<input type="search">`, or a text input named `q`, `query`,
  `search`, `s` or `term`.
- To tell people from automated browsers it also reads: globals and attributes automation tools leave in the page
  (sent as fixed tokens, never the names), where `navigator.webdriver` is defined, window frame size, plugin count
  and `pdfViewerEnabled`, the notification permission against the Permissions API, the WebGL renderer (an offscreen
  canvas, once per site in each browser, only in an idle period, kept in `localStorage` as `analytics.gl`; not in
  Safari), mouse moves and presses, input events a script dispatched.
- Local development on a host not in `sites.json` is refused (403) and the script stops: no switch needed. Including
  the tag only in production builds is fine too.

Custom events, for actions the defaults miss (a signup, a plan chosen):

```js
window.trakk.event("signup", { plan: "pro" });
```

The name is clipped to 128 bytes; props follow the README's limits for `p` (64 keys, 50 array items, 8 levels,
4 KiB). Never put personal data in a name or props. Calls before the script has loaded need a stub, replayed on
load (the first 50):

```html
<script>window.trakk=window.trakk||{q:[],event:function(){this.q.push(arguments)}}</script>
```

## Server feed

`POST https://trakk.example.com/api/feed`, `Authorization: Bearer <key>`, `Content-Type: application/json`:

```json
{"v": 1, "site": "example.com", "events": [
  {"t": 1764000000000, "ip": "203.0.113.7", "method": "GET", "path": "/users/42", "route": "GET /users/{id}",
   "status": 200, "dur_ms": 12, "bytes": 5123, "ua": "Mozilla/5.0 ...", "ref": "https://example.org/",
   "lang": "en-US,en;q=0.9", "ch_ua": "\"Chromium\";v=\"142\"", "sf_site": "cross-site", "sf_mode": "navigate",
   "sf_dest": "document", "sig_agent": "\"https://chatgpt.com\"", "account": "u_8f3a",
   "op": "", "err": ""}
]}
```

`site` is the key's own domain; another is a 400.

| Field | What |
|---|---|
| `t` | Unix milliseconds the request arrived. Over an hour behind or a minute ahead, Trakk uses the receive time. |
| `ip` | The client's address as the product resolves it, after its own trusted proxies. Required: without one the event is dropped. |
| `method`, `path` | The request line. Query and fragment are dropped: strip them before sending, they can hold tokens. |
| `route` | The router's pattern with the method, as Go's `r.Pattern` gives it (`GET /users/{id}`). Empty for an unmatched request: Trakk then names the path by the site's rules. |
| `status`, `dur_ms`, `bytes` | Response status, handler time in ms, response body bytes. |
| `ua`, `ref`, `lang` | `User-Agent`, `Referer`, `Accept-Language`. |
| `ch_ua`, `sf_site`, `sf_mode`, `sf_dest` | `Sec-CH-UA`, `Sec-Fetch-Site`, `Sec-Fetch-Mode`, `Sec-Fetch-Dest`. |
| `sig_agent` | `Signature-Agent` (Web Bot Auth): the agent a client says it is, as sent. Empty when absent. |
| `account` | The account the request acted as, once its key or session was checked: an opaque id, never an email or a name. Never one a client merely claims (a login attempt's user name). Report it on every request that came with a good key or session: Trakk takes a client as signed in when at least half of its requests carry one, those answered 401, 403 or 429 not counted as carrying it. A signed-in program is an API client, whatever its address or user agent is listed as; a signed-in browser a person's once it loaded a document or an asset, sent Sec-Fetch-Site, or its script reported. |
| `op` | What the request did inside its route, where one route serves several things (an MCP tool, an RPC method): the operation's name. Empty when the route says it all. |
| `err` | Why the request failed, where its status does not say it (a JSON-RPC error answered 200, a 429 that is a quota or a rate limit): the product's own code (`invalid_params`). Empty on success. |

`op` and `err` are names from fixed lists of the product's own, never text a client chose (an unknown tool's name):
Trakk lists and groups by them. Both are optional; anything but ASCII letters, digits and `_ . / : -` is stored as
`_`. A call failed when `err` is set or its status is 400 or above; a 404 is no call, whatever its route, nor is a 401,
403 or 405 without a `route` (a scanner's path behind a key check, an MCP client's `GET` of a POST-only path). A site
of kind `api` is shown by them: calls, accounts and failures per endpoint (`route` and `op`), with durations.

Send exactly these fields, never raw headers (they carry cookies and credentials). Every request counts, 404s and
401s above all: the probing is what the feed is for. Leave out only the emitter's own traffic and local health
checks.

The emitter must never slow down or break the product:

- Record in a middleware after the handler, into a bounded in-memory queue. A full queue drops the event, never
  blocks the request.
- One background sender: a batch every few seconds or at 500 events, at most 256 KiB, a 30 s timeout.
- 408, 429 and 5xx: send the same batch again after `Retry-After` (or a growing back-off without one). 400, 401 and
  413 will be refused again: drop the batch and log it.
- Trakk unreachable or the key unset (local development): the product runs as before, the feed is off.

## Settings

The same three variables in every product, under these names, no prefix of the product's own:

| Variable | What |
|---|---|
| `ANALYTICS_URL` | Trakk's origin. Alone it turns on the browser script: the tag and the two CSP entries. |
| `ANALYTICS_FEED_KEY` | The site's feed key, a secret. Adds the server feed; needs the other two. |
| `ANALYTICS_SITE` | The domain the key was issued for, the feed's `site`. |

- All three come from `.env`: no value in code, none written into `compose.yaml`, no site derived from another
  setting. `.env.example` lists them.
- Compose passes `.env` on only for what `compose.yaml` names: list all three as `${ANALYTICS_URL:-}` and so on. A
  deployment needs the new `compose.yaml` together with the image.
- All unset: no tag, the CSP as before, no feed.
