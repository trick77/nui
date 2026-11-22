# nui

Self-hosted, passive usage analytics for your own web products. One host serves the collector under `/api` and the
dashboard under `/dashboard`.

- A browser script (`/nui.js`) and an optional server feed from your backend deliver events.
- The dashboard (behind a sign-in: OIDC, an admin token, or both) shows per-domain figures, trends and who is asking: people, known bots, uptime
  probes and unknown automation, each with the reasons it was classified that way.
- With an [llmwire](https://github.com/trick77/toolkit) model key it adds a short briefing and readings of
  automated visitors; every figure is computed without it.

The name: *nui* is Hawaiian and Māori for "many", for all the data that comes in.

## Development

```sh
cd ui && npm install
npm run dev        # http://127.0.0.1:5173, /api proxied to the Go server on :8080
npm test           # unit tests (vitest)
make check         # the full gate (ship), from the repo root
```

The Go server embeds the built UI: `npm run build` in `ui/`, then in `backend/`:

```sh
mkdir -p data && cp ../docs/sites.example.json data/sites.json
BACKEND_DATA_DIR=./data BACKEND_AUTH_DISABLED=true go run ./cmd/server -demo 30
```

and open <http://localhost:8080/dashboard/>. `-demo 30` fills an empty database with 30 days of synthetic traffic.

## Running it

`compose.yaml` runs the released image behind an existing Traefik. A deployment is three files in one directory:
`compose.yaml`, `.env` and `data/sites.json`. The dashboard is at `https://<PUBLIC_FQDN>/dashboard/`; the bare host
answers 404 like any unknown path and does not lead there.

```sh
curl -fsSLO https://raw.githubusercontent.com/trick77/nui/master/compose.yaml
curl -fsSL -o .env https://raw.githubusercontent.com/trick77/nui/master/.env.example
mkdir -p data
curl -fsSL -o data/sites.json https://raw.githubusercontent.com/trick77/nui/master/docs/sites.example.json
# edit .env: PUBLIC_FQDN, the OIDC client and/or BACKEND_ADMIN_TOKEN, BACKEND_SESSION_SECRET
# edit data/sites.json: your domains (next section)
sudo chown -R 65532:65532 data
docker compose pull && docker compose up -d
```

### Sign-in

The dashboard signs in with OIDC (`BACKEND_OIDC_*`), with an admin token (`BACKEND_ADMIN_TOKEN`, 32 to 2048 bytes,
`openssl rand -hex 32`), or with either: the sign-in card offers what is configured. With `PUBLIC_FQDN` set nui
refuses to start with neither. The token is the way in when the identity provider is down; its session is named
`admin`. The session cookie is signed with a key derived from a session secret together with the token: changing
either signs everyone out. The secret is `BACKEND_SESSION_SECRET`, or, left empty, one nui makes at first start and
keeps in `data/session.secret` (owner-only; a data directory nui cannot write stops the start). Token attempts are
spaced a second apart per address or IPv6 /64, half a second per IPv4 /24 or IPv6 /48 and a quarter of a second
across all callers: at most 4 guesses a second in all. Past a short queue they get a 429 with `Retry-After`.

nui reads the caller's address from `X-Forwarded-For` as many entries from the right as there are proxies in front
of it (`BACKEND_TRUSTED_HOPS`, 1 for the compose file's Traefik). Reached without a proxy (a published port, a local
run), set it to 0: the caller could otherwise write its own address into the header.

### Sites

nui only stores events for the sites listed in `data/sites.json` (the container's `/data/sites.json`; shape in
[docs/sites.example.json](docs/sites.example.json)): domain, product, path rules that group raw paths into
templates, and for a server feed the SHA-256 of its key. The file is read at start: restart nui after changing it.
Without it nui still starts, logs `Sites file missing`, refuses every event (403) and the dashboard lists no domain;
with it, the start log names the loaded domains (`Sites loaded`).

A site without a server feed leaves `feed_key_sha256` empty. For one with a feed, create a key and its hash with

```sh
docker compose run --rm nui -new-feed-key
```

The key goes to the product's feed emitter (`Authorization: Bearer <key>` on `POST /api/feed`), the hash into
`sites.json`.

### Browser script

On a page, include the script:

```html
<script src="https://nui.example.com/nui.js" defer></script>
```

It posts batches to `POST /api/in` and keeps every batch within the collector's limits on its own; a batch over
one is refused whole (413, or 400 for a malformed one) and its events are lost. For anyone sending there
otherwise, the limits are:

| What | Limit |
|---|---|
| Body | 32 KiB, 50 events |
| `url`, `ref`, `src`, `href` values; an event's `url` | 2048 bytes |
| Every other string, `title`, `n`, `vid`, `sid` | 512 bytes |
| An event's `p` | 4 KiB as sent; maps 64 keys of at most 64 bytes, arrays 50 items, 8 levels |
| `ctx` without `url` and `ref`, as stored | 4 KiB |
| An event's own row, as stored | 16 KiB (the batch's context and headers aside) |
| A batch's rows, as stored | 64 KiB |

"As stored" counts the event's columns, its `url` with its path, a page view's referrer and the props re-encoded
(U+2028 and U+2029 take six bytes each); the request's client-hint and fetch headers are kept up to 2 KiB, on the
batch's first row. The rate is 60 events a minute per address or IPv6 /64 and 600 per /48 (IPv4 /24), with a burst
of 60; a batch is charged one event per event or per 2 KiB stored, whichever is more, and every request pays one on
arrival (429 with `Retry-After` beyond it). `Retry-After` covers the retry: the same batch sent again after it
passes, its arrival token included, unless other requests from the address spent the tokens meanwhile. A body not
received within 10 s is a 408: send it again.

### Server feed

`POST /api/feed` takes `{"v": 1, "site": "<domain>", "events": [...]}` as JSON, at most 256 KiB and 500 events per
batch, rate-limited per key (200 events a second, a burst of 5000): a batch is charged one per event it stores and
passes while the key has a token left, running it into debt; a key without one is refused before its body is read.
`Retry-After` covers the retry: the same batch sent again after it passes. The batch as a whole is refused only for an unparseable envelope (400), a wrong key
(401), another site's name (400), its size or event count (413), a body not received within 30 s (408) or the rate
(429). One event's content never refuses the batch: strings over their limit are clipped (on a character boundary),
and an event that is no JSON object, has a field of the wrong type, lacks a parseable `ip` or has an impossible
`status` (outside 0-999), `dur_ms` or `bytes` (negative) is dropped and counted as rejected in the dashboard's
footer, the rest is stored. A request with a wrong key names no site nui can trust: it is counted collector-wide,
with the unknown domains every footer shows.

| Field | Limit (bytes) |
|---|---|
| `method` | 16 |
| `sf_site`, `sf_mode`, `sf_dest` | 32 |
| `account` | 128 |
| `route` | 200 |
| `ua`, `lang`, `ch_ua` | 512 |
| `path`, `ref` | 2048 (query and fragment dropped) |
| `ip` | an IPv4 or IPv6 address; a zone (`%eth0`) is dropped |

Send only these fields, never raw request headers: they can carry credentials. A refused batch (408, 429, 5xx)
can be sent again; 400, 401 and 413 will be refused again. Events an accepted batch dropped are counted as
rejected once; a batch refused for the rate is not counted until it is taken.

### Dashboard API

`/api/dash/*` is the dashboard's own API (a session required). For links into it: the log's `addr` filter takes an
address or an IPv6 /64 (any other prefix is a 400), its `asn` filter an AS number.

## License

MIT
