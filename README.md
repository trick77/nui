# nui

Self-hosted, passive usage analytics for your own web products. One host serves the collector under `/api` and the
dashboard under `/dashboard`.

- A browser script (`/nui.js`) and an optional server feed from your backend deliver events.
- The dashboard (behind OIDC) shows per-domain figures, trends and who is asking: people, known bots, uptime
  probes and unknown automation, each with the reasons it was classified that way.
- With an [llmwire](https://github.com/trick77/toolkit) model key it adds a short briefing and readings of
  automated visitors; every figure is computed without it.

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
`compose.yaml`, `.env` and `data/sites.json`.

```sh
curl -fsSLO https://raw.githubusercontent.com/trick77/nui/master/compose.yaml
curl -fsSL -o .env https://raw.githubusercontent.com/trick77/nui/master/.env.example
mkdir -p data
curl -fsSL -o data/sites.json https://raw.githubusercontent.com/trick77/nui/master/docs/sites.example.json
# edit .env: PUBLIC_FQDN, the OIDC client, BACKEND_SESSION_SECRET
# edit data/sites.json: your domains (next section)
sudo chown -R 65532:65532 data
docker compose pull && docker compose up -d
```

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

### Server feed

`POST /api/feed` takes `{"v": 1, "site": "<domain>", "events": [...]}` as JSON, at most 256 KiB and 500 events per
batch, rate-limited per key. The batch as a whole is refused only for bad JSON (400), a wrong key (401), another
site's name (400), its size or event count (413) or the rate (429). One event's content never refuses the batch:
strings over their limit are clipped (on a character boundary), and an event without a parseable `ip` or with an
impossible `status` (outside 0-999), `dur_ms` or `bytes` (negative) is dropped and counted as rejected in the
dashboard's footer, the rest is stored.

| Field | Limit (bytes) |
|---|---|
| `method` | 16 |
| `sf_site`, `sf_mode`, `sf_dest` | 32 |
| `account` | 128 |
| `route` | 200 |
| `ua`, `lang`, `ch_ua` | 512 |
| `path`, `ref` | 2048 (query and fragment dropped) |

Send only these fields, never raw request headers: they can carry credentials.

On a page, include the script:

```html
<script src="https://nui.example.com/nui.js" defer></script>
```

## License

MIT
